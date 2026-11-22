// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gryviav1 "github.com/zyvorai/gryvia/operators/network-intelligence/api/v1"
	"github.com/zyvorai/gryvia/operators/network-intelligence/pkg/sources"
)

func TestServiceGraphEdgesFromCollector(t *testing.T) {
	sg := &gryviav1.GryviaServiceGraph{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "g"},
		Spec: gryviav1.GryviaServiceGraphSpec{Namespaces: []string{"shop"}}}
	col := &fakeCollector{stats: sources.Stats{Reachable: 2, Total: 2}, graph: sources.Graph{
		Nodes: []sources.Node{{ID: "web", Namespace: "shop"}},
		Edges: []sources.Edge{
			{Source: "web", Target: "api", Protocol: "TCP", Port: 8080, BytesTotal: 2048, FlowCount: 12, LatencyMs: 4.25},
			{Source: "api", Target: "10.1.2.3", Protocol: "TCP", Port: 443, BytesTotal: 10, FlowCount: 1},
			{Source: "x", Target: "y", Protocol: "TCP", Port: 1, BytesTotal: 99}, // neither end in scope
		}}}
	c := newClient(t, sg, namespaceObj("shop"), svc("shop", "web", nil), svc("shop", "api", nil))
	r := &GryviaServiceGraphReconciler{Client: c, Scheme: testScheme(), Collector: col}
	mustReconcile(t, r, "shop", "g")

	got := &gryviav1.GryviaServiceGraph{}
	mustGet(t, c, "shop", "g", got)
	if len(got.Status.Edges) != 1 {
		t.Fatalf("want 1 edge (external and out-of-scope dropped), got %+v", got.Status.Edges)
	}
	e := got.Status.Edges[0]
	if e.Source != "web" || e.Destination != "api" || e.Protocol != "tcp" || e.Port != 8080 || e.BytesTotal != 2048 ||
		e.FlowCount != 12 || e.LatencyP50 != "4.2ms" && e.LatencyP50 != "4.3ms" || e.Throughput != "2.0 KiB" || e.LatencyP99 != "" || e.Verdict != "" {
		t.Fatalf("edge %+v", e)
	}
	if c := cond(got.Status.Conditions, ConditionSourceAvailable); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("condition %+v", c)
	}
	if got.Status.LastUpdated.IsZero() {
		t.Fatal("lastUpdated must be set on a successful refresh")
	}
	for _, n := range got.Status.Nodes {
		if n.Health != "unknown" {
			t.Fatalf("health must stay unknown without verdicts: %+v", n)
		}
	}

	// IncludeExternal keeps the IP edge and marks the node external.
	got.Spec.IncludeExternal = true
	if err := c.Update(t.Context(), got); err != nil {
		t.Fatal(err)
	}
	mustReconcile(t, r, "shop", "g")
	mustGet(t, c, "shop", "g", got)
	if len(got.Status.Edges) != 2 {
		t.Fatalf("includeExternal edges %+v", got.Status.Edges)
	}
}

func TestServiceGraphKeepsEdgesAndSaysWhenCollectorMissing(t *testing.T) {
	sg := &gryviav1.GryviaServiceGraph{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "g"},
		Spec:   gryviav1.GryviaServiceGraphSpec{Namespaces: []string{"shop"}},
		Status: gryviav1.GryviaServiceGraphStatus{Edges: []gryviav1.ServiceGraphEdge{{Source: "a", Destination: "b"}}}}
	c := newClient(t, sg, namespaceObj("shop"), svc("shop", "a", nil))
	for name, col := range map[string]sources.Collector{"none": nil, "no pods": &fakeCollector{err: notConfigured()}} {
		r := &GryviaServiceGraphReconciler{Client: c, Scheme: testScheme(), Collector: col}
		mustReconcile(t, r, "shop", "g")
		got := &gryviav1.GryviaServiceGraph{}
		mustGet(t, c, "shop", "g", got)
		cd := cond(got.Status.Conditions, ConditionSourceAvailable)
		if cd == nil || cd.Status != metav1.ConditionFalse || cd.Reason == "" || cd.Message == "" || len(got.Status.Edges) != 1 {
			t.Fatalf("%s: condition %+v edges %+v", name, cd, got.Status.Edges)
		}
	}
}

func TestServiceGraphPartialNodes(t *testing.T) {
	sg := &gryviav1.GryviaServiceGraph{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "g"}}
	c := newClient(t, sg, namespaceObj("shop"))
	r := &GryviaServiceGraphReconciler{Client: c, Scheme: testScheme(), Collector: &fakeCollector{stats: sources.Stats{Reachable: 2, Total: 3}}}
	mustReconcile(t, r, "shop", "g")
	got := &gryviav1.GryviaServiceGraph{}
	mustGet(t, c, "shop", "g", got)
	cd := cond(got.Status.Conditions, ConditionSourceAvailable)
	if cd.Status != metav1.ConditionTrue || cd.Reason != sources.ReasonPartial {
		t.Fatalf("%+v", cd)
	}
}

func TestNetworkAnomalyFromCollector(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	na := &gryviav1.GryviaNetworkAnomaly{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "a"},
		Spec: gryviav1.GryviaNetworkAnomalySpec{TargetService: "api"}}
	col := &fakeCollector{stats: sources.Stats{Reachable: 1, Total: 1}, anomalies: []sources.Anomaly{
		{Type: "latency_spike", Service: "api", Value: 90, Threshold: 20, DetectedAt: now.Add(-time.Minute), Message: "api latency 90ms"},
		{Type: "traffic_burst", Service: "web", Value: 5, Threshold: 1, DetectedAt: now},
	}}
	c := newClient(t, na)
	r := &GryviaNetworkAnomalyReconciler{Client: c, Scheme: testScheme(), Collector: col}
	mustReconcile(t, r, "shop", "a")
	mustReconcile(t, r, "shop", "a") // second poll must not duplicate
	got := &gryviav1.GryviaNetworkAnomaly{}
	mustGet(t, c, "shop", "a", got)
	if len(got.Status.Anomalies) != 1 {
		t.Fatalf("want 1 anomaly of api, got %+v", got.Status.Anomalies)
	}
	a := got.Status.Anomalies[0]
	if a.Type != "latency_spike" || a.Severity != "high" || a.Description != "api latency 90ms" || !a.Detected.Time.Equal(now.Add(-time.Minute)) {
		t.Fatalf("%+v", a)
	}
	if got.Status.LastCheck.IsZero() || cond(got.Status.Conditions, ConditionSourceAvailable).Status != metav1.ConditionTrue {
		t.Fatalf("%+v", got.Status)
	}

	// rules select the collector type: a throughput rule drops the latency anomaly
	got.Spec.DetectionRules = []gryviav1.DetectionRule{{Metric: "throughput", Operator: "gt", Threshold: 1}}
	got.Spec.TargetService = ""
	got.Status = gryviav1.GryviaNetworkAnomalyStatus{}
	if err := c.Update(t.Context(), got); err != nil {
		t.Fatal(err)
	}
	fresh := newAnomalies(got, col.anomalies, nil)
	if len(fresh) != 1 || fresh[0].Type != "traffic_burst" {
		t.Fatalf("%+v", fresh)
	}
}

func TestNetworkAnomalyUnavailable(t *testing.T) {
	na := &gryviav1.GryviaNetworkAnomaly{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "a"}}
	c := newClient(t, na)
	mustReconcile(t, &GryviaNetworkAnomalyReconciler{Client: c, Scheme: testScheme(), Collector: &fakeCollector{err: notConfigured()}}, "shop", "a")
	got := &gryviav1.GryviaNetworkAnomaly{}
	mustGet(t, c, "shop", "a", got)
	cd := cond(got.Status.Conditions, ConditionSourceAvailable)
	if cd.Status != metav1.ConditionFalse || cd.Reason != sources.ReasonNoCollectors || len(got.Status.Anomalies) != 0 {
		t.Fatalf("%+v", got.Status)
	}
}

func TestSecurityPolicyCountsCollectorAlertsOnce(t *testing.T) {
	t0 := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	sp := &gryviav1.GryviaSecurityPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "ml", Name: "p"},
		Spec: gryviav1.GryviaSecurityPolicySpec{AutoBlock: true, DetectionRules: []gryviav1.SecurityDetectionRule{
			{Type: "mining", Enabled: true}, {Type: "escape", Enabled: true}, {Type: "exfiltration", Enabled: false}}}}
	col := &fakeCollector{stats: sources.Stats{Reachable: 1, Total: 1}, alerts: []sources.SecurityAlert{
		{Timestamp: t0, EventType: "crypto_mining", Severity: "high", ProcessName: "xmrig", Details: "pool"},
		{Timestamp: t0.Add(time.Second), EventType: "namespace_breach", Severity: "critical"},
		{Timestamp: t0.Add(2 * time.Second), EventType: "data_exfiltration"}, // rule disabled
		{Timestamp: t0.Add(3 * time.Second), EventType: "suspicious_exec"},   // no rule
	}}
	c := newClient(t, sp)
	r := &GryviaSecurityPolicyReconciler{Client: c, Scheme: testScheme(), Collector: col}
	mustReconcile(t, r, "ml", "p")
	mustReconcile(t, r, "ml", "p") // same alerts again: no double count
	got := &gryviav1.GryviaSecurityPolicy{}
	mustGet(t, c, "ml", "p", got)
	if got.Status.AlertsTriggered != 2 || got.Status.DetectionCounts["mining"] != 1 || got.Status.DetectionCounts["escape"] != 1 || got.Status.ActiveDetections != 2 {
		t.Fatalf("%+v", got.Status)
	}
	if got.Status.Phase != "Active" || !got.Status.LastAlert.Time.Equal(t0.Add(time.Second)) {
		t.Fatalf("%+v", got.Status)
	}
	if ab := cond(got.Status.Conditions, ConditionAutoBlockIgnored); ab == nil || ab.Status != metav1.ConditionTrue {
		t.Fatalf("autoBlock must be reported as ignored: %+v", got.Status.Conditions)
	}
	// a new alert is counted, old ones are not
	col.alerts = append(col.alerts, sources.SecurityAlert{Timestamp: t0.Add(time.Minute), EventType: "crypto_mining"})
	mustReconcile(t, r, "ml", "p")
	mustGet(t, c, "ml", "p", got)
	if got.Status.AlertsTriggered != 3 || got.Status.DetectionCounts["mining"] != 2 {
		t.Fatalf("%+v", got.Status)
	}
}

func TestSecurityPolicyWebhookUsesOperatorContract(t *testing.T) {
	out := toWebhookAlerts([]sources.SecurityAlert{{EventType: "crypto_mining", Severity: "high", ProcessName: "xmrig", Path: "/tmp/x", SrcIP: "10.0.0.1", Details: "pool"}})
	if out[0].Type != "crypto_mining" || out[0].Process != "xmrig" || out[0].SourceIP != "10.0.0.1" || out[0].Message != "pool" || out[0].Path != "/tmp/x" {
		t.Fatalf("%+v", out[0])
	}
}

func TestSecurityPolicyDegradedWithoutCollector(t *testing.T) {
	sp := &gryviav1.GryviaSecurityPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "ml", Name: "p"}}
	c := newClient(t, sp)
	mustReconcile(t, &GryviaSecurityPolicyReconciler{Client: c, Scheme: testScheme()}, "ml", "p")
	got := &gryviav1.GryviaSecurityPolicy{}
	mustGet(t, c, "ml", "p", got)
	if got.Status.Phase != "Degraded" || cond(got.Status.Conditions, ConditionSourceAvailable).Status != metav1.ConditionFalse {
		t.Fatalf("%+v", got.Status)
	}
}

func TestTrafficInsightTopTalkersAndRate(t *testing.T) {
	now := time.Now().UTC()
	ti := &gryviav1.GryviaTrafficInsight{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "t"},
		Spec: gryviav1.GryviaTrafficInsightSpec{Service: "api", Namespace: "shop", Window: "1m"}}
	col := &fakeCollector{stats: sources.Stats{Reachable: 1, Total: 1}, graph: sources.Graph{Edges: []sources.Edge{
		{Source: "web", Target: "api", Protocol: "TCP", Port: 80, BytesTotal: 1000, FlowCount: 10, LatencyMs: 10},
		{Source: "batch", Target: "shop/api", Protocol: "TCP", Port: 80, BytesTotal: 5000, FlowCount: 10, LatencyMs: 30},
		{Source: "api", Target: "db", Protocol: "TCP", Port: 5432, BytesTotal: 7},
	}}, anomalies: []sources.Anomaly{{Type: "latency_spike", Service: "api", Value: 4, Threshold: 1, DetectedAt: now, Message: "m"}}}
	clock := now
	c := newClient(t, ti)
	r := &GryviaTrafficInsightReconciler{Client: c, Scheme: testScheme(), Collector: col, now: func() time.Time { return clock }}
	mustReconcile(t, r, "shop", "t")
	got := &gryviav1.GryviaTrafficInsight{}
	mustGet(t, c, "shop", "t", got)
	if len(got.Status.TopTalkers) != 2 || got.Status.TopTalkers[0].Service != "batch" || got.Status.TopTalkers[0].Bytes != 5000 {
		t.Fatalf("%+v", got.Status.TopTalkers)
	}
	if got.Status.P50Latency != "20.0ms" || got.Status.ThroughputBps != 0 || got.Status.P99Latency != "" || got.Status.DropCount != 0 {
		t.Fatalf("%+v", got.Status)
	}
	if len(got.Status.Anomalies) != 1 || got.Status.Anomalies[0].Severity != "high" {
		t.Fatalf("%+v", got.Status.Anomalies)
	}
	// 6000 more bytes in 30 s -> 200 B/s
	col.graph.Edges[0].BytesTotal += 1000
	col.graph.Edges[1].BytesTotal += 5000
	clock = clock.Add(30 * time.Second)
	mustReconcile(t, r, "shop", "t")
	mustGet(t, c, "shop", "t", got)
	if got.Status.ThroughputBps != 200 {
		t.Fatalf("throughput %d", got.Status.ThroughputBps)
	}
}
