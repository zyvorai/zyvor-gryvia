package controllers

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/ai-operator/pkg/admission"
)

const (
	// ConditionBudgetWarning is set on a job that was admitted although a soft budget would be
	// exceeded or a budget could not be checked.
	ConditionBudgetWarning = "BudgetWarning"
	// ConditionAdmissionUnchecked is set when the gate could not read its inputs and let the job through.
	ConditionAdmissionUnchecked = "AdmissionUnchecked"
)

// Decision is the admission gate's verdict (see pkg/admission).
type Decision = admission.Decision

// admit asks the gate (operator flag --admission-gate) whether the job may be created: quota
// limits (allowed GPU types and SKUs, max GPUs per job, concurrent GPUs) and hard budgets
// against the metered spend plus a forecast for this job. An error means the gate could not
// decide; the caller fails open.
//
// With --preflight-enforce the job's preflight estimate (pkg/preflight) is checked too, with or
// without --admission-gate: a job that no node pool can hold is rejected (PreflightBlocked).
func (r *GryviaAIJobReconciler) admit(ctx context.Context, job *gryviav1.GryviaAIJob) (Decision, error) {
	g := &admission.Gate{Client: r.Client, DefaultHours: r.AdmissionDefaultHours}
	d := Decision{Allow: true}
	if r.AdmissionGate {
		var err error
		if d, err = g.Evaluate(ctx, job); err != nil || !d.Allow {
			return d, err
		}
	}
	if r.PreflightEnforce {
		p, err := g.Preflight(ctx, job)
		if err != nil {
			return Decision{}, err
		}
		if !p.Allow {
			return p, nil
		}
		d.Warnings = append(d.Warnings, p.Warnings...)
	}
	return d, nil
}

// admissionGate is the single call site in reconcileAIJob. It runs only for jobs that have not
// started (phase Pending) and only when the gate is enabled, before anything is created. It
// returns done=true when it rejected the job (nothing is created, and the Rejected phase is
// sticky, so a rejected job is never re-evaluated). Failing open: any lookup error lets the
// job through with a Warning event and an AdmissionUnchecked condition.
func (r *GryviaAIJobReconciler) admissionGate(ctx context.Context, job *gryviav1.GryviaAIJob) (bool, ctrl.Result, error) {
	if (!r.AdmissionGate && !r.PreflightEnforce) || job.Status.Phase != PhasePending {
		return false, ctrl.Result{}, nil
	}
	log := r.Log.WithValues("gryviaaijob", job.Namespace+"/"+job.Name)

	d, err := r.admit(ctx, job)
	if err != nil {
		log.Info("admission gate could not decide; allowing the job", "error", err.Error())
		r.setGateCondition(ctx, job, ConditionAdmissionUnchecked, metav1.ConditionTrue, "LookupFailed",
			"admission gate skipped: "+err.Error(), corev1.EventTypeWarning)
		return false, ctrl.Result{}, nil
	}
	if !d.Allow {
		msg := d.Message()
		log.Info("admission gate rejected the job", "reason", msg)
		base := job.DeepCopy()
		job.Status.Phase = PhaseRejected
		job.Status.Message = msg
		meta.SetStatusCondition(&job.Status.Conditions, metav1.Condition{
			Type: "Rejected", Status: metav1.ConditionTrue, Reason: d.Code, Message: msg,
			LastTransitionTime: metav1.Now(),
		})
		if err := r.Status().Patch(ctx, job, client.MergeFrom(base)); err != nil {
			return true, ctrl.Result{}, err
		}
		if r.Recorder != nil {
			r.Recorder.Event(job, corev1.EventTypeWarning, "AdmissionRejected", msg)
		}
		return true, ctrl.Result{}, nil
	}
	if len(d.Warnings) > 0 {
		r.setGateCondition(ctx, job, ConditionBudgetWarning, metav1.ConditionTrue, "BudgetWarning",
			joinWarnings(d.Warnings), corev1.EventTypeWarning)
	}
	return false, ctrl.Result{}, nil
}

func joinWarnings(w []string) string {
	out := ""
	for i, s := range w {
		if i > 0 {
			out += "; "
		}
		out += s
	}
	return out
}

// setGateCondition records a warning-level condition (and an Event) once per distinct message:
// the gate re-runs while a job stays Pending, so an unchanged warning is not written again.
func (r *GryviaAIJobReconciler) setGateCondition(ctx context.Context, job *gryviav1.GryviaAIJob, condType string, status metav1.ConditionStatus, reason, msg, eventType string) {
	if c := meta.FindStatusCondition(job.Status.Conditions, condType); c != nil && c.Status == status && c.Message == msg {
		return
	}
	base := job.DeepCopy()
	meta.SetStatusCondition(&job.Status.Conditions, metav1.Condition{
		Type: condType, Status: status, Reason: reason, Message: msg, LastTransitionTime: metav1.Now(),
	})
	if err := r.Status().Patch(ctx, job, client.MergeFrom(base)); err != nil {
		r.Log.Info("could not record admission condition", "condition", condType, "error", err.Error())
		return
	}
	if r.Recorder != nil {
		r.Recorder.Event(job, eventType, reason, msg)
	}
}
