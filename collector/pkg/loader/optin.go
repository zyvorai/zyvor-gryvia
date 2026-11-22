// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0
//
// Opt-in eBPF objects. The loader attaches every .o file in its directory whose hooks
// resolve, so an object nothing consumes would still cost a probe (and a ring buffer read)
// on every node. The programs below emit signals or keep counters that the collector does not
// interpret yet, or are steered by maps nothing writes; they attach only when named in
// Config.EnablePrograms (-enable-programs, chart ebpf.enablePrograms). The check is made on
// the parsed ELF, before anything is loaded into the kernel, and the programs still appear in
// Status as skipped, like the other opt-in programs (quota_pace, ibv_verbs).
//
// xdp_mux and roce_ecn are not here: they are governed by -xdp-mux.

package loader

import (
	"fmt"
	"sort"
	"strings"
)

// optInObjects maps an object file to why it is opt-in.
var optInObjects = map[string]string{
	"nccl_transport.o": "its signal is only counted, and nothing writes transport_hint",
	"p2p_fallback.o":   "its signal is only counted per job, not acted on",
	"capture_gate.o":   "nothing writes capture_lease, so it never arms and costs a map lookup per TCP send",
	"gpu_oom.o":        "its signal is only counted per job (routine under a caching allocator)",
	"graph_stall.o":    "its signal is only counted per job, not acted on",
	"gdr_fail.o":       "its signal is only counted per job, not acted on",
	"infer_ttft.o":     "its signals are only exported (TTFT p99, gap count), not acted on",
	"weight_mmap.o":    "its signal is only logged and counted, and it probes every mmap and IPv4 connect on the node",
	"gpu_dev.o":        "nothing writes gpu_dev_cfg or allowed_cg, so it only counts GPU opens while probing every file open",
	"ucx_complete.o":   "its signal is only counted per job, not acted on",
}

// EnableAllPrograms is the -enable-programs value that enables every opt-in program.
const EnableAllPrograms = "all"

// OptInPrograms returns the names (without ".o") of the opt-in programs, sorted.
func OptInPrograms() []string {
	names := make([]string, 0, len(optInObjects))
	for o := range optInObjects {
		names = append(names, strings.TrimSuffix(o, ".o"))
	}
	sort.Strings(names)
	return names
}

// ParseEnablePrograms parses a comma-separated -enable-programs value into names without ".o".
// "all" enables every opt-in program. An unknown name is an error, so a typo does not silently
// leave a program off.
func ParseEnablePrograms(list string) ([]string, error) {
	var out []string
	for _, raw := range strings.Split(list, ",") {
		name := strings.TrimSuffix(strings.TrimSpace(raw), ".o")
		if name == "" {
			continue
		}
		if name != EnableAllPrograms {
			if _, ok := optInObjects[name+".o"]; !ok {
				return nil, fmt.Errorf("unknown program %q for -enable-programs (opt-in programs: %s, or %q)",
					name, strings.Join(OptInPrograms(), ", "), EnableAllPrograms)
			}
		}
		out = append(out, name)
	}
	return out, nil
}

// OptInSkipReason explains why an object must not be loaded ("" = it may): it is opt-in and
// not named in cfg.EnablePrograms.
func OptInSkipReason(object string, cfg Config) string {
	why, ok := optInObjects[object]
	if !ok {
		return ""
	}
	name := strings.TrimSuffix(object, ".o")
	for _, e := range cfg.EnablePrograms {
		if e == EnableAllPrograms || e == name {
			return ""
		}
	}
	return "opt-in program: " + why + " (enable with -enable-programs=" + name + ")"
}
