// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zyvorai/gryvia/collector/pkg/exporter"
	"github.com/zyvorai/gryvia/collector/pkg/fabric"
)

// promauto registers globally: one Metrics per test binary.
var testMetrics = exporter.NewMetrics()

func writeFake(t *testing.T, root, dev, sub, name, v string) {
	t.Helper()
	dir := filepath.Join(root, dev, "ports", "1", sub)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(v), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestNICPollerFoldsIntoFabric drives the poller with a fake sysfs tree
// (UNVERIFIED on real RDMA hardware).
func TestNICPollerFoldsIntoFabric(t *testing.T) {
	root := t.TempDir()
	set := func(retry, cnp string) {
		writeFake(t, root, "mlx5_0", "hw_counters", "out_of_sequence", retry)
		writeFake(t, root, "mlx5_0", "hw_counters", "rp_cnp_handled", cnp)
		writeFake(t, root, "mlx5_0", "counters", "port_xmit_data", "0")
	}
	set("0", "0")
	f := fabric.NewFolder(time.Minute)
	p := newNICPoller(root, f, testMetrics, zap.NewNop().Sugar())
	t0 := time.Now()
	p.poll(t0) // primes
	set("20", "300")
	p.poll(t0.Add(10 * time.Second))
	snap := f.Snapshot()
	if got := snap[fabric.NICJob].NICRetryRate; got <= 0 {
		t.Errorf("NIC retry rate = %v", got)
	}
	if got := snap[fabric.CNPJob].CNPRate; got <= 0 {
		t.Errorf("NIC CNP rate = %v", got)
	}
}

func TestNICPollerAbsentSysfsIsQuiet(t *testing.T) {
	f := fabric.NewFolder(time.Minute)
	p := newNICPoller(filepath.Join(t.TempDir(), "missing"), f, testMetrics, zap.NewNop().Sugar())
	p.poll(time.Now())
	p.poll(time.Now().Add(time.Second))
	if len(f.Snapshot()) != 0 {
		t.Error("no RDMA devices must record nothing")
	}
}
