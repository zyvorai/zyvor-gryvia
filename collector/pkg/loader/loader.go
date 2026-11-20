// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Package loader handles loading, attaching, and lifecycle management
// of compiled eBPF programs.  It discovers .o files in a directory,
// loads them with cilium/ebpf (CO-RE relocations use the kernel BTF from
// /sys/kernel/btf/vmlinux automatically), attaches each program according
// to its ELF section name, and provides event readers.  Individual
// failures are logged and recorded in a status list; they never abort
// the whole load.
package loader

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/perf"
	"github.com/cilium/ebpf/ringbuf"
	"go.uber.org/zap"
)

// ProgramStatus records the outcome of loading/attaching one program.
type ProgramStatus struct {
	Object   string `json:"object"`
	Program  string `json:"program"`
	Section  string `json:"section"`
	Kind     string `json:"kind"`
	Target   string `json:"target,omitempty"`
	Attached bool   `json:"attached"`
	Reason   string `json:"reason,omitempty"` // why not attached
	// NotRequested marks a program that was not attached because the configuration did not ask for it: an
	// opt-in program that was not enabled (optin.go), an opt-in feature that is off (quota pacing, the
	// libibverbs probes, -xdp-mux, no -infer-ports), or XDP/TCX without -iface and sockops/sk_msg without
	// -cgroup-path. That is not a failure to attach, so it gets no gryvia_ebpf_program_attached series (the
	// gauge that drives GryviaEbpfProgramNotAttached). A missing library or symbol, an interface that is
	// already taken and an attach error are not "not requested": they still export 0.
	NotRequested bool `json:"notRequested,omitempty"`
}

// AttachGauge is the value of gryvia_ebpf_program_attached for this program, and whether the series
// should exist at all.
func (s ProgramStatus) AttachGauge() (value float64, export bool) {
	switch {
	case s.NotRequested:
		return 0, false
	case s.Attached:
		return 1, true
	}
	return 0, true
}

// MapReader is an opened event reader plus its dispatch class.
type MapReader struct {
	Object string
	Map    string
	Class  MapClass
	Perf   *perf.Reader
	Ring   *ringbuf.Reader
}

// Manager owns the lifecycle of all loaded eBPF programs.
type Manager struct {
	cfg Config
	log *zap.SugaredLogger

	mu          sync.Mutex
	collections []*ebpf.Collection
	links       []io.Closer
	readers     []MapReader
	status      []ProgramStatus
	exes        map[string]*link.Executable
	objMaps     map[string]map[string]*ebpf.Map // object file -> map name -> map
	xdp         xdpOwners
	// muxFeatures is xdp_mux's xdp_features prog array once the mux is attached (Config.XDPMux).
	// XDP feature programs loaded after that share it and are placed in their slot instead of being
	// attached on their own.
	muxFeatures *ebpf.Map
}

// New creates a Manager for the given configuration.
func New(cfg Config, log *zap.SugaredLogger) (*Manager, error) {
	if cfg.Dir == "" {
		cfg.Dir = "/opt/gryvia/ebpf"
	}
	if _, err := os.Stat(cfg.Dir); err != nil {
		return nil, fmt.Errorf("ebpf directory not found: %w", err)
	}
	return &Manager{cfg: cfg, log: log, exes: map[string]*link.Executable{},
		objMaps: map[string]map[string]*ebpf.Map{}}, nil
}

// LoadAndAttach loads every .o file in the directory and attaches its
// programs. It returns an error only if the directory cannot be read or
// nothing at all could be attached.
func (m *Manager) LoadAndAttach() error {
	entries, err := os.ReadDir(m.cfg.Dir)
	if err != nil {
		return fmt.Errorf("reading ebpf dir: %w", err)
	}
	m.cfg = ResolveLibraries(m.cfg, NewUprobeResolver(""), nil)
	m.log.Infow("uprobe libraries", "nccl", m.cfg.NCCLLib, "cuda", m.cfg.CUDALib, "cufile", m.cfg.CuFileLib, "ucx", m.cfg.UCXLib, "ibverbs", m.cfg.IBVerbsLib)

	// The mux goes first: the XDP features loaded after it need its prog array.
	muxDone := false
	if m.cfg.XDPMux && m.cfg.Iface != "" {
		if _, err := os.Stat(filepath.Join(m.cfg.Dir, XDPMuxObject)); err == nil {
			muxDone = true
			if err := m.loadObject(XDPMuxObject, filepath.Join(m.cfg.Dir, XDPMuxObject)); err != nil {
				m.log.Warnw("failed to load object, continuing", "path", XDPMuxObject, "error", err)
				m.addStatus(ProgramStatus{Object: XDPMuxObject, Kind: "object", Reason: err.Error()})
			}
		}
	}

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".o" {
			continue
		}
		if muxDone && entry.Name() == XDPMuxObject {
			continue
		}
		objPath := filepath.Join(m.cfg.Dir, entry.Name())
		if err := m.loadObject(entry.Name(), objPath); err != nil {
			m.log.Warnw("failed to load object, continuing", "path", objPath, "error", err)
			m.addStatus(ProgramStatus{Object: entry.Name(), Kind: "object", Reason: err.Error()})
		}
	}

	attached := 0
	for _, s := range m.Status() {
		if s.Attached {
			attached++
		}
	}
	m.log.Infow("eBPF load complete", "attached", attached, "total", len(m.Status()))
	if attached == 0 {
		return fmt.Errorf("no eBPF programs attached")
	}
	return nil
}

func (m *Manager) addStatus(s ProgramStatus) {
	m.mu.Lock()
	m.status = append(m.status, s)
	m.mu.Unlock()
}

func (m *Manager) loadObject(file, path string) error {
	spec, err := ebpf.LoadCollectionSpec(path)
	if err != nil {
		return fmt.Errorf("parse collection spec: %w", err)
	}
	// An opt-in object nothing consumes yet is not loaded at all; its programs are listed as skipped.
	if reason := OptInSkipReason(file, m.cfg); reason != "" {
		names := make([]string, 0, len(spec.Programs))
		for n := range spec.Programs {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, name := range names {
			st := ProgramStatus{Object: file, Program: name, Section: spec.Programs[name].SectionName, Reason: "skipped: " + reason, NotRequested: true}
			if as, err := ParseSection(st.Section); err == nil {
				st.Kind, st.Target = string(as.Kind), as.Symbol
			}
			m.addStatus(st)
		}
		m.log.Infow("skipping object", "object", file, "reason", reason)
		return nil
	}
	// An XDP feature behind an attached mux shares the mux's prog array: every object declares its
	// own xdp_features map (headers/xdp_chain.h), replaced here by the mux's.
	var copts ebpf.CollectionOptions
	if m.muxFeatures != nil {
		if _, isFeature := xdpFeatureObject(file); isFeature {
			copts.MapReplacements = map[string]*ebpf.Map{XDPFeaturesMap: m.muxFeatures}
		}
	}
	coll, err := ebpf.NewCollectionWithOptions(spec, copts)
	if err != nil {
		return fmt.Errorf("create collection: %w", err)
	}
	m.collections = append(m.collections, coll)
	m.mu.Lock()
	m.objMaps[file] = coll.Maps
	m.mu.Unlock()

	// infer_latency.c watches only the ports userspace writes into infer_ports.
	// The map must be filled before any probe attaches; with no ports configured
	// the object is skipped entirely (its tcp_recvmsg kprobe would fire on every
	// receive for nothing).
	skipAll := ""
	if mp, ok := coll.Maps[InferPortsMap]; ok {
		if len(m.cfg.InferPorts) == 0 {
			skipAll = "no inference ports configured (set -infer-ports)"
		} else if err := FillInferPorts(mp, m.cfg.InferPorts); err != nil {
			return fmt.Errorf("fill %s: %w", InferPortsMap, err)
		}
	}

	// quota_pace.c is the only program that changes sockets: it never attaches
	// unless -quota-pace and -cgroup-path are both set.
	if _, ok := coll.Maps[PaceRateMap]; ok {
		if r := QuotaPaceSkipReason(m.cfg); r != "" && skipAll == "" {
			skipAll = r
		}
	}

	// ibv_verbs.c probes libibverbs, shared by every RDMA process: opt-in.
	if _, ok := coll.Maps[IBVCountsMap]; ok {
		if r := IBVerbsSkipReason(m.cfg); r != "" && skipAll == "" {
			skipAll = r
		}
	}

	// xdp_mux.c is only attached when chaining is on.
	if file == XDPMuxObject && skipAll == "" {
		skipAll = XDPMuxSkipReason(m.cfg)
	}

	names := make([]string, 0, len(coll.Programs))
	for n := range coll.Programs {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		st := ProgramStatus{Object: file, Program: name}
		ps := spec.Programs[name]
		st.Section = ps.SectionName
		as, err := ParseSection(ps.SectionName)
		if err != nil {
			st.Reason = err.Error()
			m.log.Warnw("skipping program", "object", file, "program", name, "reason", st.Reason)
			m.addStatus(st)
			continue
		}
		st.Kind = string(as.Kind)
		st.Target = as.Symbol
		reason := skipAll
		notRequested := skipAll != "" // every skipAll reason is a switch that is off or an input that is not set
		if reason == "" {
			reason = SkipReason(as, m.cfg)
			notRequested = reason != "" && SkipIsNotRequested(as, m.cfg)
		}
		if reason != "" {
			st.Reason = "skipped: " + reason
			st.NotRequested = notRequested
			m.log.Infow("skipping program", "object", file, "program", name, "reason", reason)
			m.addStatus(st)
			continue
		}
		owner := file + "/" + name
		// Behind an attached mux an XDP feature is not attached: it goes into its slot of the shared
		// prog array and the chain calls it.
		if as.Kind == KindXDP && m.muxFeatures != nil {
			if slot, ok := XDPChainSlot(file, name); ok {
				if err := m.muxFeatures.Put(slot, coll.Programs[name]); err != nil {
					st.Reason = fmt.Sprintf("place in %s slot %d: %v", XDPMuxObject, slot, err)
					m.log.Warnw("failed to place program in the xdp_mux chain", "object", file, "program", name, "slot", slot, "error", err)
					m.addStatus(st)
					continue
				}
				st.Attached = true
				st.Target = fmt.Sprintf("%s slot %d", XDPMuxObject, slot)
				m.addStatus(st)
				m.log.Infow("placed program in the xdp_mux chain", "object", file, "program", name, "slot", slot)
				continue
			}
		}
		if as.Kind == KindXDP {
			if cur, ok := m.xdp.claim(m.cfg.Iface, owner); !ok {
				st.Reason = "skipped: " + XDPConflictReason(m.cfg.Iface, cur)
				m.log.Warnw("skipping XDP program: interface already owned", "object", file, "program", name,
					"iface", m.cfg.Iface, "owner", cur)
				m.addStatus(st)
				continue
			}
		}
		l, err := m.attach(as, coll.Programs[name], coll)
		if err != nil && as.Kind == KindXDP {
			m.xdp.release(m.cfg.Iface)
			if errors.Is(err, syscall.EBUSY) {
				st.Reason = "skipped: interface " + m.cfg.Iface + " already has an XDP program that this collector did not load (one XDP program per interface)"
				m.log.Warnw("skipping XDP program: interface already has an XDP program", "object", file,
					"program", name, "iface", m.cfg.Iface)
				m.addStatus(st)
				continue
			}
		}
		if err != nil && IsMissingSymbol(as.Kind, err) {
			// Optional hook (e.g. mlx5_ib_post_send, nvidia_fs_read): not an error.
			st.Reason = "skipped: symbol not found (" + as.Symbol + ")"
			m.log.Infow("skipping program: symbol not found", "object", file, "program", name,
				"section", ps.SectionName, "symbol", as.Symbol)
			m.addStatus(st)
			continue
		}
		if err != nil {
			st.Reason = err.Error()
			m.log.Warnw("failed to attach program", "object", file, "program", name,
				"section", ps.SectionName, "error", err)
			m.addStatus(st)
			continue
		}
		m.mu.Lock()
		m.links = append(m.links, l)
		m.mu.Unlock()
		st.Attached = true
		m.addStatus(st)
		m.log.Infow("attached program", "object", file, "program", name, "section", ps.SectionName)
		if file == XDPMuxObject {
			// From here on, XDP features load against the mux's prog array.
			m.muxFeatures = coll.Maps[XDPFeaturesMap]
		}
	}

	// Open a reader of the matching type for each event map.
	for name, mp := range coll.Maps {
		class := ClassifyMap(name, mp.Type())
		if class == ClassNone {
			continue
		}
		mr := MapReader{Object: file, Map: name, Class: class}
		if mp.Type() == ebpf.PerfEventArray {
			mr.Perf, err = perf.NewReader(mp, os.Getpagesize()*64)
		} else {
			mr.Ring, err = ringbuf.NewReader(mp)
		}
		if err != nil {
			m.log.Warnw("failed to open event reader", "map", name, "error", err)
			continue
		}
		m.mu.Lock()
		m.readers = append(m.readers, mr)
		m.mu.Unlock()
		m.log.Infow("opened event reader", "object", file, "map", name, "class", string(class))
	}
	return nil
}

func (m *Manager) executable(path string) (*link.Executable, error) {
	if ex, ok := m.exes[path]; ok {
		return ex, nil
	}
	ex, err := link.OpenExecutable(path)
	if err != nil {
		return nil, err
	}
	m.exes[path] = ex
	return ex, nil
}

func (m *Manager) attach(as AttachSpec, prog *ebpf.Program, coll *ebpf.Collection) (io.Closer, error) {
	switch as.Kind {
	case KindKprobe:
		return link.Kprobe(as.Symbol, prog, nil)
	case KindKretprobe:
		return link.Kretprobe(as.Symbol, prog, nil)
	case KindUprobe, KindUretprobe:
		lib := LibraryFor(as.Symbol, m.cfg)
		ex, err := m.executable(lib)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", lib, err)
		}
		if as.Kind == KindUprobe {
			return ex.Uprobe(as.Symbol, prog, nil)
		}
		return ex.Uretprobe(as.Symbol, prog, nil)
	case KindTracepoint:
		return link.Tracepoint(as.Group, as.Symbol, prog, nil)
	case KindRawTracepoint:
		return link.AttachRawTracepoint(link.RawTracepointOptions{Name: as.Symbol, Program: prog})
	case KindTPBTF:
		return link.AttachTracing(link.TracingOptions{Program: prog})
	case KindXDP:
		iface, err := net.InterfaceByName(m.cfg.Iface)
		if err != nil {
			return nil, fmt.Errorf("interface %s: %w", m.cfg.Iface, err)
		}
		return link.AttachXDP(link.XDPOptions{Program: prog, Interface: iface.Index})
	case KindTCXIngress, KindTCXEgress:
		iface, err := net.InterfaceByName(m.cfg.Iface)
		if err != nil {
			return nil, fmt.Errorf("interface %s: %w", m.cfg.Iface, err)
		}
		at := ebpf.AttachTCXIngress
		if as.Kind == KindTCXEgress {
			at = ebpf.AttachTCXEgress
		}
		return link.AttachTCX(link.TCXOptions{Program: prog, Attach: at, Interface: iface.Index})
	case KindSockOps:
		return link.AttachCgroup(link.CgroupOptions{
			Path: m.cfg.CgroupPath, Program: prog, Attach: ebpf.AttachCGroupSockOps})
	case KindSkMsg:
		for _, mp := range coll.Maps {
			if t := mp.Type(); t == ebpf.SockHash || t == ebpf.SockMap {
				opts := link.RawAttachProgramOptions{
					Target: mp.FD(), Program: prog, Attach: ebpf.AttachSkMsgVerdict}
				if err := link.RawAttachProgram(opts); err != nil {
					return nil, err
				}
				return closerFunc(func() error {
					return link.RawDetachProgram(link.RawDetachProgramOptions{
						Target: opts.Target, Program: prog, Attach: opts.Attach})
				}), nil
			}
		}
		return nil, fmt.Errorf("no sockmap/sockhash in object")
	}
	return nil, fmt.Errorf("unsupported attach kind %q", as.Kind)
}

// Map returns a loaded map of an object file (nil when the object or map is not
// loaded). The map belongs to the Manager: do not Close it.
func (m *Manager) Map(object, name string) *ebpf.Map {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.objMaps[object][name]
}

// ReadCounter returns the sum over all CPUs of a __u64 counter in a map of the
// given object (e.g. roce_cnp.o, cnp_count, slot 0). It works for per-CPU and
// plain arrays. An error means the object or map was not loaded.
func (m *Manager) ReadCounter(object, name string, key uint32) (uint64, error) {
	m.mu.Lock()
	mp := m.objMaps[object][name]
	m.mu.Unlock()
	if mp == nil {
		return 0, fmt.Errorf("map %s/%s not loaded", object, name)
	}
	return sumCounter(mp, key)
}

func sumCounter(mp *ebpf.Map, key uint32) (uint64, error) {
	switch mp.Type() {
	case ebpf.PerCPUArray, ebpf.PerCPUHash, ebpf.LRUCPUHash:
		var per []uint64
		if err := mp.Lookup(&key, &per); err != nil {
			return 0, err
		}
		var sum uint64
		for _, v := range per {
			sum += v
		}
		return sum, nil
	}
	var v uint64
	if err := mp.Lookup(&key, &v); err != nil {
		return 0, err
	}
	return v, nil
}

// Readers returns all opened event readers.
func (m *Manager) Readers() []MapReader {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]MapReader(nil), m.readers...)
}

// PerfReaders returns perf readers of the given class.
func (m *Manager) PerfReaders(class MapClass) []*perf.Reader {
	var out []*perf.Reader
	for _, r := range m.Readers() {
		if r.Perf != nil && r.Class == class {
			out = append(out, r.Perf)
		}
	}
	return out
}

// RingBufReaders returns ring buffer readers of the given class.
func (m *Manager) RingBufReaders(class MapClass) []*ringbuf.Reader {
	var out []*ringbuf.Reader
	for _, r := range m.Readers() {
		if r.Ring != nil && r.Class == class {
			out = append(out, r.Ring)
		}
	}
	return out
}

// Status returns a snapshot of per-program load/attach results.
func (m *Manager) Status() []ProgramStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]ProgramStatus(nil), m.status...)
}

// Close detaches all programs and frees resources.
func (m *Manager) Close() {
	for _, l := range m.links {
		if err := l.Close(); err != nil {
			m.log.Warnw("closing link", "error", err)
		}
	}
	for _, r := range m.readers {
		if r.Perf != nil {
			_ = r.Perf.Close()
		}
		if r.Ring != nil {
			_ = r.Ring.Close()
		}
	}
	for _, c := range m.collections {
		c.Close()
	}
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }
