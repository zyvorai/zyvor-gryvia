// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"math"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gryviav1 "github.com/zyvorai/gryvia/operators/quota-operator/api/v1"
)

var t0 = time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)

func boolPtr(b bool) *bool { return &b }

func usageJob(ns, name, uid, phase, gpuType string, gpus int32, start, done *time.Time) *gryviav1.GryviaAIJob {
	j := &gryviav1.GryviaAIJob{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID(uid)},
		Spec:       gryviav1.GryviaAIJobSpec{GPUs: gpus, GpuType: gpuType},
		Status:     gryviav1.GryviaAIJobStatus{Phase: phase},
	}
	if start != nil {
		s := metav1.NewTime(*start)
		j.Status.StartTime = &s
	}
	if done != nil {
		d := metav1.NewTime(*done)
		j.Status.CompletionTime = &d
	}
	return j
}

func sku(name, gpuType string, rate float64, enabled *bool) *gryviav1.GryviaGpuSku {
	return &gryviav1.GryviaGpuSku{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       gryviav1.GryviaGpuSkuSpec{GpuType: gpuType, HourlyRate: rate, Currency: "EUR", Enabled: enabled},
	}
}

func tenantObj(name string, skus ...string) *gryviav1.GryviaTenant {
	return &gryviav1.GryviaTenant{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       gryviav1.GryviaTenantSpec{AllowedSkus: skus},
	}
}

func newUsageReconciler(now *time.Time, objs ...client.Object) (*GryviaUsageRecordReconciler, client.Client) {
	scheme := newQuotaTestScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&gryviav1.GryviaAIJob{}).Build()
	return &GryviaUsageRecordReconciler{Client: c, Scheme: scheme, Now: func() time.Time { return *now }}, c
}

func reconcileJob(t *testing.T, r *GryviaUsageRecordReconciler, ns, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return res
}

func getRecord(t *testing.T, c client.Client, ns, uid string) *gryviav1.GryviaUsageRecord {
	t.Helper()
	rec := &gryviav1.GryviaUsageRecord{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "usage-" + uid}, rec); err != nil {
		t.Fatalf("get record: %v", err)
	}
	return rec
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestUsage_RecordCreatedWhileRunning(t *testing.T) {
	now := t0.Add(90 * time.Minute)
	start := t0
	r, c := newUsageReconciler(&now, usageJob("ns1", "train", "u1", "Running", "H100", 4, &start, nil))

	res := reconcileJob(t, r, "ns1", "train")
	if res.RequeueAfter != time.Minute {
		t.Errorf("RequeueAfter = %v, want 1m", res.RequeueAfter)
	}
	rec := getRecord(t, c, "ns1", "u1")
	s := rec.Spec
	if s.Final || s.End != nil {
		t.Errorf("running record must be open: final=%v end=%v", s.Final, s.End)
	}
	if !near(s.GpuHours, 6) || !near(s.Rate, 8) || !near(s.Cost, 48) {
		t.Errorf("hours/rate/cost = %v/%v/%v, want 6/8/48", s.GpuHours, s.Rate, s.Cost)
	}
	if s.Job != "train" || s.JobUID != "u1" || s.Gpus != 4 || s.GpuType != "H100" || s.Currency != "USD" || s.Sku != "" {
		t.Errorf("unexpected spec: %+v", s)
	}
	if rec.Labels["gryvia.io/tenant"] != "ns1" || rec.Labels["gryvia.io/job"] != "train" {
		t.Errorf("labels = %v", rec.Labels)
	}
}

func TestUsage_GrowsWhileRunning(t *testing.T) {
	now := t0.Add(time.Hour)
	start := t0
	r, c := newUsageReconciler(&now, usageJob("ns1", "j", "u1", "Running", "T4", 2, &start, nil))
	reconcileJob(t, r, "ns1", "j")
	if h := getRecord(t, c, "ns1", "u1").Spec.GpuHours; !near(h, 2) {
		t.Fatalf("first = %v, want 2", h)
	}
	now = t0.Add(3 * time.Hour)
	reconcileJob(t, r, "ns1", "j")
	if h := getRecord(t, c, "ns1", "u1").Spec.GpuHours; !near(h, 6) {
		t.Errorf("after growth = %v, want 6", h)
	}
}

func TestUsage_FinalizeAndImmutable(t *testing.T) {
	now := t0.Add(time.Hour)
	start, done := t0, t0.Add(2*time.Hour)
	job := usageJob("ns1", "j", "u1", "Running", "A100-80G", 2, &start, nil)
	r, c := newUsageReconciler(&now, job)
	reconcileJob(t, r, "ns1", "j")

	// job succeeds later; the record ends at completionTime, not at reconcile time
	now = t0.Add(5 * time.Hour)
	cur := &gryviav1.GryviaAIJob{}
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "ns1", Name: "j"}, cur)
	d := metav1.NewTime(done)
	cur.Status.Phase, cur.Status.CompletionTime = "Succeeded", &d
	if err := c.Status().Update(context.Background(), cur); err != nil {
		t.Fatal(err)
	}
	res := reconcileJob(t, r, "ns1", "j")
	if res.RequeueAfter != 0 || res.Requeue {
		t.Errorf("final reconcile must not requeue: %+v", res)
	}
	rec := getRecord(t, c, "ns1", "u1")
	if !rec.Spec.Final || rec.Spec.End == nil || !rec.Spec.End.Time.Equal(done) {
		t.Fatalf("not finalized: %+v", rec.Spec)
	}
	if !near(rec.Spec.GpuHours, 4) || !near(rec.Spec.Cost, 16) {
		t.Errorf("hours/cost = %v/%v, want 4/16", rec.Spec.GpuHours, rec.Spec.Cost)
	}
	rv := rec.ResourceVersion

	// Idempotent: later reconciles, even with a changed price sheet or job, leave it alone.
	now = t0.Add(50 * time.Hour)
	_ = c.Create(context.Background(), sku("h", "A100-80G", 99, nil))
	cur.Spec.GPUs = 8
	_ = c.Update(context.Background(), cur)
	reconcileJob(t, r, "ns1", "j")
	if after := getRecord(t, c, "ns1", "u1"); after.ResourceVersion != rv || after.Spec.Cost != rec.Spec.Cost {
		t.Errorf("final record was modified: %+v", after.Spec)
	}
}

func TestUsage_RestartSafeAndTerminalWithoutRecord(t *testing.T) {
	// A fresh reconciler (operator restart) finds the job already finished and no record: it
	// creates the final record straight away, exactly once.
	now := t0.Add(24 * time.Hour)
	start, done := t0, t0.Add(30*time.Minute)
	r, c := newUsageReconciler(&now, usageJob("ns1", "j", "u9", "Failed", "V100", 2, &start, &done))
	reconcileJob(t, r, "ns1", "j")
	reconcileJob(t, r, "ns1", "j")
	list := &gryviav1.GryviaUsageRecordList{}
	_ = c.List(context.Background(), list)
	if len(list.Items) != 1 {
		t.Fatalf("records = %d, want 1", len(list.Items))
	}
	s := list.Items[0].Spec
	if !s.Final || !near(s.GpuHours, 1) || !near(s.Cost, 2) {
		t.Errorf("spec = %+v", s)
	}
}

func TestUsage_NoStartNoRecord(t *testing.T) {
	now := t0
	r, c := newUsageReconciler(&now, usageJob("ns1", "j", "u1", "Pending", "T4", 1, nil, nil))
	reconcileJob(t, r, "ns1", "j")
	list := &gryviav1.GryviaUsageRecordList{}
	_ = c.List(context.Background(), list)
	if len(list.Items) != 0 {
		t.Errorf("records = %d, want 0", len(list.Items))
	}
}

func TestUsage_DeletedRunningJobIsClosed(t *testing.T) {
	now := t0.Add(time.Hour)
	start := t0
	job := usageJob("ns1", "j", "u1", "Running", "T4", 2, &start, nil)
	r, c := newUsageReconciler(&now, job)
	reconcileJob(t, r, "ns1", "j")
	_ = c.Delete(context.Background(), job)
	now = t0.Add(2 * time.Hour)
	reconcileJob(t, r, "ns1", "j")
	s := getRecord(t, c, "ns1", "u1").Spec
	if !s.Final || !near(s.GpuHours, 4) {
		t.Errorf("orphan not closed: %+v", s)
	}
}

func TestUsage_RateResolution(t *testing.T) {
	tests := []struct {
		name     string
		ns       string
		gpuType  string
		objs     []client.Object
		wantSku  string
		wantRate float64
		wantCur  string
	}{
		{"no SKUs -> default table H100", "ns1", "H100", nil, "", 8, "USD"},
		{"no SKUs -> default A100-40G", "ns1", "A100-40G", nil, "", 3.5, "USD"},
		{"no SKUs -> unknown type default", "ns1", "B200", nil, "", 1, "USD"},
		{"SKU match", "ns1", "H100", []client.Object{sku("h100-eu", "H100", 5.5, nil)}, "h100-eu", 5.5, "EUR"},
		{"SKU wins over default table", "ns1", "T4", []client.Object{sku("t4", "T4", 0.2, boolPtr(true))}, "t4", 0.2, "EUR"},
		{"disabled SKU ignored, others exist -> free", "ns1", "H100", []client.Object{sku("h", "H100", 5, boolPtr(false)), sku("t", "T4", 1, nil)}, "", 0, "USD"},
		{"unpriced type when SKUs exist -> 0", "ns1", "L40", []client.Object{sku("h", "H100", 5, nil)}, "", 0, "USD"},
		{"tenant allowedSkus picks the allowed one",
			"tenant-acme", "H100",
			[]client.Object{sku("a-h100", "H100", 9, nil), sku("b-h100", "H100", 4, nil), tenantObj("acme", "b-h100")},
			"b-h100", 4, "EUR"},
		{"tenant allowedSkus excludes type -> 0",
			"tenant-acme", "H100",
			[]client.Object{sku("h", "H100", 9, nil), sku("t", "T4", 1, nil), tenantObj("acme", "t")},
			"", 0, "USD"},
		{"tenant without allowedSkus -> any enabled SKU",
			"tenant-acme", "H100", []client.Object{sku("h", "H100", 9, nil), tenantObj("acme")}, "h", 9, "EUR"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			now := t0.Add(time.Hour)
			start := t0
			objs := append([]client.Object{usageJob(tc.ns, "j", "u1", "Running", tc.gpuType, 1, &start, nil)}, tc.objs...)
			r, c := newUsageReconciler(&now, objs...)
			reconcileJob(t, r, tc.ns, "j")
			s := getRecord(t, c, tc.ns, "u1").Spec
			if s.Sku != tc.wantSku || !near(s.Rate, tc.wantRate) || s.Currency != tc.wantCur || !near(s.Cost, tc.wantRate) {
				t.Errorf("sku/rate/cur/cost = %q/%v/%q/%v, want %q/%v/%q", s.Sku, s.Rate, s.Currency, s.Cost, tc.wantSku, tc.wantRate, tc.wantCur)
			}
		})
	}
}

func TestUsage_TenantResolution(t *testing.T) {
	teamNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ml-prod", Labels: map[string]string{"gryvia.io/team": "ml-team"}}}
	labelled := usageJob("other", "j", "u1", "Running", "T4", 1, ptr(t0), nil)
	labelled.Labels = map[string]string{"gryvia.io/team": "job-team"}
	tests := []struct {
		name string
		job  *gryviav1.GryviaAIJob
		objs []client.Object
		want string
	}{
		{"tenant namespace", usageJob("tenant-acme", "j", "u1", "Running", "T4", 1, ptr(t0), nil), []client.Object{tenantObj("acme")}, "acme"},
		{"tenant- namespace without Tenant object -> namespace", usageJob("tenant-ghost", "j", "u1", "Running", "T4", 1, ptr(t0), nil), nil, "tenant-ghost"},
		{"job team label", labelled, nil, "job-team"},
		{"namespace team label", usageJob("ml-prod", "j", "u1", "Running", "T4", 1, ptr(t0), nil), []client.Object{teamNS}, "ml-team"},
		{"plain namespace", usageJob("plain", "j", "u1", "Running", "T4", 1, ptr(t0), nil), nil, "plain"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			now := t0.Add(time.Hour)
			r, c := newUsageReconciler(&now, append([]client.Object{tc.job}, tc.objs...)...)
			reconcileJob(t, r, tc.job.Namespace, "j")
			rec := getRecord(t, c, tc.job.Namespace, "u1")
			if rec.Spec.Tenant != tc.want || rec.Labels["gryvia.io/tenant"] != tc.want {
				t.Errorf("tenant = %q (label %q), want %q", rec.Spec.Tenant, rec.Labels["gryvia.io/tenant"], tc.want)
			}
		})
	}
}

func ptr(t time.Time) *time.Time { return &t }

func TestQuota_EnforceQuota_RejectsDisallowedTypeAndSku(t *testing.T) {
	quota := func(types ...string) *gryviav1.GryviaQuota {
		return &gryviav1.GryviaQuota{
			ObjectMeta: metav1.ObjectMeta{Name: "q"},
			Spec: gryviav1.GryviaQuotaSpec{
				Team: "acme", Namespaces: []string{"tenant-acme"},
				GPUQuota: gryviav1.GPUQuotaSpec{MaxGPUs: 100, AllowedGPUTypes: types},
			},
			Status: gryviav1.GryviaQuotaStatus{Phase: "Active"},
		}
	}
	tests := []struct {
		name     string
		q        *gryviav1.GryviaQuota
		gpuType  string
		phase    string
		objs     []client.Object
		wantRej  bool
		wantCode string
	}{
		{"allowed type stays", quota("H100"), "H100", "Pending", nil, false, ""},
		{"disallowed type rejected even when quota Active", quota("H100"), "T4", "Pending", nil, true, "GpuTypeNotAllowed"},
		{"queued also rejected", quota("H100"), "T4", "Queued", nil, true, "GpuTypeNotAllowed"},
		{"running job untouched", quota("H100"), "T4", "Running", nil, false, ""},
		{"no allowed list -> untouched", quota(), "T4", "Pending", nil, false, ""},
		{"allowedSkus set but no enabled SKU for type", quota(), "T4", "Pending",
			[]client.Object{tenantObj("acme", "h"), sku("h", "H100", 1, nil), sku("t", "T4", 1, boolPtr(false))}, true, "NoEnabledSku"},
		{"allowedSkus set and SKU enabled", quota(), "H100", "Pending",
			[]client.Object{tenantObj("acme", "h"), sku("h", "H100", 1, nil)}, false, ""},
		{"empty allowedSkus = all", quota(), "T4", "Pending", []client.Object{tenantObj("acme")}, false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			job := usageJob("tenant-acme", "j", "u1", tc.phase, tc.gpuType, 1, nil, nil)
			r, c := newQuotaReconciler(append([]client.Object{tc.q, job}, tc.objs...)...)
			if err := r.enforceQuota(context.Background(), tc.q); err != nil {
				t.Fatal(err)
			}
			got := &gryviav1.GryviaAIJob{}
			_ = c.Get(context.Background(), types.NamespacedName{Namespace: "tenant-acme", Name: "j"}, got)
			rejected := got.Status.Phase == "Rejected"
			if rejected != tc.wantRej {
				t.Fatalf("phase = %q, wantRej=%v", got.Status.Phase, tc.wantRej)
			}
			if rejected && (len(got.Status.Conditions) == 0 || got.Status.Conditions[0].Reason != tc.wantCode || got.Status.Conditions[0].Message == "") {
				t.Errorf("conditions = %+v, want reason %s with message", got.Status.Conditions, tc.wantCode)
			}
		})
	}
}

// A distributed job holds nodes*gpusPerNode GPUs; spec.gpus alone would undercount it.
func TestUsage_DistributedJobCountsNodesTimesGpusPerNode(t *testing.T) {
	now := t0.Add(2 * time.Hour)
	start := t0
	j := usageJob("ns1", "dist", "ud", "Running", "H100", 8, &start, nil)
	j.Spec.Distributed = &gryviav1.GryviaAIJobDistributedSpec{Enabled: true, Nodes: 4, GpusPerNode: 8}
	r, c := newUsageReconciler(&now, j)
	reconcileJob(t, r, "ns1", "dist")
	s := getRecord(t, c, "ns1", "ud").Spec
	if s.Gpus != 32 || !near(s.GpuHours, 64) || !near(s.Cost, 64*8) {
		t.Errorf("gpus/hours/cost = %d/%v/%v, want 32/64/512", s.Gpus, s.GpuHours, s.Cost)
	}
}

func setKueue(t *testing.T, c client.Client, ns, name, phase string, status metav1.ConditionStatus, at time.Time) {
	t.Helper()
	j := &gryviav1.GryviaAIJob{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, j); err != nil {
		t.Fatal(err)
	}
	j.Status.Phase = phase
	j.Status.Conditions = []metav1.Condition{{Type: "KueueAdmitted", Status: status, Reason: "x", LastTransitionTime: metav1.NewTime(at)}}
	if err := c.Status().Update(context.Background(), j); err != nil {
		t.Fatal(err)
	}
}

func getNamed(t *testing.T, c client.Client, ns, name string) *gryviav1.GryviaUsageRecord {
	t.Helper()
	rec := &gryviav1.GryviaUsageRecord{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, rec); err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	return rec
}

// Kueue evicts a running job and admits it again: each run is metered on its own record and the
// time spent queued in between is not.
func TestUsage_KueueRequeueMetersEachRun(t *testing.T) {
	now := t0.Add(30 * time.Minute)
	start := t0
	r, c := newUsageReconciler(&now, usageJob("ns1", "kj", "uk", "Running", "T4", 2, &start, nil))
	setKueue(t, c, "ns1", "kj", "Running", metav1.ConditionTrue, t0.Add(-time.Minute))
	reconcileJob(t, r, "ns1", "kj")

	// Evicted at 1h, seen at 1h30: the first run ends at the eviction.
	now = t0.Add(90 * time.Minute)
	setKueue(t, c, "ns1", "kj", "Queued", metav1.ConditionFalse, t0.Add(time.Hour))
	if res := reconcileJob(t, r, "ns1", "kj"); res.RequeueAfter != 0 {
		t.Errorf("queued job must not requeue: %+v", res)
	}
	s := getRecord(t, c, "ns1", "uk").Spec
	if !s.Final || s.End == nil || !s.End.Time.Equal(t0.Add(time.Hour)) || !near(s.GpuHours, 2) {
		t.Fatalf("first run not closed at the eviction: %+v", s)
	}

	// Still queued later: nothing is metered.
	now = t0.Add(3 * time.Hour)
	reconcileJob(t, r, "ns1", "kj")
	list := &gryviav1.GryviaUsageRecordList{}
	_ = c.List(context.Background(), list)
	if len(list.Items) != 1 {
		t.Fatalf("records while queued = %d, want 1", len(list.Items))
	}

	// Admitted again at 4h, running at 5h: a second record from the readmission.
	now = t0.Add(5 * time.Hour)
	setKueue(t, c, "ns1", "kj", "Running", metav1.ConditionTrue, t0.Add(4*time.Hour))
	if res := reconcileJob(t, r, "ns1", "kj"); res.RequeueAfter != time.Minute {
		t.Errorf("running run must requeue: %+v", res)
	}
	s2 := getNamed(t, c, "ns1", "usage-uk-2").Spec
	if s2.Final || !s2.Start.Time.Equal(t0.Add(4*time.Hour)) || !near(s2.GpuHours, 2) || s2.JobUID != "uk" {
		t.Fatalf("second run = %+v", s2)
	}

	// Succeeds at 6h: the second run is finalized, the first left alone.
	now = t0.Add(7 * time.Hour)
	j := &gryviav1.GryviaAIJob{}
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "ns1", Name: "kj"}, j)
	done := metav1.NewTime(t0.Add(6 * time.Hour))
	j.Status.Phase, j.Status.CompletionTime = "Succeeded", &done
	_ = c.Status().Update(context.Background(), j)
	reconcileJob(t, r, "ns1", "kj")
	s2 = getNamed(t, c, "ns1", "usage-uk-2").Spec
	if !s2.Final || !near(s2.GpuHours, 4) {
		t.Errorf("second run not finalized: %+v", s2)
	}
	if s := getRecord(t, c, "ns1", "uk").Spec; !near(s.GpuHours, 2) {
		t.Errorf("first run changed: %+v", s)
	}
	reconcileJob(t, r, "ns1", "kj")
	_ = c.List(context.Background(), list)
	if len(list.Items) != 2 {
		t.Errorf("records = %d, want 2", len(list.Items))
	}
}

// A job cancelled while requeued gets no record for a run that never happened.
func TestUsage_KueueCancelledWhileQueuedOpensNoRun(t *testing.T) {
	now := t0.Add(time.Hour)
	start := t0
	r, c := newUsageReconciler(&now, usageJob("ns1", "kj", "uk", "Running", "T4", 1, &start, nil))
	setKueue(t, c, "ns1", "kj", "Running", metav1.ConditionTrue, t0)
	reconcileJob(t, r, "ns1", "kj")
	setKueue(t, c, "ns1", "kj", "Queued", metav1.ConditionFalse, t0.Add(time.Hour))
	reconcileJob(t, r, "ns1", "kj")
	now = t0.Add(2 * time.Hour)
	setKueue(t, c, "ns1", "kj", "Cancelled", metav1.ConditionFalse, t0.Add(time.Hour))
	reconcileJob(t, r, "ns1", "kj")
	list := &gryviav1.GryviaUsageRecordList{}
	_ = c.List(context.Background(), list)
	if len(list.Items) != 1 || !near(list.Items[0].Spec.GpuHours, 1) {
		t.Fatalf("records = %+v", list.Items)
	}
}

// The chargeback sums both runs of a requeued job instead of rejecting them as duplicates.
func TestChargeback_RunsOfOneJobAreNotDuplicates(t *testing.T) {
	a := budgetRec("usage-j1", "ns", "tenant", 10, 1, "USD", true)
	a.Spec.JobUID = "j1"
	b := a.DeepCopy()
	b.Name, b.Spec.Start = "usage-j1-2", metav1.NewTime(a.Spec.Start.Add(time.Minute))
	quota := &gryviav1.GryviaQuota{ObjectMeta: metav1.ObjectMeta{Name: "q"}, Spec: gryviav1.GryviaQuotaSpec{Team: "team", Namespaces: []string{"ns"}}}
	cb := &gryviav1.GryviaChargeback{Spec: gryviav1.GryviaChargebackSpec{Period: gryviav1.ChargebackPeriod{Type: "monthly"}, CostCenters: []gryviav1.CostCenter{{ID: "cc", Teams: []string{"team"}}}}}
	c := fake.NewClientBuilder().WithScheme(newQuotaTestScheme()).WithObjects(a, b, quota).Build()
	r := &GryviaChargebackReconciler{Client: c}
	if err := r.calculate(context.Background(), cb, budgetNow); err != nil {
		t.Fatal(err)
	}
	if cb.Status.CurrentPeriod.TotalCost != 20 {
		t.Errorf("total = %v, want 20", cb.Status.CurrentPeriod.TotalCost)
	}
}

func TestTotalGPUs(t *testing.T) {
	d := func(n, per int32) *gryviav1.GryviaAIJobDistributedSpec {
		return &gryviav1.GryviaAIJobDistributedSpec{Enabled: true, Nodes: n, GpusPerNode: per}
	}
	cases := []struct {
		name string
		spec gryviav1.GryviaAIJobSpec
		want int32
	}{
		{"single", gryviav1.GryviaAIJobSpec{GPUs: 4}, 4},
		{"cpu only", gryviav1.GryviaAIJobSpec{}, 0},
		{"distributed", gryviav1.GryviaAIJobSpec{GPUs: 8, Distributed: d(3, 8)}, 24},
		{"distributed default per node", gryviav1.GryviaAIJobSpec{GPUs: 2, Distributed: d(3, 0)}, 3},
		{"distributed cpu only", gryviav1.GryviaAIJobSpec{Distributed: d(3, 0)}, 0},
		{"distributed nodes default 1", gryviav1.GryviaAIJobSpec{GPUs: 8, Distributed: d(0, 8)}, 8},
		{"disabled distributed ignored", gryviav1.GryviaAIJobSpec{GPUs: 4, Distributed: &gryviav1.GryviaAIJobDistributedSpec{Nodes: 9, GpusPerNode: 9}}, 4},
	}
	for _, c := range cases {
		if got := c.spec.TotalGPUs(); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
}
