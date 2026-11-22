// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package admission

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// This file is the ai-operator copy of the quota operator's pkg/spend (a separate Go module,
// so it cannot be imported). The rules must stay identical: period boundaries, proration of
// records that straddle a period, the USD-only currency rule and the rate resolution. Both
// copies are tested against the same expectations (spend_test.go here, pkg/spend/spend_test.go
// there).

const (
	budgetCurrency = "USD"
	tenantPrefix   = "tenant-"
	labelTenant    = "gryvia.io/tenant"
)

// period is the half-open interval [start, end) a budget applies to.
type period struct{ start, end time.Time }

func periodFor(typ, startDate, endDate string, now time.Time) (period, error) {
	y, m, d := now.Date()
	loc := now.Location()
	switch strings.ToLower(typ) {
	case "daily":
		s := time.Date(y, m, d, 0, 0, 0, 0, loc)
		return period{s, s.AddDate(0, 0, 1)}, nil
	case "weekly":
		s := time.Date(y, m, d-int(now.Weekday()), 0, 0, 0, 0, loc)
		return period{s, s.AddDate(0, 0, 7)}, nil
	case "", "monthly":
		s := time.Date(y, m, 1, 0, 0, 0, 0, loc)
		return period{s, s.AddDate(0, 1, 0)}, nil
	case "quarterly":
		s := time.Date(y, time.Month((int(m)-1)/3*3+1), 1, 0, 0, 0, 0, loc)
		return period{s, s.AddDate(0, 3, 0)}, nil
	case "annual", "yearly":
		s := time.Date(y, 1, 1, 0, 0, 0, 0, loc)
		return period{s, s.AddDate(1, 0, 0)}, nil
	case "custom":
		s, err := parseDate(startDate, loc, false)
		if err != nil {
			return period{}, fmt.Errorf("custom period startDate: %w", err)
		}
		e, err := parseDate(endDate, loc, true)
		if err != nil {
			return period{}, fmt.Errorf("custom period endDate: %w", err)
		}
		if !e.After(s) {
			return period{}, fmt.Errorf("custom period end is not after start")
		}
		return period{s, e}, nil
	}
	return period{}, fmt.Errorf("unknown period type %q", typ)
}

func parseDate(v string, loc *time.Location, endOfDay bool) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, fmt.Errorf("required for a custom period")
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	t, err := time.ParseInLocation("2006-01-02", v, loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is neither RFC3339 nor YYYY-MM-DD", v)
	}
	if endOfDay {
		t = t.AddDate(0, 0, 1)
	}
	return t, nil
}

// record is the part of a GryviaUsageRecord the gate reads.
type record struct {
	namespace, tenant string
	start             time.Time
	end               *time.Time
	gpuHours, cost    float64
	currency          string
}

func parseRecord(u *unstructured.Unstructured) (record, bool) {
	r := record{namespace: u.GetNamespace(), tenant: u.GetLabels()[labelTenant]}
	if t, _, _ := unstructured.NestedString(u.Object, "spec", "tenant"); t != "" {
		r.tenant = t
	}
	s, _, _ := unstructured.NestedString(u.Object, "spec", "start")
	start, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return r, false
	}
	r.start = start
	if e, _, _ := unstructured.NestedString(u.Object, "spec", "end"); e != "" {
		if t, err := time.Parse(time.RFC3339, e); err == nil {
			r.end = &t
		}
	}
	r.gpuHours = num(u.Object, "spec", "gpuHours")
	r.cost = num(u.Object, "spec", "cost")
	r.currency, _, _ = unstructured.NestedString(u.Object, "spec", "currency")
	return r, true
}

// num reads a number that the API server may have decoded as int64 or float64.
func num(obj map[string]interface{}, fields ...string) float64 {
	v, found, _ := unstructured.NestedFieldNoCopy(obj, fields...)
	if !found {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	case int32:
		return float64(n)
	case int:
		return float64(n)
	}
	return 0
}

// scope selects records: a namespace list and/or a tenant.
type scope struct {
	namespaces []string
	tenant     string
}

func (s scope) covers(namespace string) bool {
	for _, n := range s.namespaces {
		if n == namespace {
			return true
		}
	}
	return s.tenant != "" && namespace == tenantPrefix+s.tenant
}

type total struct {
	cost, gpuHours float64
	currencies     []string
	open           int
}

// comparable reports whether cost can be compared with a USD limit.
func (t total) comparable() (bool, string) {
	switch {
	case len(t.currencies) == 0 || (len(t.currencies) == 1 && t.currencies[0] == budgetCurrency):
		return true, ""
	case len(t.currencies) == 1:
		return false, fmt.Sprintf("usage is metered in %s but budget limits are in %s", t.currencies[0], budgetCurrency)
	default:
		return false, fmt.Sprintf("usage is metered in mixed currencies (%s)", strings.Join(t.currencies, ", "))
	}
}

func sum(recs []record, sc scope, p period, now time.Time) total {
	var t total
	seen := map[string]bool{}
	for _, r := range recs {
		match := false
		for _, ns := range sc.namespaces {
			if r.namespace == ns {
				match = true
			}
		}
		if !match && sc.tenant != "" && r.tenant == sc.tenant {
			match = true
		}
		if !match {
			continue
		}
		end := now
		if r.end != nil {
			end = *r.end
		}
		if !end.After(r.start) {
			end = r.start
		}
		os, oe := r.start, end
		if os.Before(p.start) {
			os = p.start
		}
		if oe.After(p.end) {
			oe = p.end
		}
		if r.start.Equal(end) {
			if r.start.Before(p.start) || !r.start.Before(p.end) {
				continue
			}
		} else if !oe.After(os) {
			continue
		}
		frac := 1.0
		if d := end.Sub(r.start); d > 0 {
			frac = float64(oe.Sub(os)) / float64(d)
		}
		t.cost += r.cost * frac
		t.gpuHours += r.gpuHours * frac
		if r.end == nil {
			t.open++
		}
		cur := strings.ToUpper(r.currency)
		if cur == "" {
			cur = budgetCurrency
		}
		if !seen[cur] {
			seen[cur] = true
			t.currencies = append(t.currencies, cur)
		}
	}
	sort.Strings(t.currencies)
	return t
}

// defaultRates mirrors the quota operator's pkg/pricing table (used only when the cluster has no SKUs).
var defaultRates = map[string]float64{
	"H100": 8.00, "A100-80G": 4.00, "A100-40G": 3.50, "L40": 2.50, "V100": 2.00, "T4": 1.00, "default": 1.00,
}

func defaultRate(gpuType string) float64 {
	for k, v := range defaultRates {
		if k != "default" && strings.EqualFold(k, gpuType) {
			return v
		}
	}
	return defaultRates["default"]
}

type sku struct {
	name, gpuType, currency string
	rate                    float64
	enabled                 bool
}

func parseSku(u *unstructured.Unstructured) sku {
	gt, _, _ := unstructured.NestedString(u.Object, "spec", "gpuType")
	cur, _, _ := unstructured.NestedString(u.Object, "spec", "currency")
	if cur == "" {
		cur = budgetCurrency
	}
	enabled, found, _ := unstructured.NestedBool(u.Object, "spec", "enabled")
	return sku{name: u.GetName(), gpuType: gt, currency: strings.ToUpper(cur), rate: num(u.Object, "spec", "hourlyRate"), enabled: !found || enabled}
}

type rate struct {
	perGPUHour float64
	currency   string
	found      bool
}

// resolveRate prices a GPU type like the usage record producer: the first enabled SKU by name
// (restricted to allowedSkus when set), or the default table when the cluster has no SKUs.
func resolveRate(skus []sku, gpuType string, allowed []string) rate {
	if len(skus) == 0 {
		return rate{perGPUHour: defaultRate(gpuType), currency: budgetCurrency, found: true}
	}
	sorted := append([]sku(nil), skus...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].name < sorted[j].name })
	for _, s := range sorted {
		if !s.enabled || !strings.EqualFold(s.gpuType, gpuType) {
			continue
		}
		if len(allowed) > 0 && !containsFold(allowed, s.name) {
			continue
		}
		return rate{perGPUHour: s.rate, currency: s.currency, found: true}
	}
	return rate{currency: budgetCurrency}
}

func containsFold(list []string, v string) bool {
	for _, x := range list {
		if strings.EqualFold(x, v) {
			return true
		}
	}
	return false
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
