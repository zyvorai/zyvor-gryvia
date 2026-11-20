// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Package spend computes what a scope (team, tenant, namespace list) has spent in a
// budget period, from the metered GryviaUsageRecords, and what a requested job would add.
//
// It is pure: callers list the records and SKUs and pass them in. The numbers are
// estimates from job wall-clock time (see GryviaUsageRecord), not invoices.
//
// The admission gate in the AI operator (operators/ai-operator/pkg/admission) implements the
// same rules on unstructured objects because the two operators are separate Go modules;
// keep the two in step (period boundaries, proration, currency rule, forecast).
package spend

import (
	"fmt"
	"sort"
	"strings"
	"time"

	gryviav1 "github.com/zyvorai/gryvia/operators/quota-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/quota-operator/pkg/pricing"
)

const (
	// LabelTenant is the label the usage record producer sets on every record.
	LabelTenant = "gryvia.io/tenant"
	// TenantNamespacePrefix is the namespace convention of the GryviaTenant controller.
	TenantNamespacePrefix = "tenant-"
	// DefaultForecastHours is the duration assumed for a job without spec.timeout.
	DefaultForecastHours = 1.0
	// BudgetCurrency is the only currency budget limits are expressed in (costUSD, monthlyBudget).
	BudgetCurrency = "USD"
)

// Scope selects usage records. A record matches when its namespace is listed or, when Tenant
// is set, when its tenant (spec.tenant or the gryvia.io/tenant label) is that tenant.
type Scope struct {
	Namespaces []string
	Tenant     string
}

// Period is the half-open interval [Start, End) a budget applies to.
type Period struct {
	Start, End time.Time
}

// PeriodFor resolves a budget period at time now. Types: daily, weekly (weeks start on
// Sunday, like the previous controller), monthly (default and fallback for an empty type),
// quarterly, annual and custom (startDate and endDate are required, RFC3339 or YYYY-MM-DD;
// the end date is inclusive for the date form).
func PeriodFor(typ, startDate, endDate string, now time.Time) (Period, error) {
	y, m, d := now.Date()
	loc := now.Location()
	switch strings.ToLower(typ) {
	case "daily":
		s := time.Date(y, m, d, 0, 0, 0, 0, loc)
		return Period{s, s.AddDate(0, 0, 1)}, nil
	case "weekly":
		s := time.Date(y, m, d-int(now.Weekday()), 0, 0, 0, 0, loc)
		return Period{s, s.AddDate(0, 0, 7)}, nil
	case "", "monthly":
		s := time.Date(y, m, 1, 0, 0, 0, 0, loc)
		return Period{s, s.AddDate(0, 1, 0)}, nil
	case "quarterly":
		s := time.Date(y, time.Month((int(m)-1)/3*3+1), 1, 0, 0, 0, 0, loc)
		return Period{s, s.AddDate(0, 3, 0)}, nil
	case "annual", "yearly":
		s := time.Date(y, 1, 1, 0, 0, 0, 0, loc)
		return Period{s, s.AddDate(1, 0, 0)}, nil
	case "custom":
		s, _, err := parseDate(startDate, loc, false)
		if err != nil {
			return Period{}, fmt.Errorf("custom period startDate: %w", err)
		}
		e, _, err := parseDate(endDate, loc, true)
		if err != nil {
			return Period{}, fmt.Errorf("custom period endDate: %w", err)
		}
		if !e.After(s) {
			return Period{}, fmt.Errorf("custom period end %s is not after start %s", endDate, startDate)
		}
		return Period{s, e}, nil
	}
	return Period{}, fmt.Errorf("unknown period type %q", typ)
}

func parseDate(v string, loc *time.Location, endOfDay bool) (time.Time, bool, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, false, fmt.Errorf("required for a custom period")
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, true, nil
	}
	t, err := time.ParseInLocation("2006-01-02", v, loc)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("%q is neither RFC3339 nor YYYY-MM-DD", v)
	}
	if endOfDay {
		t = t.AddDate(0, 0, 1) // the end date itself is included
	}
	return t, false, nil
}

// Contains reports whether t lies inside the period.
func (p Period) Contains(t time.Time) bool { return !t.Before(p.Start) && t.Before(p.End) }

// Total is the spend of a scope in a period.
type Total struct {
	Cost         float64
	GPUHours     float64
	GPUTypeHours map[string]float64
	Records      int
	OpenRecords  int
	// Currencies lists the distinct currencies seen (sorted). Records without a currency count as USD.
	Currencies []string
}

// Comparable reports whether Cost can be compared with a USD limit. It is false for mixed
// currencies or any currency other than USD; Reason then says why. No records is comparable.
func (t Total) Comparable() (bool, string) {
	switch {
	case len(t.Currencies) == 0 || (len(t.Currencies) == 1 && t.Currencies[0] == BudgetCurrency):
		return true, ""
	case len(t.Currencies) == 1:
		return false, fmt.Sprintf("usage is metered in %s but budget limits are in %s", t.Currencies[0], BudgetCurrency)
	default:
		return false, fmt.Sprintf("usage is metered in mixed currencies (%s)", strings.Join(t.Currencies, ", "))
	}
}

// Sum adds the records of a scope that overlap the period. Open (non-final) records count
// with the cost they hold now; a record that started before the period is prorated by the
// share of its wall-clock time that falls inside it. now bounds open records.
func Sum(recs []gryviav1.GryviaUsageRecord, sc Scope, p Period, now time.Time) Total {
	t := Total{GPUTypeHours: map[string]float64{}}
	seen := map[string]bool{}
	for i := range recs {
		r := &recs[i]
		if !matches(r, sc) {
			continue
		}
		start := r.Spec.Start.Time
		end := now
		if r.Spec.End != nil && !r.Spec.End.IsZero() {
			end = r.Spec.End.Time
		}
		if !end.After(start) {
			end = start
		}
		// Overlap of [start, end] with [p.Start, p.End).
		os, oe := start, end
		if os.Before(p.Start) {
			os = p.Start
		}
		if oe.After(p.End) {
			oe = p.End
		}
		if start.Equal(end) {
			if !p.Contains(start) {
				continue
			}
		} else if !oe.After(os) {
			continue
		}
		frac := 1.0
		if d := end.Sub(start); d > 0 {
			frac = float64(oe.Sub(os)) / float64(d)
		}
		t.Cost += r.Spec.Cost * frac
		t.GPUHours += r.Spec.GpuHours * frac
		t.GPUTypeHours[r.Spec.GpuType] += r.Spec.GpuHours * frac
		t.Records++
		if !r.Spec.Final {
			t.OpenRecords++
		}
		cur := strings.ToUpper(r.Spec.Currency)
		if cur == "" {
			cur = BudgetCurrency
		}
		if !seen[cur] {
			seen[cur] = true
			t.Currencies = append(t.Currencies, cur)
		}
	}
	sort.Strings(t.Currencies)
	return t
}

func matches(r *gryviav1.GryviaUsageRecord, sc Scope) bool {
	for _, ns := range sc.Namespaces {
		if r.Namespace == ns {
			return true
		}
	}
	if sc.Tenant != "" {
		if r.Spec.Tenant == sc.Tenant || r.Labels[LabelTenant] == sc.Tenant {
			return true
		}
	}
	return false
}

// TenantScope is the scope of one tenant: its label and its namespace tenant-<name>.
func TenantScope(name string) Scope {
	return Scope{Namespaces: []string{TenantNamespacePrefix + name}, Tenant: name}
}

// Rate is the price of one GPU-hour for a GPU type.
type Rate struct {
	Sku        string
	PerGPUHour float64
	Currency   string
	// Found is false when the SKU catalog exists but has no enabled SKU for the type: the
	// usage record producer meters such a job at rate 0 and the quota controller rejects it
	// when the tenant restricts SKUs. A forecast then cannot be priced.
	Found bool
}

// FindSKU returns the enabled SKU (stable by name) whose gpuType matches, honouring
// allowedSkus when non-empty.
func FindSKU(items []gryviav1.GryviaGpuSku, gpuType string, allowedSkus []string) *gryviav1.GryviaGpuSku {
	sorted := make([]*gryviav1.GryviaGpuSku, 0, len(items))
	for i := range items {
		sorted = append(sorted, &items[i])
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for _, s := range sorted {
		if (s.Spec.Enabled != nil && !*s.Spec.Enabled) || !strings.EqualFold(s.Spec.GpuType, gpuType) {
			continue
		}
		if len(allowedSkus) > 0 && !contains(allowedSkus, s.Name) {
			continue
		}
		return s
	}
	return nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// ResolveRate prices a GPU type exactly like the usage record producer: the matching enabled
// SKU, or the shared default table when the cluster has no SKU at all.
func ResolveRate(skus []gryviav1.GryviaGpuSku, gpuType string, allowedSkus []string) Rate {
	if len(skus) == 0 {
		return Rate{PerGPUHour: pricing.DefaultRate(gpuType), Currency: pricing.DefaultCurrency, Found: true}
	}
	if s := FindSKU(skus, gpuType, allowedSkus); s != nil {
		cur := s.Spec.Currency
		if cur == "" {
			cur = pricing.DefaultCurrency
		}
		return Rate{Sku: s.Name, PerGPUHour: s.Spec.HourlyRate, Currency: strings.ToUpper(cur), Found: true}
	}
	return Rate{Currency: pricing.DefaultCurrency}
}

// JobHours is the wall-clock duration a job is assumed to run: its timeout, else def hours.
func JobHours(timeoutSeconds *int64, def float64) float64 {
	if timeoutSeconds != nil && *timeoutSeconds > 0 {
		return float64(*timeoutSeconds) / 3600
	}
	if def <= 0 {
		def = DefaultForecastHours
	}
	return def
}

// Forecast is the cost a job would add: total GPUs (nodes*gpusPerNode) times hours times rate.
func Forecast(totalGPUs int32, hours float64, r Rate) float64 {
	if totalGPUs <= 0 || hours <= 0 {
		return 0
	}
	return float64(totalGPUs) * hours * r.PerGPUHour
}
