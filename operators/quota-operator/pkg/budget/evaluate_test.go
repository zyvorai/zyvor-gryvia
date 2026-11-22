// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package budget

import (
	"context"
	"math"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gryviav1 "github.com/zyvorai/gryvia/operators/quota-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/quota-operator/pkg/spend"
)

var now = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

func spec(limit float64, block bool, alerts ...gryviav1.BudgetAlert) gryviav1.GryviaBudgetSpec {
	s := gryviav1.GryviaBudgetSpec{Limits: gryviav1.BudgetLimits{CostUSD: limit}, Alerts: alerts}
	if block {
		s.Enforcement = &gryviav1.BudgetEnforcement{Enabled: true, Action: "block"}
	}
	return s
}

func alert(th float64, actions ...string) gryviav1.BudgetAlert {
	return gryviav1.BudgetAlert{Threshold: th, Actions: actions}
}

func TestState(t *testing.T) {
	cases := []struct {
		name    string
		spec    gryviav1.GryviaBudgetSpec
		percent float64
		want    string
	}{
		{"no alerts, low", spec(100, false), 10, StateActive},
		{"no alerts, default warning at 80", spec(100, false), 80, StateWarning},
		{"custom warn threshold", spec(100, false, alert(50, "notify"), alert(90, "notify")), 55, StateWarning},
		{"below custom threshold stays active (regression: no override)", spec(100, false, alert(70, "notify")), 60, StateActive},
		{"exceeded without blocking", spec(100, false, alert(80, "notify")), 100, StateExceeded},
		{"exceeded over 100", spec(100, false), 250, StateExceeded},
		{"blocking budget blocks at 100", spec(100, true), 100, StateBlocked},
		{"blocking budget warns before", spec(100, true), 85, StateWarning},
		{"block alert lowers block point", spec(100, true, alert(90, "notify", "block")), 92, StateBlocked},
		{"block alert not reached (lowest alert is the warning point)", spec(100, true, alert(90, "block")), 89, StateActive},
		{"block action ignored without enforcement", spec(100, false, alert(90, "block")), 95, StateWarning},
		{"enforcement warn action does not block", gryviav1.GryviaBudgetSpec{Enforcement: &gryviav1.BudgetEnforcement{Enabled: true, Action: "warn"}}, 100, StateExceeded},
		{"enforcement disabled does not block", gryviav1.GryviaBudgetSpec{Enforcement: &gryviav1.BudgetEnforcement{Action: "block"}}, 100, StateExceeded},
	}
	for _, c := range cases {
		if got := State(c.spec, c.percent); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

func total(cost, hours float64, cur ...string) spend.Total {
	return spend.Total{Cost: cost, GPUHours: hours, GPUTypeHours: map[string]float64{"H100": hours}, Currencies: cur, Records: 2}
}

func TestEvaluate(t *testing.T) {
	p, _ := spend.PeriodFor("monthly", "", "", now)
	s := spec(1000, true, alert(50, "notify"), alert(80, "notify"), alert(100, "block"))
	ev := Evaluate(s, total(850, 100, "USD"), p, now, 2, 16)
	if ev.State != StateWarning || math.Abs(ev.Percent-85) > 1e-9 {
		t.Errorf("state/percent = %s/%v", ev.State, ev.Percent)
	}
	if len(ev.Crossed) != 2 || ev.Crossed[0] != 50 || ev.Crossed[1] != 80 {
		t.Errorf("crossed = %v", ev.Crossed)
	}
	if ev.Usage.CurrentConcurrentJobs != 2 || ev.Usage.CurrentConcurrentGPUs != 16 || ev.Usage.CostUSD != 850 {
		t.Errorf("usage = %+v", ev.Usage)
	}
	// 14.5 days elapsed: 850 -> ~1758 projected, not sufficient.
	if ev.Forecast.BudgetSufficient || ev.Forecast.ProjectedCostUSD < 1700 || ev.Forecast.EstimatedExhaustionDate == nil {
		t.Errorf("forecast = %+v", ev.Forecast)
	}
	if ev.BlockPercent != 100 {
		t.Errorf("block percent = %v", ev.BlockPercent)
	}

	ev = Evaluate(s, total(1000, 100, "USD"), p, now, 0, 0)
	if ev.State != StateBlocked {
		t.Errorf("at limit: %s", ev.State)
	}
}

func TestEvaluateGPUHoursLimitDrivesState(t *testing.T) {
	p, _ := spend.PeriodFor("monthly", "", "", now)
	s := gryviav1.GryviaBudgetSpec{Limits: gryviav1.BudgetLimits{GPUHours: 100}}
	ev := Evaluate(s, total(1, 120, "USD"), p, now, 0, 0)
	if ev.State != StateExceeded || ev.Utilization.GPUHoursPercent != 120 {
		t.Errorf("gpu-hour limit: %s %+v", ev.State, ev.Utilization)
	}
}

func TestEvaluateMixedCurrencyNotEnforceable(t *testing.T) {
	p, _ := spend.PeriodFor("monthly", "", "", now)
	ev := Evaluate(spec(100, true), total(5000, 10, "EUR", "USD"), p, now, 0, 0)
	if ev.Enforceable || ev.Reason == "" || ev.State != StateActive || ev.Utilization.CostPercent != 0 {
		t.Errorf("mixed currency must be reported, not enforced: %+v", ev)
	}
}

func TestBlockPercent(t *testing.T) {
	if BlockPercent(spec(100, false, alert(50, "block"))) != 0 {
		t.Error("non-blocking budget has no block percent")
	}
	if BlockPercent(spec(100, true)) != 100 || BlockPercent(spec(100, true, alert(70, "block"), alert(60, "block"))) != 60 {
		t.Error("block percent")
	}
	if BlockPercent(spec(100, true, alert(150, "block"))) != 100 {
		t.Error("block percent is capped at 100")
	}
}

func TestProject(t *testing.T) {
	p, _ := spend.PeriodFor("monthly", "", "", now)
	if got := Project(0, p, now); got != 0 {
		t.Errorf("no spend: %v", got)
	}
	if got := Project(10, p, p.Start); got != 10 {
		t.Errorf("no time elapsed returns spend: %v", got)
	}
	half := p.Start.Add(p.End.Sub(p.Start) / 2)
	if got := Project(10, p, half); math.Abs(got-20) > 1e-6 {
		t.Errorf("half elapsed: %v", got)
	}
}

func TestCalculateBudgetFromUsageRecords(t *testing.T) {
	s := runtime.NewScheme()
	_ = gryviav1.AddToScheme(s)
	start := now.Add(-2 * time.Hour)
	mk := func(ns string, cost float64, final bool) *gryviav1.GryviaUsageRecord {
		return &gryviav1.GryviaUsageRecord{
			ObjectMeta: metav1.ObjectMeta{Name: "usage-" + ns + string(rune('a'+int(cost))), Namespace: ns},
			Spec:       gryviav1.GryviaUsageRecordSpec{Start: metav1.NewTime(start), Cost: cost, GpuHours: 1, Currency: "USD", Final: final},
		}
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(mk("ml", 100, true), mk("ml", 50, false), mk("other", 900, true)).Build()
	q := &gryviav1.GryviaQuota{Spec: gryviav1.GryviaQuotaSpec{Namespaces: []string{"ml"},
		Budget: &gryviav1.BudgetSpec{MonthlyBudget: 300}}}
	st, err := calculateBudgetAt(context.Background(), c, q, now)
	if err != nil {
		t.Fatal(err)
	}
	if st.SpentThisMonth != 150 || st.RemainingBudget != 150 || st.PercentUsed != 50 || st.ProjectedSpend <= 150 {
		t.Errorf("status = %+v", st)
	}
	q.Spec.Budget = nil
	if st, _ := calculateBudgetAt(context.Background(), c, q, now); st.SpentThisMonth != 0 {
		t.Errorf("no budget -> empty status, got %+v", st)
	}
}
