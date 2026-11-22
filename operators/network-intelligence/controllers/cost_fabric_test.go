// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"encoding/json"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gryviav1 "github.com/zyvorai/gryvia/operators/network-intelligence/api/v1"
)

func usageRecord(ns, name, node, hour, zone string, egress int64) client.Object {
	return unstr("GryviaNetworkUsageRecord", ns, name, map[string]interface{}{
		"tenant": "acme", "node": node, "source": "collector", "hour": hour, "peerClass": "external",
		"zoneClass": zone, "egressBytes": egress, "final": true}, nil)
}

func networkRate(name string, same, cross, internet float64, cur string) client.Object {
	return unstr("GryviaNetworkRate", "", name, map[string]interface{}{
		"sameZone": same, "crossZone": cross, "internetEgress": internet, "currency": cur}, nil)
}

func TestNetworkCostFromUsageRecordsAndRate(t *testing.T) {
	nc := &gryviav1.GryviaNetworkCost{ObjectMeta: metav1.ObjectMeta{Namespace: "gryvia-system", Name: "c"},
		Spec: gryviav1.GryviaNetworkCostSpec{CostCenters: []gryviav1.CostCenterMapping{{Namespace: "tenant-acme", Team: "ml"}}}}
	const GB = 1_000_000_000
	c := newClient(t, nc,
		usageRecord("tenant-acme", "r1", "n1", "2026-03-01T10:00:00Z", "internet", 2*GB),
		usageRecord("tenant-acme", "r2", "n2", "2026-03-01T11:00:00Z", "internet", 1*GB),
		usageRecord("tenant-acme", "r3", "n1", "2026-03-01T10:00:00Z", "cross-zone", 10*GB),
		usageRecord("tenant-acme", "r3dup", "n1", "2026-03-01T10:00:00Z", "cross-zone", 4*GB), // duplicate fragment: larger kept, not summed
		usageRecord("tenant-acme", "r4", "n1", "2026-03-02T00:00:00Z", "same-zone", 5*GB),
		usageRecord("tenant-acme", "r5", "n1", "2026-03-02T00:00:00Z", "same-node", 7*GB), // unpriced class
		networkRate("default", 0.01, 0.02, 0.09, "USD"),
	)
	r := &GryviaNetworkCostReconciler{Client: c, Scheme: testScheme()}
	mustReconcile(t, r, "gryvia-system", "c")
	mustReconcile(t, r, "gryvia-system", "c") // idempotent: reports are replaced, not appended
	got := &gryviav1.GryviaNetworkCost{}
	mustGet(t, c, "gryvia-system", "c", got)
	if len(got.Status.Reports) != 2 || got.Status.Phase != "Active" {
		t.Fatalf("%+v", got.Status)
	}
	d1, d2 := got.Status.Reports[0], got.Status.Reports[1]
	if d1.Period != "2026-03-01" || d1.Namespace != "tenant-acme" || d1.Team != "ml" || d1.ExternalBytes != 3*GB || d1.CrossZoneBytes != 10*GB {
		t.Fatalf("day 1 %+v", d1)
	}
	// 3 GB * 0.09 + 10 GB * 0.02 = 0.47
	if d1.TotalCostUSD < 0.4699 || d1.TotalCostUSD > 0.4701 {
		t.Fatalf("cost %v", d1.TotalCostUSD)
	}
	if d2.Period != "2026-03-02" || d2.SameZoneBytes != 5*GB || d2.TotalCostUSD < 0.0499 || d2.TotalCostUSD > 0.0501 {
		t.Fatalf("day 2 %+v", d2)
	}
	if c := cond(got.Status.Conditions, ConditionSourceAvailable); c.Status != metav1.ConditionTrue {
		t.Fatalf("%+v", c)
	}
	if c := cond(got.Status.Conditions, "RatesConfigured"); c.Status != metav1.ConditionTrue {
		t.Fatalf("%+v", c)
	}
}

func TestNetworkCostSpecRateBeatsClusterRateAndNoDataIsHonest(t *testing.T) {
	nc := &gryviav1.GryviaNetworkCost{ObjectMeta: metav1.ObjectMeta{Namespace: "s", Name: "c"},
		Spec: gryviav1.GryviaNetworkCostSpec{TargetNamespaces: []string{"tenant-acme"}, CostPerGB: gryviav1.CostPerGB{InternetEgress: 1}}}
	c := newClient(t, nc, networkRate("default", 0, 0, 5, "USD"))
	mustReconcile(t, &GryviaNetworkCostReconciler{Client: c, Scheme: testScheme()}, "s", "c")
	got := &gryviav1.GryviaNetworkCost{}
	mustGet(t, c, "s", "c", got)
	if got.Status.Phase != "AwaitingData" || len(got.Status.Reports) != 0 {
		t.Fatalf("%+v", got.Status)
	}
	if cd := cond(got.Status.Conditions, ConditionSourceAvailable); cd.Status != metav1.ConditionFalse || cd.Reason != "NoData" {
		t.Fatalf("%+v", cd)
	}
	// no price at all is reported, bytes still counted
	nc2 := &gryviav1.GryviaNetworkCost{ObjectMeta: metav1.ObjectMeta{Namespace: "s", Name: "c2"}}
	c2 := newClient(t, nc2, usageRecord("tenant-acme", "r", "n", "2026-03-01T10:00:00Z", "internet", 1000))
	mustReconcile(t, &GryviaNetworkCostReconciler{Client: c2, Scheme: testScheme()}, "s", "c2")
	mustGet(t, c2, "s", "c2", got)
	if cond(got.Status.Conditions, "RatesConfigured").Status != metav1.ConditionFalse || got.Status.Reports[0].ExternalBytes != 1000 || got.Status.Reports[0].TotalCostUSD != 0 {
		t.Fatalf("%+v", got.Status)
	}
}

func signalObj(ns, name, job string, updated time.Time, status map[string]interface{}) client.Object {
	if !updated.IsZero() {
		status["updatedAt"] = updated.UTC().Format(time.RFC3339)
	}
	return unstr("GryviaFabricSignal", ns, name, map[string]interface{}{"jobRef": job}, status)
}

func TestTrainingInsightFromFabricSignal(t *testing.T) {
	now := time.Now()
	ti := &gryviav1.GryviaTrainingInsight{ObjectMeta: metav1.ObjectMeta{Namespace: "ml", Name: "ti"},
		Spec: gryviav1.GryviaTrainingInsightSpec{TargetJob: "llama"}}
	c := newClient(t, ti,
		signalObj("ml", "sig", "llama", now, map[string]interface{}{"stragglerRank": int64(3), "ncclP99ms": 42.5, "collectiveMaxSkewMs": 9.0,
			"overlapIdleRatio": 0.1, "rdmaRetryRate": 0.002, "scoreDelta": 0.4}),
		signalObj("ml", "other", "other-job", now, map[string]interface{}{"ncclP99ms": 1.0}))
	r := &GryviaTrainingInsightReconciler{Client: c, Scheme: testScheme(), now: func() time.Time { return now }}
	mustReconcile(t, r, "ml", "ti")
	got := &gryviav1.GryviaTrainingInsight{}
	mustGet(t, c, "ml", "ti", got)
	s := got.Status
	if s.Phase != "Active" || s.SignalRef != "sig" || s.NCCLP99Ms != 42.5 || s.CollectiveMaxSkewMs != 9 || s.ScoreDelta != 0.4 ||
		len(s.Stragglers) != 1 || s.Stragglers[0].Rank != 3 || s.Bottleneck != "communication" || s.CommPattern != "unknown" || len(s.RankStats) != 0 {
		t.Fatalf("%+v", s)
	}
	if cd := cond(s.Conditions, ConditionSourceAvailable); cd.Status != metav1.ConditionTrue {
		t.Fatalf("%+v", cd)
	}
}

func TestTrainingAndInferenceInsightHonestWithoutSignal(t *testing.T) {
	now := time.Now()
	ti := &gryviav1.GryviaTrainingInsight{ObjectMeta: metav1.ObjectMeta{Namespace: "ml", Name: "ti"}, Spec: gryviav1.GryviaTrainingInsightSpec{TargetJob: "j"}}
	ii := &gryviav1.GryviaInferenceInsight{ObjectMeta: metav1.ObjectMeta{Namespace: "ml", Name: "ii"}, Spec: gryviav1.GryviaInferenceInsightSpec{TargetService: "svc"}}
	stale := signalObj("ml", "stale", "svc", now.Add(-time.Hour), map[string]interface{}{"e2eP99ms": 10.0})
	c := newClient(t, ti, ii)
	mustReconcile(t, &GryviaTrainingInsightReconciler{Client: c, Scheme: testScheme(), now: func() time.Time { return now }}, "ml", "ti")
	gt := &gryviav1.GryviaTrainingInsight{}
	mustGet(t, c, "ml", "ti", gt)
	if cd := cond(gt.Status.Conditions, ConditionSourceAvailable); gt.Status.Phase != "AwaitingData" || cd.Status != metav1.ConditionFalse || cd.Reason != "NoSignal" {
		t.Fatalf("%+v", gt.Status)
	}
	if err := c.Create(t.Context(), stale); err != nil {
		t.Fatal(err)
	}
	mustReconcile(t, &GryviaInferenceInsightReconciler{Client: c, Scheme: testScheme(), now: func() time.Time { return now }}, "ml", "ii")
	gi := &gryviav1.GryviaInferenceInsight{}
	mustGet(t, c, "ml", "ii", gi)
	if cd := cond(gi.Status.Conditions, ConditionSourceAvailable); cd.Status != metav1.ConditionFalse || cd.Reason != "Stale" {
		t.Fatalf("%+v", gi.Status.Conditions)
	}
}

func TestInferenceInsightFromFabricSignal(t *testing.T) {
	now := time.Now()
	ii := &gryviav1.GryviaInferenceInsight{ObjectMeta: metav1.ObjectMeta{Namespace: "ml", Name: "ii"}, Spec: gryviav1.GryviaInferenceInsightSpec{TargetService: "svc"}}
	c := newClient(t, ii, signalObj("ml", "sig", "svc", now, map[string]interface{}{
		"engine": "vllm", "ttftP99ms": 120.0, "itlP99ms": 15.0, "queueTimeP99ms": 60.0, "e2eP99ms": 100.0,
		"inferWaitP99ms": 2.0, "requestsWaiting": int64(4), "kvCacheUsage": 0.5}))
	mustReconcile(t, &GryviaInferenceInsightReconciler{Client: c, Scheme: testScheme(), now: func() time.Time { return now }}, "ml", "ii")
	got := &gryviav1.GryviaInferenceInsight{}
	mustGet(t, c, "ml", "ii", got)
	s := got.Status
	if s.Phase != "Active" || s.Engine != "vllm" || s.TTFTP99Ms == nil || *s.TTFTP99Ms != 120 || s.ITLP99Ms == nil || *s.ITLP99Ms != 15 ||
		s.P99TotalNs != 100_000_000 || s.LatencyBreakdown.GPUQueueNs != 60_000_000 || s.LatencyBreakdown.TotalNs != 100_000_000 ||
		s.Bottleneck != "queue" || s.RequestsWaiting == nil || *s.RequestsWaiting != 4 || s.P50TotalNs != 0 || s.LatencyBreakdown.DNSNs != 0 {
		b, _ := json.Marshal(s)
		t.Fatalf("%s", b)
	}
	// unmeasured stays unset, never zero
	ii2 := &gryviav1.GryviaInferenceInsight{ObjectMeta: metav1.ObjectMeta{Namespace: "ml", Name: "ii2"}, Spec: gryviav1.GryviaInferenceInsightSpec{TargetService: "s2"}}
	if err := c.Create(t.Context(), ii2); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(t.Context(), signalObj("ml", "sig2", "s2", now, map[string]interface{}{"inferWaitP99ms": 1.0})); err != nil {
		t.Fatal(err)
	}
	mustReconcile(t, &GryviaInferenceInsightReconciler{Client: c, Scheme: testScheme(), now: func() time.Time { return now }}, "ml", "ii2")
	mustGet(t, c, "ml", "ii2", got)
	if got.Status.TTFTP99Ms != nil || got.Status.E2EP99Ms != nil || got.Status.Bottleneck != "unknown" {
		t.Fatalf("%+v", got.Status)
	}
}

var _ = corev1.Pod{}
