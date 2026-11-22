// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package fabric

import (
	"testing"
	"time"
)

// Ring signals 11-22 are counted per job; before, they were read and dropped.
func TestFolderCountsOptInSignals(t *testing.T) {
	f := NewFolder(time.Minute)
	f.Bind(1, "ns", "train")
	for _, typ := range []uint8{SigGPUOOM, SigGPUOOM, SigGraphStall, SigGDRFail, SigP2PFallback, SigNCCLTransport,
		SigInferGap, SigWeightMmap, SigGPUDev, SigUCXWait} {
		f.Add(Signal{PID: 1, Type: typ, Bytes: 4096, Comm: "py"})
	}
	f.Add(Signal{PID: 1, Type: SigInferTTFT, LatencyNS: 80_000_000})
	st := f.Snapshot()[JobKey{Namespace: "ns", Job: "train"}]
	want := map[string]uint64{"gpu_oom": 2, "graph_stall": 1, "gdr_fail": 1, "p2p_fallback": 1, "nccl_transport": 1,
		"infer_gap": 1, "weight_mmap": 1, "gpu_dev": 1, "ucx_wait": 1, "infer_ttft": 1}
	for k, v := range want {
		if st.Events[k] != v {
			t.Errorf("Events[%s] = %d, want %d (all: %v)", k, st.Events[k], v, st.Events)
		}
	}
	if st.InferTTFTP99MS != 80 {
		t.Errorf("InferTTFTP99MS = %v, want 80", st.InferTTFTP99MS)
	}
}

// A ring signal must never be read as a synthesised NIC counter: type 11/12 used to
// collide with SigNICRetry/SigNICError and p2p copy sizes landed in the NIC error rate.
func TestRingSignalsDoNotPolluteNICRates(t *testing.T) {
	f := NewFolder(time.Minute)
	f.Bind(1, "ns", "train")
	f.Add(Signal{PID: 1, Type: SigP2PFallback, Bytes: 1 << 30})
	f.Add(Signal{PID: 1, Type: SigNCCLTransport, Bytes: 7})
	st := f.Snapshot()[JobKey{Namespace: "ns", Job: "train"}]
	if st.NICErrorRate != 0 || st.NICRetryRate != 0 {
		t.Errorf("NIC rates polluted by ring signals: retry=%v error=%v", st.NICRetryRate, st.NICErrorRate)
	}
}
