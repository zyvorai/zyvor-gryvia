// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package netcost

import (
	"context"
	"net/netip"
	"time"

	"github.com/cilium/ebpf"
)

// ScanInterval is how often the counter maps are folded into the Meter.
const ScanInterval = 15 * time.Second

// costKey / costKey6 / costValue mirror struct cost_key / cost_key6 / cost_value of ebpf/cost_tracker.c (8, 32
// and 32 bytes). Addresses are raw network-order bytes, so decoding does not depend on the host byte order.
type costKey struct{ Src, Dst [4]byte }
type costKey6 struct{ Src, Dst [16]byte }
type costValue struct{ BytesSent, BytesRecv, PacketsSent, PacketsRecv uint64 }

// ReadMap reads the whole traffic_costs (IPv4) map. Iteration over a hash map that changes concurrently can repeat
// a key; Meter.Observe ignores the repeats, so a pair is never counted twice.
func ReadMap(m *ebpf.Map) ([]Sample, error) {
	var (
		k   costKey
		v   costValue
		out []Sample
	)
	it := m.Iterate()
	for it.Next(&k, &v) {
		out = append(out, Sample{Local: netip.AddrFrom4(k.Src), Remote: netip.AddrFrom4(k.Dst), BytesSent: v.BytesSent, BytesRecv: v.BytesRecv})
		if len(out) >= 4*MaxPairs {
			break
		}
	}
	return out, it.Err()
}

// ReadMap6 reads the whole traffic_costs6 (IPv6) map, like ReadMap.
func ReadMap6(m *ebpf.Map) ([]Sample, error) {
	var (
		k   costKey6
		v   costValue
		out []Sample
	)
	it := m.Iterate()
	for it.Next(&k, &v) {
		out = append(out, Sample{Local: netip.AddrFrom16(k.Src), Remote: netip.AddrFrom16(k.Dst), BytesSent: v.BytesSent, BytesRecv: v.BytesRecv})
		if len(out) >= 4*MaxPairs {
			break
		}
	}
	return out, it.Err()
}

// ReadMaps reads the IPv4 map and, when m6 is not nil, the IPv6 map into one scan. A failing IPv6 read fails the
// scan (the Meter must never see half a scan: pairs missing from it would be forgotten and re-counted).
func ReadMaps(m4, m6 *ebpf.Map) ([]Sample, error) {
	out, err := ReadMap(m4)
	if err != nil || m6 == nil {
		return out, err
	}
	more, err := ReadMap6(m6)
	if err != nil {
		return nil, err
	}
	return append(out, more...), nil
}

// RunScan folds the maps into the meter every ScanInterval until ctx is done. m6 may be nil (IPv4 only).
func RunScan(ctx context.Context, m, m6 *ebpf.Map, meter *Meter, onError func(error)) {
	t := time.NewTicker(ScanInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			samples, err := ReadMaps(m, m6)
			if err != nil {
				if onError != nil {
					onError(err)
				}
				continue
			}
			meter.Observe(samples, now)
		}
	}
}
