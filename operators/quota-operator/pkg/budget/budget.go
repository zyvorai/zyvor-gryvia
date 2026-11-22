// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package budget

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	gryviav1 "github.com/zyvorai/gryvia/operators/quota-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/quota-operator/pkg/pricing"
	"github.com/zyvorai/gryvia/operators/quota-operator/pkg/spend"
)

// gpuPricingMu protects concurrent access to gpuPricing
var gpuPricingMu sync.RWMutex

// gpuPricing starts as a copy of the shared default table in pkg/pricing and can
// be overridden at runtime with UpdatePricing.
var gpuPricing = func() map[string]float64 {
	m := make(map[string]float64, len(pricing.DefaultRates))
	for k, v := range pricing.DefaultRates {
		m[k] = v
	}
	return m
}()

// CalculateBudget computes GryviaQuota.status.budgetStatus from the metered usage records of
// the quota's namespaces in the current calendar month: spentThisMonth is the sum of
// GryviaUsageRecord.spec.cost (open records included, each priced with its own SKU rate),
// projectedSpend extrapolates the month-to-date burn linearly. When the records are not all
// in USD the spend is still reported but percentUsed stays 0 (a USD limit cannot be compared).
func CalculateBudget(ctx context.Context, k8sClient client.Client, quota *gryviav1.GryviaQuota, currentUsage *gryviav1.QuotaUsage) (*gryviav1.BudgetStatus, error) {
	return calculateBudgetAt(ctx, k8sClient, quota, time.Now())
}

func calculateBudgetAt(ctx context.Context, k8sClient client.Client, quota *gryviav1.GryviaQuota, now time.Time) (*gryviav1.BudgetStatus, error) {
	budgetStatus := &gryviav1.BudgetStatus{}
	if quota.Spec.Budget == nil {
		return budgetStatus, nil
	}
	recs := &gryviav1.GryviaUsageRecordList{}
	if err := k8sClient.List(ctx, recs); err != nil {
		return nil, err
	}
	period, _ := spend.PeriodFor("monthly", "", "", now)
	total := spend.Sum(recs.Items, spend.Scope{Namespaces: quota.Spec.Namespaces}, period, now)

	limit := quota.Spec.Budget.MonthlyBudget
	budgetStatus.SpentThisMonth = total.Cost
	budgetStatus.RemainingBudget = limit - total.Cost
	if ok, _ := total.Comparable(); ok && limit > 0 {
		budgetStatus.PercentUsed = total.Cost / limit * 100
	}
	budgetStatus.ProjectedSpend = Project(total.Cost, period, now)
	return budgetStatus, nil
}

// Project extrapolates spend to the end of the period from the burn so far (linear). Before
// any time has elapsed it returns the spend itself.
func Project(spent float64, p spend.Period, now time.Time) float64 {
	elapsed := now.Sub(p.Start)
	total := p.End.Sub(p.Start)
	if elapsed <= 0 || total <= 0 {
		return spent
	}
	if elapsed > total {
		return spent
	}
	return spent / float64(elapsed) * float64(total)
}

// GetGPURate returns the hourly rate for a GPU type
func GetGPURate(gpuType string) float64 {
	gpuPricingMu.RLock()
	defer gpuPricingMu.RUnlock()

	if rate, exists := gpuPricing[gpuType]; exists {
		return rate
	}
	return gpuPricing["default"]
}

// UpdatePricing allows updating GPU pricing (useful for custom on-prem pricing).
// Returns an error if gpuType is empty or hourlyRate is negative or NaN.
func UpdatePricing(gpuType string, hourlyRate float64) error {
	if gpuType == "" {
		return fmt.Errorf("GPU type must not be empty")
	}
	if hourlyRate < 0 || math.IsNaN(hourlyRate) || math.IsInf(hourlyRate, 0) {
		return fmt.Errorf("hourly rate must be a non-negative finite number, got %v", hourlyRate)
	}
	gpuPricingMu.Lock()
	defer gpuPricingMu.Unlock()
	gpuPricing[gpuType] = hourlyRate
	return nil
}
