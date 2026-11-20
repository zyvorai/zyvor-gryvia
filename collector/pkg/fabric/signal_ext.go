// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0
//
// Additive signal types for nccl_transport.c, p2p_fallback.c, capture_gate.c and
// the XDP slot map of xdp_mux.c. Keep the numbers aligned with
// ebpf/headers/fabric_signal.h and ebpf/headers/xdp_chain.h (signal_ext_test.go checks it).
// struct fabric_signal stays 80 bytes; do not add fields.
//
// Folder counts these per job in Status.Events (exported as gryvia_fabric_signal_events) and
// derives Status.InferTTFTP99MS; none of them feeds ScoreDelta. Nothing writes the maps that steer
// the programs (transport_hint, capture_lease, allowed_cg, gpu_dev_cfg), so the programs are
// opt-in in the collector (-enable-programs, see pkg/loader/optin.go).

package fabric

const (
	// SigNCCLTransport is emitted by nccl_transport.c when ncclCommInitRank
	// (or ncclGetUniqueId) returns 0. RetryCount is the transport hint.
	SigNCCLTransport uint8 = 11
	// SigP2PFallback is emitted by p2p_fallback.c. Bytes is the copy size,
	// RetryCount the peer-enable failures, PeerLatencyNS the age of the last one.
	SigP2PFallback uint8 = 12
	// SigCaptureArmed is synthesised by the collector from capture_lease.
	// The kernel program does not write it to a ring buffer.
	SigCaptureArmed uint8 = 13
	// 14 is reserved (no program emits it).

	// SigGPUOOM: cudaMalloc* returned out-of-memory (gpu_oom.c). Bytes is the request,
	// RetryCount 0 malloc / 1 async / 2 pool. Routine in PyTorch's caching allocator.
	SigGPUOOM uint8 = 15
	// SigGraphStall: cudaDeviceSynchronize inside an open CUDA graph capture (graph_stall.c).
	SigGraphStall uint8 = 16
	// SigGDRFail: nvidia_p2p_get_pages failed (gdr_fail.c). RetryCount is its return value.
	SigGDRFail uint8 = 17
	// SigInferTTFT / SigInferGap: accept -> first send, and a later send gap (infer_ttft.c).
	SigInferTTFT uint8 = 18
	SigInferGap  uint8 = 19
	// SigWeightMmap: mmap of a weight file, then a public IPv4 connect (weight_mmap.c).
	SigWeightMmap uint8 = 20
	// SigGPUDev: GPU device open from a cgroup not in allowed_cg (gpu_dev.c).
	SigGPUDev uint8 = 21
	// SigUCXWait: progress ran >= 5 ms after a UCX send returned a request (ucx_complete.c).
	SigUCXWait uint8 = 22
	// 23 is reserved (no program emits it).
)

// Transport hints written into nccl_transport's transport_hint map
// (NCCL_XPORT_* in nccl_transport.c).
const (
	XportUnknown = 0
	XportP2P     = 1
	XportSHM     = 2
	XportNet     = 3
	XportNetIB   = 4
	XportNetRoCE = 5
)

// XDP chain slots. Must match ebpf/headers/xdp_chain.h. xdp_mux tail-calls the first
// populated slot and each feature tail-calls the next one, so every loaded feature runs
// (collector/pkg/loader/xdpmux.go).
const (
	XDPSlotRoceCNP      uint32 = 0
	XDPSlotPFCPause     uint32 = 1
	XDPSlotDNS          uint32 = 2
	XDPSlotPacketFilter uint32 = 3
	XDPSlotRoceECN      uint32 = 4
)
