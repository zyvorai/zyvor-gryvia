// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

// Package nic reads RDMA NIC hardware counters from sysfs
// (/sys/class/infiniband/<dev>/ports/<n>/{counters,hw_counters}) and turns
// them into per-second rates.
//
// Why: NCCL posts work requests and rings doorbells from USERSPACE (libibverbs
// / the provider driver), so the kernel ib_post_send / ib_poll_cq kprobes of
// rdma_trace.c / rdma_health.c never see NCCL's data path. The NIC counters
// count everything the hardware moved, whichever path posted it.
//
// UNVERIFIED ON HARDWARE: developed and tested against fake sysfs trees laid
// out like the kernel documents; counter availability differs per vendor and
// driver (mlx5 exports hw_counters such as out_of_sequence, np_cnp_sent,
// rp_cnp_handled; other providers export others or none), so every file is
// optional. Nothing here fails when /sys/class/infiniband is absent.
package nic

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultRoot is the sysfs directory of RDMA devices.
const DefaultRoot = "/sys/class/infiniband"

// ErrNotPresent means the sysfs root does not exist (no RDMA devices or not mounted).
var ErrNotPresent = errors.New("no RDMA devices: " + DefaultRoot + " is absent")

// PortKey identifies one port of one device.
type PortKey struct {
	Device string
	Port   string
}

// Counters maps counter name -> raw value for one port (counters/ and
// hw_counters/ merged; on a name clash hw_counters wins).
type Counters map[string]uint64

// Snapshot is one read of every port.
type Snapshot struct {
	At    time.Time
	Ports map[PortKey]Counters
}

// Reader reads a sysfs tree. Root defaults to DefaultRoot.
type Reader struct{ Root string }

func (r Reader) root() string {
	if r.Root == "" {
		return DefaultRoot
	}
	return r.Root
}

// maxPorts / maxCounters bound what one read accepts from a hostile or odd tree.
const (
	maxDevices  = 64
	maxPorts    = 16
	maxCounters = 512
)

// Read snapshots all counters. Missing or unreadable files are skipped;
// ErrNotPresent is returned only when the root itself is absent.
func (r Reader) Read(now time.Time) (Snapshot, error) {
	snap := Snapshot{At: now, Ports: map[PortKey]Counters{}}
	devs, err := os.ReadDir(r.root())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return snap, ErrNotPresent
		}
		return snap, err
	}
	for i, d := range devs {
		if i >= maxDevices {
			break
		}
		ports, err := os.ReadDir(filepath.Join(r.root(), d.Name(), "ports"))
		if err != nil {
			continue
		}
		for j, p := range ports {
			if j >= maxPorts {
				break
			}
			c := Counters{}
			for _, sub := range []string{"counters", "hw_counters"} {
				readDir(filepath.Join(r.root(), d.Name(), "ports", p.Name(), sub), c)
			}
			if len(c) > 0 {
				snap.Ports[PortKey{d.Name(), p.Name()}] = c
			}
		}
	}
	return snap, nil
}

func readDir(dir string, into Counters) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if len(into) >= maxCounters {
			return
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil || len(b) > 64 {
			continue
		}
		v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
		if err != nil {
			continue // "N/A" and other non-numeric files
		}
		into[e.Name()] = v
	}
}

// counterBits are the widths of the classic (non-extended) IB PMA counters, used
// only to tell a 32/16/8-bit wrap from a counter reset. Unlisted counters are
// treated as 64-bit: a decrease is a reset.
var counterBits = map[string]uint{
	"symbol_error": 16, "port_rcv_errors": 16, "port_xmit_discards": 16, "VL15_dropped": 16,
	"port_rcv_remote_physical_errors": 16, "port_rcv_switch_relay_errors": 16,
	"link_downed": 8, "link_error_recovery": 8,
	"port_xmit_constraint_errors": 8, "port_rcv_constraint_errors": 8,
	"local_link_integrity_errors": 4, "excessive_buffer_overrun_errors": 4,
	"port_xmit_data": 32, "port_rcv_data": 32, "port_xmit_packets": 32, "port_rcv_packets": 32,
	"port_unicast_xmit_packets": 32, "port_unicast_rcv_packets": 32,
	"port_multicast_xmit_packets": 32, "port_multicast_rcv_packets": 32,
}

// Delta returns cur - prev with counter semantics: a decrease is a wrap when the
// counter has a known narrow width and prev was near the top of its range and
// cur is near the bottom, otherwise a reset (the delta is then cur, the
// count since the reset).
func Delta(name string, prev, cur uint64) uint64 {
	if cur >= prev {
		return cur - prev
	}
	if bits, ok := counterBits[name]; ok && bits < 64 {
		max := uint64(1) << bits
		if prev < max && prev >= max/4*3 && cur < max/4 {
			return cur + max - prev
		}
	}
	return cur
}

// Rates are per-second deltas between two snapshots.
type Rates struct {
	Interval time.Duration
	Ports    map[PortKey]map[string]float64
}

// Sampler keeps the previous snapshot and yields rates.
type Sampler struct {
	Reader Reader
	prev   *Snapshot
}

// Step reads counters at now. The first call (and any call after ErrNotPresent)
// returns ok=false: rates need two snapshots. Ports or counters present in only
// one snapshot are skipped for that step.
func (s *Sampler) Step(now time.Time) (Rates, bool, error) {
	cur, err := s.Reader.Read(now)
	if err != nil {
		s.prev = nil
		return Rates{}, false, err
	}
	prev := s.prev
	s.prev = &cur
	if prev == nil {
		return Rates{}, false, nil
	}
	dt := cur.At.Sub(prev.At)
	if dt <= 0 {
		return Rates{}, false, nil
	}
	out := Rates{Interval: dt, Ports: map[PortKey]map[string]float64{}}
	secs := dt.Seconds()
	for k, c := range cur.Ports {
		p, ok := prev.Ports[k]
		if !ok {
			continue
		}
		r := map[string]float64{}
		for name, v := range c {
			if pv, ok := p[name]; ok {
				r[name] = float64(Delta(name, pv, v)) / secs
			}
		}
		out.Ports[k] = r
	}
	return out, true, nil
}

// Categories of counters folded into the fabric status. Names are those
// documented for the Linux RDMA core and the mlx5 driver; a device that lacks
// them simply contributes nothing.
var (
	// RetryCounters are transport retry / sequence-error events (requester or responder).
	RetryCounters = []string{"out_of_sequence", "packet_seq_err", "local_ack_timeout_err", "implied_nak_seq_err", "rnr_nak_retry_err", "duplicate_request"}
	// ErrorCounters are physical/link errors and drops.
	ErrorCounters = []string{"symbol_error", "link_downed", "port_rcv_errors", "rx_discards_phy", "port_xmit_discards", "link_error_recovery"}
	// CNPHandled counts RoCEv2 CNPs this NIC received (the sender-side reaction
	// point); it is the counterpart of roce_cnp.c's ingress CNP count.
	CNPHandled = "rp_cnp_handled"
	// CNPSent counts CNPs this NIC generated for ECN-marked packets.
	CNPSent = "np_cnp_sent"
	// ECNMarked counts RoCE packets received with the ECN mark.
	ECNMarked = "np_ecn_marked_roce_packets"
)

// Totals sums the rates of the given counters across ports; present reports
// whether at least one port exported at least one of the counters.
func (r Rates) Totals(names []string) (sum float64, present bool) {
	for _, c := range r.Ports {
		for _, n := range names {
			if v, ok := c[n]; ok {
				sum += v
				present = true
			}
		}
	}
	return
}

// PauseTotal sums every rate whose name is an rx/tx pause frame counter (contains
// "pause", not a duration/transition counter).
func (r Rates) PauseTotal() (sum float64, present bool) {
	for _, c := range r.Ports {
		for n, v := range c {
			l := strings.ToLower(n)
			if strings.Contains(l, "pause") && !strings.Contains(l, "duration") && !strings.Contains(l, "transition") {
				sum += v
				present = true
			}
		}
	}
	return
}

// GaugeNames are the counters exported as per-port gauges (bounded label
// cardinality). *_data counters are in 4-byte units (see BytesPerDataUnit).
var GaugeNames = []string{
	"port_xmit_data", "port_rcv_data", "port_xmit_packets", "port_rcv_packets",
	"symbol_error", "link_downed", "port_rcv_errors", "port_xmit_discards",
	"out_of_sequence", "packet_seq_err", "local_ack_timeout_err", "implied_nak_seq_err",
	"np_cnp_sent", "rp_cnp_handled", "np_ecn_marked_roce_packets", "rx_discards_phy",
}

// BytesPerDataUnit: IB port_{xmit,rcv}_data count 32-bit words (4 bytes), per the
// IB spec and the kernel's sysfs ABI documentation.
const BytesPerDataUnit = 4

// SortedPorts returns the keys of r in stable order.
func (r Rates) SortedPorts() []PortKey {
	ks := make([]PortKey, 0, len(r.Ports))
	for k := range r.Ports {
		ks = append(ks, k)
	}
	sort.Slice(ks, func(i, j int) bool {
		if ks[i].Device != ks[j].Device {
			return ks[i].Device < ks[j].Device
		}
		return ks[i].Port < ks[j].Port
	})
	return ks
}
