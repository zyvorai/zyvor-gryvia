// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"crypto/sha256"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gryviav1 "github.com/zyvorai/gryvia/operators/quota-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/quota-operator/pkg/budget"
	"github.com/zyvorai/gryvia/operators/quota-operator/pkg/notify"
	"github.com/zyvorai/gryvia/operators/quota-operator/pkg/spend"
)

const budgetRequeue = time.Minute

// GryviaBudgetReconciler computes the status of a GryviaBudget from the metered
// GryviaUsageRecords (see pkg/spend and pkg/budget) once a minute. Usage records are
// refreshed every minute while a job runs, so the figures lag by at most about two minutes.
//
// Enforcement at submission time is the AI operator's admission gate; this controller
// only reports, plus a reactive fallback: when the budget is blocked it rejects jobs still
// Pending or Queued in the scope (they never got a workload if the gate is on).
type GryviaBudgetReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Now returns the current time; tests override it. Defaults to time.Now.
	Now            func() time.Time
	EventNamespace string

	// Webhook, when set, receives every budget threshold alert (see publishAlerts). nil = Events only.
	Webhook *notify.Webhook
	// Reader reads alert Events without the informer cache (the operator has no list/watch on events). nil = Client.
	Reader client.Reader
}

// annotationWebhookDelivered marks an alert Event whose webhook delivery succeeded, so a retry after a
// failed delivery re-sends (at least once) and a delivered alert is never sent twice.
const annotationWebhookDelivered = "gryvia.io/webhook-delivered"

// budgetAlertPayload is the JSON body POSTed to the webhook.
type budgetAlertPayload struct {
	Type       string    `json:"type"`
	Budget     string    `json:"budget"`
	Namespace  string    `json:"namespace,omitempty"`
	Threshold  float64   `json:"threshold"`
	Message    string    `json:"message"`
	Timestamp  time.Time `json:"timestamp"`
	Recipients []string  `json:"recipients,omitempty"`
}

//+kubebuilder:rbac:groups=gryvia.io,resources=gryviabudgets,verbs=get;list;watch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviabudgets/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviausagerecords,verbs=get;list;watch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaaijobs,verbs=get;list;watch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaaijobs/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaquotas,verbs=get;list;watch

func (r *GryviaBudgetReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Reconcile implements reconcile.Reconciler.
func (r *GryviaBudgetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	fb := &gryviav1.GryviaBudget{}
	if err := r.Get(ctx, req.NamespacedName, fb); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !fb.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	if err := r.reconcileBudget(ctx, fb); err != nil {
		setBudgetCondition(fb, "Ready", metav1.ConditionFalse, "ReconcileFailed", err.Error())
		if serr := r.Status().Update(ctx, fb); serr != nil && !errors.IsNotFound(serr) {
			log.FromContext(ctx).Error(serr, "update status after failure")
		}
		return ctrl.Result{}, err
	}
	if err := r.Status().Update(ctx, fb); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.publishAlerts(ctx, fb); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: budgetRequeue}, nil
}

func (r *GryviaBudgetReconciler) reconcileBudget(ctx context.Context, fb *gryviav1.GryviaBudget) error {
	logger := log.FromContext(ctx)
	now := r.now()

	period, err := spend.PeriodFor(fb.Spec.Period.Type, fb.Spec.Period.StartDate, fb.Spec.Period.EndDate, now)
	if err != nil {
		fb.Status.State = budget.StateActive
		setBudgetCondition(fb, "Ready", metav1.ConditionFalse, "InvalidPeriod", err.Error())
		return nil
	}
	sc, supported, err := budget.ScopeFor(ctx, r.Client, fb.Spec.Scope)
	if err != nil {
		return err
	}
	if !supported {
		fb.Status.State = budget.StateActive
		setBudgetCondition(fb, "Ready", metav1.ConditionFalse, "ScopeUnsupported",
			fmt.Sprintf("scope type %q has no usage attribution (supported: team, tenant, namespace); nothing is measured or enforced", fb.Spec.Scope.Type))
		return nil
	}

	recs := &gryviav1.GryviaUsageRecordList{}
	if err := r.List(ctx, recs); err != nil {
		return fmt.Errorf("list usage records: %w", err)
	}
	total := spend.Sum(recs.Items, sc, period, now)

	jobs, err := r.scopeJobs(ctx, sc)
	if err != nil {
		return err
	}
	running, gpus := 0, 0
	for i := range jobs {
		if jobs[i].Status.Phase == "Running" {
			running++
			gpus += int(jobs[i].Spec.TotalGPUs())
		}
	}

	ev := budget.Evaluate(fb.Spec, total, period, now, running, gpus)

	fb.Status.CurrentPeriod = &gryviav1.BudgetCurrentPeriod{
		StartDate:     metav1.NewTime(period.Start),
		EndDate:       metav1.NewTime(period.End),
		DaysRemaining: max(int(period.End.Sub(now).Hours()/24), 0),
	}
	usage := ev.Usage
	fb.Status.Usage = &usage
	util := ev.Utilization
	fb.Status.Utilization = &util
	fc := ev.Forecast
	fb.Status.Forecast = &fc
	fb.Status.State = ev.State
	r.recordAlerts(fb, ev, period, now)

	if ev.Enforceable {
		setBudgetCondition(fb, "Enforceable", metav1.ConditionTrue, "Comparable", "usage and limits share a currency")
	} else {
		setBudgetCondition(fb, "Enforceable", metav1.ConditionFalse, "CurrencyMismatch", ev.Reason+"; the budget is reported but not enforced")
	}
	setBudgetCondition(fb, "Ready", metav1.ConditionTrue, "BudgetReconciled",
		fmt.Sprintf("Budget %s: %.1f%% consumed", ev.State, ev.Percent))

	if ev.State == budget.StateBlocked {
		if err := r.rejectHeldJobs(ctx, fb, jobs); err != nil {
			logger.Error(err, "reject held jobs")
		}
	}
	return nil
}

// recordAlerts appends one alert per newly crossed threshold and drops alerts of earlier
// periods, so a threshold alerts again in the next period. At most 20 are kept.
func (r *GryviaBudgetReconciler) recordAlerts(fb *gryviav1.GryviaBudget, ev budget.Evaluation, p spend.Period, now time.Time) {
	kept := fb.Status.Alerts[:0:0]
	have := map[float64]bool{}
	for _, a := range fb.Status.Alerts {
		if !a.Timestamp.Time.Before(p.Start) {
			kept = append(kept, a)
			have[a.Threshold] = true
		}
	}
	for _, th := range ev.Crossed {
		if have[th] {
			continue
		}
		kept = append(kept, gryviav1.BudgetAlertEvent{
			Timestamp: metav1.NewTime(now),
			Threshold: th,
			Message:   fmt.Sprintf("Budget %.0f%% consumed (threshold %.0f%%)", ev.Percent, th),
		})
	}
	if len(kept) > 20 {
		kept = kept[len(kept)-20:]
	}
	fb.Status.Alerts = kept
}

// scopeJobs lists the jobs in the scope's namespaces.
func (r *GryviaBudgetReconciler) scopeJobs(ctx context.Context, sc spend.Scope) ([]gryviav1.GryviaAIJob, error) {
	var out []gryviav1.GryviaAIJob
	seen := map[string]bool{}
	nss := append([]string{}, sc.Namespaces...)
	if sc.Tenant != "" {
		nss = append(nss, spend.TenantNamespacePrefix+sc.Tenant)
	}
	for _, ns := range nss {
		if seen[ns] {
			continue
		}
		seen[ns] = true
		list := &gryviav1.GryviaAIJobList{}
		if err := r.List(ctx, list, client.InNamespace(ns)); err != nil {
			return nil, fmt.Errorf("list jobs in %s: %w", ns, err)
		}
		out = append(out, list.Items...)
	}
	return out, nil
}

// rejectHeldJobs is the reactive fallback for a blocked budget: jobs still Pending or Queued
// are Rejected through a status merge patch.
func (r *GryviaBudgetReconciler) rejectHeldJobs(ctx context.Context, fb *gryviav1.GryviaBudget, jobs []gryviav1.GryviaAIJob) error {
	logger := log.FromContext(ctx)
	for i := range jobs {
		job := &jobs[i]
		if job.Status.Phase != "Pending" && job.Status.Phase != "Queued" {
			continue
		}
		msg := fmt.Sprintf("Budget %s blocked for %s %s", fb.Name, fb.Spec.Scope.Type, fb.Spec.Scope.Name)
		if err := patchJobPhase(ctx, r.Client, job, "Rejected", msg, &metav1.Condition{
			Type: "Rejected", Status: metav1.ConditionTrue, Reason: "BudgetExceeded", Message: msg,
			LastTransitionTime: metav1.Now(),
		}); err != nil {
			logger.Error(err, "reject job", "job", job.Name)
			continue
		}
	}
	return nil
}

func setBudgetCondition(fb *gryviav1.GryviaBudget, condType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&fb.Status.Conditions, metav1.Condition{
		Type: condType, Status: status, Reason: reason, Message: message,
		ObservedGeneration: fb.Generation, LastTransitionTime: metav1.Now(),
	})
}

// SetupWithManager sets up the controller with the Manager. There is no watch on jobs or
// usage records: the budget is recomputed every minute, which matches how often records refresh.
func (r *GryviaBudgetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("gryviabudget").
		For(&gryviav1.GryviaBudget{}).
		Complete(r)
}

// enforceBudget rejects the held (Pending/Queued) jobs of the budget's scope.
func (r *GryviaBudgetReconciler) enforceBudget(ctx context.Context, fb *gryviav1.GryviaBudget) error {
	sc, ok, err := budget.ScopeFor(ctx, r.Client, fb.Spec.Scope)
	if err != nil || !ok {
		return err
	}
	jobs, err := r.scopeJobs(ctx, sc)
	if err != nil {
		return err
	}
	return r.rejectHeldJobs(ctx, fb, jobs)
}

// Deterministic event names make delivery idempotent and retryable after status persistence.
func (r *GryviaBudgetReconciler) publishAlerts(ctx context.Context, fb *gryviav1.GryviaBudget) error {
	for _, a := range fb.Status.Alerts {
		ns := r.EventNamespace
		if ns == "" {
			ns = "default"
		}
		digest := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s/%s/%g", fb.UID, a.Timestamp.Time.Format(time.RFC3339Nano), a.Threshold))))[:24]
		e := &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "gryvia-budget-" + digest, Namespace: ns}, InvolvedObject: corev1.ObjectReference{APIVersion: "gryvia.io/v1", Kind: "GryviaBudget", Name: fb.Name, Namespace: fb.Namespace, UID: types.UID(fb.UID)}, Reason: "BudgetThresholdCrossed", Message: a.Message, Type: corev1.EventTypeWarning, FirstTimestamp: a.Timestamp, LastTimestamp: a.Timestamp, Count: 1, Source: corev1.EventSource{Component: "gryvia-quota-operator"}}
		err := r.Create(ctx, e)
		switch {
		case err == nil:
			// new alert: fall through to delivery
		case errors.IsAlreadyExists(err):
			if r.Webhook == nil {
				continue
			}
			var rd client.Reader = r.Client
			if r.Reader != nil {
				rd = r.Reader
			}
			if err := rd.Get(ctx, client.ObjectKeyFromObject(e), e); err != nil {
				return err
			}
			if e.Annotations[annotationWebhookDelivered] == "true" {
				continue
			}
		default:
			return err
		}
		if err := r.deliverAlert(ctx, fb, a, e); err != nil {
			return err
		}
	}
	return nil
}

// deliverAlert POSTs one alert to the webhook and then marks its Event delivered. A failure is returned so
// the budget is reconciled again (at most a minute later) and the delivery retried.
func (r *GryviaBudgetReconciler) deliverAlert(ctx context.Context, fb *gryviav1.GryviaBudget, a gryviav1.BudgetAlertEvent, e *corev1.Event) error {
	if r.Webhook == nil {
		return nil
	}
	p := budgetAlertPayload{Type: "budget.threshold", Budget: fb.Name, Namespace: fb.Namespace,
		Threshold: a.Threshold, Message: a.Message, Timestamp: a.Timestamp.Time}
	for _, rule := range fb.Spec.Alerts {
		if rule.Threshold == a.Threshold {
			p.Recipients = append(p.Recipients, rule.Recipients...)
		}
	}
	body, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if err := r.Webhook.Post(ctx, body); err != nil {
		return fmt.Errorf("budget %s alert %g%%: webhook delivery failed: %w", fb.Name, a.Threshold, err)
	}
	patch := client.MergeFrom(e.DeepCopy())
	if e.Annotations == nil {
		e.Annotations = map[string]string{}
	}
	e.Annotations[annotationWebhookDelivered] = "true"
	return r.Patch(ctx, e, patch)
}
