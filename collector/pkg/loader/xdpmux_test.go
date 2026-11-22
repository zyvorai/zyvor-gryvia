// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

//go:build bpfroot

// Runs the XDP chain in the kernel with BPF_PROG_TEST_RUN. Needs root, a kernel with BTF
// and the objects built (make -C ebpf), so it is behind a build tag and run from
// .github/workflows/ebpf.yml:
//
//	go test -c -tags bpfroot -o loader.test ./pkg/loader
//	sudo env GRYVIA_EBPF_DIR=../ebpf ./loader.test -test.run TestXDPChain -test.v
package loader

import (
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/rlimit"
	"go.uber.org/zap"
)

const (
	xdpPass = 2 // XDP_PASS

	// ebpf/headers/fabric_signal.h
	cnpSlotCNP  = 0
	cnpSlotRoCE = 1
	// ebpf/roce_ecn.c
	ecnSlotRoCE = 0
	ecnSlotCE   = 1
)

func ebpfDir(t *testing.T) string {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	dir := os.Getenv("GRYVIA_EBPF_DIR")
	if dir == "" {
		dir = "../../../ebpf"
	}
	if _, err := os.Stat(filepath.Join(dir, "xdp_mux.o")); err != nil {
		t.Skipf("objects not built in %s: %v", dir, err)
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatal(err)
	}
	return dir
}

// roceCNP is an Ethernet/IPv4/UDP frame to port 4791 whose first BTH byte is the CNP
// opcode (0x81) and whose IP ECN bits say Congestion Experienced: roce_cnp counts it as a
// RoCE packet and a CNP, roce_ecn as a RoCE packet and a CE mark.
func roceCNP() []byte {
	pkt := make([]byte, 64)
	binary.BigEndian.PutUint16(pkt[12:], 0x0800) // IPv4
	pkt[14] = 0x45                               // version 4, ihl 5
	pkt[15] = 0x03                               // tos: ECN = CE
	binary.BigEndian.PutUint16(pkt[16:], 50)     // total length
	pkt[22] = 64                                 // ttl
	pkt[23] = 17                                 // UDP
	binary.BigEndian.PutUint16(pkt[36:], 4791)   // UDP dest port (20+14+2)
	binary.BigEndian.PutUint16(pkt[38:], 30)     // UDP length
	pkt[42] = 0x81                               // BTH opcode: CNP
	return pkt
}

func run(t *testing.T, x *XDPMux, pkt []byte) {
	t.Helper()
	ret, _, err := x.Mux.Test(pkt)
	if err != nil {
		t.Fatalf("run xdp_mux: %v", err)
	}
	if ret != xdpPass {
		t.Fatalf("xdp_mux returned %d, want XDP_PASS (%d)", ret, xdpPass)
	}
}

func want(t *testing.T, x *XDPMux, object, m string, key uint32, expect uint64) {
	t.Helper()
	got, err := x.Counter(object, m, key)
	if err != nil {
		t.Fatalf("%s %s[%d]: %v", object, m, key, err)
	}
	if got != expect {
		t.Errorf("%s %s[%d] = %d, want %d", object, m, key, got, expect)
	}
}

// With every feature loaded, one packet through the mux is counted by the first feature
// and by the last one: the chain passed through the slots in between.
func TestXDPChainRunsEveryFeature(t *testing.T) {
	x, err := LoadXDPMux(ebpfDir(t), DefaultXDPFeatures())
	if err != nil {
		t.Fatal(err)
	}
	defer x.Close()
	if n := len(x.Loaded()); n != XDPSlots {
		t.Fatalf("loaded %d features, want %d", n, XDPSlots)
	}
	run(t, x, roceCNP())
	want(t, x, "xdp_mux.o", "xdp_mux_stats", 0, 1) // seen
	want(t, x, "xdp_mux.o", "xdp_mux_stats", 1, 0) // no miss
	want(t, x, "roce_cnp.o", "cnp_count", cnpSlotRoCE, 1)
	want(t, x, "roce_cnp.o", "cnp_count", cnpSlotCNP, 1)
	want(t, x, "roce_ecn.o", "ecn_count", ecnSlotRoCE, 1)
	want(t, x, "roce_ecn.o", "ecn_count", ecnSlotCE, 1)
	run(t, x, roceCNP())
	want(t, x, "roce_cnp.o", "cnp_count", cnpSlotCNP, 2)
	want(t, x, "roce_ecn.o", "ecn_count", ecnSlotCE, 2)
}

// An empty slot is passed over: with only the first and the last feature loaded, both run.
func TestXDPChainSkipsEmptySlots(t *testing.T) {
	var feats []XDPFeature
	for _, f := range DefaultXDPFeatures() {
		if f.Slot == XDPSlotRoceCNP || f.Slot == XDPSlotRoceECN {
			feats = append(feats, f)
		}
	}
	x, err := LoadXDPMux(ebpfDir(t), feats)
	if err != nil {
		t.Fatal(err)
	}
	defer x.Close()
	run(t, x, roceCNP())
	want(t, x, "roce_cnp.o", "cnp_count", cnpSlotCNP, 1)
	want(t, x, "roce_ecn.o", "ecn_count", ecnSlotCE, 1)
}

// The first populated slot need not be slot 0.
func TestXDPChainStartsAtFirstPopulatedSlot(t *testing.T) {
	var feats []XDPFeature
	for _, f := range DefaultXDPFeatures() {
		if f.Slot == XDPSlotRoceECN {
			feats = append(feats, f)
		}
	}
	x, err := LoadXDPMux(ebpfDir(t), feats)
	if err != nil {
		t.Fatal(err)
	}
	defer x.Close()
	run(t, x, roceCNP())
	want(t, x, "roce_ecn.o", "ecn_count", ecnSlotCE, 1)
	want(t, x, "xdp_mux.o", "xdp_mux_stats", 1, 0)
}

// With no feature, the mux counts a miss and still passes the packet.
func TestXDPChainEmptyPassesAndCountsMiss(t *testing.T) {
	x, err := LoadXDPMux(ebpfDir(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer x.Close()
	run(t, x, roceCNP())
	want(t, x, "xdp_mux.o", "xdp_mux_stats", 0, 1)
	want(t, x, "xdp_mux.o", "xdp_mux_stats", 1, 1)
}

// A feature attached on its own (its xdp_features map is empty) still counts and passes.
func TestXDPFeatureStandalone(t *testing.T) {
	dir := ebpfDir(t)
	spec, err := ebpf.LoadCollectionSpec(filepath.Join(dir, "roce_cnp.o"))
	if err != nil {
		t.Fatal(err)
	}
	coll, err := ebpf.NewCollection(spec)
	if err != nil {
		t.Fatal(err)
	}
	defer coll.Close()
	ret, _, err := coll.Programs["gryvia_roce_cnp"].Test(roceCNP())
	if err != nil || ret != xdpPass {
		t.Fatalf("standalone roce_cnp: ret %d err %v", ret, err)
	}
	var per []uint64
	if err := coll.Maps["cnp_count"].Lookup(uint32(cnpSlotCNP), &per); err != nil {
		t.Fatal(err)
	}
	var sum uint64
	for _, v := range per {
		sum += v
	}
	if sum != 1 {
		t.Errorf("standalone cnp_count[CNP] = %d, want 1", sum)
	}
}

// The Manager, configured with -xdp-mux, attaches xdp_mux to an interface and places the XDP
// features in its chain. On the loopback interface, real UDP packets to port 4791 are then seen
// by both roce_cnp and roce_ecn: the same packets are counted by the first and the last slot.
func TestManagerXDPChainOnLoopback(t *testing.T) {
	src := ebpfDir(t)
	dir := t.TempDir()
	for _, f := range []string{"xdp_mux.o", "roce_cnp.o", "roce_ecn.o"} {
		if err := os.Symlink(filepath.Join(mustAbs(t, src), f), filepath.Join(dir, f)); err != nil {
			t.Fatal(err)
		}
	}
	m, err := New(Config{Dir: dir, Iface: "lo", XDPMux: true}, zap.NewNop().Sugar())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.LoadAndAttach(); err != nil {
		t.Fatalf("LoadAndAttach: %v", err)
	}

	byObject := map[string]ProgramStatus{}
	for _, s := range m.Status() {
		byObject[s.Object] = s
	}
	if s := byObject["xdp_mux.o"]; !s.Attached {
		t.Fatalf("xdp_mux not attached: %+v", s)
	}
	for _, f := range []struct{ obj, target string }{
		{"roce_cnp.o", "xdp_mux.o slot 0"}, {"roce_ecn.o", "xdp_mux.o slot 4"},
	} {
		if s := byObject[f.obj]; !s.Attached || s.Target != f.target {
			t.Errorf("%s: attached=%v target=%q reason=%q, want attached to %q", f.obj, s.Attached, s.Target, s.Reason, f.target)
		}
	}

	// Real packets: UDP to 4791 over loopback, first payload byte 0x81 (a CNP opcode). A listener on
	// the port keeps the kernel from answering with ICMP port-unreachable, which would make the
	// connected sender's later writes fail with "connection refused".
	listener, err := net.ListenPacket("udp", "127.0.0.1:4791")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	conn, err := net.Dial("udp", "127.0.0.1:4791")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	const n = 5
	for i := 0; i < n; i++ {
		if _, err := conn.Write([]byte{0x81, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}); err != nil {
			t.Fatal(err)
		}
	}

	cnp, err := m.ReadCounter("roce_cnp.o", "cnp_count", cnpSlotRoCE)
	if err != nil {
		t.Fatal(err)
	}
	ecn, err := m.ReadCounter("roce_ecn.o", "ecn_count", ecnSlotRoCE)
	if err != nil {
		t.Fatal(err)
	}
	if cnp < n || ecn < n {
		t.Errorf("RoCE packets counted: roce_cnp %d, roce_ecn %d, want at least %d each", cnp, ecn, n)
	}
	if cnp != ecn {
		t.Errorf("roce_cnp (slot 0) counted %d and roce_ecn (slot 4) %d: a feature in the chain missed packets", cnp, ecn)
	}
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	a, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// An opt-in object is not loaded by default: its programs are listed as skipped and nothing is
// attached. Named in EnablePrograms, the same object loads and attaches.
func TestManagerOptInObject(t *testing.T) {
	src := mustAbs(t, ebpfDir(t))
	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(src, "capture_gate.o"), filepath.Join(dir, "capture_gate.o")); err != nil {
		t.Fatal(err)
	}

	m, err := New(Config{Dir: dir}, zap.NewNop().Sugar())
	if err != nil {
		t.Fatal(err)
	}
	// Nothing attached, so LoadAndAttach reports an error; the statuses are what matters.
	_ = m.LoadAndAttach()
	st := m.Status()
	if len(st) == 0 {
		t.Fatal("no status for the skipped object")
	}
	for _, s := range st {
		if s.Attached || !s.NotRequested || !strings.Contains(s.Reason, "opt-in program") || s.Object != "capture_gate.o" {
			t.Errorf("default: %+v, want a skipped opt-in program", s)
		}
	}
	if m.Map("capture_gate.o", "capture_lease") != nil {
		t.Error("default: capture_gate.o was loaded into the kernel")
	}
	m.Close()

	m, err = New(Config{Dir: dir, EnablePrograms: []string{"capture_gate"}}, zap.NewNop().Sugar())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.LoadAndAttach(); err != nil {
		t.Fatalf("enabled: LoadAndAttach: %v", err)
	}
	attached := false
	for _, s := range m.Status() {
		attached = attached || s.Attached
	}
	if !attached || m.Map("capture_gate.o", "capture_lease") == nil {
		t.Errorf("enabled: capture_gate.o did not load and attach: %+v", m.Status())
	}
}

// Programs the configuration did not ask for are marked NotRequested and export no gauge series;
// a program the node cannot attach for lack of a library is not, and still exports 0.
func TestManagerNotRequested(t *testing.T) {
	src := mustAbs(t, ebpfDir(t))
	dir := t.TempDir()
	for _, f := range []string{"quota_pace.o", "ibv_verbs.o", "dns_tracker.o", "straggler.o"} {
		if err := os.Symlink(filepath.Join(src, f), filepath.Join(dir, f)); err != nil {
			t.Fatal(err)
		}
	}
	m, err := New(Config{Dir: dir}, zap.NewNop().Sugar())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	_ = m.LoadAndAttach() // nothing may attach on a runner without NCCL; the statuses are the point

	seen := map[string]int{}
	for _, s := range m.Status() {
		seen[s.Object]++
		_, export := s.AttachGauge()
		switch s.Object {
		case "quota_pace.o", "ibv_verbs.o", "dns_tracker.o":
			// quota pacing off, libibverbs probes off, and an XDP program with no -iface.
			if s.Attached || !s.NotRequested || export {
				t.Errorf("%s/%s: attached=%v notRequested=%v export=%v (%q), want a skipped, not-requested program with no series",
					s.Object, s.Program, s.Attached, s.NotRequested, export, s.Reason)
			}
		case "straggler.o":
			// uprobes on libnccl: either the library is found and they attach, or it is missing and the
			// gap is reported (series present, value 0) rather than hidden.
			if s.NotRequested || !export {
				t.Errorf("straggler.o/%s: notRequested=%v export=%v (%q), want a status that still exports", s.Program, s.NotRequested, export, s.Reason)
			}
		}
	}
	for _, f := range []string{"quota_pace.o", "ibv_verbs.o", "dns_tracker.o", "straggler.o"} {
		if seen[f] == 0 {
			t.Errorf("no status for %s", f)
		}
	}
}
