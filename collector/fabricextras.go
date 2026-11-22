// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package main

// Opt-in fabric extras wired next to the eBPF-driven fabric pipeline:
//   - NIC hardware counters (-nic-counters): pkg/nic -> fabric Folder + metrics
//   - libibverbs control-path counters (-ibverbs-probes): ibv_verbs.o counters
//   - DCGM correlation (-dcgm-correlate): pkg/dcgm scraper -> fabric Folder
// All of it is UNVERIFIED on real GPU/RDMA/DCGM hardware (see docs/nccl-rdma-gpu-correlation.md).

import (
	"context"
	"errors"
	"math"
	"time"

	"go.uber.org/zap"

	"github.com/zyvorai/gryvia/collector/pkg/dcgm"
	"github.com/zyvorai/gryvia/collector/pkg/exporter"
	"github.com/zyvorai/gryvia/collector/pkg/fabric"
	"github.com/zyvorai/gryvia/collector/pkg/flight"
	"github.com/zyvorai/gryvia/collector/pkg/nic"
)

// Slots of ibv_verbs.c's ibv_counts array (IBV_SLOT_* in ebpf/ibv_verbs.c).
const (
	ibvSlotQPCreated    uint32 = 0
	ibvSlotQPDestroyed  uint32 = 1
	ibvSlotMRRegistered uint32 = 2
	ibvSlotMRBytes      uint32 = 3
	ibvSlots                   = 4
)

// nicPoller turns sysfs NIC counters into fabric folder signals and gauges.
// A node without /sys/class/infiniband is noted once and then polled quietly
// (devices can appear later, e.g. after a driver load).
type nicPoller struct {
	sampler *nic.Sampler
	folder  *fabric.Folder
	metrics *exporter.Metrics
	log     *zap.SugaredLogger
	noted   bool
}

func newNICPoller(root string, f *fabric.Folder, m *exporter.Metrics, log *zap.SugaredLogger) *nicPoller {
	return &nicPoller{sampler: &nic.Sampler{Reader: nic.Reader{Root: root}}, folder: f, metrics: m, log: log}
}

func (p *nicPoller) poll(now time.Time) {
	rates, ok, err := p.sampler.Step(now)
	if err != nil {
		if errors.Is(err, nic.ErrNotPresent) {
			if !p.noted {
				p.noted = true
				p.log.Infow("NIC counters: no RDMA devices (sysfs root absent); skipping until they appear")
			}
			return
		}
		p.log.Warnw("reading NIC counters", "error", err)
		return
	}
	p.noted = false
	if !ok {
		return
	}
	p.metrics.RecordNIC(rates)
	secs := rates.Interval.Seconds()
	count := func(rate float64) int64 { return int64(math.Round(rate * secs)) }
	retry, _ := rates.Totals(nic.RetryCounters)
	errs, _ := rates.Totals(nic.ErrorCounters)
	cnp, cnpOK := rates.Totals([]string{nic.CNPHandled})
	pause, pauseOK := rates.PauseTotal()
	cnpN, pauseN := int64(-1), int64(-1) // -1: counter not exported, XDP stays authoritative
	if cnpOK {
		cnpN = count(cnp)
	}
	if pauseOK {
		pauseN = count(pause)
	}
	p.folder.AddNIC(count(retry), count(errs), cnpN, pauseN)
}

// ibvPoller folds the ibv_verbs.o counters into metrics.
type ibvPoller struct {
	mgr interface {
		ReadCounter(object, name string, key uint32) (uint64, error)
	}
	metrics *exporter.Metrics
	log     *zap.SugaredLogger
	last    [ibvSlots]uint64
}

func (p *ibvPoller) poll() {
	var cur, delta [ibvSlots]uint64
	for slot := uint32(0); slot < ibvSlots; slot++ {
		v, err := p.mgr.ReadCounter("ibv_verbs.o", "ibv_counts", slot)
		if err != nil {
			p.log.Warnw("reading ibv_verbs counters", "slot", slot, "error", err)
			return
		}
		cur[slot] = v
		delta[slot] = v - p.last[slot]
		if v < p.last[slot] { // reset (program reloaded)
			delta[slot] = v
		}
	}
	p.last = cur
	p.metrics.RecordIBVerbs(delta[ibvSlotQPCreated], delta[ibvSlotQPDestroyed], delta[ibvSlotMRRegistered], delta[ibvSlotMRBytes])
}

// startDCGM scrapes dcgm-exporter and lets the folder correlate the samples of
// each job's GPUs with its NCCL call windows. A GPU sample is attributed to a
// job only through the exporter's Kubernetes labels (namespace, pod) resolved
// with the Flight Recorder's pod list; samples without them are never guessed.
func startDCGM(ctx context.Context, log *zap.SugaredLogger, f *fabric.Folder, id *flight.Resolver, url string, interval time.Duration, threshold float64) error {
	if err := dcgm.ValidateURL(url); err != nil {
		return err
	}
	store := dcgm.NewStore(fabric.DefaultWindow+time.Minute, 0)
	sc := &dcgm.Scraper{URL: url, Store: store}
	f.SetGPUSource(func(k fabric.JobKey, from time.Time) []dcgm.GPUSample {
		return store.Since(from, func(s dcgm.GPUSample) bool {
			if s.Namespace != k.Namespace || s.Pod == "" {
				return false
			}
			job, ok := id.JobOfPod(s.Namespace, s.Pod)
			return ok && job == k.Job
		})
	}, threshold)
	lastLog := time.Time{}
	go sc.Run(ctx, interval, func(err error) {
		if time.Since(lastLog) > time.Minute { // an absent exporter must not flood the log
			lastLog = time.Now()
			log.Warnw("dcgm-exporter scrape failed", "url", url, "error", err)
		}
	})
	log.Infow("DCGM correlation enabled (unverified on hardware)", "url", url, "interval", interval.String(), "idle_threshold", threshold)
	return nil
}
