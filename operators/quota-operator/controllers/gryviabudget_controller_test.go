// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controllers

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gryviav1 "github.com/zyvorai/gryvia/operators/quota-operator/api/v1"
)

var budgetNow = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

func budgetRec(name, ns, tenant string, cost, hours float64, cur string, final bool) *gryviav1.GryviaUsageRecord {
	start := budgetNow.Add(-3 * time.Hour)
	r := &gryviav1.GryviaUsageRecord{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{"gryvia.io/tenant": tenant}},
		Spec: gryviav1.GryviaUsageRecordSpec{Tenant: tenant, GpuType: "H100", Start: metav1.NewTime(start),
			Cost: cost, GpuHours: hours, Currency: cur, Final: final},
	}
	if final {
		e := metav1.NewTime(budgetNow.Add(-time.Hour))
		r.Spec.End = &e
	}
	return r
}

func newBudget(name, scopeType, scopeName string, limit float64, block bool, alerts ...gryviav1.BudgetAlert) *gryviav1.GryviaBudget {
	b := &gryviav1.GryviaBudget{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: gryviav1.GryviaBudgetSpec{
			Scope:  gryviav1.BudgetScope{Type: scopeType, Name: scopeName},
			Period: gryviav1.BudgetPeriod{Type: "monthly"},
			Limits: gryviav1.BudgetLimits{CostUSD: limit},
			Alerts: alerts,
		},
	}
	if block {
		b.Spec.Enforcement = &gryviav1.BudgetEnforcement{Enabled: true, Action: "block"}
	}
	return b
}

func budgetReconciler(objs ...client.Object) (*GryviaBudgetReconciler, client.Client) {
	s := newQuotaTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithStatusSubresource(&gryviav1.GryviaBudget{}, &gryviav1.GryviaAIJob{}).Build()
	return &GryviaBudgetReconciler{Client: c, Scheme: s, Now: func() time.Time { return budgetNow }}, c
}

func reconcileBudget(t *testing.T, r *GryviaBudgetReconciler, c client.Client, name string) *gryviav1.GryviaBudget {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != time.Minute {
		t.Errorf("requeue = %v", res.RequeueAfter)
	}
	fb := &gryviav1.GryviaBudget{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, fb); err != nil {
		t.Fatal(err)
	}
	return fb
}

func TestBudgetController_TenantScopeWarningAndAlerts(t *testing.T) {
	b := newBudget("b", "tenant", "a", 1000, false, gryviav1.BudgetAlert{Threshold: 50, Actions: []string{"notify"}})
	r, c := budgetReconciler(b,
		budgetRec("u1", "tenant-a", "a", 400, 50, "USD", true),
		budgetRec("u2", "tenant-a", "a", 200, 25, "USD", false),
		budgetRec("u3", "tenant-b", "b", 9000, 1, "USD", true))
	fb := reconcileBudget(t, r, c, "b")
	if fb.Status.State != "warning" {
		t.Errorf("state = %q (600/1000, alert at 50)", fb.Status.State)
	}
	if fb.Status.Usage.CostUSD != 600 || fb.Status.Usage.GPUHours != 75 {
		t.Errorf("usage = %+v", fb.Status.Usage)
	}
	if len(fb.Status.Alerts) != 1 || fb.Status.Alerts[0].Threshold != 50 {
		t.Errorf("alerts = %+v", fb.Status.Alerts)
	}
	// Reconciling again must not repeat the alert.
	fb = reconcileBudget(t, r, c, "b")
	if len(fb.Status.Alerts) != 1 {
		t.Errorf("alert repeated: %+v", fb.Status.Alerts)
	}
}

func TestBudgetController_BelowThresholdIsActive(t *testing.T) {
	// Regression: the old controller forced "active" below 50% whatever the alerts said.
	b := newBudget("b", "namespace", "ml", 1000, false, gryviav1.BudgetAlert{Threshold: 20, Actions: []string{"notify"}})
	r, c := budgetReconciler(b, budgetRec("u1", "ml", "ml", 300, 30, "USD", true))
	if fb := reconcileBudget(t, r, c, "b"); fb.Status.State != "warning" {
		t.Errorf("30%% with a 20%% alert must warn, got %q", fb.Status.State)
	}
	r, c = budgetReconciler(newBudget("b", "namespace", "ml", 1000, false, gryviav1.BudgetAlert{Threshold: 90}),
		budgetRec("u1", "ml", "ml", 300, 30, "USD", true))
	if fb := reconcileBudget(t, r, c, "b"); fb.Status.State != "active" {
		t.Errorf("state = %q", fb.Status.State)
	}
}

func TestBudgetController_BlockedRejectsHeldJobsOnly(t *testing.T) {
	b := newBudget("b", "namespace", "ml", 100, true)
	pending := typedJob("held", "ml", "Pending")
	running := typedJob("run", "ml", "Running")
	r, c := budgetReconciler(b, budgetRec("u1", "ml", "ml", 150, 30, "USD", true), pending, running)
	fb := reconcileBudget(t, r, c, "b")
	if fb.Status.State != "blocked" {
		t.Fatalf("state = %q", fb.Status.State)
	}
	got := &gryviav1.GryviaAIJob{}
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "ml", Name: "held"}, got)
	if got.Status.Phase != "Rejected" {
		t.Errorf("held job phase = %s", got.Status.Phase)
	}
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "ml", Name: "run"}, got)
	if got.Status.Phase != "Running" {
		t.Errorf("a running job must not be touched, phase = %s", got.Status.Phase)
	}
}

func TestBudgetController_CustomPeriodHonoured(t *testing.T) {
	b := newBudget("b", "namespace", "ml", 100, false)
	b.Spec.Period = gryviav1.BudgetPeriod{Type: "custom", StartDate: "2026-08-01", EndDate: "2026-08-31"}
	old := budgetRec("old", "ml", "ml", 500, 5, "USD", true)
	old.Spec.Start = metav1.NewTime(time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC))
	e := metav1.NewTime(time.Date(2026, 8, 10, 1, 0, 0, 0, time.UTC))
	old.Spec.End = &e
	r, c := budgetReconciler(b, old, budgetRec("cur", "ml", "ml", 5, 1, "USD", true))
	fb := reconcileBudget(t, r, c, "b")
	if fb.Status.Usage.CostUSD != 500 || fb.Status.State != "exceeded" {
		t.Errorf("custom window: %+v state %s", fb.Status.Usage, fb.Status.State)
	}
	if fb.Status.CurrentPeriod.StartDate.Time.Day() != 1 || fb.Status.CurrentPeriod.StartDate.Time.Month() != 8 {
		t.Errorf("period = %+v", fb.Status.CurrentPeriod)
	}
}

func TestBudgetController_MixedCurrencyReportedNotEnforced(t *testing.T) {
	b := newBudget("b", "namespace", "ml", 10, true)
	r, c := budgetReconciler(b, budgetRec("u1", "ml", "ml", 500, 5, "EUR", true), budgetRec("u2", "ml", "ml", 5, 1, "USD", true),
		typedJob("held", "ml", "Pending"))
	fb := reconcileBudget(t, r, c, "b")
	if fb.Status.State != "active" {
		t.Errorf("state = %q", fb.Status.State)
	}
	var enforceable *metav1.Condition
	for i := range fb.Status.Conditions {
		if fb.Status.Conditions[i].Type == "Enforceable" {
			enforceable = &fb.Status.Conditions[i]
		}
	}
	if enforceable == nil || enforceable.Status != metav1.ConditionFalse {
		t.Errorf("Enforceable condition = %+v", enforceable)
	}
	job := &gryviav1.GryviaAIJob{}
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "ml", Name: "held"}, job)
	if job.Status.Phase != "Pending" {
		t.Errorf("mixed currency must not reject: %s", job.Status.Phase)
	}
}

func TestBudgetController_TeamScopeAndUnsupported(t *testing.T) {
	quota := &gryviav1.GryviaQuota{ObjectMeta: metav1.ObjectMeta{Name: "q"},
		Spec: gryviav1.GryviaQuotaSpec{Team: "vision", Namespaces: []string{"v1", "v2"}}}
	b := newBudget("team", "team", "vision", 1000, false)
	u := newBudget("user", "user", "alice", 10, true)
	r, c := budgetReconciler(quota, b, u,
		budgetRec("a", "v1", "x", 100, 1, "USD", true), budgetRec("b", "v2", "y", 100, 1, "USD", true),
		budgetRec("c", "other", "z", 100, 1, "USD", true))
	if fb := reconcileBudget(t, r, c, "team"); fb.Status.Usage.CostUSD != 200 {
		t.Errorf("team scope cost = %v", fb.Status.Usage.CostUSD)
	}
	fb := &gryviav1.GryviaBudget{}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "user"}}); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "user"}, fb)
	if fb.Status.State != "active" || len(fb.Status.Conditions) == 0 || fb.Status.Conditions[0].Reason != "ScopeUnsupported" {
		t.Errorf("unsupported scope: %+v", fb.Status)
	}
}

func TestBudgetController_InvalidPeriodAndNotFound(t *testing.T) {
	b := newBudget("b", "namespace", "ml", 10, false)
	b.Spec.Period = gryviav1.BudgetPeriod{Type: "custom"}
	r, c := budgetReconciler(b)
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "b"}})
	if err != nil {
		t.Fatal(err)
	}
	fb := &gryviav1.GryviaBudget{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "b"}, fb)
	if fb.Status.Conditions[0].Reason != "InvalidPeriod" {
		t.Errorf("conditions = %+v", fb.Status.Conditions)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "nope"}}); err != nil {
		t.Errorf("not found must be ignored: %v", err)
	}
}
