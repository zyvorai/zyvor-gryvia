// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

// Package trace connects the W3C trace context that ebpf/trace_correlator.c reads from ingress
// TCP payloads to the Flight Recorder timeline.
//
// What it can and cannot see (kernel-level, so these are hard limits, not TODOs):
//   - plaintext HTTP/1.x only: TLS-terminated or HTTP/2 (HPACK) traffic carries no readable
//     traceparent header;
//   - the header must be inside the first 512 payload bytes of a packet, so a request whose
//     headers span packets or come late is missed;
//   - IPv4 only;
//   - the trace is keyed by TCP 4-tuple, so on a keep-alive connection the newest request wins
//     and a slow earlier request on the same connection can be attributed to the newer trace.
//
// The trace id is user data. It is never logged by this package.
package trace

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/cilium/ebpf/ringbuf"

	"github.com/zyvorai/gryvia/collector/pkg/flight"
)

// Size and byte offsets of struct trace_context in ebpf/trace_correlator.c (little endian; the
// C side pins them with _Static_asserts).
const (
	EventSize  = 56
	offTraceHi = 0
	offTraceLo = 8
	offSpan    = 16
	offSrcIP   = 32
	offDstIP   = 36
	offSrcPort = 40
	offDstPort = 42
)

// Observation is one decoded trace_events record.
type Observation struct {
	TraceID string // 32 lowercase hex characters
	SpanID  string // 16 lowercase hex characters
	Client  Endpoint
	Server  Endpoint
}

// Endpoint is an IPv4 address and port.
type Endpoint struct {
	IP   [4]byte
	Port uint16
}

func (e Endpoint) String() string {
	return net.JoinHostPort(net.IP(e.IP[:]).String(), strconv.Itoa(int(e.Port)))
}

// Decode parses one trace_events ring buffer record. It rejects short records and the all-zero
// trace or span id (invalid in the W3C spec).
func Decode(b []byte) (Observation, bool) {
	if len(b) < EventSize {
		return Observation{}, false
	}
	le := binary.LittleEndian
	hi, lo, span := le.Uint64(b[offTraceHi:]), le.Uint64(b[offTraceLo:]), le.Uint64(b[offSpan:])
	if (hi == 0 && lo == 0) || span == 0 {
		return Observation{}, false
	}
	var id [16]byte
	binary.BigEndian.PutUint64(id[0:], hi)
	binary.BigEndian.PutUint64(id[8:], lo)
	var sp [8]byte
	binary.BigEndian.PutUint64(sp[:], span)
	o := Observation{TraceID: hex.EncodeToString(id[:]), SpanID: hex.EncodeToString(sp[:])}
	// The IPs were copied from the packet header, so their bytes are already in network order;
	// the ports were converted to host order by the probe.
	copy(o.Client.IP[:], b[offSrcIP:offSrcIP+4])
	copy(o.Server.IP[:], b[offDstIP:offDstIP+4])
	o.Client.Port = le.Uint16(b[offSrcPort:])
	o.Server.Port = le.Uint16(b[offDstPort:])
	return o, true
}

// ValidID reports whether s is a well-formed W3C trace id (32 hex digits, not all zero).
func ValidID(s string) bool {
	if len(s) != 32 {
		return false
	}
	zero := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			if c != '0' {
				zero = false
			}
		case c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
			zero = false
		default:
			return false
		}
	}
	return !zero
}

// Entry is one indexed request.
type Entry struct {
	Observation
	Seen     time.Time
	Identity flight.Identity
}

type tupleKey struct {
	c, s Endpoint
}

// Defaults for NewIndex.
const (
	DefaultTTL         = 10 * time.Minute
	DefaultTupleWindow = 30 * time.Second
	DefaultPortWindow  = 2 * time.Second
	DefaultMaxTuples   = 16384
	perTuple           = 8
)

// Index is a bounded, time-limited store of the traces seen on this node, keyed by connection.
// Only requests addressed to a job pod are stored (see Ingest), so unrelated traffic leaves no
// trace id behind.
type Index struct {
	TTL         time.Duration
	TupleWindow time.Duration
	PortWindow  time.Duration
	MaxTuples   int

	now  func() time.Time
	mu   sync.Mutex
	m    map[tupleKey][]Entry
	fifo []tupleKey
}

// NewIndex creates an Index with the default limits.
func NewIndex() *Index {
	return &Index{TTL: DefaultTTL, TupleWindow: DefaultTupleWindow, PortWindow: DefaultPortWindow,
		MaxTuples: DefaultMaxTuples, now: time.Now, m: map[tupleKey][]Entry{}}
}

// Add stores an entry. When the index is full the oldest connections are forgotten.
func (x *Index) Add(e Entry) {
	x.mu.Lock()
	defer x.mu.Unlock()
	k := tupleKey{e.Client, e.Server}
	list, exists := x.m[k]
	if !exists {
		x.fifo = append(x.fifo, k)
	}
	list = append(list, e)
	if len(list) > perTuple {
		list = append(list[:0], list[len(list)-perTuple:]...)
	}
	x.m[k] = list
	cut := x.now().Add(-x.TTL)
	for len(x.fifo) > 0 {
		head := x.fifo[0]
		l, ok := x.m[head]
		switch {
		case !ok:
			// already gone
		case len(x.m) > x.MaxTuples || l[len(l)-1].Seen.Before(cut):
			delete(x.m, head)
		default:
			return
		}
		x.fifo = x.fifo[1:]
	}
	x.fifo = nil
}

func (x *Index) fresh(e Entry, now time.Time) bool { return !e.Seen.Before(now.Add(-x.TTL)) }

func abs(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// ByTuple implements flight.Correlator.
func (x *Index) ByTuple(t flight.Tuple, at time.Time) (string, bool) {
	a := Endpoint{IP: t.SrcIP, Port: t.SrcPort}
	b := Endpoint{IP: t.DstIP, Port: t.DstPort}
	x.mu.Lock()
	defer x.mu.Unlock()
	now := x.now()
	best, bestD, found := "", time.Duration(0), false
	for _, k := range []tupleKey{{a, b}, {b, a}} {
		for _, e := range x.m[k] {
			d := abs(at.Sub(e.Seen))
			if x.fresh(e, now) && d <= x.TupleWindow && (!found || d < bestD) {
				best, bestD, found = e.TraceID, d, true
			}
		}
	}
	return best, found
}

// ByPodPort implements flight.Correlator.
func (x *Index) ByPodPort(namespace, pod string, port uint16, at time.Time) (string, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	now := x.now()
	id, found := "", false
	for k, list := range x.m {
		if k.s.Port != port {
			continue
		}
		for _, e := range list {
			if e.Identity.Namespace != namespace || e.Identity.Pod != pod || !x.fresh(e, now) || abs(at.Sub(e.Seen)) > x.PortWindow {
				continue
			}
			if found && id != e.TraceID {
				return "", false // several distinct requests compete: do not guess
			}
			id, found = e.TraceID, true
		}
	}
	return id, found
}

// Lookup returns every fresh entry of a trace id, oldest first.
func (x *Index) Lookup(traceID string) []Entry {
	x.mu.Lock()
	defer x.mu.Unlock()
	now := x.now()
	var out []Entry
	for _, list := range x.m {
		for _, e := range list {
			if e.TraceID == traceID && x.fresh(e, now) {
				out = append(out, e)
			}
		}
	}
	sortEntries(out)
	return out
}

func sortEntries(s []Entry) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j].Seen.Before(s[j-1].Seen); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// Len is the number of tracked connections.
func (x *Index) Len() int {
	x.mu.Lock()
	defer x.mu.Unlock()
	return len(x.m)
}

// Resolver finds the job pod that owns a destination address.
type Resolver interface {
	ResolveIP(ip string) (flight.Identity, bool)
}

// Ingest indexes one observation if its destination is a job pod on this node; anything else is
// dropped, so traffic to unrelated workloads is never retained.
func (x *Index) Ingest(o Observation, r Resolver) bool {
	id, ok := r.ResolveIP(net.IP(o.Server.IP[:]).String())
	if !ok {
		return false
	}
	x.Add(Entry{Observation: o, Seen: x.now(), Identity: id})
	return true
}

// Consume reads trace_events records from a ring buffer and ingests them until the reader is
// closed. It never logs record contents.
func (x *Index) Consume(rd *ringbuf.Reader, r Resolver) {
	for {
		rec, err := rd.Read()
		if err != nil {
			if errors.Is(err, ringbuf.ErrClosed) {
				return
			}
			continue
		}
		if o, ok := Decode(rec.RawSample); ok {
			x.Ingest(o, r)
		}
	}
}

// TupleFromFlow converts the raw IPv4 words and host-order ports of a tcp_trace flow event
// (decoder.FlowEvent; IPs are the kernel's network-order words read as little endian) into a
// Tuple. local is the socket's own side, peer the remote side.
func TupleFromFlow(localIP uint32, localPort uint16, peerIP uint32, peerPort uint16) flight.Tuple {
	w := func(v uint32) [4]byte { return [4]byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)} }
	return flight.Tuple{SrcIP: w(localIP), SrcPort: localPort, DstIP: w(peerIP), DstPort: peerPort}
}
