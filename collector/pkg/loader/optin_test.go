// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package loader

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseEnablePrograms(t *testing.T) {
	for _, c := range []struct {
		in      string
		want    []string
		wantErr string
	}{
		{"", nil, ""},
		{" , ,", nil, ""},
		{"capture_gate", []string{"capture_gate"}, ""},
		{"capture_gate.o, gpu_dev", []string{"capture_gate", "gpu_dev"}, ""},
		{"all", []string{"all"}, ""},
		{"capture_gat", nil, `unknown program "capture_gat"`},
		{"gpu_dev,roce_cnp", nil, `unknown program "roce_cnp"`}, // a stable program is not opt-in
		{"xdp_mux", nil, `unknown program "xdp_mux"`},           // governed by -xdp-mux
	} {
		got, err := ParseEnablePrograms(c.in)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("ParseEnablePrograms(%q) error = %v, want one containing %q", c.in, err, c.wantErr)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("ParseEnablePrograms(%q) = %v, %v; want %v", c.in, got, err, c.want)
		}
	}
}

func TestOptInSkipReason(t *testing.T) {
	opt := OptInPrograms()
	if len(opt) != 10 {
		t.Fatalf("expected 10 opt-in programs, got %d: %v", len(opt), opt)
	}
	for _, name := range opt {
		obj := name + ".o"
		r := OptInSkipReason(obj, Config{})
		if !strings.Contains(r, "-enable-programs="+name) {
			t.Errorf("%s is not skipped by default or does not say how to enable it: %q", obj, r)
		}
		if r := OptInSkipReason(obj, Config{EnablePrograms: []string{name}}); r != "" {
			t.Errorf("%s must attach when named, got %q", obj, r)
		}
		if r := OptInSkipReason(obj, Config{EnablePrograms: []string{EnableAllPrograms}}); r != "" {
			t.Errorf("%s must attach with \"all\", got %q", obj, r)
		}
		if r := OptInSkipReason(obj, Config{EnablePrograms: []string{"some_other"}}); r == "" {
			t.Errorf("%s must stay skipped when another program is named", obj)
		}
	}
	// Everything else is untouched: the stable programs and the ones governed by -xdp-mux.
	for _, obj := range []string{"tcp_trace.o", "straggler.o", "roce_cnp.o", "roce_ecn.o", "xdp_mux.o", "driver_fim.o"} {
		if r := OptInSkipReason(obj, Config{}); r != "" {
			t.Errorf("%s must not be opt-in, got %q", obj, r)
		}
	}
}
