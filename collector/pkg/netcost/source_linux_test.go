// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

//go:build linux

package netcost

import (
	"net/netip"
	"os"
	"testing"

	"github.com/cilium/ebpf"
)

// TestReadMapKernel creates a real LRU hash map with the key/value sizes of traffic_costs, fills it and reads
// it back with ReadMap. It needs CAP_BPF, so it only runs with GRYVIA_KERNEL_TEST=1 (as root).
func TestReadMapKernel(t *testing.T) {
	if os.Getenv("GRYVIA_KERNEL_TEST") != "1" {
		t.Skip("set GRYVIA_KERNEL_TEST=1 and run as root")
	}
	m, err := ebpf.NewMap(&ebpf.MapSpec{Type: ebpf.LRUHash, KeySize: 8, ValueSize: 32, MaxEntries: 16})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	want := costValue{BytesSent: 1234, BytesRecv: 99, PacketsSent: 2, PacketsRecv: 1}
	if err := m.Put(costKey{Src: [4]byte{10, 0, 1, 1}, Dst: [4]byte{8, 8, 8, 8}}, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadMap(m)
	if err != nil || len(got) != 1 {
		t.Fatalf("%v %v", got, err)
	}
	if got[0].Local.String() != "10.0.1.1" || got[0].Remote.String() != "8.8.8.8" || got[0].BytesSent != 1234 || got[0].BytesRecv != 99 {
		t.Fatalf("%+v", got[0])
	}
}

// TestReadMap6Kernel does the same for the 32-byte IPv6 key of traffic_costs6 and for the combined ReadMaps scan.
func TestReadMap6Kernel(t *testing.T) {
	if os.Getenv("GRYVIA_KERNEL_TEST") != "1" {
		t.Skip("set GRYVIA_KERNEL_TEST=1 and run as root")
	}
	m4, err := ebpf.NewMap(&ebpf.MapSpec{Type: ebpf.LRUHash, KeySize: 8, ValueSize: 32, MaxEntries: 16})
	if err != nil {
		t.Fatal(err)
	}
	defer m4.Close()
	m6, err := ebpf.NewMap(&ebpf.MapSpec{Type: ebpf.LRUHash, KeySize: 32, ValueSize: 32, MaxEntries: 16})
	if err != nil {
		t.Fatal(err)
	}
	defer m6.Close()
	l, r := netip.MustParseAddr("fd00:10::1").As16(), netip.MustParseAddr("2001:4860:4860::8888").As16()
	want := costValue{BytesSent: 4321, BytesRecv: 12, PacketsSent: 3, PacketsRecv: 1}
	if err := m6.Put(costKey6{Src: l, Dst: r}, want); err != nil {
		t.Fatal(err)
	}
	if err := m4.Put(costKey{Src: [4]byte{10, 0, 1, 1}, Dst: [4]byte{8, 8, 8, 8}}, costValue{BytesSent: 1}); err != nil {
		t.Fatal(err)
	}
	got, err := ReadMap6(m6)
	if err != nil || len(got) != 1 {
		t.Fatalf("%v %v", got, err)
	}
	if got[0].Local != netip.AddrFrom16(l) || got[0].Remote != netip.AddrFrom16(r) || got[0].BytesSent != 4321 || got[0].BytesRecv != 12 {
		t.Fatalf("%+v", got[0])
	}
	all, err := ReadMaps(m4, m6)
	if err != nil || len(all) != 2 {
		t.Fatalf("combined scan: %v %v", all, err)
	}
	if only, err := ReadMaps(m4, nil); err != nil || len(only) != 1 {
		t.Fatalf("v4-only scan: %v %v", only, err)
	}
}
