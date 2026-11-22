// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package fabric

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Quota-driven pacing. MUTATING and OFF by default: with -quota-pace,
// -cgroup-path and -quota-pace-sync, every QuotaSyncInterval the collector reads
// GryviaQuota objects that set spec.network.maxEgressMbps and grants the Pacer a
// lease for the cgroups of that quota's pods that run on THIS node.
//
// Safety rules, each pinned by a test in quotasync_test.go / hostcgroup_test.go:
//   - a failed quota or pod list (any API error) grants nothing new and revokes
//     nothing: the existing leases just run out on their own (fail open);
//   - only cgroups under -cgroup-path are ever granted (HostCgroups);
//   - kube-system, gryvia-system and the collector's own namespace are never paced;
//   - the cap is bounded (1..MaxEgressMbps Mbit/s); an invalid one is skipped, never clamped up;
//   - a quota that is removed, set to 0, or whose pod is gone is revoked;
//   - -quota-pace-dry-run only logs.

const (
	// QuotaSyncInterval is how often the quotas are reconciled (and leases renewed).
	QuotaSyncInterval = 30 * time.Second
	// QuotaSyncLeaseTTL is the lease of a synced grant: four syncs. If the API
	// server is unreachable for longer, pacing switches itself off.
	QuotaSyncLeaseTTL = 2 * time.Minute
	// MaxEgressMbps is the largest accepted cap (32 Gbit/s); the kernel option holds
	// 32-bit bytes per second (about 34 Gbit/s).
	MaxEgressMbps = 32000
	// MinEgressMbps is the smallest accepted cap; the program itself never goes below 1 Mbit/s.
	MinEgressMbps = 1

	mbpsToBytesPerSec = 125_000
)

// EgressCap is one GryviaQuota that sets spec.network.maxEgressMbps.
type EgressCap struct {
	Quota      string
	Namespaces []string
	Mbps       int
}

// PodRef is a running pod on this node.
type PodRef struct {
	Namespace string
	Name      string
	UID       string
	QOS       string // Guaranteed, Burstable or BestEffort
}

// CapSource lists the quotas that set an egress cap.
type CapSource interface {
	EgressCaps(ctx context.Context) ([]EgressCap, error)
}

// PodSource lists the running pods of a namespace on this node.
type PodSource interface {
	NodePods(ctx context.Context, namespace string) ([]PodRef, error)
}

// CgroupFinder returns the cgroup ids of a pod (the pod cgroup and its container
// cgroups: the program matches the exact id of the connecting process's cgroup,
// descendants are not covered). Every id must lie under -cgroup-path.
type CgroupFinder interface {
	CgroupIDs(p PodRef) ([]uint64, error)
}

// Granter is the part of *Pacer the syncer uses.
type Granter interface {
	Grant(cgroupID, bytesPerSec uint64) (time.Time, uint64, error)
	Revoke(cgroupID uint64) error
}

// ProtectedNamespaces are never paced, whatever a quota lists.
func ProtectedNamespaces(own string) map[string]bool {
	m := map[string]bool{"kube-system": true, "kube-public": true, "kube-node-lease": true, "gryvia-system": true}
	if own != "" {
		m[own] = true
	}
	return m
}

// QuotaSyncer reconciles quota caps into Pacer grants. Sync must not be called
// concurrently with itself (Run is the only caller).
type QuotaSyncer struct {
	Caps      CapSource
	Pods      PodSource
	Cgroups   CgroupFinder
	Pacer     Granter
	Protected map[string]bool
	DryRun    bool
	Log       Logger

	granted map[uint64]uint64 // ids this syncer granted -> rate (real mode)
	planned map[uint64]uint64 // dry-run: what it would have granted
}

// NewQuotaSyncer creates a syncer. own is the collector's own namespace; the
// caller must refuse to start when it is unknown.
func NewQuotaSyncer(caps CapSource, pods PodSource, cg CgroupFinder, pacer Granter, own string, dryRun bool, log Logger) *QuotaSyncer {
	if log == nil {
		log = nopLogger{}
	}
	return &QuotaSyncer{Caps: caps, Pods: pods, Cgroups: cg, Pacer: pacer, Protected: ProtectedNamespaces(own),
		DryRun: dryRun, Log: log, granted: map[uint64]uint64{}, planned: map[uint64]uint64{}}
}

// capBytesPerSec converts a cap to bytes per second; ok is false for an invalid cap.
func capBytesPerSec(mbps int) (uint64, bool) {
	if mbps < MinEgressMbps || mbps > MaxEgressMbps {
		return 0, false
	}
	return uint64(mbps) * mbpsToBytesPerSec, true
}

// Sync runs one reconcile. A non-nil error means nothing was changed.
func (s *QuotaSyncer) Sync(ctx context.Context) error {
	caps, err := s.Caps.EgressCaps(ctx)
	if err != nil {
		s.Log.Warnw("quota pace sync: listing GryviaQuota failed; granting and revoking nothing (leases expire on their own)", "error", err)
		return fmt.Errorf("list quotas: %w", err)
	}

	// Strictest cap per namespace when several quotas list it.
	nsRate := map[string]uint64{}
	nsQuota := map[string]string{}
	for _, c := range caps {
		bps, ok := capBytesPerSec(c.Mbps)
		if !ok {
			if c.Mbps != 0 {
				s.Log.Warnw("quota pace sync: maxEgressMbps out of range, quota skipped", "quota", c.Quota, "max_egress_mbps", c.Mbps,
					"min", MinEgressMbps, "max", MaxEgressMbps)
			}
			continue
		}
		for _, ns := range c.Namespaces {
			if ns == "" || s.Protected[ns] {
				if ns != "" {
					s.Log.Warnw("quota pace sync: protected namespace is never paced", "quota", c.Quota, "namespace", ns)
				}
				continue
			}
			if cur, ok := nsRate[ns]; !ok || bps < cur {
				nsRate[ns], nsQuota[ns] = bps, c.Quota
			}
		}
	}

	// Phase 1: read everything. Any API error aborts before a single write.
	namespaces := make([]string, 0, len(nsRate))
	for ns := range nsRate {
		namespaces = append(namespaces, ns)
	}
	sort.Strings(namespaces)
	desired := map[uint64]uint64{}
	owner := map[uint64]string{}
	for _, ns := range namespaces {
		pods, err := s.Pods.NodePods(ctx, ns)
		if err != nil {
			s.Log.Warnw("quota pace sync: listing pods failed; granting and revoking nothing (leases expire on their own)", "namespace", ns, "error", err)
			return fmt.Errorf("list pods in %s: %w", ns, err)
		}
		for _, p := range pods {
			if p.Namespace != ns || s.Protected[p.Namespace] {
				continue // the source must only return what was asked for
			}
			ids, err := s.Cgroups.CgroupIDs(p)
			if err != nil {
				s.Log.Warnw("quota pace sync: pod cgroup not resolved, pod skipped", "namespace", p.Namespace, "pod", p.Name, "error", err)
				continue
			}
			for _, id := range ids {
				if cur, dup := desired[id]; !dup || nsRate[ns] < cur {
					desired[id] = nsRate[ns]
					owner[id] = nsQuota[ns] + " " + p.Namespace + "/" + p.Name
				}
			}
		}
	}

	if s.DryRun {
		s.dryRun(desired, owner)
		return nil
	}

	// Phase 2: write. Errors on single entries are logged; the rest continues.
	var errs []error
	ids := sortedIDs(desired)
	for _, id := range ids {
		_, rate, err := s.Pacer.Grant(id, desired[id])
		if err != nil {
			s.Log.Warnw("quota pace sync: grant failed", "cgroup_id", id, "target", owner[id], "error", err)
			errs = append(errs, err)
			continue
		}
		if _, had := s.granted[id]; !had {
			s.Log.Infow("quota pace sync: pacing pod cgroup", "cgroup_id", id, "target", owner[id], "bytes_per_sec", rate)
		}
		s.granted[id] = rate
	}
	for _, id := range sortedIDs(s.granted) {
		if _, want := desired[id]; want {
			continue
		}
		if err := s.Pacer.Revoke(id); err != nil {
			errs = append(errs, err) // Pacer logged it; the lease is kept and expires
			continue
		}
		s.Log.Infow("quota pace sync: no longer wanted, revoked", "cgroup_id", id)
		delete(s.granted, id)
	}
	return errors.Join(errs...)
}

func (s *QuotaSyncer) dryRun(desired map[uint64]uint64, owner map[uint64]string) {
	for _, id := range sortedIDs(desired) {
		if old, ok := s.planned[id]; !ok || old != desired[id] {
			s.Log.Infow("quota pace dry-run: would grant", "cgroup_id", id, "target", owner[id], "bytes_per_sec", desired[id])
		}
	}
	for _, id := range sortedIDs(s.planned) {
		if _, ok := desired[id]; !ok {
			s.Log.Infow("quota pace dry-run: would revoke", "cgroup_id", id)
		}
	}
	s.planned = desired
}

// Granted returns the cgroup ids this syncer currently holds grants for.
func (s *QuotaSyncer) Granted() []uint64 { return sortedIDs(s.granted) }

func sortedIDs(m map[uint64]uint64) []uint64 {
	out := make([]uint64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Run syncs immediately and then every interval until ctx is done. It never
// removes anything itself at exit: Pacer.Run / Pacer.Shutdown do (fail open).
func (s *QuotaSyncer) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = QuotaSyncInterval
	}
	_ = s.Sync(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = s.Sync(ctx)
		}
	}
}
