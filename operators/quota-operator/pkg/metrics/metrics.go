// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Package metrics exports quota, budget, usage and tenant gauges from the quota operator.
//
// Everything is computed at scrape time by one custom collector that lists GryviaQuota, GryviaUsageRecord and
// GryviaTenant objects through the manager's cached client, so the reconcilers need no hooks and a metric can never
// go stale or leak after an object is deleted. The collector is registered on controller-runtime's metrics registry,
// so the series appear on the operator's normal :8080/metrics endpoint next to controller_runtime_*.
//
// Cardinality is bounded by design: quota series carry only the team; usage series carry only tenant, sku and
// currency (never job or namespace names). Beyond maxUsageSeries distinct (tenant, sku, currency) combinations the
// rest are folded into tenant="_other". Not verified against a live cluster; the tests use the fake client.
package metrics

import (
	"context"
	"sort"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	gryviav1 "github.com/zyvorai/gryvia/operators/quota-operator/api/v1"
)

const (
	listTimeout    = 5 * time.Second
	maxUsageSeries = 1000
	otherReserve   = 4 // room kept for the folded _other series (one per currency; a few currencies expected)
	otherTenant    = "_other"
	defaultSKU     = "default"
	unknownValue   = "unknown"
)

// Collector implements prometheus.Collector over a controller-runtime reader (the manager's cache-backed client).
type Collector struct {
	reader client.Reader

	gpusAllocated *prometheus.Desc
	gpusMax       *prometheus.Desc
	runningJobs   *prometheus.Desc
	queuedJobs    *prometheus.Desc
	budgetPercent *prometheus.Desc
	budgetSpent   *prometheus.Desc
	gpuHours      *prometheus.Desc
	usageCost     *prometheus.Desc
	tenants       *prometheus.Desc
	recordsOpen   *prometheus.Desc
	listErrors    *prometheus.Desc
}

// NewCollector builds the collector; it does not register it.
func NewCollector(reader client.Reader) *Collector {
	team := []string{"team"}
	usage := []string{"tenant", "sku", "currency"}
	return &Collector{
		reader:        reader,
		gpusAllocated: prometheus.NewDesc("gryvia_quota_gpus_allocated", "GPUs currently allocated to the team (GryviaQuota status.currentUsage).", team, nil),
		gpusMax:       prometheus.NewDesc("gryvia_quota_gpus_max", "GPU limit of the team (GryviaQuota spec.gpuQuota.maxGPUs).", team, nil),
		runningJobs:   prometheus.NewDesc("gryvia_quota_running_jobs", "Running jobs counted against the team quota.", team, nil),
		queuedJobs:    prometheus.NewDesc("gryvia_quota_queued_jobs", "Queued jobs counted against the team quota.", team, nil),
		budgetPercent: prometheus.NewDesc("gryvia_quota_budget_percent_used", "Percent (0-100) of the team monthly budget used; only for quotas with a budget status.", team, nil),
		budgetSpent:   prometheus.NewDesc("gryvia_quota_budget_spent", "Spent this month against the team budget (the budget's own currency, USD by spec).", team, nil),
		gpuHours:      prometheus.NewDesc("gryvia_usage_gpu_hours_total", "GPU-hours summed over the GryviaUsageRecords that currently exist. Falls if records are deleted, so rate() may show a reset.", usage, nil),
		usageCost:     prometheus.NewDesc("gryvia_usage_cost_total", "Estimated cost summed over the GryviaUsageRecords that currently exist (estimates, not invoices).", usage, nil),
		tenants:       prometheus.NewDesc("gryvia_tenants", "Number of GryviaTenant objects.", nil, nil),
		recordsOpen:   prometheus.NewDesc("gryvia_usage_records_open", "GryviaUsageRecords not yet final (jobs still running).", nil, nil),
		listErrors:    prometheus.NewDesc("gryvia_quota_metrics_list_errors", "Kinds that could not be listed during this scrape (their series are omitted).", nil, nil),
	}
}

// Register adds a collector for the reader to controller-runtime's registry (the operator's /metrics endpoint).
func Register(reader client.Reader) error {
	return ctrlmetrics.Registry.Register(NewCollector(reader))
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.gpusAllocated, c.gpusMax, c.runningJobs, c.queuedJobs, c.budgetPercent,
		c.budgetSpent, c.gpuHours, c.usageCost, c.tenants, c.recordsOpen, c.listErrors} {
		ch <- d
	}
}

type usageKey struct{ tenant, sku, currency string }
type usageSum struct{ hours, cost float64 }

// Collect implements prometheus.Collector.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
	defer cancel()
	failed := 0

	var quotas gryviav1.GryviaQuotaList
	if err := c.reader.List(ctx, &quotas); err != nil {
		failed++
	} else {
		for i := range quotas.Items {
			q := &quotas.Items[i]
			team := q.Spec.Team
			if team == "" {
				team = q.Name
			}
			ch <- prometheus.MustNewConstMetric(c.gpusMax, prometheus.GaugeValue, float64(q.Spec.GPUQuota.MaxGPUs), team)
			ch <- prometheus.MustNewConstMetric(c.gpusAllocated, prometheus.GaugeValue, float64(q.Status.CurrentUsage.AllocatedGPUs), team)
			ch <- prometheus.MustNewConstMetric(c.runningJobs, prometheus.GaugeValue, float64(q.Status.CurrentUsage.RunningJobs), team)
			ch <- prometheus.MustNewConstMetric(c.queuedJobs, prometheus.GaugeValue, float64(q.Status.CurrentUsage.QueuedJobs), team)
			if b := q.Status.BudgetStatus; b != nil {
				ch <- prometheus.MustNewConstMetric(c.budgetPercent, prometheus.GaugeValue, b.PercentUsed, team)
				ch <- prometheus.MustNewConstMetric(c.budgetSpent, prometheus.GaugeValue, b.SpentThisMonth, team)
			}
		}
	}

	var records gryviav1.GryviaUsageRecordList
	if err := c.reader.List(ctx, &records); err != nil {
		failed++
	} else {
		open := 0
		sums := map[usageKey]*usageSum{}
		for i := range records.Items {
			s := &records.Items[i].Spec
			if !s.Final {
				open++
			}
			k := usageKey{tenant: orDefault(s.Tenant, unknownValue), sku: orDefault(s.Sku, defaultSKU), currency: orDefault(s.Currency, unknownValue)}
			add(sums, k, s.GpuHours, s.Cost)
		}
		for _, k := range boundedKeys(sums) {
			v := sums[k]
			ch <- prometheus.MustNewConstMetric(c.gpuHours, prometheus.CounterValue, v.hours, k.tenant, k.sku, k.currency)
			ch <- prometheus.MustNewConstMetric(c.usageCost, prometheus.CounterValue, v.cost, k.tenant, k.sku, k.currency)
		}
		ch <- prometheus.MustNewConstMetric(c.recordsOpen, prometheus.GaugeValue, float64(open))
	}

	var tenants gryviav1.GryviaTenantList
	if err := c.reader.List(ctx, &tenants); err != nil {
		failed++
	} else {
		ch <- prometheus.MustNewConstMetric(c.tenants, prometheus.GaugeValue, float64(len(tenants.Items)))
	}
	ch <- prometheus.MustNewConstMetric(c.listErrors, prometheus.GaugeValue, float64(failed))
}

func add(m map[usageKey]*usageSum, k usageKey, hours, cost float64) {
	s := m[k]
	if s == nil {
		s = &usageSum{}
		m[k] = s
	}
	s.hours += hours
	s.cost += cost
}

// sortedKeys returns the keys of m in a stable order.
func sortedKeys(m map[usageKey]*usageSum) []usageKey {
	keys := make([]usageKey, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.tenant != b.tenant {
			return a.tenant < b.tenant
		}
		if a.sku != b.sku {
			return a.sku < b.sku
		}
		return a.currency < b.currency
	})
	return keys
}

// boundedKeys returns the keys to export. Above maxUsageSeries the surplus (the last keys in sort order) is folded
// into one {tenant="_other", sku="_other"} series per currency, modifying m.
func boundedKeys(m map[usageKey]*usageSum) []usageKey {
	keys := sortedKeys(m)
	if len(keys) <= maxUsageSeries {
		return keys
	}
	for _, k := range keys[maxUsageSeries-otherReserve:] {
		v := m[k]
		delete(m, k)
		add(m, usageKey{otherTenant, otherTenant, k.currency}, v.hours, v.cost)
	}
	return sortedKeys(m)
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}
