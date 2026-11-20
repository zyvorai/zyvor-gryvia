// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controllers

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	gryviav1 "github.com/zyvorai/gryvia/operators/network-intelligence/api/v1"
	"github.com/zyvorai/gryvia/operators/network-intelligence/pkg/sources"
)

func pod(ns, name string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels}}
}

func TestTraceSessionCapturesNetraFlowsIntoConfigMap(t *testing.T) {
	now := time.Now().UTC()
	ts := &gryviav1.GryviaTraceSession{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "tr"},
		Spec: gryviav1.GryviaTraceSessionSpec{Service: "api", Namespace: "shop", Duration: "5m",
			Filters: &gryviav1.TraceFilters{Port: 8080}}}
	netra := &fakeNetra{on: true, records: []sources.FlowRecord{
		{ObservedAt: now.Add(time.Second), Namespace: "shop", Pod: "api-1", Peer: "db", Protocol: "tcp", Port: 8080, Bytes: 10},
		{ObservedAt: now.Add(time.Second), Namespace: "shop", Pod: "api-1", Peer: "db", Protocol: "tcp", Port: 9999, Bytes: 10}, // filtered by port
		{ObservedAt: now.Add(time.Second), Namespace: "shop", Pod: "web-1", Peer: "api", Protocol: "tcp", Port: 8080},           // other pod
		{ObservedAt: now.Add(time.Second), Namespace: "other", Pod: "api-1", Port: 8080},                                        // other namespace
		{ObservedAt: now.Add(-time.Hour), Namespace: "shop", Pod: "api-1", Port: 8080},                                          // before the session
		{ObservedAt: now.Add(2 * time.Second), Namespace: "shop", Pod: "api-2", Peer: "10.0.0.9", Protocol: "tcp", Port: 8080, Blocked: true},
	}}
	c := newClient(t, ts, svc("shop", "api", map[string]string{"app": "api"}),
		pod("shop", "api-1", map[string]string{"app": "api"}), pod("shop", "api-2", map[string]string{"app": "api"}), pod("shop", "web-1", map[string]string{"app": "web"}))
	r := &GryviaTraceSessionReconciler{Client: c, Scheme: testScheme(), Netra: netra}
	mustReconcile(t, r, "shop", "tr") // start
	mustReconcile(t, r, "shop", "tr") // active poll
	got := &gryviav1.GryviaTraceSession{}
	mustGet(t, c, "shop", "tr", got)
	if got.Status.Phase != "active" || got.Status.FlowsCaptured != 2 || got.Status.ResultRef == nil {
		t.Fatalf("%+v", got.Status)
	}
	cm := &corev1.ConfigMap{}
	mustGet(t, c, "shop", got.Status.ResultRef.Name, cm)
	var flows []capturedFlow
	if err := json.Unmarshal([]byte(cm.Data["flows"]), &flows); err != nil || len(flows) != 2 {
		t.Fatalf("%v %s", err, cm.Data["flows"])
	}
	if flows[1].Verdict != "DROP" || flows[0].Source != "shop/api-1" || flows[0].Protocol != "TCP" {
		t.Fatalf("%+v", flows)
	}
	// polling again does not double count
	mustReconcile(t, r, "shop", "tr")
	mustGet(t, c, "shop", "tr", got)
	if got.Status.FlowsCaptured != 2 {
		t.Fatalf("flowsCaptured %d", got.Status.FlowsCaptured)
	}
}

func TestTraceSessionWithoutNetraSaysSo(t *testing.T) {
	ts := &gryviav1.GryviaTraceSession{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "tr"}, Spec: gryviav1.GryviaTraceSessionSpec{Service: "api"}}
	c := newClient(t, ts)
	r := &GryviaTraceSessionReconciler{Client: c, Scheme: testScheme()}
	mustReconcile(t, r, "shop", "tr")
	mustReconcile(t, r, "shop", "tr")
	got := &gryviav1.GryviaTraceSession{}
	mustGet(t, c, "shop", "tr", got)
	cd := cond(got.Status.Conditions, ConditionSourceAvailable)
	if cd == nil || cd.Status != metav1.ConditionFalse || cd.Reason != sources.ReasonNotConfigured || !strings.Contains(cd.Message, "GRYVIA_NETRA_URL") || got.Status.FlowsCaptured != 0 {
		t.Fatalf("%+v", got.Status)
	}
}

func TestFlowPolicySuggestedIsNotEnforcedUntilApproved(t *testing.T) {
	fp := &gryviav1.GryviaFlowPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "sugg", Labels: map[string]string{LabelSuggested: "true"}},
		Spec: gryviav1.GryviaFlowPolicySpec{Source: &gryviav1.FlowEndpoint{Service: "web"}, Destination: &gryviav1.FlowEndpoint{Service: "api", Port: 8080}, Protocol: "tcp", Action: "allow"}}
	c := newClient(t, fp, svc("shop", "web", map[string]string{"app": "web"}), svc("shop", "api", map[string]string{"app": "api"}))
	r := &GryviaFlowPolicyReconciler{Client: c, Scheme: testScheme()}
	mustReconcile(t, r, "shop", "sugg")
	got := &gryviav1.GryviaFlowPolicy{}
	mustGet(t, c, "shop", "sugg", got)
	if got.Status.Phase != "Suggested" || got.Status.Enforced || got.Status.CiliumPolicyRef != "" {
		t.Fatalf("%+v", got.Status)
	}
	cnp := &unstructured.Unstructured{}
	cnp.SetGroupVersionKind(schema.GroupVersionKind{Group: "cilium.io", Version: "v2", Kind: "CiliumNetworkPolicy"})
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: "shop", Name: "ffp-sugg"}, cnp); err == nil {
		t.Fatal("a suggestion must not create a CiliumNetworkPolicy")
	}

	// approval = label removed -> enforced with selectors resolved from the Services
	delete(got.Labels, LabelSuggested)
	if err := c.Update(t.Context(), got); err != nil {
		t.Fatal(err)
	}
	mustReconcile(t, r, "shop", "sugg")
	mustGet(t, c, "shop", "sugg", got)
	if got.Status.Phase != "Enforced" || !got.Status.Enforced {
		t.Fatalf("%+v", got.Status)
	}
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: "shop", Name: "ffp-sugg"}, cnp); err != nil {
		t.Fatal(err)
	}
	spec := cnp.Object["spec"].(map[string]interface{})
	sel := spec["endpointSelector"].(map[string]interface{})["matchLabels"].(map[string]interface{})
	egress := spec["egress"].([]interface{})[0].(map[string]interface{})
	to := egress["toEndpoints"].([]interface{})[0].(map[string]interface{})["matchLabels"].(map[string]interface{})
	if sel["app"] != "web" || to["app"] != "api" {
		t.Fatalf("selectors %+v %+v", sel, to)
	}
	if len(cnp.GetOwnerReferences()) != 1 {
		t.Fatal("cilium policy must be owned by the flow policy")
	}
	// matchedFlows without Netra stays unset and is explained
	if got.Status.MatchedFlows != 0 || cond(got.Status.Conditions, ConditionSourceAvailable).Reason != sources.ReasonNotConfigured {
		t.Fatalf("%+v", got.Status)
	}
}

func TestFlowPolicyUnresolvableServiceFailsInsteadOfBroadPolicy(t *testing.T) {
	fp := &gryviav1.GryviaFlowPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "p"},
		Spec: gryviav1.GryviaFlowPolicySpec{Source: &gryviav1.FlowEndpoint{Service: "ghost"}, Destination: &gryviav1.FlowEndpoint{Port: 1}, Action: "allow"}}
	c := newClient(t, fp)
	mustReconcile(t, &GryviaFlowPolicyReconciler{Client: c, Scheme: testScheme()}, "shop", "p")
	got := &gryviav1.GryviaFlowPolicy{}
	mustGet(t, c, "shop", "p", got)
	if got.Status.Phase != "Failed" || cond(got.Status.Conditions, "Enforceable").Reason != "UnresolvedEndpoint" {
		t.Fatalf("%+v", got.Status)
	}
	cnp := &unstructured.Unstructured{}
	cnp.SetGroupVersionKind(schema.GroupVersionKind{Group: "cilium.io", Version: "v2", Kind: "CiliumNetworkPolicy"})
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: "shop", Name: "ffp-p"}, cnp); err == nil {
		t.Fatal("no policy may be created")
	}
}

func TestFlowPolicyMatchedFlowsFromNetra(t *testing.T) {
	now := time.Now()
	fp := &gryviav1.GryviaFlowPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "p"},
		Spec: gryviav1.GryviaFlowPolicySpec{Source: &gryviav1.FlowEndpoint{Service: "web"}, Destination: &gryviav1.FlowEndpoint{Service: "api", Port: 8080}, Protocol: "tcp", Action: "allow"}}
	netra := &fakeNetra{on: true, records: []sources.FlowRecord{
		{Namespace: "shop", Pod: "web-1", Port: 8080, Protocol: "tcp", ObservedAt: now},
		{Namespace: "shop", Pod: "web-1", Port: 8080, Protocol: "tcp", ObservedAt: now},
		{Namespace: "shop", Pod: "web-1", Port: 53, Protocol: "udp", ObservedAt: now},
		{Namespace: "shop", Pod: "batch-1", Port: 8080, Protocol: "tcp", ObservedAt: now},
	}}
	c := newClient(t, fp, svc("shop", "web", map[string]string{"app": "web"}), svc("shop", "api", map[string]string{"app": "api"}),
		pod("shop", "web-1", map[string]string{"app": "web"}), pod("shop", "batch-1", map[string]string{"app": "batch"}))
	mustReconcile(t, &GryviaFlowPolicyReconciler{Client: c, Scheme: testScheme(), Netra: netra}, "shop", "p")
	got := &gryviav1.GryviaFlowPolicy{}
	mustGet(t, c, "shop", "p", got)
	if got.Status.MatchedFlows != 2 || cond(got.Status.Conditions, ConditionSourceAvailable).Status != metav1.ConditionTrue {
		t.Fatalf("%+v", got.Status)
	}
}

func autoPolicyFixture(t *testing.T, mode string, window string, created time.Time) (*gryviav1.GryviaAutoPolicy, *fakeCollector) {
	t.Helper()
	ap := &gryviav1.GryviaAutoPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "learn", UID: "u1", CreationTimestamp: metav1.NewTime(created)},
		Spec: gryviav1.GryviaAutoPolicySpec{Mode: mode, LearningWindow: window, TargetNamespaces: []string{"shop"}, ExcludeServices: []string{"kube-dns"}}}
	col := &fakeCollector{stats: sources.Stats{Reachable: 1, Total: 1}, graph: sources.Graph{Edges: []sources.Edge{
		{Source: "web", Target: "api", Protocol: "TCP", Port: 8080, BytesTotal: 1, FlowCount: 200},
		{Source: "api", Target: "db", Protocol: "TCP", Port: 5432, BytesTotal: 1, FlowCount: 12},
		{Source: "api", Target: "kube-dns", Protocol: "UDP", Port: 53, FlowCount: 100}, // excluded
		{Source: "api", Target: "1.2.3.4", Protocol: "TCP", Port: 443},                 // not a Service
		{Source: "web", Target: "api", Protocol: "ICMP", Port: 0},                      // not tcp/udp
	}}}
	return ap, col
}

func TestAutoPolicySuggestsDeterministicLabelledOwnedNeverApplied(t *testing.T) {
	ap, col := autoPolicyFixture(t, "suggest", "", time.Now().Add(-time.Hour))
	c := newClient(t, ap, svc("shop", "web", nil), svc("shop", "api", nil), svc("shop", "db", nil), svc("shop", "kube-dns", nil))
	r := &GryviaAutoPolicyReconciler{Client: c, Scheme: testScheme(), Collector: col}
	mustReconcile(t, r, "shop", "learn")
	mustReconcile(t, r, "shop", "learn") // idempotent

	list := &gryviav1.GryviaFlowPolicyList{}
	if err := c.List(t.Context(), list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 2 {
		t.Fatalf("want 2 suggestions, got %d", len(list.Items))
	}
	names := map[string]bool{}
	for _, p := range list.Items {
		names[p.Name] = true
		if p.Labels[LabelSuggested] != "true" || p.Labels[LabelAutoPolicy] != "learn" || p.Spec.Action != "allow" ||
			len(p.OwnerReferences) != 1 || p.OwnerReferences[0].Name != "learn" || p.Annotations["gryvia.io/confidence"] == "" {
			t.Fatalf("%+v", p.ObjectMeta)
		}
		if len(p.Name) > 63 || strings.ToLower(p.Name) != p.Name {
			t.Fatalf("name %q", p.Name)
		}
	}
	e := learnedEdge{Source: "web", Destination: "api", SourceNS: "shop", DestNS: "shop", Protocol: "tcp", Port: 8080}
	if !names[suggestionName(ap, e)] {
		t.Fatalf("names %v, want %s", names, suggestionName(ap, e))
	}
	got := &gryviav1.GryviaAutoPolicy{}
	mustGet(t, c, "shop", "learn", got)
	if got.Status.Phase != "suggesting" || got.Status.LearnedPolicies != 2 || len(got.Status.SuggestedPolicies) != 2 || got.Status.AppliedPolicies != 0 {
		t.Fatalf("%+v", got.Status)
	}
	// nothing is applied: no CiliumNetworkPolicy
	cnps := &unstructured.UnstructuredList{}
	cnps.SetGroupVersionKind(schema.GroupVersionKind{Group: "cilium.io", Version: "v2", Kind: "CiliumNetworkPolicyList"})
	if err := c.List(t.Context(), cnps); err == nil && len(cnps.Items) != 0 {
		t.Fatal("no Cilium policy may be created by AutoPolicy")
	}
	// learned edges survive a collector that no longer reports them (edges expire after 10 minutes there)
	col.graph = sources.Graph{}
	mustReconcile(t, r, "shop", "learn")
	mustGet(t, c, "shop", "learn", got)
	if got.Status.LearnedPolicies != 2 {
		t.Fatalf("learned edges must persist: %+v", got.Status)
	}
	// human approval (label removed) is counted and the suggestion is not recreated or reverted
	first := list.Items[0]
	delete(first.Labels, LabelSuggested)
	if err := c.Update(t.Context(), &first); err != nil {
		t.Fatal(err)
	}
	mustReconcile(t, r, "shop", "learn")
	mustGet(t, c, "shop", "learn", got)
	again := &gryviav1.GryviaFlowPolicy{}
	mustGet(t, c, "shop", first.Name, again)
	if got.Status.AppliedPolicies != 1 || again.Labels[LabelSuggested] != "" {
		t.Fatalf("applied %d labels %v", got.Status.AppliedPolicies, again.Labels)
	}
}

func TestAutoPolicyLearnModeAndWindowCreateNothing(t *testing.T) {
	for name, tc := range map[string]struct{ mode, window string }{
		"learn mode":      {"learn", ""},
		"window not over": {"suggest", "24h"},
		"enforce is suggest but honours the window": {"enforce", "24h"},
	} {
		ap, col := autoPolicyFixture(t, tc.mode, tc.window, time.Now().Add(-time.Minute))
		c := newClient(t, ap, svc("shop", "web", nil), svc("shop", "api", nil), svc("shop", "db", nil))
		mustReconcile(t, &GryviaAutoPolicyReconciler{Client: c, Scheme: testScheme(), Collector: col}, "shop", "learn")
		list := &gryviav1.GryviaFlowPolicyList{}
		_ = c.List(t.Context(), list)
		got := &gryviav1.GryviaAutoPolicy{}
		mustGet(t, c, "shop", "learn", got)
		if len(list.Items) != 0 || got.Status.Phase != "learning" || got.Status.LearnedPolicies != 2 {
			t.Fatalf("%s: %d objects, %+v", name, len(list.Items), got.Status)
		}
	}
}

func TestAutoPolicyEnforceModeNeverAutoApplies(t *testing.T) {
	ap, col := autoPolicyFixture(t, "enforce", "", time.Now().Add(-time.Hour))
	ap.Spec.ApprovalRequired = false
	c := newClient(t, ap, svc("shop", "web", nil), svc("shop", "api", nil), svc("shop", "db", nil))
	mustReconcile(t, &GryviaAutoPolicyReconciler{Client: c, Scheme: testScheme(), Collector: col}, "shop", "learn")
	list := &gryviav1.GryviaFlowPolicyList{}
	_ = c.List(t.Context(), list)
	for _, p := range list.Items {
		if p.Labels[LabelSuggested] != "true" {
			t.Fatalf("enforce mode must leave suggestions labelled: %+v", p.Labels)
		}
	}
	got := &gryviav1.GryviaAutoPolicy{}
	mustGet(t, c, "shop", "learn", got)
	if cond(got.Status.Conditions, ConditionEnforceNotAutomatic) == nil || got.Status.AppliedPolicies != 0 {
		t.Fatalf("%+v", got.Status)
	}
}

func TestAutoPolicyForeignSourceNamespaceSkippedAndCollectorMissing(t *testing.T) {
	ap, col := autoPolicyFixture(t, "suggest", "", time.Now().Add(-time.Hour))
	ap.Namespace = "other"
	ap.Spec.TargetNamespaces = []string{"shop"}
	c := newClient(t, ap, svc("shop", "web", nil), svc("shop", "api", nil), svc("shop", "db", nil))
	mustReconcile(t, &GryviaAutoPolicyReconciler{Client: c, Scheme: testScheme(), Collector: col}, "other", "learn")
	list := &gryviav1.GryviaFlowPolicyList{}
	_ = c.List(t.Context(), list)
	got := &gryviav1.GryviaAutoPolicy{}
	mustGet(t, c, "other", "learn", got)
	if len(list.Items) != 0 || !strings.Contains(cond(got.Status.Conditions, ConditionSourceAvailable).Message, "outside namespace other") {
		t.Fatalf("%d %+v", len(list.Items), got.Status.Conditions)
	}
	// no collector: honest condition, no objects
	ap2, _ := autoPolicyFixture(t, "suggest", "", time.Now().Add(-time.Hour))
	c2 := newClient(t, ap2)
	mustReconcile(t, &GryviaAutoPolicyReconciler{Client: c2, Scheme: testScheme(), Collector: &fakeCollector{err: notConfigured()}}, "shop", "learn")
	mustGet(t, c2, "shop", "learn", got)
	if cond(got.Status.Conditions, ConditionSourceAvailable).Status != metav1.ConditionFalse {
		t.Fatalf("%+v", got.Status)
	}
}
