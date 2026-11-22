// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0
//
// Load xdp_mux and the XDP feature programs as one chain, so several of them can
// observe the same interface. Linux allows one XDP program per device, so only the
// mux is attached: it tail-calls the first populated slot of its xdp_features prog
// array, and each feature tail-calls the next populated slot (ebpf/headers/xdp_chain.h).
//
// Every feature object declares its own xdp_features map; it is replaced here by the
// mux's, so that all of them share one array. A feature whose object is missing is
// skipped (its slot stays empty and the chain passes over it).
//
// LoadXDPMux is the standalone form, used by the tests. The collector's Manager does the same
// with Config.XDPMux (-xdp-mux, chart ebpf.xdpMux) while it loads the object directory.

package loader

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
)

// XDP chain slots. Must match ebpf/headers/xdp_chain.h.
const (
	XDPSlotRoceCNP      uint32 = 0
	XDPSlotPFCPause     uint32 = 1
	XDPSlotDNS          uint32 = 2
	XDPSlotPacketFilter uint32 = 3
	XDPSlotRoceECN      uint32 = 4
	XDPSlots                   = 5
)

const (
	muxObject  = "xdp_mux.o"
	muxProgram = "gryvia_xdp_mux"
	muxMapName = "xdp_features"
)

// XDPFeature names the object and program that belong in a slot.
type XDPFeature struct {
	Slot    uint32
	Object  string // file name inside the object directory
	Program string // program (function) name inside the object
}

// DefaultXDPFeatures is the Gryvia set, in slot order.
func DefaultXDPFeatures() []XDPFeature {
	return []XDPFeature{
		{XDPSlotRoceCNP, "roce_cnp.o", "gryvia_roce_cnp"},
		{XDPSlotPFCPause, "pfc_pause.o", "gryvia_pfc_pause"},
		{XDPSlotDNS, "dns_tracker.o", "xdp_dns_tracker"},
		{XDPSlotPacketFilter, "packet_filter.o", "xdp_packet_filter"},
		{XDPSlotRoceECN, "roce_ecn.o", "gryvia_roce_ecn"},
	}
}

// XDPMuxObject is the object file of the mux; XDPFeaturesMap the prog array it shares with the features.
const (
	XDPMuxObject   = muxObject
	XDPFeaturesMap = muxMapName
)

// XDPChainSlot returns the slot of a feature program, and whether (object, program) is one.
func XDPChainSlot(object, program string) (uint32, bool) {
	for _, f := range DefaultXDPFeatures() {
		if f.Object == object && f.Program == program {
			return f.Slot, true
		}
	}
	return 0, false
}

// xdpFeatureObject reports whether an object file holds an XDP chain feature.
func xdpFeatureObject(object string) (XDPFeature, bool) {
	for _, f := range DefaultXDPFeatures() {
		if f.Object == object {
			return f, true
		}
	}
	return XDPFeature{}, false
}

// XDPMux is a loaded mux with its feature chain. Close releases everything.
type XDPMux struct {
	// Mux is the only program to attach to the interface.
	Mux *ebpf.Program
	// Features is the shared xdp_features prog array.
	Features *ebpf.Map

	muxColl  *ebpf.Collection
	features map[string]*ebpf.Collection // by object file name
	loaded   []XDPFeature
	link     link.Link
}

// LoadXDPMux loads xdp_mux.o and the given features from dir. Features whose object
// file does not exist are skipped; any other failure unloads everything and returns it.
func LoadXDPMux(dir string, features []XDPFeature) (*XDPMux, error) {
	muxColl, err := loadCollection(filepath.Join(dir, muxObject), nil)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", muxObject, err)
	}
	x := &XDPMux{muxColl: muxColl, features: map[string]*ebpf.Collection{}}
	x.Mux = muxColl.Programs[muxProgram]
	x.Features = muxColl.Maps[muxMapName]
	if x.Mux == nil || x.Features == nil {
		x.Close()
		return nil, fmt.Errorf("%s has no program %q or map %q", muxObject, muxProgram, muxMapName)
	}

	for _, f := range features {
		if f.Slot >= XDPSlots {
			x.Close()
			return nil, fmt.Errorf("%s: slot %d out of range (max %d)", f.Object, f.Slot, XDPSlots-1)
		}
		path := filepath.Join(dir, f.Object)
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			continue
		}
		coll, err := loadCollection(path, map[string]*ebpf.Map{muxMapName: x.Features})
		if err != nil {
			x.Close()
			return nil, fmt.Errorf("load %s: %w", f.Object, err)
		}
		x.features[f.Object] = coll
		prog := coll.Programs[f.Program]
		if prog == nil {
			x.Close()
			return nil, fmt.Errorf("%s has no program %q", f.Object, f.Program)
		}
		if err := x.Features.Put(f.Slot, prog); err != nil {
			x.Close()
			return nil, fmt.Errorf("put %s into %s[%d]: %w", f.Program, muxMapName, f.Slot, err)
		}
		x.loaded = append(x.loaded, f)
	}
	return x, nil
}

func loadCollection(path string, replace map[string]*ebpf.Map) (*ebpf.Collection, error) {
	spec, err := ebpf.LoadCollectionSpec(path)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	return ebpf.NewCollectionWithOptions(spec, ebpf.CollectionOptions{
		MapReplacements: replace,
		Programs:        ebpf.ProgramOptions{LogSizeStart: 16 << 20},
	})
}

// Loaded returns the features that were found and placed in a slot.
func (x *XDPMux) Loaded() []XDPFeature { return append([]XDPFeature(nil), x.loaded...) }

// Attach attaches the mux (and only the mux) to the interface.
func (x *XDPMux) Attach(ifindex int) error {
	if x.link != nil {
		return errors.New("xdp_mux is already attached")
	}
	l, err := link.AttachXDP(link.XDPOptions{Program: x.Mux, Interface: ifindex})
	if err != nil {
		return fmt.Errorf("attach xdp_mux: %w", err)
	}
	x.link = l
	return nil
}

// Counter sums a per-CPU (or plain) uint64 counter of a loaded feature object, e.g.
// ("roce_cnp.o", "cnp_count", 0). The mux's own stats use object "xdp_mux.o".
func (x *XDPMux) Counter(object, mapName string, key uint32) (uint64, error) {
	coll := x.features[object]
	if object == muxObject {
		coll = x.muxColl
	}
	if coll == nil {
		return 0, fmt.Errorf("object %s is not loaded", object)
	}
	m := coll.Maps[mapName]
	if m == nil {
		return 0, fmt.Errorf("%s has no map %q", object, mapName)
	}
	var per []uint64
	if err := m.Lookup(key, &per); err != nil {
		return 0, err
	}
	var sum uint64
	for _, v := range per {
		sum += v
	}
	return sum, nil
}

// Close detaches the mux and releases every program and map.
func (x *XDPMux) Close() {
	if x.link != nil {
		_ = x.link.Close()
		x.link = nil
	}
	for _, c := range x.features {
		c.Close()
	}
	x.features = nil
	if x.muxColl != nil {
		x.muxColl.Close()
		x.muxColl = nil
	}
}
