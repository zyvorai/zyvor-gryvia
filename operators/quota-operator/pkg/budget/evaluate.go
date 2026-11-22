// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package budget

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	gryviav1 "github.com/zyvorai/gryvia/operators/quota-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/quota-operator/pkg/spend"
)

// Budget states, in increasing severity.
const (
	StateActive   = "active"
	StateWarning  = "warning"
	StateExceeded = "exceeded"
	StateBlocked  = "blocked"
)

// DefaultWarningPercent is the warning threshold of a budget without alerts.
const DefaultWarningPercent = 80.0

// Evaluation is everything the GryviaBudget controller writes to status, computed from one
// spend total. It is pure and covered by tests.
type Evaluation struct {
	Period      spend.Period
	Usage       gryviav1.BudgetUsage
	Utilization gryviav1.BudgetUtilization
	Forecast    gryviav1.BudgetForecast
	// Percent is the governing utilization: the larger of cost and GPU-hours percent.
	Percent float64
	State   string
	// Crossed are the alert thresholds reached (ascending, deduplicated).
	Crossed []float64
	// Enforceable is false when the spend cannot be compared with a USD limit (mixed or
	// non-USD currency); the state then stays active and Reason says why.
	Enforceable bool
	Reason      string
	// BlockPercent is the utilization at which new jobs are refused; 0 when the budget does
	// not block.
	BlockPercent float64
}

// HardBlocking reports whether the budget refuses jobs: enforcement enabled with action block.
func HardBlocking(spec *gryviav1.BudgetEnforcement) bool {
	return spec != nil && spec.Enabled && strings.EqualFold(spec.Action, "block")
}

// BlockPercent is the utilization at which a blocking budget refuses new jobs: the lowest alert
// threshold carrying the action "block", capped at 100, else 100. It is 0 for a budget that
// does not block.
func BlockPercent(spec gryviav1.GryviaBudgetSpec) float64 {
	if !HardBlocking(spec.Enforcement) {
		return 0
	}
	p := 100.0
	for _, a := range spec.Alerts {
		if hasAction(a.Actions, "block") && a.Threshold > 0 && a.Threshold < p {
			p = a.Threshold
		}
	}
	return p
}

func hasAction(actions []string, want string) bool {
	for _, a := range actions {
		if strings.EqualFold(a, want) {
			return true
		}
	}
	return false
}

// WarningPercent is the utilization at which a budget starts to warn: the lowest alert
// threshold, or DefaultWarningPercent when there are no alerts.
func WarningPercent(alerts []gryviav1.BudgetAlert) float64 {
	w := math.Inf(1)
	for _, a := range alerts {
		if a.Threshold > 0 && a.Threshold < w {
			w = a.Threshold
		}
	}
	if math.IsInf(w, 1) {
		return DefaultWarningPercent
	}
	return w
}

// State is the budget state for a utilization percentage:
//
//	blocked   the budget blocks (enforcement enabled, action block) and percent has reached BlockPercent
//	exceeded  percent >= 100 (and the budget does not block, or not yet at its block threshold)
//	warning   percent >= WarningPercent (lowest alert threshold; 80 without alerts)
//	active    otherwise
//
// The state depends only on the percentage and the spec, never on the previous state.
func State(spec gryviav1.GryviaBudgetSpec, percent float64) string {
	if bp := BlockPercent(spec); bp > 0 && percent >= bp {
		return StateBlocked
	}
	if percent >= 100 {
		return StateExceeded
	}
	if percent >= WarningPercent(spec.Alerts) {
		return StateWarning
	}
	return StateActive
}

// Evaluate computes usage, utilization, forecast and state of a budget from a spend total in
// its period. concurrentJobs/concurrentGPUs are the live counts (not part of the spend).
func Evaluate(spec gryviav1.GryviaBudgetSpec, total spend.Total, period spend.Period, now time.Time, concurrentJobs, concurrentGPUs int) Evaluation {
	ev := Evaluation{Period: period, Enforceable: true}
	ev.Usage = gryviav1.BudgetUsage{
		CostUSD:               total.Cost,
		GPUHours:              total.GPUHours,
		GPUTypeHours:          total.GPUTypeHours,
		JobCount:              total.Records,
		CurrentConcurrentJobs: concurrentJobs,
		CurrentConcurrentGPUs: concurrentGPUs,
	}
	if ok, why := total.Comparable(); !ok {
		ev.Enforceable, ev.Reason = false, why
	}

	l := spec.Limits
	if ev.Enforceable && l.CostUSD > 0 {
		ev.Utilization.CostPercent = total.Cost / l.CostUSD * 100
	}
	if l.GPUHours > 0 {
		ev.Utilization.GPUHoursPercent = total.GPUHours / l.GPUHours * 100
	}
	if l.MaxConcurrentJobs > 0 {
		ev.Utilization.JobsPercent = float64(concurrentJobs) / float64(l.MaxConcurrentJobs) * 100
	}
	// Concurrent-job usage limits how many run at once; it does not spend money, so it does
	// not drive the state. Cost and GPU hours do.
	ev.Percent = math.Max(ev.Utilization.CostPercent, ev.Utilization.GPUHoursPercent)
	ev.State = State(spec, ev.Percent)
	ev.BlockPercent = BlockPercent(spec)

	seen := map[float64]bool{}
	for _, a := range spec.Alerts {
		if a.Threshold > 0 && ev.Percent >= a.Threshold && !seen[a.Threshold] {
			seen[a.Threshold] = true
			ev.Crossed = append(ev.Crossed, a.Threshold)
		}
	}
	sort.Float64s(ev.Crossed)

	ev.Forecast = forecast(spec, total, period, now, ev.Enforceable)
	return ev
}

func forecast(spec gryviav1.GryviaBudgetSpec, total spend.Total, p spend.Period, now time.Time, enforceable bool) gryviav1.BudgetForecast {
	f := gryviav1.BudgetForecast{BudgetSufficient: true}
	elapsed := now.Sub(p.Start)
	if elapsed <= 0 || now.After(p.End) {
		return f
	}
	f.ProjectedCostUSD = Project(total.Cost, p, now)
	f.ProjectedGPUHours = Project(total.GPUHours, p, now)
	limit := spec.Limits.CostUSD
	if enforceable && limit > 0 {
		f.BudgetSufficient = f.ProjectedCostUSD <= limit
		if total.Cost > 0 && total.Cost < limit {
			burn := total.Cost / float64(elapsed) // per nanosecond
			t := metav1Time(now.Add(time.Duration((limit - total.Cost) / burn)))
			if t.Time.Before(p.End) {
				f.EstimatedExhaustionDate = &t
			}
		}
	}
	return f
}

// ScopeFor resolves a budget scope to the usage records it covers. team: the namespaces of
// the GryviaQuotas of that team plus records attributed to the tenant of that name;
// namespace: that namespace; tenant: the tenant label and namespace tenant-<name>. Other
// types (user, project) have no usage attribution: ok is false and the budget reports it.
func ScopeFor(ctx context.Context, c client.Client, s gryviav1.BudgetScope) (sc spend.Scope, ok bool, err error) {
	switch strings.ToLower(s.Type) {
	case "namespace":
		return spend.Scope{Namespaces: []string{s.Name}}, true, nil
	case "tenant":
		return spend.TenantScope(s.Name), true, nil
	case "team":
		quotas := &gryviav1.GryviaQuotaList{}
		if err := c.List(ctx, quotas); err != nil {
			return sc, false, fmt.Errorf("list quotas: %w", err)
		}
		sc.Tenant = s.Name
		for _, q := range quotas.Items {
			if q.Spec.Team == s.Name {
				sc.Namespaces = append(sc.Namespaces, q.Spec.Namespaces...)
			}
		}
		return sc, true, nil
	}
	return sc, false, nil
}
