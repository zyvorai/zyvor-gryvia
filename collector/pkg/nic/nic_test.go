// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package nic

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeTree builds a sysfs tree: root/<dev>/ports/<port>/{counters,hw_counters}/<name>.
func fakeTree(t *testing.T, dev, port string, counters, hw map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for sub, m := range map[string]map[string]string{"counters": counters, "hw_counters": hw} {
		dir := filepath.Join(root, dev, "ports", port, sub)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for k, v := range m {
			if err := os.WriteFile(filepath.Join(dir, k), []byte(v+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

func TestReadToleratesMissingAndJunk(t *testing.T) {
	root := fakeTree(t, "mlx5_0", "1",
		map[string]string{"port_xmit_data": "1000", "symbol_error": "3", "weird": "N/A"},
		map[string]string{"out_of_sequence": "7", "rp_cnp_handled": "40"})
	// a device with no ports dir, and a port with only counters/
	if err := os.MkdirAll(filepath.Join(root, "empty_dev"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "mlx5_1", "ports", "1", "counters"), 0o755); err != nil {
		t.Fatal(err)
	}
	snap, err := Reader{Root: root}.Read(time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	c := snap.Ports[PortKey{"mlx5_0", "1"}]
	if c["port_xmit_data"] != 1000 || c["out_of_sequence"] != 7 || c["rp_cnp_handled"] != 40 || len(c) != 4 {
		t.Errorf("counters %v (non-numeric must be skipped)", c)
	}
	if len(snap.Ports) != 1 {
		t.Errorf("ports without any readable counter must not appear: %v", snap.Ports)
	}
}

func TestReadAbsentRoot(t *testing.T) {
	_, err := Reader{Root: filepath.Join(t.TempDir(), "nope")}.Read(time.Now())
	if !errors.Is(err, ErrNotPresent) {
		t.Errorf("err = %v, want ErrNotPresent", err)
	}
}

func TestDeltaWrapAndReset(t *testing.T) {
	cases := []struct {
		name      string
		prev, cur uint64
		want      uint64
	}{
		{"port_xmit_data", 100, 250, 150},
		{"port_xmit_data", 1<<32 - 10, 5, 15},   // 32-bit wrap
		{"symbol_error", 65530, 4, 10},          // 16-bit wrap
		{"link_downed", 250, 3, 9},              // 8-bit wrap
		{"port_xmit_data", 5_000_000, 100, 100}, // low prev: a reset, not a wrap
		{"out_of_sequence", 1 << 40, 7, 7},      // 64-bit counter: a decrease is a reset
		{"symbol_error", 65530, 40000, 40000},   // cur not near the bottom: reset
		{"unknown_counter", 9, 9, 0},
	}
	for _, c := range cases {
		if got := Delta(c.name, c.prev, c.cur); got != c.want {
			t.Errorf("Delta(%s,%d,%d) = %d, want %d", c.name, c.prev, c.cur, got, c.want)
		}
	}
}

func TestSamplerRates(t *testing.T) {
	root := fakeTree(t, "mlx5_0", "1",
		map[string]string{"port_xmit_data": "1000", "symbol_error": "0"},
		map[string]string{"out_of_sequence": "10", "packet_seq_err": "0", "rp_cnp_handled": "0", "np_cnp_sent": "5", "rx_pause_ctrl_phy": "0", "rx_pause_duration": "0"})
	s := &Sampler{Reader: Reader{Root: root}}
	t0 := time.Unix(100, 0)
	if _, ok, err := s.Step(t0); ok || err != nil {
		t.Fatalf("first step must only prime: ok=%v err=%v", ok, err)
	}
	write := func(sub, name, v string) {
		if err := os.WriteFile(filepath.Join(root, "mlx5_0", "ports", "1", sub, name), []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("counters", "port_xmit_data", "1500")
	write("counters", "symbol_error", "4")
	write("hw_counters", "out_of_sequence", "30")
	write("hw_counters", "rp_cnp_handled", "200")
	write("hw_counters", "rx_pause_ctrl_phy", "50")
	write("hw_counters", "rx_pause_duration", "9999")
	rates, ok, err := s.Step(t0.Add(10 * time.Second))
	if !ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	p := rates.Ports[PortKey{"mlx5_0", "1"}]
	if p["port_xmit_data"] != 50 || p["symbol_error"] != 0.4 || p["out_of_sequence"] != 2 || p["rp_cnp_handled"] != 20 {
		t.Errorf("rates %v", p)
	}
	if v, present := rates.Totals(RetryCounters); !present || v != 2 {
		t.Errorf("retry total %v %v", v, present)
	}
	if v, present := rates.Totals(ErrorCounters); !present || v != 0.4 {
		t.Errorf("error total %v %v", v, present)
	}
	if v, present := rates.PauseTotal(); !present || v != 5 {
		t.Errorf("pause total %v %v (duration counters must be excluded)", v, present)
	}
	if _, present := rates.Totals([]string{"nonexistent"}); present {
		t.Error("absent counter must not be present")
	}
}

func TestSamplerAbsentRootResets(t *testing.T) {
	s := &Sampler{Reader: Reader{Root: filepath.Join(t.TempDir(), "none")}}
	if _, ok, err := s.Step(time.Now()); ok || !errors.Is(err, ErrNotPresent) {
		t.Errorf("ok=%v err=%v", ok, err)
	}
}
