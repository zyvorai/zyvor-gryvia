// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controllers

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gryviav1 "github.com/zyvorai/gryvia/operators/network-intelligence/api/v1"
)

const (
	// defaultCostReportingInterval is the default interval between cost reports
	defaultCostReportingInterval = 1 * time.Hour

	// maxCostReports is the maximum number of reports to retain (30 days for ~24 namespaces at daily granularity)
	maxCostReports = 720

	// gbBytes is the price unit, 1 GB = 10^9 bytes (same as services/api-gateway/routers/netusage.py).
	gbBytes = 1_000_000_000
)

var (
	usageRecordListGVK = schema.GroupVersionKind{Group: "gryvia.io", Version: "v1alpha1", Kind: "GryviaNetworkUsageRecordList"}
	networkRateListGVK = schema.GroupVersionKind{Group: "gryvia.io", Version: "v1alpha1", Kind: "GryviaNetworkRateList"}
)

// GryviaNetworkCostReconciler reconciles a GryviaNetworkCost object.
//
// No HTTP: reports are built from the GryviaNetworkUsageRecord objects the collector writes into the
// tenant namespaces (collector flag -publish-network-usage) and priced per GB with spec.costPerGB, or
// when that is all zero with the cluster's GryviaNetworkRate (first by name), with the pricing rules of
// the gateway (services/api-gateway/routers/netusage.py): only egress is priced, only zone classes
// same-zone / cross-zone / internet have a price, 1 GB = 10^9 bytes, exactly one measurement source
// ("collector") is used and duplicate fragments keep the larger value. One report per namespace and UTC
// day; a report is recomputed while its records exist and kept afterwards. Estimates, not invoices.
type GryviaNetworkCostReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=gryvia.io,resources=gryvianetworkcosts,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gryvia.io,resources=gryvianetworkcosts/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryvianetworkcosts/finalizers,verbs=update
//+kubebuilder:rbac:groups=gryvia.io,resources=gryvianetworkusagerecords,verbs=get;list;watch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryvianetworkrates,verbs=get;list;watch

func (r *GryviaNetworkCostReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	cost := &gryviav1.GryviaNetworkCost{}
	if err := r.Get(ctx, req.NamespacedName, cost); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	interval := defaultCostReportingInterval
	if cost.Spec.ReportingInterval != "" {
		if parsed, err := time.ParseDuration(cost.Spec.ReportingInterval); err == nil && parsed > 0 {
			interval = parsed
		}
	}

	records, rerr := r.listUsageRecords(ctx, cost.Spec.TargetNamespaces)
	rates := ratesOf(cost.Spec.CostPerGB)
	rateSource := "spec.costPerGB"
	currency := "USD"
	if !rates.any() {
		rateSource = ""
		if rr, cur, ok, err := r.clusterRate(ctx); err == nil && ok {
			rates, currency, rateSource = rr, cur, "GryviaNetworkRate"
		}
	}

	var fresh []gryviav1.NetworkCostReport
	if rerr == nil {
		fresh = buildCostReports(cost, records, rates)
	}

	uerr := updateStatus(ctx, r.Client, req.NamespacedName, func() *gryviav1.GryviaNetworkCost { return &gryviav1.GryviaNetworkCost{} },
		func(c *gryviav1.GryviaNetworkCost) {
			switch {
			case rerr != nil && isNoKind(rerr):
				c.Status.Phase = "Degraded"
				setCondition(&c.Status.Conditions, c.Generation, ConditionSourceAvailable, metav1.ConditionFalse, "CRDNotInstalled",
					"the GryviaNetworkUsageRecord CRD is not installed")
			case rerr != nil:
				c.Status.Phase = "Degraded"
				setCondition(&c.Status.Conditions, c.Generation, ConditionSourceAvailable, metav1.ConditionFalse, "Unreachable",
					"cannot list GryviaNetworkUsageRecord: "+rerr.Error())
			case len(records) == 0:
				c.Status.Phase = "AwaitingData"
				setCondition(&c.Status.Conditions, c.Generation, ConditionSourceAvailable, metav1.ConditionFalse, sourceNoData,
					"no GryviaNetworkUsageRecord found; enable the collector with ebpf.attributeNetwork and ebpf.publishNetworkUsage")
			default:
				c.Status.Phase = "Active"
				setCondition(&c.Status.Conditions, c.Generation, ConditionSourceAvailable, metav1.ConditionTrue, "Available",
					fmt.Sprintf("built from %d usage records (source collector)", len(records)))
				c.Status.Reports = mergeCostReports(c.Status.Reports, fresh)
				c.Status.LastReport = metav1.Now()
			}
			if !rates.any() {
				setCondition(&c.Status.Conditions, c.Generation, "RatesConfigured", metav1.ConditionFalse, "NoRate",
					"no price: set spec.costPerGB or create a cluster GryviaNetworkRate; totalCostUSD stays 0")
			} else {
				msg := "prices from " + rateSource
				if currency != "USD" {
					msg += fmt.Sprintf("; the rate currency is %s although the field is named totalCostUSD", currency)
				}
				setCondition(&c.Status.Conditions, c.Generation, "RatesConfigured", metav1.ConditionTrue, "Priced", msg)
			}
		})
	if uerr != nil && !errors.IsNotFound(uerr) {
		return ctrl.Result{}, uerr
	}
	logger.Info("GryviaNetworkCost reconciled", "records", len(records), "reports", len(fresh))
	return ctrl.Result{RequeueAfter: interval}, nil
}

const sourceNoData = "NoData"

type zoneRates struct{ same, cross, internet float64 }

func ratesOf(c gryviav1.CostPerGB) zoneRates {
	return zoneRates{same: c.SameZone, cross: c.CrossZone, internet: c.InternetEgress}
}
func (z zoneRates) any() bool { return z.same != 0 || z.cross != 0 || z.internet != 0 }

// listUsageRecords lists the usage records of the target namespaces (every namespace when none).
func (r *GryviaNetworkCostReconciler) listUsageRecords(ctx context.Context, namespaces []string) ([]unstructured.Unstructured, error) {
	list := func(opts ...client.ListOption) ([]unstructured.Unstructured, error) {
		l := &unstructured.UnstructuredList{}
		l.SetGroupVersionKind(usageRecordListGVK)
		if err := r.List(ctx, l, opts...); err != nil {
			return nil, err
		}
		return l.Items, nil
	}
	if len(namespaces) == 0 {
		return list()
	}
	var out []unstructured.Unstructured
	for _, ns := range namespaces {
		items, err := list(client.InNamespace(ns))
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
	}
	return out, nil
}

// clusterRate returns the first GryviaNetworkRate by name.
func (r *GryviaNetworkCostReconciler) clusterRate(ctx context.Context) (zoneRates, string, bool, error) {
	l := &unstructured.UnstructuredList{}
	l.SetGroupVersionKind(networkRateListGVK)
	if err := r.List(ctx, l); err != nil {
		return zoneRates{}, "", false, err
	}
	if len(l.Items) == 0 {
		return zoneRates{}, "", false, nil
	}
	sort.Slice(l.Items, func(i, j int) bool { return l.Items[i].GetName() < l.Items[j].GetName() })
	spec, _ := l.Items[0].Object["spec"].(map[string]interface{})
	cur, _ := spec["currency"].(string)
	if cur == "" {
		cur = "USD"
	}
	return zoneRates{same: num(spec["sameZone"]), cross: num(spec["crossZone"]), internet: num(spec["internetEgress"])}, cur, true, nil
}

func num(v interface{}) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	case int:
		return float64(n)
	}
	return 0
}

type recordView struct {
	namespace, node, tenant, peerClass, zoneClass string
	hour                                          time.Time
	egress                                        int64
}

// cleanRecords keeps the fragments of the "collector" source, de-duplicated on
// (node, tenant, hour, peerClass, zoneClass) keeping the larger egress (netusage.py clean()).
func cleanRecords(items []unstructured.Unstructured) []recordView {
	best := map[string]recordView{}
	for _, u := range items {
		spec, _ := u.Object["spec"].(map[string]interface{})
		if src, _ := spec["source"].(string); src != "" && src != "collector" {
			continue
		}
		hourStr, _ := spec["hour"].(string)
		hour, err := time.Parse(time.RFC3339, hourStr)
		if err != nil {
			continue
		}
		tenant, _ := spec["tenant"].(string)
		if tenant == "" {
			tenant = strings.TrimPrefix(u.GetNamespace(), "tenant-")
		}
		node, _ := spec["node"].(string)
		peer, _ := spec["peerClass"].(string)
		zone, _ := spec["zoneClass"].(string)
		v := recordView{namespace: u.GetNamespace(), node: node, tenant: tenant, peerClass: peer, zoneClass: zone,
			hour: hour.UTC(), egress: int64(num(spec["egressBytes"]))}
		k := fmt.Sprintf("%s|%s|%s|%d|%s|%s", v.namespace, node, tenant, hour.Unix(), peer, zone)
		if old, ok := best[k]; !ok || v.egress > old.egress {
			best[k] = v
		}
	}
	out := make([]recordView, 0, len(best))
	for _, v := range best {
		out = append(out, v)
	}
	return out
}

// buildCostReports aggregates the records to one report per (namespace, UTC day).
func buildCostReports(cost *gryviav1.GryviaNetworkCost, items []unstructured.Unstructured, rates zoneRates) []gryviav1.NetworkCostReport {
	team := map[string]string{}
	for _, cc := range cost.Spec.CostCenters {
		team[cc.Namespace] = cc.Team
	}
	type key struct{ ns, period string }
	agg := map[key]*gryviav1.NetworkCostReport{}
	for _, v := range cleanRecords(items) {
		k := key{v.namespace, v.hour.Format("2006-01-02")}
		rep := agg[k]
		if rep == nil {
			rep = &gryviav1.NetworkCostReport{Period: k.period, Namespace: k.ns, Team: team[k.ns]}
			agg[k] = rep
		}
		switch v.zoneClass {
		case "same-zone":
			rep.SameZoneBytes += v.egress
			rep.TotalCostUSD += float64(v.egress) / gbBytes * rates.same
		case "cross-zone":
			rep.CrossZoneBytes += v.egress
			rep.TotalCostUSD += float64(v.egress) / gbBytes * rates.cross
		case "internet":
			rep.ExternalBytes += v.egress
			rep.TotalCostUSD += float64(v.egress) / gbBytes * rates.internet
		}
		// same-node and unknown-zone egress has no price and no report field.
	}
	out := make([]gryviav1.NetworkCostReport, 0, len(agg))
	for _, rep := range agg {
		rep.TotalCostUSD = float64(int64(rep.TotalCostUSD*1e6+0.5)) / 1e6
		out = append(out, *rep)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Period != out[j].Period {
			return out[i].Period < out[j].Period
		}
		return out[i].Namespace < out[j].Namespace
	})
	return out
}

// mergeCostReports replaces the stored reports of every (namespace, period) that was recomputed and keeps
// the others (records may have been garbage-collected), capped at maxCostReports (oldest dropped).
func mergeCostReports(existing, fresh []gryviav1.NetworkCostReport) []gryviav1.NetworkCostReport {
	type key struct{ ns, period string }
	have := map[key]bool{}
	for _, f := range fresh {
		have[key{f.Namespace, f.Period}] = true
	}
	var out []gryviav1.NetworkCostReport
	for _, e := range existing {
		if !have[key{e.Namespace, e.Period}] {
			out = append(out, e)
		}
	}
	out = append(out, fresh...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Period != out[j].Period {
			return out[i].Period < out[j].Period
		}
		return out[i].Namespace < out[j].Namespace
	})
	if len(out) > maxCostReports {
		out = out[len(out)-maxCostReports:]
	}
	return out
}

// SetupWithManager sets up the controller with the Manager
func (r *GryviaNetworkCostReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gryviav1.GryviaNetworkCost{}).
		Complete(r)
}
