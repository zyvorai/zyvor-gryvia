// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package loader

import (
	"os"
	"regexp"
	"strconv"
	"testing"
)

// TestXDPSlotsMatchHeader pins the Go slot numbers to ebpf/headers/xdp_chain.h.
func TestXDPSlotsMatchHeader(t *testing.T) {
	src, err := os.ReadFile("../../../ebpf/headers/xdp_chain.h")
	if err != nil {
		t.Skipf("header not available: %v", err)
	}
	for name, want := range map[string]uint32{
		"XDP_SLOT_ROCE_CNP": XDPSlotRoceCNP, "XDP_SLOT_PFC_PAUSE": XDPSlotPFCPause,
		"XDP_SLOT_DNS": XDPSlotDNS, "XDP_SLOT_PACKET_FILTER": XDPSlotPacketFilter,
		"XDP_SLOT_ROCE_ECN": XDPSlotRoceECN, "XDP_SLOT_MAX": XDPSlots,
	} {
		m := regexp.MustCompile(`#define ` + name + `\s+(\d+)`).FindSubmatch(src)
		if m == nil {
			t.Fatalf("%s not found in header", name)
		}
		got, _ := strconv.ParseUint(string(m[1]), 10, 32)
		if uint32(got) != want {
			t.Errorf("%s: header %d, Go %d", name, got, want)
		}
	}
}

func TestXDPChainSlot(t *testing.T) {
	for _, c := range []struct {
		object, program string
		slot            uint32
		ok              bool
	}{
		{"roce_cnp.o", "gryvia_roce_cnp", XDPSlotRoceCNP, true},
		{"roce_ecn.o", "gryvia_roce_ecn", XDPSlotRoceECN, true},
		{"packet_filter.o", "xdp_packet_filter", XDPSlotPacketFilter, true},
		{"roce_cnp.o", "some_other_program", 0, false}, // right object, wrong program
		{"tcp_trace.o", "gryvia_roce_cnp", 0, false},   // not a chain object
		{"xdp_mux.o", "gryvia_xdp_mux", 0, false},      // the mux itself is not a feature
	} {
		slot, ok := XDPChainSlot(c.object, c.program)
		if ok != c.ok || (ok && slot != c.slot) {
			t.Errorf("XDPChainSlot(%s, %s) = %d, %v; want %d, %v", c.object, c.program, slot, ok, c.slot, c.ok)
		}
	}
}

func TestXDPMuxSkipReason(t *testing.T) {
	if r := XDPMuxSkipReason(Config{}); r == "" {
		t.Error("the mux must be skipped unless XDPMux is set")
	}
	if r := XDPMuxSkipReason(Config{XDPMux: true}); r != "" {
		t.Errorf("the mux must attach when XDPMux is set, got %q", r)
	}
}
