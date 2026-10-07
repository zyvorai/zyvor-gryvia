package controllers

import (
	"context"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

const (
	// AnnotationCancel on a GryviaAIJob ("true") cancels it: phase Cancelled, workload deleted, PVC kept.
	AnnotationCancel = "gryvia.io/cancel"
	// AnnotationEnvVersion marks the pod template of StatefulSets built with the v2 environment.
	AnnotationEnvVersion = "gryvia.io/env-version"
	// LabelKueueQueue is copied from the GryviaAIJob to its batch Job (Kueue integration seam).
	LabelKueueQueue = "kueue.x-k8s.io/queue-name"

	// envVersionCurrent is the environment layout of new workloads (RANK, NODE_RANK,
	// namespace-qualified MASTER_ADDR). Version 1 is the layout of StatefulSets created
	// before it existed and is kept for them so an upgrade never restarts running pods.
	envVersionCurrent = 2
)

func headlessServiceName(job *gryviav1.GryviaAIJob) string {
	return fmt.Sprintf("%s-headless", job.Name)
}

// isTerminalPhase reports phases that never change again.
func isTerminalPhase(phase string) bool {
	switch phase {
	case PhaseSucceeded, PhaseFailed, PhaseCancelled, PhaseRejected:
		return true
	}
	return false
}

func conditionTrue(job *gryviav1.GryviaAIJob, condType string) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == condType {
			return c.Status == metav1.ConditionTrue
		}
	}
	return false
}

func statefulSetEnvVersion(sts *appsv1.StatefulSet) int {
	if sts.Spec.Template.Annotations[AnnotationEnvVersion] == "2" {
		return 2
	}
	return 1
}

// resolveWorkloadKind decides which workload backs the job. An existing workload always
// wins (workloads are never migrated in place); then spec.workloadKind; then the default
// by type (inference: statefulset, everything else: job).
func (r *GryviaAIJobReconciler) resolveWorkloadKind(ctx context.Context, job *gryviav1.GryviaAIJob) (string, error) {
	sts := &appsv1.StatefulSet{}
	err := r.Get(ctx, types.NamespacedName{Namespace: job.Namespace, Name: r.getStatefulSetName(job)}, sts)
	if err == nil && metav1.IsControlledBy(sts, job) {
		return gryviav1.WorkloadKindStatefulSet, nil
	}
	if err != nil && !errors.IsNotFound(err) {
		return "", err
	}
	bj := &batchv1.Job{}
	err = r.Get(ctx, types.NamespacedName{Namespace: job.Namespace, Name: job.Name}, bj)
	if err == nil && metav1.IsControlledBy(bj, job) {
		return gryviav1.WorkloadKindJob, nil
	}
	if err != nil && !errors.IsNotFound(err) {
		return "", err
	}
	switch job.Spec.WorkloadKind {
	case gryviav1.WorkloadKindJob, gryviav1.WorkloadKindStatefulSet:
		return job.Spec.WorkloadKind, nil
	}
	if job.Spec.Type == "inference" {
		return gryviav1.WorkloadKindStatefulSet, nil
	}
	return gryviav1.WorkloadKindJob, nil
}

// buildJob builds the Indexed batch/v1 Job for a run-to-completion GryviaAIJob.
//
// restartPolicy is Never: a failed pod is replaced by a NEW pod with the same completion
// index (same hostname <job>-<index>, same DNS name, same RANK), and every failure is
// counted against backoffLimit (= spec.retryLimit), so retries are exact and the failed
// pod's logs stay available. OnFailure would restart the container inside the same pod
// (keeping rank identity too) but hides the failure count, cannot reschedule to another
// node and is incompatible with podFailurePolicy. Neither restarts the other ranks: this
// is not elastic training.
func (r *GryviaAIJobReconciler) buildJob(job *gryviav1.GryviaAIJob) (*batchv1.Job, error) {
	if err := validateRecoveryOptions(job); err != nil {
		return nil, err
	}
	labels := map[string]string{
		"gryvia.io/job":  job.Name,
		"gryvia.io/type": job.Spec.Type,
	}
	nodes := r.getReplicaCount(job)
	tpl := r.buildPodTemplate(job, labels, r.getGPUsPerPod(job), gryviav1.WorkloadKindJob, envVersionCurrent)
	if key, val, _ := topologyAnnotation(job); val != "" {
		if tpl.Annotations == nil {
			tpl.Annotations = map[string]string{}
		}
		tpl.Annotations[key] = val
	}
	if pc := job.Annotations["gryvia.io/priority-class"]; pc != "" {
		tpl.Spec.PriorityClassName = pc
	}
	tpl.Spec.RestartPolicy = corev1.RestartPolicyNever
	tpl.Spec.Subdomain = headlessServiceName(job) // pod DNS: <job>-<index>.<svc>.<ns>.svc

	deadline, err := job.Spec.TimeoutSeconds()
	if err != nil {
		return nil, err
	}
	backoff := job.Spec.RetryLimit
	if backoff < 0 {
		backoff = 0
	}
	mode := batchv1.IndexedCompletion
	suspend := job.Spec.Suspend
	jobLabels := map[string]string{"gryvia.io/job": job.Name, "gryvia.io/type": job.Spec.Type}
	if q := job.Labels[LabelKueueQueue]; q != "" {
		jobLabels[LabelKueueQueue] = q
		suspend = true // Kueue admits (unsuspends) it
	}

	size := initialJobSize(job, nodes)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: job.Name, Namespace: job.Namespace, Labels: jobLabels},
		Spec: batchv1.JobSpec{
			Parallelism:           &size,
			Completions:           &size,
			CompletionMode:        &mode,
			BackoffLimit:          &backoff,
			ActiveDeadlineSeconds: deadline,
			Suspend:               &suspend,
			SuccessPolicy:         elasticSuccessPolicy(job),
			// Evictions, drains and preemptions (DisruptionTarget) are not the job's fault:
			// they replace the pod without consuming backoffLimit. Needs Kubernetes >= 1.26
			// (podFailurePolicy is unverified on older clusters).
			PodFailurePolicy: &batchv1.PodFailurePolicy{Rules: []batchv1.PodFailurePolicyRule{{
				Action: batchv1.PodFailurePolicyActionIgnore,
				OnPodConditions: []batchv1.PodFailurePolicyOnPodConditionsPattern{
					{Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue},
				},
			}}},
			Template: tpl,
		},
	}, nil
}

// reconcileBatchJob creates the Job if needed, keeps spec.suspend and an elastic job's size in
// sync (the rest of a Job's spec is immutable, so other spec edits do not reach a created Job) and
// derives the GryviaAIJob status from the Job.
func (r *GryviaAIJobReconciler) reconcileBatchJob(ctx context.Context, job *gryviav1.GryviaAIJob) error {
	bj := &batchv1.Job{}
	key := types.NamespacedName{Namespace: job.Namespace, Name: job.Name}
	err := r.Get(ctx, key, bj)
	if errors.IsNotFound(err) {
		bj, err = r.buildJob(job)
		if err != nil {
			return err
		}
		if err := controllerutil.SetControllerReference(job, bj, r.Scheme); err != nil {
			return err
		}
		if err := r.applyKueueToJob(ctx, job, bj); err != nil {
			return err
		}
		if err := r.startCheckpointGuard(ctx, job, &bj.Spec.Template.Spec); err != nil {
			return err
		}
		if err := r.Create(ctx, bj); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else {
		if !metav1.IsControlledBy(bj, job) {
			return fmt.Errorf("batch Job %s/%s already exists and is not owned by this GryviaAIJob", bj.Namespace, bj.Name)
		}
		// With a Kueue queue label on the Job, Kueue owns spec.suspend after creation.
		if bj.Labels[LabelKueueQueue] == "" && (bj.Spec.Suspend == nil || *bj.Spec.Suspend != job.Spec.Suspend) {
			want := job.Spec.Suspend
			bj.Spec.Suspend = &want
			if err := r.Update(ctx, bj); err != nil {
				return err
			}
		}
		if err := r.reconcileElasticResize(ctx, job, bj); err != nil {
			return err
		}
	}
	r.applyJobStatus(job, bj)
	r.applyKueueStatus(ctx, job, bj)
	if job.Status.Checkpoint != nil {
		r.syncCheckpointStatus(ctx, job)
		if err := r.recoverFromNodeLoss(ctx, job, time.Now()); err != nil {
			return err
		}
	}
	return nil
}

// jobOutcome is what a batch Job says about the GryviaAIJob.
type jobOutcome struct {
	Phase     string // Scheduling, Running, Succeeded or Failed
	Ready     int32
	Retries   int32
	Message   string
	Suspended bool
	Start     *metav1.Time
	End       *metav1.Time
}

// summarizeJob maps a batch Job to a phase. Terminal conditions win over counts:
// Complete=True -> Succeeded, Failed=True -> Failed (BackoffLimitExceeded, DeadlineExceeded,
// PodFailurePolicy, ...). Otherwise Running once a pod is ready or any index succeeded,
// else Scheduling (pods not ready yet, or the Job suspended).
func summarizeJob(bj *batchv1.Job, expected int32) jobOutcome {
	out := jobOutcome{Phase: PhaseScheduling, Retries: bj.Status.Failed, Start: bj.Status.StartTime}
	if bj.Status.Ready != nil {
		out.Ready = *bj.Status.Ready
	}
	for _, c := range bj.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			out.Phase = PhaseSucceeded
			out.End = bj.Status.CompletionTime
			if out.End == nil {
				t := c.LastTransitionTime
				out.End = &t
			}
			return out
		case batchv1.JobFailed:
			out.Phase = PhaseFailed
			t := c.LastTransitionTime
			out.End = &t
			out.Message = strings.TrimSpace(fmt.Sprintf("%s: %s (%d/%d pod(s) failed)", c.Reason, c.Message, bj.Status.Failed, expected))
			return out
		case batchv1.JobSuspended:
			out.Suspended = true
		}
	}
	if bj.Spec.Suspend != nil && *bj.Spec.Suspend {
		out.Suspended = true
	}
	if out.Suspended {
		out.Message = "Job is suspended (waiting to be admitted)"
		return out
	}
	if out.Ready > 0 || bj.Status.Succeeded > 0 {
		out.Phase = PhaseRunning
	}
	return out
}

func (r *GryviaAIJobReconciler) applyJobStatus(job *gryviav1.GryviaAIJob, bj *batchv1.Job) {
	expected := r.getReplicaCount(job)
	if bj.Spec.Parallelism != nil && *bj.Spec.Parallelism < expected {
		expected = *bj.Spec.Parallelism // Kueue admitted an elastic job with fewer workers
	}
	out := summarizeJob(bj, expected)
	job.Status.ReplicasReady = out.Ready
	job.Status.Retries = out.Retries
	now := metav1.Now()

	switch out.Phase {
	case PhaseSucceeded, PhaseFailed:
		job.Status.Phase = out.Phase
		job.Status.Message = out.Message
		if job.Status.StartTime == nil {
			job.Status.StartTime = firstTime(out.Start, &now)
		}
		if job.Status.CompletionTime == nil {
			job.Status.CompletionTime = firstTime(out.End, &now)
		}
		if out.Phase == PhaseSucceeded {
			r.updateCondition(job, ConditionReady, metav1.ConditionFalse, "Completed", "All indexes completed")
		} else {
			r.updateCondition(job, ConditionReady, metav1.ConditionFalse, "Failed", out.Message)
		}
	case PhaseRunning:
		job.Status.Phase = PhaseRunning
		job.Status.Message = ""
		if job.Status.StartTime == nil {
			job.Status.StartTime = firstTime(out.Start, &now)
		}
		if out.Ready == expected {
			r.updateCondition(job, ConditionReady, metav1.ConditionTrue, "Ready", "All replicas are ready")
		} else {
			r.updateCondition(job, ConditionReady, metav1.ConditionFalse, "PartiallyReady",
				fmt.Sprintf("%d/%d replicas are ready", out.Ready, expected))
		}
	default: // waiting: never move a job that already ran back to an earlier phase
		if job.Status.Phase != PhaseRunning {
			job.Status.Phase = PhaseScheduling
		}
		job.Status.Message = out.Message
		reason := "PodsNotReady"
		if out.Suspended {
			reason = "Suspended"
		}
		r.updateCondition(job, ConditionReady, metav1.ConditionFalse, reason,
			fmt.Sprintf("%d/%d replicas are ready", out.Ready, expected))
	}
}

func firstTime(a *metav1.Time, fallback *metav1.Time) *metav1.Time {
	if a != nil && !a.IsZero() {
		t := *a
		return &t
	}
	t := *fallback
	return &t
}

// cancelJob handles the cancel annotation.
func (r *GryviaAIJobReconciler) cancelJob(ctx context.Context, job *gryviav1.GryviaAIJob) (ctrl.Result, error) {
	if err := r.finishJob(ctx, job, PhaseCancelled, "Cancelled", "Cancelled by request (annotation "+AnnotationCancel+")"); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, r.teardown(ctx, job, false)
}

// failJob marks the job Failed for a reason that is not a workload failure (invalid spec).
func (r *GryviaAIJobReconciler) failJob(ctx context.Context, job *gryviav1.GryviaAIJob, reason, msg string) error {
	return r.finishJob(ctx, job, PhaseFailed, reason, msg)
}

func (r *GryviaAIJobReconciler) finishJob(ctx context.Context, job *gryviav1.GryviaAIJob, phase, reason, msg string) error {
	now := metav1.Now()
	job.Status.Phase = phase
	job.Status.Message = msg
	if job.Status.CompletionTime == nil {
		job.Status.CompletionTime = &now
	}
	r.updateCondition(job, ConditionReady, metav1.ConditionFalse, reason, msg)
	return r.Status().Update(ctx, job)
}

// teardown deletes the workload (Job and/or StatefulSet) the controller created; the PVC
// is kept. With removeAux the headless Service and the PVC go too (a Rejected job never
// ran, so nothing of it is worth keeping). Objects not controlled by this job are left alone.
func (r *GryviaAIJobReconciler) teardown(ctx context.Context, job *gryviav1.GryviaAIJob, removeAux bool) error {
	targets := []client.Object{
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: job.Namespace, Name: job.Name}},
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Namespace: job.Namespace, Name: r.getStatefulSetName(job)}},
	}
	if removeAux {
		targets = append(targets,
			&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: job.Namespace, Name: headlessServiceName(job)}},
			&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: job.Namespace, Name: fmt.Sprintf("%s-data", job.Name)}},
		)
	}
	for _, obj := range targets {
		if err := r.deleteIfOwned(ctx, job, obj); err != nil {
			return err
		}
	}
	return nil
}

func (r *GryviaAIJobReconciler) deleteIfOwned(ctx context.Context, job *gryviav1.GryviaAIJob, obj client.Object) error {
	if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(obj, job) || !obj.GetDeletionTimestamp().IsZero() {
		return nil
	}
	// Background propagation: the API default for batch/v1 Jobs is orphaning, which would leak the pods.
	return client.IgnoreNotFound(r.Delete(ctx, obj, client.PropagationPolicy(metav1.DeletePropagationBackground)))
}
