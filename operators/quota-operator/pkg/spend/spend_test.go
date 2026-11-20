// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package spend

import (
	"math"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gryviav1 "github.com/zyvorai/gryvia/operators/quota-operator/api/v1"
)

var now = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func rec(ns, tenant string, start time.Time, end *time.Time, hours, cost float64, cur string, final bool) gryviav1.GryviaUsageRecord {
	r := gryviav1.GryviaUsageRecord{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Labels: map[string]string{LabelTenant: tenant}},
		Spec: gryviav1.GryviaUsageRecordSpec{
			Tenant: tenant, GpuType: "H100", Start: metav1.NewTime(start), GpuHours: hours, Cost: cost, Currency: cur, Final: final,
		},
	}
	if end != nil {
		e := metav1.NewTime(*end)
		r.Spec.End = &e
	}
	return r
}

func TestPeriodFor(t *testing.T) {
	cases := []struct {
		typ, s, e  string
		start, end time.Time
		err        bool
	}{
		{"", "", "", time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), false},
		{"monthly", "", "", time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), false},
		{"daily", "", "", time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC), false},
		{"weekly", "", "", time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC), false},
		{"quarterly", "", "", time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), false},
		{"annual", "", "", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), false},
		{"custom", "2026-08-10", "2026-08-20", time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC), time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC), false},
		{"custom", "2026-08-10T06:00:00Z", "2026-08-20T06:00:00Z", time.Date(2026, 8, 10, 6, 0, 0, 0, time.UTC), time.Date(2026, 8, 20, 6, 0, 0, 0, time.UTC), false},
		{"custom", "", "2026-08-20", time.Time{}, time.Time{}, true},
		{"custom", "2026-08-20", "2026-08-10", time.Time{}, time.Time{}, true},
		{"custom", "yesterday", "2026-08-10", time.Time{}, time.Time{}, true},
		{"fortnightly", "", "", time.Time{}, time.Time{}, true},
	}
	for _, c := range cases {
		p, err := PeriodFor(c.typ, c.s, c.e, now)
		if c.err {
			if err == nil {
				t.Errorf("%s %q..%q: want error", c.typ, c.s, c.e)
			}
			continue
		}
		if err != nil || !p.Start.Equal(c.start) || !p.End.Equal(c.end) {
			t.Errorf("%s: got %v..%v err %v, want %v..%v", c.typ, p.Start, p.End, err, c.start, c.end)
		}
	}
}

func TestSumOpenFinalAndScope(t *testing.T) {
	p, _ := PeriodFor("monthly", "", "", now)
	sep2 := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	sep3 := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	recs := []gryviav1.GryviaUsageRecord{
		rec("tenant-a", "a", sep2, &sep3, 24, 100, "USD", true),
		rec("tenant-a", "a", now.Add(-time.Hour), nil, 4, 30, "USD", false), // open
		rec("tenant-b", "b", sep2, &sep3, 24, 999, "USD", true),             // other tenant
		rec("ml-prod", "ml", sep2, &sep3, 10, 50, "USD", true),              // namespace scope
	}
	got := Sum(recs, TenantScope("a"), p, now)
	if !near(got.Cost, 130) || !near(got.GPUHours, 28) || got.Records != 2 || got.OpenRecords != 1 {
		t.Errorf("tenant a: %+v", got)
	}
	got = Sum(recs, Scope{Namespaces: []string{"ml-prod", "tenant-b"}}, p, now)
	if !near(got.Cost, 1049) {
		t.Errorf("namespace list: %+v", got)
	}
	// Tenant matched by label alone, wherever the record lives.
	got = Sum(recs, Scope{Tenant: "ml"}, p, now)
	if !near(got.Cost, 50) {
		t.Errorf("label match: %+v", got)
	}
}

func TestSumPeriodBoundaryProration(t *testing.T) {
	p, _ := PeriodFor("monthly", "", "", now) // Sep 1 .. Oct 1
	// 10h record, 4h before the month began and 6h inside it: 60% counts.
	start := p.Start.Add(-4 * time.Hour)
	end := p.Start.Add(6 * time.Hour)
	recs := []gryviav1.GryviaUsageRecord{
		rec("n", "t", start, &end, 10, 100, "USD", true),
		rec("n", "t", p.Start.Add(-48*time.Hour), ptr(p.Start.Add(-40*time.Hour)), 8, 80, "USD", true), // wholly last month
		rec("n", "t", p.End.Add(time.Hour), ptr(p.End.Add(2*time.Hour)), 1, 5, "USD", true),            // wholly next month
	}
	got := Sum(recs, Scope{Namespaces: []string{"n"}}, p, now)
	if !near(got.Cost, 60) || !near(got.GPUHours, 6) || got.Records != 1 {
		t.Errorf("proration: %+v", got)
	}
}

func ptr(t time.Time) *time.Time { return &t }

func TestSumCustomPeriod(t *testing.T) {
	p, _ := PeriodFor("custom", "2026-08-10", "2026-08-20", now)
	in := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	inEnd := in.Add(2 * time.Hour)
	out := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	recs := []gryviav1.GryviaUsageRecord{
		rec("n", "t", in, &inEnd, 2, 20, "USD", true),
		rec("n", "t", out, ptr(out.Add(time.Hour)), 1, 10, "USD", true),
	}
	if got := Sum(recs, Scope{Namespaces: []string{"n"}}, p, now); !near(got.Cost, 20) {
		t.Errorf("custom: %+v", got)
	}
}

func TestComparableCurrencies(t *testing.T) {
	p, _ := PeriodFor("monthly", "", "", now)
	s := now.Add(-2 * time.Hour)
	sc := Scope{Namespaces: []string{"n"}}
	one := Sum([]gryviav1.GryviaUsageRecord{rec("n", "t", s, nil, 1, 1, "", false)}, sc, p, now)
	if ok, _ := one.Comparable(); !ok {
		t.Error("empty currency counts as USD")
	}
	if ok, _ := (Total{}).Comparable(); !ok {
		t.Error("no records is comparable")
	}
	eur := Sum([]gryviav1.GryviaUsageRecord{rec("n", "t", s, nil, 1, 1, "eur", false)}, sc, p, now)
	if ok, why := eur.Comparable(); ok || why == "" {
		t.Errorf("EUR must not compare with a USD limit: %v %q", ok, why)
	}
	mixed := Sum([]gryviav1.GryviaUsageRecord{
		rec("n", "t", s, nil, 1, 1, "USD", false), rec("n", "t", s, nil, 1, 1, "EUR", false)}, sc, p, now)
	if ok, why := mixed.Comparable(); ok || why == "" || len(mixed.Currencies) != 2 {
		t.Errorf("mixed: %v %q %v", ok, why, mixed.Currencies)
	}
}

func TestResolveRateAndForecast(t *testing.T) {
	off := false
	skus := []gryviav1.GryviaGpuSku{
		{ObjectMeta: metav1.ObjectMeta{Name: "b-h100"}, Spec: gryviav1.GryviaGpuSkuSpec{GpuType: "H100", HourlyRate: 9, Currency: "usd"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "a-h100"}, Spec: gryviav1.GryviaGpuSkuSpec{GpuType: "H100", HourlyRate: 7}},
		{ObjectMeta: metav1.ObjectMeta{Name: "a-old"}, Spec: gryviav1.GryviaGpuSkuSpec{GpuType: "V100", HourlyRate: 1, Enabled: &off}},
	}
	// Stable by name; empty currency defaults to USD.
	if r := ResolveRate(skus, "h100", nil); r.Sku != "a-h100" || r.PerGPUHour != 7 || r.Currency != "USD" || !r.Found {
		t.Errorf("stable pick: %+v", r)
	}
	if r := ResolveRate(skus, "H100", []string{"b-h100"}); r.Sku != "b-h100" || r.PerGPUHour != 9 {
		t.Errorf("allowedSkus: %+v", r)
	}
	if r := ResolveRate(skus, "V100", nil); r.Found {
		t.Errorf("disabled SKU must not price: %+v", r)
	}
	// No SKUs at all: shared default table.
	if r := ResolveRate(nil, "H100", nil); r.PerGPUHour != 8 || r.Currency != "USD" || !r.Found {
		t.Errorf("defaults: %+v", r)
	}

	if f := Forecast(32, 2, Rate{PerGPUHour: 8}); !near(f, 512) {
		t.Errorf("forecast = %v", f)
	}
	if Forecast(0, 2, Rate{PerGPUHour: 8}) != 0 || Forecast(4, 0, Rate{PerGPUHour: 8}) != 0 {
		t.Error("zero GPUs or hours forecast nothing")
	}
	secs := int64(5400)
	if h := JobHours(&secs, 1); !near(h, 1.5) {
		t.Errorf("timeout hours = %v", h)
	}
	if JobHours(nil, 0) != DefaultForecastHours || JobHours(nil, 3) != 3 {
		t.Error("default hours")
	}
}
