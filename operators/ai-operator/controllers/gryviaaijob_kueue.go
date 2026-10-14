package controllers

// Kueue integration for GryviaAIJob (opt-in: ai-operator flag --kueue-integration).
//
// With the integration on, the batch Job of a job that has a queue is created SUSPENDED and
// labelled kueue.x-k8s.io/queue-name (plus kueue.x-k8s.io/priority-class). Kueue reserves quota for
// ALL pods of the Job at once (gang admission) and only then unsuspends it. From that moment
// Kueue owns spec.suspend; this controller never touches it again.
//
// Kueue objects are read and written through the unstructured client only: there is no import of the
// Kueue Go module. The API version is v1beta1 (served by every Kueue release from 0.6 to at least
// 0.19; see docs/kueue-integration.md). Nothing here has been run against a real Kueue outside the
// kind workflow .github/workflows/e2e-kueue.yml; the unit tests use a fake client.

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	batchv1 "k8s.io/api/batch/v1"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

//+kubebuilder:rbac:groups=kueue.x-k8s.io,resources=workloads;localqueues;clusterqueues,verbs=get;list;watch
//+kubebuilder:rbac:groups=kueue.x-k8s.io,resources=workloadpriorityclasses,verbs=get;create

const (
	// AnnotationQueueName is the annotation alternative to spec.queueName.
	AnnotationQueueName = "gryvia.io/queue-name"
	// LabelKueuePriorityClass names the WorkloadPriorityClass of a Job's Workload.
	LabelKueuePriorityClass = "kueue.x-k8s.io/priority-class"
	// ConditionKueue is set on a GryviaAIJob while Kueue manages its admission: False while it
	// waits (Queued), True once Kueue admitted it. Its presence marks a Queued phase as
	// "waiting for Kueue" (the reconciler keeps working) as opposed to "held by the quota operator".
	ConditionKueue = "KueueAdmitted"

	// DefaultKueueQueue is the LocalQueue the quota operator creates in every tenant namespace.
	DefaultKueueQueue = "gryvia"
	// tenantNamespacePrefix is the namespace prefix of GryviaTenant namespaces.
	tenantNamespacePrefix = "tenant-"

	// PriorityClassPrefix prefixes the WorkloadPriorityClasses this operator manages.
	PriorityClassPrefix = "gryvia-priority-"

	// AnnotationKueueMinParallelism lets Kueue admit a Job with fewer pods (partial admission, Kueue >= 0.5).
	AnnotationKueueMinParallelism = "kueue.x-k8s.io/job-min-parallelism"
	// AnnotationKueueCompletionsEqualParallelism makes Kueue lower completions with parallelism (Indexed Jobs).
	AnnotationKueueCompletionsEqualParallelism = "kueue.x-k8s.io/job-completions-equal-parallelism"

	kueueGroup   = "kueue.x-k8s.io"
	kueueVersion = "v1beta1"
)

// KueueGVK returns the group/version/kind of a Kueue kind.
func KueueGVK(kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: kueueGroup, Version: kueueVersion, Kind: kind}
}

// PriorityBucket maps spec.priority (0-100) to its bucket: rounded down to a multiple of 10.
func PriorityBucket(p int32) int32 {
	if p < 0 {
		p = 0
	}
	if p > 100 {
		p = 100
	}
	return p / 10 * 10
}

// PriorityClassName is the WorkloadPriorityClass for a spec.priority ("gryvia-priority-<bucket>").
func PriorityClassName(p int32) string {
	return fmt.Sprintf("%s%d", PriorityClassPrefix, PriorityBucket(p))
}

// queueFromSpec returns the queue the user asked for: spec.queueName, the annotation
// gryvia.io/queue-name, or the label kueue.x-k8s.io/queue-name, in that order.
func queueFromSpec(job *gryviav1.GryviaAIJob) string {
	if job.Spec.QueueName != "" {
		return job.Spec.QueueName
	}
	if q := job.Annotations[AnnotationQueueName]; q != "" {
		return q
	}
	return job.Labels[LabelKueueQueue]
}

// localQueueExists reports whether the LocalQueue exists. A missing Kueue (no CRD) counts as absent.
func (r *GryviaAIJobReconciler) localQueueExists(ctx context.Context, namespace, name string) (bool, error) {
	lq := &unstructured.Unstructured{}
	lq.SetGroupVersionKind(KueueGVK("LocalQueue"))
	err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, lq)
	switch {
	case err == nil:
		return true, nil
	case errors.IsNotFound(err) || meta.IsNoMatchError(err):
		return false, nil
	}
	return false, err
}

// resolveKueueQueue decides the queue of a job that is about to get its batch Job. It returns
// the queue ("" = not managed by Kueue) and a note explaining why a job ended up without one.
//
// An explicitly requested queue is trusted and not checked: if the LocalQueue is missing the Job
// stays Queued and Kueue's message says so, instead of silently running outside the quota. The
// default queue is only used when the LocalQueue exists, so jobs in namespaces without Kueue
// objects keep running exactly as before.
func (r *GryviaAIJobReconciler) resolveKueueQueue(ctx context.Context, job *gryviav1.GryviaAIJob) (queue, note string, err error) {
	if !r.KueueIntegration {
		return "", "", nil
	}
	if q := queueFromSpec(job); q != "" {
		return q, "", nil
	}
	if !strings.HasPrefix(job.Namespace, tenantNamespacePrefix) {
		return "", "", nil
	}
	def := r.KueueDefaultQueue
	if def == "" {
		def = DefaultKueueQueue
	}
	if r.KueueStrictAdmission {
		// A missing CRD, queue or controller must leave the Job suspended, never bypass admission.
		return def, "", nil
	}
	ok, err := r.localQueueExists(ctx, job.Namespace, def)
	if err != nil {
		return "", "", err
	}
	if !ok {
		return "", fmt.Sprintf("LocalQueue %q not found in namespace %s: running without Kueue admission", def, job.Namespace), nil
	}
	return def, "", nil
}

// ensurePriorityClass creates the WorkloadPriorityClass for a bucket if it does not exist. An
// existing one is never modified (Kueue treats its value as immutable).
func (r *GryviaAIJobReconciler) ensurePriorityClass(ctx context.Context, bucket int32) error {
	name := fmt.Sprintf("%s%d", PriorityClassPrefix, bucket)
	if _, ok := r.priorityClasses.Load(name); ok {
		return nil
	}
	wpc := &unstructured.Unstructured{}
	wpc.SetGroupVersionKind(KueueGVK("WorkloadPriorityClass"))
	err := r.Get(ctx, types.NamespacedName{Name: name}, wpc)
	if errors.IsNotFound(err) {
		wpc = &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": kueueGroup + "/" + kueueVersion,
			"kind":       "WorkloadPriorityClass",
			"metadata": map[string]interface{}{
				"name":   name,
				"labels": map[string]interface{}{"app.kubernetes.io/managed-by": "gryvia-ai-operator"},
			},
			"value":       int64(bucket),
			"description": fmt.Sprintf("GryviaAIJob spec.priority %d-%d", bucket, bucket+9),
		}}
		err = r.Create(ctx, wpc)
		if errors.IsAlreadyExists(err) {
			err = nil
		}
	}
	if err != nil {
		return err
	}
	r.priorityClasses.Store(name, true)
	return nil
}

// applyKueueToJob turns a freshly built batch Job into a Kueue-managed one: queue label, priority
// class label and suspend=true. Only called when the Job is created; the pod template and labels
// of an existing Job are never rewritten.
func (r *GryviaAIJobReconciler) applyKueueToJob(ctx context.Context, job *gryviav1.GryviaAIJob, bj *batchv1.Job) error {
	queue, note, err := r.resolveKueueQueue(ctx, job)
	if err != nil {
		return err
	}
	if queue == "" {
		if note != "" {
			r.Log.Info(note, "gryviaaijob", job.Name)
			r.updateCondition(job, ConditionKueue, metav1.ConditionFalse, "LocalQueueNotFound", note)
		}
		return nil
	}
	if bj.Labels == nil {
		bj.Labels = map[string]string{}
	}
	bj.Labels[LabelKueueQueue] = queue
	suspend := true // Kueue admits (unsuspends) it
	bj.Spec.Suspend = &suspend
	// An elastic job may start with anything from minNodes to nodes workers, so Kueue may admit it with
	// fewer pods when the quota is short. Completions follow parallelism (the success policy's
	// succeededCount = minNodes stays valid), and Kueue restores both if it evicts the Job.
	if min, max, ok := job.Spec.Distributed.ElasticBounds(); ok && min < max {
		if bj.Annotations == nil {
			bj.Annotations = map[string]string{}
		}
		bj.Annotations[AnnotationKueueMinParallelism] = fmt.Sprintf("%d", min)
		bj.Annotations[AnnotationKueueCompletionsEqualParallelism] = "true"
	}
	if bucket := PriorityBucket(job.Spec.Priority); bucket > 0 {
		if err := r.ensurePriorityClass(ctx, bucket); err != nil {
			// Without the class the job would still queue, just at the default priority.
			r.Log.Error(err, "cannot ensure WorkloadPriorityClass, queueing with the default priority", "bucket", bucket)
		} else {
			bj.Labels[LabelKueuePriorityClass] = PriorityClassName(job.Spec.Priority)
		}
	}
	return nil
}

// isKueueQueued reports a Queued phase that this controller set on behalf of Kueue (as opposed
// to Queued held by the quota operator, which creates nothing).
func isKueueQueued(job *gryviav1.GryviaAIJob) bool {
	if job.Status.Phase != PhaseQueued {
		return false
	}
	for _, c := range job.Status.Conditions {
		if c.Type == ConditionKueue {
			return true
		}
	}
	return false
}

// workloadState is what a Kueue Workload says about admission.
type workloadState struct {
	Found         bool
	Admitted      bool
	QuotaReserved bool
	Evicted       bool
	Finished      bool
	EvictedReason string
	EvictedMsg    string
	PendingMsg    string // why it is not admitted: QuotaReserved=False (or Admitted=False) message
}

// interpretWorkload reads status.conditions of a Workload (Admitted, QuotaReserved, Evicted, Finished).
func interpretWorkload(wl *unstructured.Unstructured) workloadState {
	st := workloadState{}
	if wl == nil {
		return st
	}
	st.Found = true
	conds, _, _ := unstructured.NestedSlice(wl.Object, "status", "conditions")
	for _, raw := range conds {
		c, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		typ, _ := c["type"].(string)
		status, _ := c["status"].(string)
		reason, _ := c["reason"].(string)
		msg, _ := c["message"].(string)
		isTrue := status == string(metav1.ConditionTrue)
		switch typ {
		case "Admitted":
			st.Admitted = isTrue
			if !isTrue && msg != "" && st.PendingMsg == "" {
				st.PendingMsg = msg
			}
		case "QuotaReserved":
			st.QuotaReserved = isTrue
			if !isTrue && msg != "" {
				st.PendingMsg = msg // the most specific pending reason wins over Admitted's
			}
		case "Evicted":
			st.Evicted = isTrue
			if isTrue {
				st.EvictedReason, st.EvictedMsg = reason, msg
			}
		case "Finished":
			st.Finished = isTrue
		}
	}
	return st
}

// queuedMessage is the status.message of a job waiting for Kueue.
func queuedMessage(st workloadState) (reason, msg string) {
	switch {
	case !st.Found:
		return "WaitingForWorkload", "Waiting for Kueue to create the Workload (is Kueue installed and running? kubectl get workloads)"
	case st.Evicted:
		m := fmt.Sprintf("Evicted by Kueue (%s), requeued: %s", st.EvictedReason, st.EvictedMsg)
		if st.PendingMsg != "" {
			m += " | " + st.PendingMsg
		}
		return "Evicted", m
	case st.QuotaReserved && !st.Admitted:
		return "AdmissionChecksPending", "Quota reserved, waiting for admission checks"
	case st.PendingMsg != "":
		return "Pending", "Queued by Kueue: " + st.PendingMsg
	}
	return "Pending", "Queued by Kueue: waiting for quota in the queue"
}

// workloadFor finds the Kueue Workload owned by the batch Job (named job-<name>-<hash> by Kueue).
func (r *GryviaAIJobReconciler) workloadFor(ctx context.Context, bj *batchv1.Job) (*unstructured.Unstructured, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(KueueGVK("WorkloadList"))
	if err := r.List(ctx, list, client.InNamespace(bj.Namespace)); err != nil {
		if meta.IsNoMatchError(err) {
			return nil, nil
		}
		return nil, err
	}
	for i := range list.Items {
		for _, o := range list.Items[i].GetOwnerReferences() {
			if o.UID == bj.UID && o.Kind == "Job" {
				return &list.Items[i], nil
			}
		}
	}
	return nil, nil
}

// applyKueueStatus overlays Kueue's admission state on the status derived from the batch Job:
//
//   - Job suspended (Kueue has not admitted it, or evicted it again): phase Queued with Kueue's
//     reason as message. This also applies to a job that already ran: a Kueue eviction (preemption
//     by a higher-priority workload, waitForPodsReady timeout) is mapped back to Queued, NOT to
//     the terminal Preempted phase, because Kueue requeues the workload and unsuspends the Job
//     again when it fits. The PVC and the Job (with its completed indexes' pods gone) are kept.
//   - Job not suspended: Kueue admitted it; the phase comes from the Job (Scheduling, Running).
//
// A Job that finished is left to summarizeJob. Only Jobs created with the queue label are touched.
func (r *GryviaAIJobReconciler) applyKueueStatus(ctx context.Context, job *gryviav1.GryviaAIJob, bj *batchv1.Job) {
	if !r.KueueIntegration || bj.Labels[LabelKueueQueue] == "" {
		return
	}
	out := summarizeJob(bj, r.getReplicaCount(job))
	if out.Phase == PhaseSucceeded || out.Phase == PhaseFailed {
		return
	}
	if !out.Suspended {
		job.Status.GpusAllocated = job.Spec.GPUs
		job.Status.QueueExplanation = nil
		r.updateCondition(job, ConditionKueue, metav1.ConditionTrue, "Admitted", "Admitted by Kueue")
		return
	}
	wl, err := r.workloadFor(ctx, bj)
	if err != nil {
		r.Log.Error(err, "cannot read the Kueue Workload", "gryviaaijob", job.Name)
		// Keep what we know: a suspended Job is at least waiting.
	}
	st := interpretWorkload(wl)
	reason, msg := queuedMessage(st)
	if conditionTrue(job, ConditionKueue) && !st.Evicted {
		// It was admitted and is suspended again: Kueue evicted it (preemption, waitForPodsReady
		// timeout, ...) even if the Workload's Evicted condition is not visible (any more).
		reason, msg = "Evicted", "Evicted by Kueue after admission, requeued"
		if st.PendingMsg != "" {
			msg += ": " + st.PendingMsg
		}
	}
	job.Status.Phase = PhaseQueued
	job.Status.GpusAllocated = 0
	job.Status.Message = msg
	job.Status.QueueExplanation = r.explainQueue(ctx, job, bj, st, reason, msg)
	r.updateCondition(job, ConditionKueue, metav1.ConditionFalse, reason, msg)
	r.updateCondition(job, ConditionReady, metav1.ConditionFalse, "Queued", msg)
}
