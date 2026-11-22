// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package fabric

import (
	"os"
	"regexp"
	"strconv"
	"testing"
)

// grabNum returns the integer captured by the first group of re in the file.
func grabNum(t *testing.T, path, re string) uint64 {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("%s not available: %v", path, err)
	}
	m := regexp.MustCompile(re).FindSubmatch(src)
	if m == nil {
		t.Fatalf("%s: no match for %s", path, re)
	}
	v, err := strconv.ParseUint(string(m[1]), 0, 64)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// TestSignalExtConstantsMatchC pins the Go mirror of the new signal types, transport hints and XDP slots.
func TestSignalExtConstantsMatchC(t *testing.T) {
	hdr := "../../../ebpf/headers/fabric_signal.h"
	for name, want := range map[string]uint8{
		"FABRIC_SIG_NCCL_XPORT":    SigNCCLTransport,
		"FABRIC_SIG_P2P_FALLBACK":  SigP2PFallback,
		"FABRIC_SIG_CAPTURE_ARMED": SigCaptureArmed,
		"FABRIC_SIG_GPU_OOM":       SigGPUOOM,
		"FABRIC_SIG_GRAPH_STALL":   SigGraphStall,
		"FABRIC_SIG_GDR_FAIL":      SigGDRFail,
		"FABRIC_SIG_INFER_TTFT":    SigInferTTFT,
		"FABRIC_SIG_INFER_GAP":     SigInferGap,
		"FABRIC_SIG_WEIGHT_MMAP":   SigWeightMmap,
		"FABRIC_SIG_GPU_DEV":       SigGPUDev,
		"FABRIC_SIG_UCX_WAIT":      SigUCXWait,
	} {
		if got := grabNum(t, hdr, name+`\s*=\s*(\d+)`); got != uint64(want) {
			t.Errorf("%s: header %d, Go %d", name, got, want)
		}
	}
	nccl := "../../../ebpf/nccl_transport.c"
	for name, want := range map[string]int{
		"NCCL_XPORT_UNKNOWN": XportUnknown, "NCCL_XPORT_P2P": XportP2P, "NCCL_XPORT_SHM": XportSHM,
		"NCCL_XPORT_NET": XportNet, "NCCL_XPORT_NET_IB": XportNetIB, "NCCL_XPORT_NET_ROCE": XportNetRoCE,
	} {
		if got := grabNum(t, nccl, `#define `+name+`\s+(\d+)`); got != uint64(want) {
			t.Errorf("%s: C %d, Go %d", name, got, want)
		}
	}
	mux := "../../../ebpf/headers/xdp_chain.h"
	for name, want := range map[string]uint32{
		"XDP_SLOT_ROCE_CNP": XDPSlotRoceCNP, "XDP_SLOT_PFC_PAUSE": XDPSlotPFCPause,
		"XDP_SLOT_DNS": XDPSlotDNS, "XDP_SLOT_PACKET_FILTER": XDPSlotPacketFilter,
		"XDP_SLOT_ROCE_ECN": XDPSlotRoceECN,
	} {
		if got := grabNum(t, mux, `#define `+name+`\s+(\d+)`); got != uint64(want) {
			t.Errorf("%s: C %d, Go %d", name, got, want)
		}
	}
}
