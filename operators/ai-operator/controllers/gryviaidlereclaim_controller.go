package controllers

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

const (
	// AnnotationIdleReclaim set to "false" on a job exempts it from idle-GPU reclaim.
	AnnotationIdleReclaim = "gryvia.io/idle-reclaim"
	// AnnotationIdleSince records when the job's GPU utilization first dropped below the threshold.
	// It is stored on the job so the idle window survives an operator restart.
	AnnotationIdleSince = "gryvia.io/idle-since"
	// ConditionIdleGPU is True while a running job has been below the threshold for the window.
	ConditionIdleGPU = "IdleGPU"

	idleRecheckInterval = time.Minute
)

// GryviaIdleReclaimReconciler flags running jobs that hold GPUs without using them. In advisory
// mode (default) it sets the IdleGPU condition and emits one event. With Suspend it also sets
// spec.suspend, which releases the job's GPUs (and Kueue quota); resume by clearing spec.suspend.
//
// The utilization input is status.metrics.gpuUtilization, which the GryviaTrainingProfiler writes,
// so a job without a profiler is never judged idle.
type GryviaIdleReclaimReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder EventRecorder

	// Threshold is the utilization percentage below which a job counts as idle.
	Threshold float64
	// Window is how long a job must stay below Threshold before it is flagged.
	Window time.Duration
	// Suspend makes the reconciler suspend a flagged job instead of only reporting it.
	Suspend bool

	// Now is overridable for tests.
	Now func() time.Time
}

//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaaijobs,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaaijobs/status,verbs=get;update;patch

func (r *GryviaIdleReclaimReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *GryviaIdleReclaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	job := &gryviav1.GryviaAIJob{}
	if err := r.Get(ctx, req.NamespacedName, job); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	exempt := job.Annotations[AnnotationIdleReclaim] == "false"
	if job.Status.Phase != PhaseRunning || job.Status.Metrics == nil || exempt || job.Spec.GPUs == 0 {
		return ctrl.Result{}, r.clear(ctx, job)
	}

	if job.Status.Metrics.GpuUtilization >= r.Threshold {
		return ctrl.Result{RequeueAfter: idleRecheckInterval}, r.clear(ctx, job)
	}

	now := r.now()
	since, err := time.Parse(time.RFC3339, job.Annotations[AnnotationIdleSince])
	if err != nil {
		patch := client.MergeFrom(job.DeepCopy())
		if job.Annotations == nil {
			job.Annotations = map[string]string{}
		}
		job.Annotations[AnnotationIdleSince] = now.UTC().Format(time.RFC3339)
		if err := r.Patch(ctx, job, patch); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: idleRecheckInterval}, nil
	}

	if idleFor := now.Sub(since); idleFor < r.Window {
		return ctrl.Result{RequeueAfter: minDuration(idleRecheckInterval, r.Window-idleFor)}, nil
	}

	msg := fmt.Sprintf("GPU utilization %.0f%% has been below %.0f%% since %s", job.Status.Metrics.GpuUtilization, r.Threshold, since.UTC().Format(time.RFC3339))
	already := apimeta.IsStatusConditionTrue(job.Status.Conditions, ConditionIdleGPU)
	if !already {
		apimeta.SetStatusCondition(&job.Status.Conditions, metav1.Condition{
			Type: ConditionIdleGPU, Status: metav1.ConditionTrue, Reason: "BelowThreshold", Message: msg,
			ObservedGeneration: job.Generation,
		})
		if err := r.Status().Update(ctx, job); err != nil {
			return ctrl.Result{}, err
		}
		if r.Recorder != nil {
			r.Recorder.Event(job, corev1.EventTypeWarning, "IdleGPU", msg)
		}
	}

	if r.Suspend && !job.Spec.Suspend {
		patch := client.MergeFrom(job.DeepCopy())
		job.Spec.Suspend = true
		if err := r.Patch(ctx, job, patch); err != nil {
			return ctrl.Result{}, err
		}
		if r.Recorder != nil {
			r.Recorder.Event(job, corev1.EventTypeWarning, "IdleGPUSuspended", "suspended to release idle GPUs: "+msg)
		}
	}
	return ctrl.Result{RequeueAfter: idleRecheckInterval}, nil
}

// clear drops the idle bookkeeping once a job is busy again, finished, or exempt.
func (r *GryviaIdleReclaimReconciler) clear(ctx context.Context, job *gryviav1.GryviaAIJob) error {
	if _, ok := job.Annotations[AnnotationIdleSince]; ok {
		patch := client.MergeFrom(job.DeepCopy())
		delete(job.Annotations, AnnotationIdleSince)
		if err := r.Patch(ctx, job, patch); err != nil {
			return err
		}
	}
	if apimeta.IsStatusConditionTrue(job.Status.Conditions, ConditionIdleGPU) {
		apimeta.RemoveStatusCondition(&job.Status.Conditions, ConditionIdleGPU)
		return r.Status().Update(ctx, job)
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *GryviaIdleReclaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("gryvia-idle-reclaim").
		For(&gryviav1.GryviaAIJob{}).
		Complete(r)
}
