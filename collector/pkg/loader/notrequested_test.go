// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package loader

import "testing"

func TestSkipIsNotRequested(t *testing.T) {
	for _, c := range []struct {
		name string
		spec AttachSpec
		cfg  Config
		want bool
	}{
		{"xdp without -iface", AttachSpec{Kind: KindXDP}, Config{}, true},
		{"xdp with -iface", AttachSpec{Kind: KindXDP}, Config{Iface: "eth0"}, false},
		{"tcx ingress without -iface", AttachSpec{Kind: KindTCXIngress}, Config{}, true},
		{"tcx egress without -iface", AttachSpec{Kind: KindTCXEgress}, Config{}, true},
		{"sockops without -cgroup-path", AttachSpec{Kind: KindSockOps}, Config{}, true},
		{"sk_msg without -cgroup-path", AttachSpec{Kind: KindSkMsg}, Config{}, true},
		{"sockops with -cgroup-path", AttachSpec{Kind: KindSockOps}, Config{CgroupPath: "/sys/fs/cgroup"}, false},
		// The node lacking a library is a gap worth hearing about, not a setting left empty.
		{"uprobe with no library", AttachSpec{Kind: KindUprobe, Symbol: "ncclAllReduce"}, Config{}, false},
		{"uretprobe with no library", AttachSpec{Kind: KindUretprobe, Symbol: "cudaMalloc"}, Config{}, false},
		{"kprobe", AttachSpec{Kind: KindKprobe, Symbol: "tcp_sendmsg"}, Config{}, false},
	} {
		if got := SkipIsNotRequested(c.spec, c.cfg); got != c.want {
			t.Errorf("%s: SkipIsNotRequested = %v, want %v", c.name, got, c.want)
		}
	}
}

// The gauge behind GryviaEbpfProgramNotAttached: a program the configuration did not ask for has no
// series, an attached one is 1, and one that should have attached but did not is 0 and can alert.
func TestAttachGauge(t *testing.T) {
	for _, c := range []struct {
		name       string
		s          ProgramStatus
		wantValue  float64
		wantExport bool
	}{
		{"attached", ProgramStatus{Attached: true}, 1, true},
		{"failed to attach", ProgramStatus{Reason: "attach: boom"}, 0, true},
		{"library missing on the node", ProgramStatus{Reason: "skipped: no library found for symbol ncclAllReduce"}, 0, true},
		{"interface already taken", ProgramStatus{Reason: "skipped: interface eth0 already has XDP program x"}, 0, true},
		{"not requested", ProgramStatus{Reason: "skipped: quota pacing is off", NotRequested: true}, 0, false},
		{"opt-in not enabled", ProgramStatus{Reason: "skipped: opt-in program: x", NotRequested: true}, 0, false},
	} {
		v, export := c.s.AttachGauge()
		if v != c.wantValue || export != c.wantExport {
			t.Errorf("%s: AttachGauge = (%v, %v), want (%v, %v)", c.name, v, export, c.wantValue, c.wantExport)
		}
	}
}
