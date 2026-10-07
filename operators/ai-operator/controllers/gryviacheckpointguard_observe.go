package controllers

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

func (r *GryviaCheckpointGuardReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// observeCheckpoints fills status.jobs from each matched job's status.checkpoint, counts newly committed steps
// and sets CheckpointValid: False when a running guarded job has not committed within two intervals.
func (r *GryviaCheckpointGuardReconciler) observeCheckpoints(guard *gryviav1.GryviaCheckpointGuard, jobs []gryviav1.GryviaAIJob) {
	prev := map[string]int64{}
	for _, j := range guard.Status.Jobs {
		prev[j.Name] = j.LastCommittedStep
	}
	interval := time.Duration(guard.Spec.CheckpointPolicy.IntervalMinutes) * time.Minute
	if interval <= 0 {
		interval = 30 * time.Minute
	}
	now := r.now()
	var out []gryviav1.GuardedJob
	var stale, notInjected []string
	for i := range jobs {
		job := &jobs[i]
		g := gryviav1.GuardedJob{Name: job.Name}
		cp := job.Status.Checkpoint
		switch {
		case cp != nil && cp.Guard == guard.Name:
			g.Injected = true
			g.LastCommittedStep, g.LastCommittedAt, g.NodeLossRecoveries = cp.LastCommittedStep, cp.LastCommittedAt, cp.NodeLossRecoveries
			if p, seen := prev[job.Name]; cp.LastCommittedAt != nil && (!seen || cp.LastCommittedStep > p) {
				guard.Status.TotalCheckpoints++
				guard.Status.ValidCheckpoints++ // a reported step is one the commit protocol completed
				if guard.Status.LastCheckpointTime == nil || cp.LastCommittedAt.After(guard.Status.LastCheckpointTime.Time) {
					t := *cp.LastCommittedAt
					guard.Status.LastCheckpointTime = &t
					guard.Status.LastValidCheckpoint = fmt.Sprintf("%s/step-%d", job.Name, cp.LastCommittedStep)
				}
			}
			if job.Status.Phase == PhaseRunning {
				since := job.Status.StartTime
				if cp.LastCommittedAt != nil {
					since = cp.LastCommittedAt
				}
				if since != nil && now.Sub(since.Time) > 2*interval {
					stale = append(stale, job.Name)
				}
			}
		case cp != nil:
			g.Message = "managed by guard " + cp.Guard
		case !r.Enabled:
			g.Message = "checkpoint injection is off (ai-operator --checkpoint-guard)"
			notInjected = append(notInjected, job.Name)
		case checkpointIneligible(job) != "":
			g.Message = checkpointIneligible(job)
			notInjected = append(notInjected, job.Name)
		default:
			g.Message = "created before this guard or without it; only new jobs get the checkpoint environment"
			notInjected = append(notInjected, job.Name)
		}
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	guard.Status.Jobs = out
	switch {
	case len(stale) > 0:
		r.updateGuardCondition(guard, ConditionCheckpointValid, metav1.ConditionFalse, "Stale",
			fmt.Sprintf("no committed checkpoint within %s: %s", 2*interval, strings.Join(stale, ", ")))
	case guard.Status.LastCheckpointTime != nil:
		r.updateGuardCondition(guard, ConditionCheckpointValid, metav1.ConditionTrue, "Committed",
			"last committed "+guard.Status.LastValidCheckpoint)
	case len(notInjected) > 0:
		r.updateGuardCondition(guard, ConditionCheckpointValid, metav1.ConditionFalse, "NotInjected",
			"jobs without the checkpoint environment: "+strings.Join(notInjected, ", "))
	}
}

// requestCheckpoints writes requestedAt/requestReason into the status ConfigMap of each running guarded job
// that has no open request; the trainer's checkpoint_status hook polls it and commits at its next step.
// Returns the number of requests written.
func (r *GryviaCheckpointGuardReconciler) requestCheckpoints(ctx context.Context, guard *gryviav1.GryviaCheckpointGuard, jobs []gryviav1.GryviaAIJob, reason string) int {
	n := 0
	now := r.now().UTC()
	for i := range jobs {
		job := &jobs[i]
		cp := job.Status.Checkpoint
		if job.Status.Phase != PhaseRunning || cp == nil || cp.Guard != guard.Name || cp.StatusConfigMap == "" {
			continue
		}
		cm := &corev1.ConfigMap{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: job.Namespace, Name: cp.StatusConfigMap}, cm); err != nil {
			continue
		}
		if at, err := time.Parse(time.RFC3339, cm.Data[cmRequestedAt]); err == nil {
			committed, cerr := time.Parse(time.RFC3339, cm.Data[cmCommittedAt])
			if cerr != nil || !committed.After(at) {
				continue // the last request has not been answered by a commit yet
			}
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[cmRequestedAt] = now.Format(time.RFC3339)
		cm.Data[cmRequestReason] = reason
		if err := r.Update(ctx, cm); err == nil {
			n++
		}
	}
	return n
}
