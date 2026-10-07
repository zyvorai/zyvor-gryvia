package controllers

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

// validateElastic checks spec.distributed.elastic. kind is the resolved workload kind: elastic
// scaling needs the Indexed batch Job (inference StatefulSets have a fixed replica set).
func validateElastic(job *gryviav1.GryviaAIJob, kind string) error {
	min, max, ok := job.Spec.Distributed.ElasticBounds()
	if !ok {
		return nil
	}
	if kind == gryviav1.WorkloadKindStatefulSet {
		return fmt.Errorf("distributed.elastic needs a run-to-completion Job; a StatefulSet (inference) has a fixed replica count")
	}
	if fw := job.Spec.Distributed.Framework; fw != "" && fw != "pytorch" {
		return fmt.Errorf("distributed.elastic is for PyTorch (torchrun); framework %q is not supported", fw)
	}
	if min < 1 {
		return fmt.Errorf("distributed.elastic.minNodes must be at least 1, got %d", min)
	}
	if max < min {
		return fmt.Errorf("distributed.nodes (%d) must be at least distributed.elastic.minNodes (%d)", max, min)
	}
	if err := validateDesiredNodes(job); err != nil {
		return err
	}
	return nil
}

func validateDesiredNodes(job *gryviav1.GryviaAIJob) error {
	min, max, _ := job.Spec.Distributed.ElasticBounds()
	if d := job.Spec.Distributed.Elastic.DesiredNodes; d != 0 && (d < min || d > max) {
		return fmt.Errorf("distributed.elastic.desiredNodes must be between minNodes (%d) and distributed.nodes (%d), got %d", min, max, d)
	}
	return nil
}

// initialJobSize is the parallelism (and completions) of a new Indexed Job. An elastic job under Kueue
// asks for every worker and lets partial admission lower it.
func initialJobSize(job *gryviav1.GryviaAIJob, nodes int32) int32 {
	if job.Labels[LabelKueueQueue] != "" {
		return nodes
	}
	if n, ok := job.Spec.Distributed.DesiredWorkers(); ok {
		return n
	}
	return nodes
}

// ConditionResized and ConditionResizeBlocked report live resizes of elastic jobs.
const (
	ConditionResized       = "Resized"
	ConditionResizeBlocked = "ResizeBlocked"
)

// reconcileElasticResize keeps a running elastic job's Indexed Job at elastic.desiredNodes. Parallelism and
// completions change together (elastic Indexed Jobs, Kubernetes >= 1.27): the Job controller then starts the
// new indexes or removes the highest ones, and torchrun's rendezvous re-forms the group with the members left.
// Kueue owns the size of a job it manages, so such a job is not resized.
func (r *GryviaAIJobReconciler) reconcileElasticResize(ctx context.Context, job *gryviav1.GryviaAIJob, bj *batchv1.Job) error {
	want, ok := job.Spec.Distributed.DesiredWorkers()
	if !ok || bj.Spec.Parallelism == nil || isTerminalPhase(job.Status.Phase) || jobFinished(bj) {
		return nil
	}
	current := *bj.Spec.Parallelism
	st := job.Status.Elastic
	if st == nil {
		st = &gryviav1.ElasticStatus{}
		job.Status.Elastic = st
	}
	st.CurrentNodes, st.DesiredNodes = current, want
	if want == current {
		return nil
	}
	if bj.Labels[LabelKueueQueue] != "" {
		if job.Spec.Distributed.Elastic.DesiredNodes != 0 {
			r.updateCondition(job, ConditionResizeBlocked, metav1.ConditionTrue, "KueueManaged",
				fmt.Sprintf("Kueue admitted %d workers; desiredNodes %d is not applied to a Kueue-managed job", current, want))
		}
		return nil
	}
	if err := validateDesiredNodes(job); err != nil {
		r.updateCondition(job, ConditionResizeBlocked, metav1.ConditionTrue, "OutOfBounds", err.Error())
		return nil
	}
	n := want
	bj.Spec.Parallelism, bj.Spec.Completions = &n, &n
	if err := r.Update(ctx, bj); err != nil {
		return err
	}
	now := metav1.Now()
	st.CurrentNodes, st.Resizes, st.LastResizeTime = want, st.Resizes+1, &now
	reason := "Grown"
	if want < current {
		reason = "Shrunk"
	}
	msg := fmt.Sprintf("%d -> %d workers", current, want)
	r.updateCondition(job, ConditionResized, metav1.ConditionTrue, reason, msg)
	r.updateCondition(job, ConditionResizeBlocked, metav1.ConditionFalse, reason, msg)
	if r.Recorder != nil {
		r.Recorder.Eventf(job, corev1.EventTypeNormal, "Resized", "elastic job resized: %s", msg)
	}
	return nil
}

func jobFinished(bj *batchv1.Job) bool {
	for _, c := range bj.Status.Conditions {
		if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed || c.Type == batchv1.JobSuccessCriteriaMet) &&
			c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// elasticSuccessPolicy lets an elastic Job finish once minNodes indexes succeeded: workers that never
// got scheduled (capacity lost) would otherwise keep the Job from completing. nil when not elastic or
// when min == max. Needs a Kubernetes version with batch/v1 Job successPolicy (beta in 1.31, GA 1.33).
func elasticSuccessPolicy(job *gryviav1.GryviaAIJob) *batchv1.SuccessPolicy {
	min, max, ok := job.Spec.Distributed.ElasticBounds()
	if !ok || min >= max {
		return nil
	}
	return &batchv1.SuccessPolicy{Rules: []batchv1.SuccessPolicyRule{{SucceededCount: &min}}}
}

func hasEnv(env []corev1.EnvVar, name string) bool {
	for _, e := range env {
		if e.Name == name {
			return true
		}
	}
	return false
}

// replaceEnv sets name to value, replacing an existing entry or appending.
func replaceEnv(env []corev1.EnvVar, name, value string) []corev1.EnvVar {
	for i := range env {
		if env[i].Name == name {
			env[i].Value, env[i].ValueFrom = value, nil
			return env
		}
	}
	return append(env, corev1.EnvVar{Name: name, Value: value})
}
