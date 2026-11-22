// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package loader

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
)

// AttachKind identifies which link.* call is used for a program.
type AttachKind string

const (
	KindKprobe        AttachKind = "kprobe"
	KindKretprobe     AttachKind = "kretprobe"
	KindUprobe        AttachKind = "uprobe"
	KindUretprobe     AttachKind = "uretprobe"
	KindTracepoint    AttachKind = "tracepoint"
	KindRawTracepoint AttachKind = "raw_tracepoint"
	KindTPBTF         AttachKind = "tp_btf"
	KindXDP           AttachKind = "xdp"
	KindTCXIngress    AttachKind = "tcx_ingress"
	KindTCXEgress     AttachKind = "tcx_egress"
	KindSockOps       AttachKind = "sockops"
	KindSkMsg         AttachKind = "sk_msg"
)

// AttachSpec is the parsed form of an ELF section name.
type AttachSpec struct {
	Kind   AttachKind
	Symbol string // kernel/user symbol, tracepoint name, or btf tracepoint
	Group  string // tracepoint group (KindTracepoint only)
}

// ParseSection converts an ELF section name such as "kprobe/tcp_v4_connect"
// or "tracepoint/tcp/tcp_probe" into an AttachSpec.
func ParseSection(section string) (AttachSpec, error) {
	section = strings.TrimSpace(section)
	head, rest, hasRest := strings.Cut(section, "/")

	need := func(k AttachKind) (AttachSpec, error) {
		if !hasRest || rest == "" {
			return AttachSpec{}, fmt.Errorf("section %q: missing symbol", section)
		}
		return AttachSpec{Kind: k, Symbol: rest}, nil
	}

	switch head {
	case "kprobe", "ksyscall":
		return need(KindKprobe)
	case "kretprobe", "kretsyscall":
		return need(KindKretprobe)
	case "uprobe":
		return need(KindUprobe)
	case "uretprobe":
		return need(KindUretprobe)
	case "raw_tracepoint", "raw_tp":
		return need(KindRawTracepoint)
	case "tp_btf":
		return need(KindTPBTF)
	case "tracepoint", "tp":
		if !hasRest {
			return AttachSpec{}, fmt.Errorf("section %q: missing tracepoint", section)
		}
		group, name, ok := strings.Cut(rest, "/")
		if !ok || group == "" || name == "" {
			return AttachSpec{}, fmt.Errorf("section %q: want tracepoint/<group>/<name>", section)
		}
		return AttachSpec{Kind: KindTracepoint, Group: group, Symbol: name}, nil
	case "xdp":
		return AttachSpec{Kind: KindXDP}, nil
	case "tcx":
		switch rest {
		case "ingress":
			return AttachSpec{Kind: KindTCXIngress}, nil
		case "egress":
			return AttachSpec{Kind: KindTCXEgress}, nil
		}
		return AttachSpec{}, fmt.Errorf("section %q: want tcx/ingress or tcx/egress", section)
	case "sockops":
		return AttachSpec{Kind: KindSockOps}, nil
	case "sk_msg":
		return AttachSpec{Kind: KindSkMsg}, nil
	}
	return AttachSpec{}, fmt.Errorf("unsupported section %q", section)
}

// Config controls which config-dependent program types get attached.
type Config struct {
	Dir        string // directory with compiled .o files
	Iface      string // interface for XDP/TCX; empty disables them
	CgroupPath string // cgroup v2 path for sockops/sk_msg; empty disables them
	NCCLLib    string // path to libnccl.so; empty = auto-discover
	CUDALib    string // path to libcudart.so; empty = auto-discover
	CuFileLib  string // path to libcufile.so (GPUDirect Storage); empty = auto-discover
	UCXLib     string // path to libucp.so (UCX) for ucx_gloo.c; empty = auto-discover
	IBVerbsLib string // path to libibverbs.so for ibv_verbs.c; empty = auto-discover
	UprobePID  int    // if >0, find libraries via /proc/<pid>/maps
	// InferPorts are the local TCP ports infer_latency.c watches (at most
	// MaxInferPorts); empty skips that object.
	InferPorts []uint16
	// QuotaPace lets quota_pace.c (the only program that changes sockets) attach.
	// Default false; it also needs CgroupPath.
	QuotaPace bool
	// IBVerbs lets ibv_verbs.c (libibverbs control-path uprobes) attach. Default false.
	IBVerbs bool
	// EnablePrograms names the opt-in programs to attach (see optin.go); "all" enables every one.
	EnablePrograms []string
	// XDPMux attaches xdp_mux.o as the interface's only XDP program and chains the XDP
	// feature programs (roce_cnp, pfc_pause, dns_tracker, packet_filter, roce_ecn) behind it,
	// so they all run, instead of one XDP program per interface. Needs Iface. Default false.
	XDPMux bool
}

// XDPMuxSkipReason explains why xdp_mux.o must not attach ("" = it may).
func XDPMuxSkipReason(cfg Config) string {
	if !cfg.XDPMux {
		return "XDP chaining is off (set -xdp-mux together with -iface)"
	}
	return ""
}

// IBVCountsMap is the counter map of ibv_verbs.c; an object that has it is
// skipped unless Config.IBVerbs (opt-in: it probes a library shared by every
// RDMA application on the node).
const IBVCountsMap = "ibv_counts"

// IBVerbsSkipReason explains why the libibverbs probes must not attach ("" = they may).
func IBVerbsSkipReason(cfg Config) string {
	if !cfg.IBVerbs {
		return "libibverbs probes are off (set -ibverbs-probes)"
	}
	return ""
}

// PaceRateMap is the hash map quota_pace.c reads its per-cgroup rates from. An
// object that has it is the pacing program and is skipped unless Config.QuotaPace.
const PaceRateMap = "pace_rate"

// QuotaPaceSkipReason explains why the pacing object must not attach ("" = it may).
// Pacing is opt-in twice over: -quota-pace, and a cgroup to attach to.
func QuotaPaceSkipReason(cfg Config) string {
	switch {
	case !cfg.QuotaPace:
		return "quota pacing is off (mutating program; set -quota-pace together with -cgroup-path)"
	case cfg.CgroupPath == "":
		return "no cgroup path configured (set -cgroup-path)"
	}
	return ""
}

// xdpOwners remembers which program owns the XDP hook of each interface. Only
// one XDP program can be attached per interface, so a second one is skipped
// with an explicit reason instead of failing (or replacing the first).
type xdpOwners struct {
	mu sync.Mutex
	by map[string]string // interface -> "object/program"
}

// claim registers owner for iface. It returns the current owner and false when
// the interface is already taken.
func (x *xdpOwners) claim(iface, owner string) (string, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if cur, ok := x.by[iface]; ok {
		return cur, false
	}
	if x.by == nil {
		x.by = map[string]string{}
	}
	x.by[iface] = owner
	return owner, true
}

// release gives the interface back (the attach failed after claiming it).
func (x *xdpOwners) release(iface string) {
	x.mu.Lock()
	delete(x.by, iface)
	x.mu.Unlock()
}

// XDPConflictReason is the skip message for a second XDP program on an interface.
func XDPConflictReason(iface, owner string) string {
	return "interface " + iface + " already has XDP program " + owner +
		" attached (one XDP program per interface: roce_cnp, pfc_pause, packet_filter, dns_tracker and roce_ecn conflict; use -xdp-mux to chain them behind xdp_mux, a separate -iface, or one collector per program)"
}

// InferPortsMap is the array map infer_latency.c reads its watched ports from.
const InferPortsMap = "infer_ports"

// MaxInferPorts is INFER_MAX_PORTS in infer_latency.c.
const MaxInferPorts = 8

// ParsePorts parses a comma-separated port list such as "8000,8001". Empty
// input yields no ports; duplicates are dropped.
func ParsePorts(s string) ([]uint16, error) {
	var out []uint16
	seen := map[uint16]bool{}
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		n, err := strconv.ParseUint(f, 10, 16)
		if err != nil || n == 0 {
			return nil, fmt.Errorf("invalid port %q (want 1-65535)", f)
		}
		if seen[uint16(n)] {
			continue
		}
		seen[uint16(n)] = true
		out = append(out, uint16(n))
	}
	if len(out) > MaxInferPorts {
		return nil, fmt.Errorf("%d ports given, at most %d are supported", len(out), MaxInferPorts)
	}
	return out, nil
}

// portSlots lays ports out as the MaxInferPorts array values (0 = empty slot).
func portSlots(ports []uint16) ([MaxInferPorts]uint16, error) {
	var slots [MaxInferPorts]uint16
	if len(ports) > MaxInferPorts {
		return slots, fmt.Errorf("%d ports given, at most %d are supported", len(ports), MaxInferPorts)
	}
	copy(slots[:], ports)
	return slots, nil
}

// FillInferPorts writes ports into the infer_ports array map, clearing the
// unused slots.
func FillInferPorts(mp *ebpf.Map, ports []uint16) error {
	slots, err := portSlots(ports)
	if err != nil {
		return err
	}
	for i, p := range slots {
		k := uint32(i)
		if err := mp.Put(&k, &p); err != nil {
			return err
		}
	}
	return nil
}

// SkipReason returns a non-empty explanation when the program described by
// spec must not be attached with the given configuration.
func SkipReason(spec AttachSpec, cfg Config) string {
	switch spec.Kind {
	case KindXDP, KindTCXIngress, KindTCXEgress:
		if cfg.Iface == "" {
			return "no network interface configured (set -iface)"
		}
	case KindSockOps, KindSkMsg:
		if cfg.CgroupPath == "" {
			return "no cgroup path configured (set -cgroup-path)"
		}
	case KindUprobe, KindUretprobe:
		if LibraryFor(spec.Symbol, cfg) == "" {
			return "no library found for symbol " + spec.Symbol + " (set -nccl-lib/-cuda-lib/-cufile-lib/-ucx-lib or -uprobe-pid)"
		}
	}
	return ""
}

// SkipIsNotRequested reports whether SkipReason skipped the program because a setting is empty
// (XDP/TCX need -iface, sockops/sk_msg need -cgroup-path), as opposed to the node lacking something it
// would need (a library for a uprobe), which is a gap the operator may want to hear about.
func SkipIsNotRequested(spec AttachSpec, cfg Config) bool {
	switch spec.Kind {
	case KindXDP, KindTCXIngress, KindTCXEgress:
		return cfg.Iface == ""
	case KindSockOps, KindSkMsg:
		return cfg.CgroupPath == ""
	}
	return false
}

// LibraryFor picks the configured library for a user-space symbol.
func LibraryFor(symbol string, cfg Config) string {
	switch {
	case strings.HasPrefix(symbol, "nccl"):
		return cfg.NCCLLib
	case strings.HasPrefix(symbol, "cuda"):
		return cfg.CUDALib
	case strings.HasPrefix(symbol, "cuFile"):
		return cfg.CuFileLib
	case strings.HasPrefix(symbol, "ucp_"):
		return cfg.UCXLib
	case strings.HasPrefix(symbol, "ibv_"):
		return cfg.IBVerbsLib
	}
	return ""
}

// defaultLibDirs are searched when no library was configured.
var defaultLibDirs = []string{
	"/usr/lib/x86_64-linux-gnu", "/usr/lib/aarch64-linux-gnu", "/usr/lib64", "/usr/lib",
	"/usr/local/lib", "/usr/local/cuda/lib64", "/usr/local/cuda/targets/x86_64-linux/lib",
	"/opt/nccl/lib", "/opt/ucx/lib", "/usr/local/ucx/lib",
}

// findLibInDirs returns the first existing file whose name starts with
// prefix (e.g. "libnccl.so") in dirs, preferring the shortest name.
func findLibInDirs(dirs []string, prefix string) string {
	for _, d := range dirs {
		matches, _ := filepath.Glob(filepath.Join(d, prefix+"*"))
		sort.Slice(matches, func(i, j int) bool { return len(matches[i]) < len(matches[j]) })
		for _, m := range matches {
			if st, err := os.Stat(m); err == nil && st.Mode().IsRegular() {
				return m
			}
		}
	}
	return ""
}

// ResolveLibraries fills NCCLLib/CUDALib/CuFileLib/UCXLib when unset, first from the process
// given by UprobePID, then from standard library directories.
func ResolveLibraries(cfg Config, res *UprobeResolver, dirs []string) Config {
	if dirs == nil {
		dirs = defaultLibDirs
	}
	if cfg.NCCLLib == "" && cfg.UprobePID > 0 && res != nil {
		if p, err := res.FindNCCLLibrary(cfg.UprobePID); err == nil {
			cfg.NCCLLib = p
		}
	}
	if cfg.CUDALib == "" && cfg.UprobePID > 0 && res != nil {
		if p, err := res.FindCUDALibrary(cfg.UprobePID); err == nil {
			cfg.CUDALib = p
		}
	}
	if cfg.CuFileLib == "" && cfg.UprobePID > 0 && res != nil {
		if p, err := res.FindCuFileLibrary(cfg.UprobePID); err == nil {
			cfg.CuFileLib = p
		}
	}
	if cfg.UCXLib == "" && cfg.UprobePID > 0 && res != nil {
		if p, err := res.FindUCXLibrary(cfg.UprobePID); err == nil {
			cfg.UCXLib = p
		}
	}
	if cfg.IBVerbsLib == "" && cfg.UprobePID > 0 && res != nil {
		if p, err := res.FindIBVerbsLibrary(cfg.UprobePID); err == nil {
			cfg.IBVerbsLib = p
		}
	}
	if cfg.NCCLLib == "" {
		cfg.NCCLLib = findLibInDirs(dirs, "libnccl.so")
	}
	if cfg.CUDALib == "" {
		cfg.CUDALib = findLibInDirs(dirs, "libcudart.so")
	}
	if cfg.CuFileLib == "" {
		cfg.CuFileLib = findLibInDirs(dirs, "libcufile.so")
	}
	if cfg.UCXLib == "" {
		cfg.UCXLib = findLibInDirs(dirs, "libucp.so")
	}
	if cfg.IBVerbsLib == "" {
		cfg.IBVerbsLib = findLibInDirs(dirs, "libibverbs.so")
	}
	return cfg
}

// MapClass says which decoder consumes a map's events.
type MapClass string

const (
	ClassFlow     MapClass = "flow"
	ClassGPU      MapClass = "gpu"
	ClassSecurity MapClass = "security"
	ClassFabric   MapClass = "fabric"
	ClassTrace    MapClass = "trace"
	ClassNone     MapClass = ""
)

var mapClasses = map[string]MapClass{
	// perf event arrays carrying struct flow_event
	"events":         ClassFlow, // tcp_trace
	"latency_events": ClassFlow, // latency_probe
	// ring buffers carrying struct gpu_event
	"nccl_events":     ClassGPU,
	"cuda_events":     ClassGPU,
	"rdma_events":     ClassGPU,
	"pattern_events":  ClassGPU,
	"pipeline_events": ClassGPU,
	"grad_events":     ClassGPU,
	// ring buffers carrying struct fabric_signal (straggler, rdma_health, gds_trace,
	// overlap, infer_latency, ucx_gloo, weight_exfil; roce_cnp and pfc_pause have
	// counters only, no ring)
	"fabric_events": ClassFabric,
	// ring buffer carrying struct trace_context (W3C traceparent seen at ingress, trace_correlator.c)
	"trace_events": ClassTrace,
	// ring buffers carrying struct security_event
	"escape_events":  ClassSecurity,
	"mining_events":  ClassSecurity,
	"exfil_events":   ClassSecurity,
	"privesc_events": ClassSecurity,
	"fim_events":     ClassSecurity,
}

// ClassifyMap returns the decoder class for an event map and whether a
// reader should be opened for it. Flow maps must be perf arrays; GPU, fabric
// and security maps must be ring buffers. Maps with no decoder yet
// (syscall_events, dns_events, connpool_events, ...) return ClassNone.
func ClassifyMap(name string, t ebpf.MapType) MapClass {
	c, ok := mapClasses[name]
	if !ok {
		return ClassNone
	}
	switch {
	case c == ClassFlow && t == ebpf.PerfEventArray:
		return c
	case (c == ClassGPU || c == ClassSecurity || c == ClassFabric || c == ClassTrace) && t == ebpf.RingBuf:
		return c
	}
	return ClassNone
}

// IsMissingSymbol reports whether an attach error for the given kind means the
// target symbol does not exist: a kprobe on a function this kernel (or a
// not-loaded module) does not have, or a uprobe on a symbol the library does
// not export. Optional hooks such as mlx5_ib_post_send or nvidia_fs_read are
// skipped, not failed. A missing library file is a real error, not a skip.
func IsMissingSymbol(kind AttachKind, err error) bool {
	switch kind {
	case KindKprobe, KindKretprobe:
		return errors.Is(err, os.ErrNotExist)
	case KindUprobe, KindUretprobe:
		return errors.Is(err, link.ErrNoSymbol)
	}
	return false
}
