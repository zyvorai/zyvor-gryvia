// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package metrics

import (
	"fmt"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gryviav1 "github.com/zyvorai/gryvia/operators/quota-operator/api/v1"
)

func newReader(t *testing.T, objs ...client.Object) client.Reader {
	t.Helper()
	s := runtime.NewScheme()
	if err := gryviav1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

func quota(name, team string, max, alloc, run, queued int, budget *gryviav1.BudgetStatus) *gryviav1.GryviaQuota {
	return &gryviav1.GryviaQuota{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       gryviav1.GryviaQuotaSpec{Team: team, GPUQuota: gryviav1.GPUQuotaSpec{MaxGPUs: max}},
		Status: gryviav1.GryviaQuotaStatus{
			CurrentUsage: gryviav1.QuotaUsage{AllocatedGPUs: alloc, RunningJobs: run, QueuedJobs: queued},
			BudgetStatus: budget,
		},
	}
}

func record(name, tenant, sku, cur string, hours, cost float64, final bool) *gryviav1.GryviaUsageRecord {
	return &gryviav1.GryviaUsageRecord{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec: gryviav1.GryviaUsageRecordSpec{Tenant: tenant, Job: "job-" + name, Sku: sku, Currency: cur,
			GpuHours: hours, Cost: cost, Final: final},
	}
}

func TestQuotaAndBudgetMetrics(t *testing.T) {
	r := newReader(t,
		quota("a", "team-a", 8, 6, 2, 1, &gryviav1.BudgetStatus{PercentUsed: 92.5, SpentThisMonth: 925}),
		quota("b", "", 4, 0, 0, 0, nil), // no team: falls back to the object name; no budget series
	)
	c := NewCollector(r)
	want := `
# HELP gryvia_quota_gpus_allocated GPUs currently allocated to the team (GryviaQuota status.currentUsage).
# TYPE gryvia_quota_gpus_allocated gauge
gryvia_quota_gpus_allocated{team="b"} 0
gryvia_quota_gpus_allocated{team="team-a"} 6
# HELP gryvia_quota_budget_percent_used Percent (0-100) of the team monthly budget used; only for quotas with a budget status.
# TYPE gryvia_quota_budget_percent_used gauge
gryvia_quota_budget_percent_used{team="team-a"} 92.5
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want),
		"gryvia_quota_gpus_allocated", "gryvia_quota_budget_percent_used"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"gryvia_quota_gpus_max", "gryvia_quota_running_jobs", "gryvia_quota_queued_jobs", "gryvia_quota_budget_spent"} {
		if n := testutil.CollectAndCount(c, name); n == 0 {
			t.Errorf("%s missing", name)
		}
	}
	if n := testutil.CollectAndCount(c, "gryvia_quota_budget_spent"); n != 1 {
		t.Errorf("budget series = %d, want 1", n)
	}
}

func TestUsageAggregationNeverLabelsJobs(t *testing.T) {
	r := newReader(t,
		record("1", "t1", "h100", "USD", 2, 10, true),
		record("2", "t1", "h100", "USD", 3, 15, false),
		record("3", "t1", "", "USD", 1, 1, true),
		record("4", "t2", "h100", "EUR", 4, 8, true),
		&gryviav1.GryviaTenant{ObjectMeta: metav1.ObjectMeta{Name: "t1"}},
		&gryviav1.GryviaTenant{ObjectMeta: metav1.ObjectMeta{Name: "t2"}},
	)
	c := NewCollector(r)
	want := `
# HELP gryvia_usage_gpu_hours_total GPU-hours summed over the GryviaUsageRecords that currently exist. Falls if records are deleted, so rate() may show a reset.
# TYPE gryvia_usage_gpu_hours_total counter
gryvia_usage_gpu_hours_total{currency="EUR",sku="h100",tenant="t2"} 4
gryvia_usage_gpu_hours_total{currency="USD",sku="default",tenant="t1"} 1
gryvia_usage_gpu_hours_total{currency="USD",sku="h100",tenant="t1"} 5
# HELP gryvia_usage_records_open GryviaUsageRecords not yet final (jobs still running).
# TYPE gryvia_usage_records_open gauge
gryvia_usage_records_open 1
# HELP gryvia_tenants Number of GryviaTenant objects.
# TYPE gryvia_tenants gauge
gryvia_tenants 2
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want),
		"gryvia_usage_gpu_hours_total", "gryvia_usage_records_open", "gryvia_tenants"); err != nil {
		t.Fatal(err)
	}
	mfs, err := gather(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if strings.HasPrefix(l.GetValue(), "job-") || l.GetName() == "job" || l.GetName() == "namespace" {
					t.Errorf("unbounded label leaked: %s=%s", l.GetName(), l.GetValue())
				}
			}
		}
	}
}

func TestUsageCardinalityIsCapped(t *testing.T) {
	var objs []client.Object
	for i := 0; i < maxUsageSeries+50; i++ {
		objs = append(objs, record(fmt.Sprint(i), fmt.Sprintf("tenant-%04d", i), "sku", "USD", 1, 2, true))
	}
	c := NewCollector(newReader(t, objs...))
	// two series (hours, cost) per key, so the cap bounds each metric to maxUsageSeries entries
	if n := testutil.CollectAndCount(c, "gryvia_usage_gpu_hours_total"); n > maxUsageSeries {
		t.Fatalf("series = %d, want <= %d", n, maxUsageSeries)
	}
	total := 0.0
	mfs, _ := gather(c)
	for _, mf := range mfs {
		if mf.GetName() == "gryvia_usage_gpu_hours_total" {
			for _, m := range mf.GetMetric() {
				total += m.GetCounter().GetValue()
			}
		}
	}
	if total != float64(maxUsageSeries+50) {
		t.Fatalf("folding lost usage: total = %v", total)
	}
}

func TestListErrorsAreReportedNotFatal(t *testing.T) {
	// A reader whose scheme lacks the kinds fails every List; the collector still reports the error gauge.
	s := runtime.NewScheme()
	c := NewCollector(fake.NewClientBuilder().WithScheme(s).Build())
	if v := testutil.ToFloat64(errGauge(c)); v != 3 {
		t.Fatalf("list errors = %v, want 3", v)
	}
}

func TestLintsClean(t *testing.T) {
	c := NewCollector(newReader(t, quota("a", "t", 1, 1, 1, 1, &gryviav1.BudgetStatus{}), record("1", "t", "s", "USD", 1, 1, true)))
	problems, err := testutil.CollectAndLint(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		// Accepted lint: the gpu_hours name is the agreed contract (linter wants seconds); the others are bare counts.
		if p.Metric == "gryvia_tenants" || p.Metric == "gryvia_usage_gpu_hours_total" || p.Metric == "gryvia_quota_metrics_list_errors" {
			continue
		}
		t.Errorf("lint: %s: %s", p.Metric, p.Text)
	}
}
