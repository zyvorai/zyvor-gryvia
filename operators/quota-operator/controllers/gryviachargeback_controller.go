// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	gryviav1 "github.com/zyvorai/gryvia/operators/quota-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/quota-operator/pkg/spend"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Chargeback reconciles persisted usage estimates, never recomputes historic prices.
type GryviaChargebackReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Now    func() time.Time
}

func (r *GryviaChargebackReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	cb := &gryviav1.GryviaChargeback{}
	if err := r.Get(ctx, req.NamespacedName, cb); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !cb.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	err := r.calculate(ctx, cb, now)
	status, reason, msg := metav1.ConditionTrue, "Reconciled", "Usage estimates reconciled; this is not a payment invoice"
	if err != nil {
		status, reason, msg = metav1.ConditionFalse, "InvalidConfiguration", err.Error()
		cb.Status.CurrentPeriod = nil
	}
	meta.SetStatusCondition(&cb.Status.Conditions, metav1.Condition{Type: "Ready", Status: status, Reason: reason, Message: msg, ObservedGeneration: cb.Generation})
	if cb.Spec.Reports != nil && cb.Spec.Reports.Enabled {
		meta.SetStatusCondition(&cb.Status.Conditions, metav1.Condition{Type: "ReportsReady", Status: metav1.ConditionFalse, Reason: "Unsupported", Message: "Export currentPeriod as JSON; PDF/email report delivery is not configured", ObservedGeneration: cb.Generation})
	}
	return ctrl.Result{RequeueAfter: time.Minute}, r.Status().Update(ctx, cb)
}
func (r *GryviaChargebackReconciler) calculate(ctx context.Context, cb *gryviav1.GryviaChargeback, now time.Time) error {
	if cb.Spec.AllocationModel.Method != "" && cb.Spec.AllocationModel.Method != "actual-usage" {
		return fmt.Errorf("only actual-usage allocation is supported")
	}
	if cb.Spec.Pricing.Storage != nil || cb.Spec.Pricing.Network != nil {
		return fmt.Errorf("storage/network attribution requires separate usage records")
	}
	if len(cb.Spec.Pricing.GPURates) > 0 {
		return fmt.Errorf("prices are frozen in usage records; configure GPU SKUs before metering")
	}
	loc := time.UTC
	var err error
	if cb.Spec.Period.Timezone != "" {
		loc, err = time.LoadLocation(cb.Spec.Period.Timezone)
		if err != nil {
			return err
		}
	}
	p, err := spend.PeriodFor(cb.Spec.Period.Type, cb.Spec.Period.StartDate, cb.Spec.Period.EndDate, now.In(loc))
	if err != nil {
		return err
	}
	records := &gryviav1.GryviaUsageRecordList{}
	if err = r.List(ctx, records); err != nil {
		return err
	}
	quotas := &gryviav1.GryviaQuotaList{}
	if err = r.List(ctx, quotas); err != nil {
		return err
	}
	nsTeam := map[string]string{}
	for _, q := range quotas.Items {
		for _, ns := range q.Spec.Namespaces {
			if prior := nsTeam[ns]; prior != "" && prior != q.Spec.Team {
				return fmt.Errorf("namespace %s has ambiguous team attribution", ns)
			}
			nsTeam[ns] = q.Spec.Team
		}
	}
	teams := map[string]string{}
	ids := map[string]bool{}
	for _, cc := range cb.Spec.CostCenters {
		if cc.ID == "" || cc.ID == "unassigned" || ids[cc.ID] {
			return fmt.Errorf("cost center IDs must be unique and nonempty; unassigned is reserved")
		}
		ids[cc.ID] = true
		for _, team := range cc.Teams {
			if teams[team] != "" {
				return fmt.Errorf("team %s belongs to multiple cost centers", team)
			}
			teams[team] = cc.ID
		}
	}
	currency := strings.ToUpper(cb.Spec.Pricing.Currency)
	if currency == "" {
		currency = "USD"
	}
	costs := map[string]float64{}
	seen := map[string]bool{}
	for _, rec := range records.Items {
		// A job has one record per admitted run, each with its own start.
		run := rec.Spec.JobUID + "/" + rec.Spec.Start.UTC().Format(time.RFC3339Nano)
		if rec.Spec.JobUID == "" || seen[run] {
			return fmt.Errorf("usage must have a unique jobUID and start")
		}
		seen[run] = true
		if math.IsNaN(rec.Spec.Cost) || math.IsInf(rec.Spec.Cost, 0) || rec.Spec.Cost < 0 {
			return fmt.Errorf("invalid metered cost")
		}
		total := spend.Sum([]gryviav1.GryviaUsageRecord{rec}, spend.Scope{Namespaces: []string{rec.Namespace}}, p, now)
		if total.Records == 0 {
			continue
		}
		if len(total.Currencies) != 1 || total.Currencies[0] != currency {
			return fmt.Errorf("usage currency must match %s; no FX conversion is performed", currency)
		}
		id := teams[nsTeam[rec.Namespace]]
		if id == "" {
			id = "unassigned"
		}
		costs[id] += total.Cost
	}
	out := &gryviav1.ChargebackCurrentPeriod{StartDate: p.Start.Format(time.RFC3339), EndDate: p.End.Format(time.RFC3339), ByResourceType: &gryviav1.ResourceTypeCosts{}}
	overhead := cb.Spec.AllocationModel.PlatformOverhead
	if math.IsNaN(overhead) || math.IsInf(overhead, 0) || overhead < 0 {
		return fmt.Errorf("platform overhead must be finite and nonnegative")
	}
	for _, cc := range cb.Spec.CostCenters {
		cost := costs[cc.ID]
		budget := 0.0
		if cc.Budget != nil {
			switch cb.Spec.Period.Type {
			case "quarterly":
				budget = cc.Budget.Quarterly
			case "annual":
				budget = cc.Budget.Annual
			default:
				budget = cc.Budget.Monthly
			}
		}
		if cb.Spec.AllocationModel.IncludePlatformCosts {
			cost *= 1 + overhead/100
		}
		percent := 0.0
		if budget > 0 {
			percent = cost / budget * 100
		}
		out.CostCenters = append(out.CostCenters, gryviav1.CostCenterStatus{ID: cc.ID, Name: cc.Name, Cost: cost, Budget: budget, Variance: cost - budget, PercentUsed: percent})
	}
	if cost := costs["unassigned"]; cost > 0 {
		if cb.Spec.AllocationModel.IncludePlatformCosts {
			cost *= 1 + overhead/100
		}
		out.CostCenters = append(out.CostCenters, gryviav1.CostCenterStatus{ID: "unassigned", Name: "Unassigned", Cost: cost})
	}
	for _, cost := range costs {
		out.ByResourceType.Compute += cost
	}
	if cb.Spec.AllocationModel.IncludePlatformCosts {
		out.ByResourceType.Platform = out.ByResourceType.Compute * overhead / 100
	}
	out.TotalCost = out.ByResourceType.Compute + out.ByResourceType.Platform
	cb.Status.CurrentPeriod = out
	return nil
}
func (r *GryviaChargebackReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&gryviav1.GryviaChargeback{}).Complete(r)
}
