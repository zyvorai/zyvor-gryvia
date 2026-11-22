// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package fabric

// EventName is the stable metric label for a counted ring signal type.
func EventName(t uint8) string {
	switch t {
	case SigNCCLTransport:
		return "nccl_transport"
	case SigP2PFallback:
		return "p2p_fallback"
	case SigGPUOOM:
		return "gpu_oom"
	case SigGraphStall:
		return "graph_stall"
	case SigGDRFail:
		return "gdr_fail"
	case SigInferTTFT:
		return "infer_ttft"
	case SigInferGap:
		return "infer_gap"
	case SigWeightMmap:
		return "weight_mmap"
	case SigGPUDev:
		return "gpu_dev"
	case SigUCXWait:
		return "ucx_wait"
	}
	return "unknown"
}
