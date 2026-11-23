// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"math"
	"testing"

	gryviav1 "github.com/zyvorai/gryvia/operators/quota-operator/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func idleChargeback(method string, idle, overhead float64, platform bool) *gryviav1.GryviaChargeback {
	return &gryviav1.GryviaChargeback{Spec: gryviav1.GryviaChargebackSpec{
		Period:          gryviav1.ChargebackPeriod{Type: "monthly"},
		AllocationModel: gryviav1.ChargebackAllocationModel{Method: method, IdleCost: idle, IncludePlatformCosts: platform, PlatformOverhead: overhead},
		CostCenters:     []gryviav1.CostCenter{{ID: "a", Teams: []string{"ta"}}, {ID: "b", Teams: []string{"tb"}}},
	}}
}

func idleFixture(t *testing.T) *GryviaChargebackReconciler {
	t.Helper()
	ra := budgetRec("ra", "nsa", "ta", 75, 1, "USD", true)
	ra.Spec.JobUID = "ja"
	rb := budgetRec("rb", "nsb", "tb", 25, 1, "USD", true)
	rb.Spec.JobUID = "jb"
	qa := &gryviav1.GryviaQuota{ObjectMeta: metav1.ObjectMeta{Name: "qa"}, Spec: gryviav1.GryviaQuotaSpec{Team: "ta", Namespaces: []string{"nsa"}}}
	qb := &gryviav1.GryviaQuota{ObjectMeta: metav1.ObjectMeta{Name: "qb"}, Spec: gryviav1.GryviaQuotaSpec{Team: "tb", Namespaces: []string{"nsb"}}}
	c := fake.NewClientBuilder().WithScheme(newQuotaTestScheme()).WithObjects(ra, rb, qa, qb).Build()
	return &GryviaChargebackReconciler{Client: c}
}

func TestChargebackIdleProportional(t *testing.T) {
	r := idleFixture(t)
	cb := idleChargeback("idle-proportional", 40, 0, false)
	if err := r.calculate(context.Background(), cb, budgetNow); err != nil {
		t.Fatal(err)
	}
	p := cb.Status.CurrentPeriod
	if p.CostCenters[0].Cost != 105 || p.CostCenters[0].IdleCost != 30 || p.CostCenters[1].Cost != 35 || p.CostCenters[1].IdleCost != 10 {
		t.Fatalf("centers = %+v", p.CostCenters)
	}
	if p.TotalCost != 140 || p.ByResourceType.Idle != 40 || p.ByResourceType.Compute != 100 || p.IdleUnallocated != 0 {
		t.Fatalf("period = %+v %+v", p, p.ByResourceType)
	}

	// Platform overhead applies to usage plus idle, consistently per center and in the total.
	cb = idleChargeback("idle-proportional", 40, 10, true)
	if err := r.calculate(context.Background(), cb, budgetNow); err != nil {
		t.Fatal(err)
	}
	p = cb.Status.CurrentPeriod
	if math.Abs(p.CostCenters[0].Cost-115.5) > 1e-9 || math.Abs(p.TotalCost-154) > 1e-9 {
		t.Fatalf("overhead: %+v total %v", p.CostCenters, p.TotalCost)
	}
}

func TestChargebackIdleDefaultsAndRefusals(t *testing.T) {
	r := idleFixture(t)
	cb := idleChargeback("", 0, 0, false)
	if err := r.calculate(context.Background(), cb, budgetNow); err != nil || cb.Status.CurrentPeriod.TotalCost != 100 {
		t.Fatalf("actual-usage unchanged: %v %+v", err, cb.Status.CurrentPeriod)
	}
	for name, cb := range map[string]*gryviav1.GryviaChargeback{
		"idle cost without the method": idleChargeback("actual-usage", 5, 0, false),
		"negative idle":                idleChargeback("idle-proportional", -1, 0, false),
		"NaN idle":                     idleChargeback("idle-proportional", math.NaN(), 0, false),
		"Inf idle":                     idleChargeback("idle-proportional", math.Inf(1), 0, false),
		"unsupported method":           idleChargeback("equal-split", 0, 0, false),
	} {
		if err := r.calculate(context.Background(), cb, budgetNow); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestChargebackIdleWithoutUsageIsUnallocated(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newQuotaTestScheme()).Build()
	r := &GryviaChargebackReconciler{Client: c}
	cb := idleChargeback("idle-proportional", 12.5, 0, false)
	if err := r.calculate(context.Background(), cb, budgetNow); err != nil {
		t.Fatal(err)
	}
	p := cb.Status.CurrentPeriod
	if p.IdleUnallocated != 12.5 || p.TotalCost != 0 {
		t.Fatalf("period = %+v", p)
	}
}
