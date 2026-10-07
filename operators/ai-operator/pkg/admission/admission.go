// Package admission is the AIJob controller's admission gate: before the controller creates
// anything for a job (PVC, Service, workload) it asks whether the quota and budgets covering
// the job's namespace allow it, using the metered GryviaUsageRecords for the spend and a
// forecast for the job itself. A refused job is rejected instead of being created and torn
// down afterwards.
//
// Everything is read as unstructured objects (the quota operator is a separate Go module) and
// every read failure is returned as an error: the caller fails OPEN, exactly like the
// admission webhook, so a missing CRD, a missing permission or a stale cache never blocks a job.
//
// The gate works on estimates. Spend comes from usage records refreshed once a minute; the
// forecast assumes the job runs for its whole timeout (default one hour) at the SKU rate.
// Concurrent submissions can each pass before any of them shows up in the records.
package admission

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/ai-operator/pkg/preflight"
	"github.com/zyvorai/gryvia/operators/ai-operator/pkg/webhook"
)

// Reason codes of a refusal (condition reason on the rejected job).
const (
	CodeQuota  = "QuotaExceeded"
	CodeBudget = "BudgetExceeded"
)

// DefaultForecastHours is the duration assumed for a job without spec.timeout.
const DefaultForecastHours = 1.0

var (
	quotaListGVK  = schema.GroupVersionKind{Group: "gryvia.io", Version: "v1alpha1", Kind: "GryviaQuotaList"}
	budgetListGVK = schema.GroupVersionKind{Group: "gryvia.io", Version: "v1alpha1", Kind: "GryviaBudgetList"}
	usageListGVK  = schema.GroupVersionKind{Group: "gryvia.io", Version: "v1alpha1", Kind: "GryviaUsageRecordList"}
	skuListGVK    = schema.GroupVersionKind{Group: "gryvia.io", Version: "v1alpha1", Kind: "GryviaGpuSkuList"}
)

// Decision is the gate's verdict.
type Decision struct {
	Allow bool
	// Code is the reason code when refused.
	Code string
	// Reasons explain a refusal.
	Reasons []string
	// Warnings are soft findings on an allowed job: a soft budget that would be exceeded, or a
	// budget that could not be checked (mixed currency, unpriced GPU type).
	Warnings []string
}

// Message is the refusal text for status.message.
func (d Decision) Message() string { return strings.Join(d.Reasons, "; ") }

// Gate evaluates jobs.
type Gate struct {
	Client client.Client
	// Now returns the current time; tests override it. Defaults to time.Now.
	Now func() time.Time
	// DefaultHours is the forecast duration of a job without a timeout (DefaultForecastHours when 0).
	DefaultHours float64
}

func (g *Gate) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

// budgetRule is one spend limit that covers the job.
type budgetRule struct {
	name          string
	kind          string // "GryviaBudget" or "GryviaQuota"
	scope         scope
	per           period
	costLimit     float64
	gpuHoursLimit float64
	hard          bool
	blockPercent  float64 // percent of the limit at which new jobs are refused (hard only)
}

// Evaluate decides whether the job may be created. A returned error means "could not
// decide": the caller must allow the job.
func (g *Gate) Evaluate(ctx context.Context, job *gryviav1.GryviaAIJob) (Decision, error) {
	total := job.Spec.TotalGPUs()
	if total <= 0 {
		return Decision{Allow: true}, nil // CPU-only: nothing to meter or limit
	}
	ns := job.Namespace
	now := g.now()

	policy, err := webhook.LoadGPUPolicy(ctx, g.Client, ns)
	if err != nil {
		return Decision{}, fmt.Errorf("quota policy lookup: %w", err)
	}
	dec := Decision{Allow: true}
	if reasons := webhook.CheckGPUPolicy(job, policy); len(reasons) > 0 {
		dec.Allow, dec.Code = false, CodeQuota
		dec.Reasons = append(dec.Reasons, reasons...)
	}

	quotas, err := g.list(ctx, quotaListGVK)
	if err != nil {
		return Decision{}, fmt.Errorf("quota lookup: %w", err)
	}
	var covering []unstructured.Unstructured
	for _, q := range quotas {
		nss, _, _ := unstructured.NestedStringSlice(q.Object, "spec", "namespaces")
		if containsFold(nss, ns) {
			covering = append(covering, q)
		}
	}

	// Concurrent GPU limit (quota.spec.gpuQuota.maxGPUs) across the quota's namespaces.
	for _, q := range covering {
		max := int32(num(q.Object, "spec", "gpuQuota", "maxGPUs"))
		if max <= 0 {
			continue
		}
		nss, _, _ := unstructured.NestedStringSlice(q.Object, "spec", "namespaces")
		used, err := g.heldGPUs(ctx, nss, job)
		if err != nil {
			return Decision{}, fmt.Errorf("job lookup: %w", err)
		}
		if used+total > max {
			dec.Allow, dec.Code = false, CodeQuota
			dec.Reasons = append(dec.Reasons, fmt.Sprintf("quota %s allows %d concurrent GPUs; %d are in use and this job needs %d",
				q.GetName(), max, used, total))
		}
	}

	rules, warns := g.budgetRules(ctx, quotas, covering, ns, now)
	dec.Warnings = append(dec.Warnings, warns...)
	if len(rules) > 0 {
		bd, err := g.checkBudgets(ctx, job, policy, rules, total, now)
		if err != nil {
			return Decision{}, err
		}
		dec.Warnings = append(dec.Warnings, bd.Warnings...)
		if !bd.Allow {
			dec.Allow, dec.Code = false, CodeBudget
			dec.Reasons = append(dec.Reasons, bd.Reasons...)
		}
	}
	if !dec.Allow {
		dec.Warnings = nil
	}
	return dec, nil
}

func (g *Gate) list(ctx context.Context, gvk schema.GroupVersionKind) ([]unstructured.Unstructured, error) {
	l := &unstructured.UnstructuredList{}
	l.SetGroupVersionKind(gvk)
	if err := g.Client.List(ctx, l); err != nil {
		return nil, err
	}
	return l.Items, nil
}

// heldGPUs sums the GPUs of jobs that hold resources (Scheduling, Running) in the namespaces,
// not counting the job itself.
func (g *Gate) heldGPUs(ctx context.Context, namespaces []string, self *gryviav1.GryviaAIJob) (int32, error) {
	var used int32
	for _, ns := range namespaces {
		jobs := &gryviav1.GryviaAIJobList{}
		if err := g.Client.List(ctx, jobs, client.InNamespace(ns)); err != nil {
			return 0, err
		}
		for i := range jobs.Items {
			j := &jobs.Items[i]
			if j.UID == self.UID && j.Name == self.Name && j.Namespace == self.Namespace {
				continue
			}
			if j.Status.Phase == "Running" || j.Status.Phase == "Scheduling" {
				used += j.Spec.TotalGPUs()
			}
		}
	}
	return used, nil
}

// budgetRules collects the spend limits covering namespace ns: the budgets of GryviaQuotas and
// GryviaBudget objects. Budgets it cannot apply (bad period, unsupported scope) become warnings.
func (g *Gate) budgetRules(ctx context.Context, quotas, covering []unstructured.Unstructured, ns string, now time.Time) ([]budgetRule, []string) {
	var rules []budgetRule
	var warns []string

	for _, q := range covering {
		limit := num(q.Object, "spec", "budget", "monthlyBudget")
		if limit <= 0 {
			continue
		}
		hard, _, _ := unstructured.NestedBool(q.Object, "spec", "budget", "hardLimit")
		nss, _, _ := unstructured.NestedStringSlice(q.Object, "spec", "namespaces")
		p, _ := periodFor("monthly", "", "", now)
		rules = append(rules, budgetRule{
			name: q.GetName(), kind: "GryviaQuota", scope: scope{namespaces: nss}, per: p,
			costLimit: limit, hard: hard, blockPercent: 100,
		})
	}

	budgets, err := g.list(ctx, budgetListGVK)
	if err != nil {
		// A cluster without the GryviaBudget CRD still has quota budgets.
		return rules, append(warns, "GryviaBudget objects could not be read: "+err.Error())
	}
	for _, b := range budgets {
		stype, _, _ := unstructured.NestedString(b.Object, "spec", "scope", "type")
		sname, _, _ := unstructured.NestedString(b.Object, "spec", "scope", "name")
		var sc scope
		switch strings.ToLower(stype) {
		case "namespace":
			sc = scope{namespaces: []string{sname}}
		case "tenant":
			sc = scope{namespaces: []string{tenantPrefix + sname}, tenant: sname}
		case "team":
			sc.tenant = sname
			for _, q := range quotas {
				if team, _, _ := unstructured.NestedString(q.Object, "spec", "team"); team == sname {
					nss, _, _ := unstructured.NestedStringSlice(q.Object, "spec", "namespaces")
					sc.namespaces = append(sc.namespaces, nss...)
				}
			}
		default:
			continue // user/project scopes have no usage attribution
		}
		if !sc.covers(ns) && !containsFold(sc.namespaces, ns) {
			continue
		}
		pt, _, _ := unstructured.NestedString(b.Object, "spec", "period", "type")
		ps, _, _ := unstructured.NestedString(b.Object, "spec", "period", "startDate")
		pe, _, _ := unstructured.NestedString(b.Object, "spec", "period", "endDate")
		per, err := periodFor(pt, ps, pe, now)
		if err != nil {
			warns = append(warns, fmt.Sprintf("budget %s not applied: %v", b.GetName(), err))
			continue
		}
		if now.Before(per.start) || !now.Before(per.end) {
			continue // a custom period that has not started or is over does not apply to a job starting now
		}
		r := budgetRule{
			name: b.GetName(), kind: "GryviaBudget", scope: sc, per: per,
			costLimit:     num(b.Object, "spec", "limits", "costUSD"),
			gpuHoursLimit: num(b.Object, "spec", "limits", "gpuHours"),
		}
		if r.costLimit <= 0 && r.gpuHoursLimit <= 0 {
			continue
		}
		enabled, _, _ := unstructured.NestedBool(b.Object, "spec", "enforcement", "enabled")
		action, _, _ := unstructured.NestedString(b.Object, "spec", "enforcement", "action")
		if enabled && strings.EqualFold(action, "block") {
			r.hard, r.blockPercent = true, 100
			alerts, _, _ := unstructured.NestedSlice(b.Object, "spec", "alerts")
			for _, a := range alerts {
				am, ok := a.(map[string]interface{})
				if !ok {
					continue
				}
				th := num(am, "threshold")
				acts, _, _ := unstructured.NestedStringSlice(am, "actions")
				if containsFold(acts, "block") && th > 0 && th < r.blockPercent {
					r.blockPercent = th
				}
			}
		}
		rules = append(rules, r)
	}
	return rules, warns
}

func (g *Gate) checkBudgets(ctx context.Context, job *gryviav1.GryviaAIJob, policy webhook.GPUPolicy, rules []budgetRule, gpus int32, now time.Time) (Decision, error) {
	dec := Decision{Allow: true}

	rawRecs, err := g.list(ctx, usageListGVK)
	if err != nil {
		return Decision{}, fmt.Errorf("usage record lookup: %w", err)
	}
	recs := make([]record, 0, len(rawRecs))
	for i := range rawRecs {
		if r, ok := parseRecord(&rawRecs[i]); ok {
			recs = append(recs, r)
		}
	}
	rawSkus, err := g.list(ctx, skuListGVK)
	if err != nil {
		return Decision{}, fmt.Errorf("SKU lookup: %w", err)
	}
	skus := make([]sku, 0, len(rawSkus))
	for i := range rawSkus {
		skus = append(skus, parseSku(&rawSkus[i]))
	}

	hours := DefaultForecastHours
	if g.DefaultHours > 0 {
		hours = g.DefaultHours
	}
	if secs, err := job.Spec.TimeoutSeconds(); err == nil && secs != nil && *secs > 0 {
		hours = float64(*secs) / 3600
	}
	rt := resolveRate(skus, job.Spec.GpuType, policy.AllowedSkus())
	forecastHours := float64(gpus) * hours
	forecastCost := forecastHours * rt.perGPUHour

	for _, r := range rules {
		t := sum(recs, r.scope, r.per, now)
		label := fmt.Sprintf("%s %q", r.kind, r.name)

		if r.costLimit > 0 {
			if ok, why := t.comparable(); !ok {
				dec.Warnings = append(dec.Warnings, fmt.Sprintf("%s: cost limit not checked: %s", label, why))
			} else if !rt.found {
				dec.Warnings = append(dec.Warnings, fmt.Sprintf("%s: cost limit not checked: no enabled SKU prices GPU type %q", label, job.Spec.GpuType))
			} else if rt.currency != budgetCurrency {
				dec.Warnings = append(dec.Warnings, fmt.Sprintf("%s: cost limit not checked: the job would be priced in %s but the limit is in %s", label, rt.currency, budgetCurrency))
			} else {
				limit := r.costLimit
				if r.hard {
					limit = r.costLimit * r.blockPercent / 100
				}
				if t.cost+forecastCost > limit {
					msg := fmt.Sprintf("%s: spend %.2f + forecast %.2f (%d GPUs x %.2fh x %.2f %s/GPU-h) exceeds %s%.2f %s",
						label, round2(t.cost), round2(forecastCost), gpus, hours, rt.perGPUHour, rt.currency,
						limitWord(r), round2(limit), budgetCurrency)
					if r.hard {
						dec.Allow = false
						dec.Reasons = append(dec.Reasons, msg)
					} else {
						dec.Warnings = append(dec.Warnings, msg+" (soft limit: job allowed)")
					}
				}
			}
		}
		if r.gpuHoursLimit > 0 {
			limit := r.gpuHoursLimit
			if r.hard {
				limit = r.gpuHoursLimit * r.blockPercent / 100
			}
			if t.gpuHours+forecastHours > limit {
				msg := fmt.Sprintf("%s: GPU hours %.2f + forecast %.2f exceeds %s%.2f", label, round2(t.gpuHours), round2(forecastHours), limitWord(r), round2(limit))
				if r.hard {
					dec.Allow = false
					dec.Reasons = append(dec.Reasons, msg)
				} else {
					dec.Warnings = append(dec.Warnings, msg+" (soft limit: job allowed)")
				}
			}
		}
	}
	return dec, nil
}

func limitWord(r budgetRule) string {
	if r.hard && r.blockPercent < 100 {
		return fmt.Sprintf("the block point (%.0f%% of the limit) ", r.blockPercent)
	}
	return "the limit "
}

// CodePreflight is the reason code when the preflight estimate finds no node pool for the job.
const CodePreflight = "PreflightBlocked"

// Preflight evaluates the job's preflight annotations against the cluster's nodes (operator flag
// --preflight-enforce). A job without gryvia.io/model-params-billions, an empty cluster and unknown
// GPU memory all allow the job; only Blocked refuses it. A returned error means "could not decide".
func (g *Gate) Preflight(ctx context.Context, job *gryviav1.GryviaAIJob) (Decision, error) {
	in, ok, errs := preflight.FromJob(job)
	if !ok || len(errs) > 0 {
		return Decision{Allow: true}, nil
	}
	nodes := &corev1.NodeList{}
	if err := g.Client.List(ctx, nodes); err != nil {
		return Decision{}, fmt.Errorf("node lookup: %w", err)
	}
	r := preflight.Evaluate(in, preflight.PoolsFromNodes(nodes.Items))
	switch r.State {
	case preflight.StateBlocked:
		return Decision{Code: CodePreflight, Reasons: []string{r.Summary()}}, nil
	case preflight.StateIncomplete:
		return Decision{Allow: true, Warnings: []string{r.Summary()}}, nil
	}
	return Decision{Allow: true}, nil
}
