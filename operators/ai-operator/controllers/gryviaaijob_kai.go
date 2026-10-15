package controllers

// KAI Scheduler integration for GryviaAIJob (opt-in: ai-operator flag --kai-scheduler).
//
// KAI Scheduler (github.com/kai-scheduler/KAI-Scheduler, Apache 2.0) places pods
// whose spec.schedulerName is kai-scheduler and that carry the label kai.scheduler/queue. Its pod grouper makes one
// PodGroup per workload; for a batch Job the annotation kai.scheduler/batch-min-member sets how many pods must be
// placed together. With the integration on, every pod of a job gets the scheduler name and the job's queue
// (spec.queueName, the annotation gryvia.io/queue-name, else --kai-default-queue), a multi-pod batch Job is gang
// scheduled, and a fractional gryvia.io/gpu-share becomes KAI's gpu-fraction (or gpu-memory) pod annotation in place
// of an nvidia.com/gpu limit. The operator's own free-GPU placement check is skipped: capacity is KAI's decision.
//
// No KAI object is read or written and there is no import of KAI's Go module. Needs KAI v0.18 or later: older builds
// give each pod of a batch Job its own PodGroup and ignore batch-min-member. Verified on kind by
// scripts/e2e-kai.sh with a simulated GPU (fake-gpu-operator); never on GPU hardware.

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/ai-operator/pkg/scheduler"
)

const (
	// KAISchedulerName is the schedulerName KAI Scheduler serves.
	KAISchedulerName = "kai-scheduler"
	// LabelKAIQueue names the KAI leaf queue of a pod.
	LabelKAIQueue = "kai.scheduler/queue"
	// AnnotationKAIBatchMinMember is the number of pods of a batch Job KAI places together.
	AnnotationKAIBatchMinMember = "kai.scheduler/batch-min-member"
	// AnnotationKAIGPUFraction and AnnotationKAIGPUMemory request part of one GPU from KAI.
	AnnotationKAIGPUFraction = "gpu-fraction"
	AnnotationKAIGPUMemory   = "gpu-memory"
	// DefaultKAIQueue is the leaf queue a fresh KAI install creates.
	DefaultKAIQueue = "default-queue"
	// LabelKAIQueueNamespace on a namespace names the KAI queue of its jobs that name none.
	LabelKAIQueueNamespace = "gryvia.io/kai-queue"
	// ReasonUnschedulable is the Ready condition reason while KAI reports a pod of the job as unschedulable.
	ReasonUnschedulable = "Unschedulable"

	kaiMessageMax = 400
)

// syncKAIWaiting copies why KAI holds a waiting job's pods (their PodScheduled=False condition) into the job's
// message and Ready condition, so `gryvia status` and the dashboard show it without access to KAI's PodGroups.
func (r *GryviaAIJobReconciler) syncKAIWaiting(ctx context.Context, job *gryviav1.GryviaAIJob) error {
	if job.Status.Phase != PhaseScheduling {
		return nil
	}
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(job.Namespace), client.MatchingLabels{"gryvia.io/job": job.Name}); err != nil {
		return err
	}
	waiting, why := 0, ""
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Spec.SchedulerName != KAISchedulerName || p.Spec.NodeName != "" || p.DeletionTimestamp != nil {
			continue
		}
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
				waiting++
				if why == "" {
					why = kaiReason(c.Message)
				}
			}
		}
	}
	if waiting == 0 {
		return nil
	}
	msg := fmt.Sprintf("KAI Scheduler (queue %s) cannot place %d pod(s)", r.kaiQueue(job), waiting)
	if why != "" {
		msg += ": " + why
	}
	job.Status.Message = msg
	r.updateCondition(job, ConditionReady, metav1.ConditionFalse, ReasonUnschedulable, msg)
	return nil
}

// kaiReason shortens KAI's PodScheduled message: it drops the "Scheduling conditions were not met for pod ..." header
// and joins the remaining lines.
func kaiReason(m string) string {
	var parts []string
	for _, line := range strings.Split(m, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Scheduling conditions were not met") {
			continue
		}
		parts = append(parts, line)
	}
	s := strings.Join(parts, "; ")
	if len(s) > kaiMessageMax {
		s = s[:kaiMessageMax] + "..."
	}
	return s
}

// kaiQueue is the KAI queue of a job: its own, else its namespace's (label gryvia.io/kai-queue, set on tenant
// namespaces by the quota-operator's --kai-integration), else the default queue.
func (r *GryviaAIJobReconciler) kaiQueue(job *gryviav1.GryviaAIJob) string {
	if q := queueFromSpec(job); q != "" {
		return q
	}
	ns := &corev1.Namespace{}
	if r.Client != nil && r.Get(context.TODO(), client.ObjectKey{Name: job.Namespace}, ns) == nil {
		if q := ns.Labels[LabelKAIQueueNamespace]; q != "" {
			return q
		}
	}
	if r.KAIDefaultQueue != "" {
		return r.KAIDefaultQueue
	}
	return DefaultKAIQueue
}

// kaiMinMember is how many pods of a batch Job of size pods KAI must place together: all of them, or an elastic
// job's minNodes.
func kaiMinMember(job *gryviav1.GryviaAIJob, size int32) int32 {
	if min, _, ok := job.Spec.Distributed.ElasticBounds(); ok && min > 0 && min < size {
		return min
	}
	return size
}

// applyKAI hands a pod template to KAI: scheduler name, queue label and, for a fraction of one GPU, KAI's sharing
// annotation instead of the GPU limit. labels may be shared with the workload's selector, so a copy is returned.
func (r *GryviaAIJobReconciler) applyKAI(job *gryviav1.GryviaAIJob, labels, annotations map[string]string, spec *corev1.PodSpec) map[string]string {
	out := make(map[string]string, len(labels)+1)
	for k, v := range labels {
		out[k] = v
	}
	out[LabelKAIQueue] = r.kaiQueue(job)
	spec.SchedulerName = KAISchedulerName
	applyKAIGPUShare(job, annotations, spec)
	return out
}

// applyKAIGPUShare maps gryvia.io/gpu-share below one GPU to KAI's gpu-memory (when gryvia.io/gpu-memory is set)
// or gpu-fraction annotation and drops the container's nvidia.com/gpu request and limit, as KAI requires.
// gryvia.io/gpu-isolation is not used: KAI's install-time GPU-sharing mode decides enforcement.
func applyKAIGPUShare(job *gryviav1.GryviaAIJob, annotations map[string]string, spec *corev1.PodSpec) {
	if job.Annotations[AnnotationGPUShare] == "" || job.Spec.MigProfile != "" {
		return
	}
	req, err := scheduler.ParseGPUShare(job.Annotations, scheduler.GPUsPerPlacedNode(job))
	if err != nil || req.MilliGPU >= scheduler.MilliGPU {
		return
	}
	if req.MemoryMiB > 0 {
		annotations[AnnotationKAIGPUMemory] = strconv.FormatInt(req.MemoryMiB, 10)
	} else {
		annotations[AnnotationKAIGPUFraction] = strconv.FormatFloat(float64(req.MilliGPU)/float64(scheduler.MilliGPU), 'f', -1, 64)
	}
	gpu := corev1.ResourceName("nvidia.com/gpu")
	for i := range spec.Containers {
		delete(spec.Containers[i].Resources.Limits, gpu)
		delete(spec.Containers[i].Resources.Requests, gpu)
	}
}
