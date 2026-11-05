// Volcano scheduling backend (opt-in: --volcano-scheduler).
//
// Sets schedulerName=volcano and scheduling.volcano.sh/queue-name from the job queue.
// Full Volcano PodGroup / vcjob conversion is intentionally out of scope for this wave:
// install Volcano and use it as the pod scheduler; Gryvia keeps job lifecycle and metering.
// See docs/volcano.md. Mutually exclusive with KAI, Kueue and the native queue gate.
package controllers

import (
	corev1 "k8s.io/api/core/v1"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

const (
	// VolcanoSchedulerName is the default schedulerName Volcano registers.
	VolcanoSchedulerName = "volcano"
	// AnnotationVolcanoQueue is Volcano's queue annotation on pods.
	AnnotationVolcanoQueue = "scheduling.volcano.sh/queue-name"
	// DefaultVolcanoQueue when the job names no queue.
	DefaultVolcanoQueue = "default"
)

// applyVolcano hands a pod template to Volcano: scheduler name and queue annotation.
func (r *GryviaAIJobReconciler) applyVolcano(job *gryviav1.GryviaAIJob, annotations map[string]string, spec *corev1.PodSpec) {
	spec.SchedulerName = VolcanoSchedulerName
	q := queueFromSpec(job)
	if q == "" {
		if r.VolcanoDefaultQueue != "" {
			q = r.VolcanoDefaultQueue
		} else {
			q = DefaultVolcanoQueue
		}
	}
	annotations[AnnotationVolcanoQueue] = q
}
