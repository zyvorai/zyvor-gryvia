package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gryviav1 "github.com/zyvorai/gryvia/operators/gpu-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/gpu-operator/pkg/agent"
)

const (
	// annRecoveryAttempts counts recovery requests in the current quarantine; annRecoveryRecorded is the
	// last request id whose result went into status.remediationHistory.
	annRecoveryAttempts = "gryvia.io/gpu-recovery-attempts"
	annRecoveryRecorded = "gryvia.io/gpu-recovery-recorded"
	unhealthyTaint      = "gryvia.io/gpu-unhealthy"
)

// recoverySummary is what the ladder did this pass, for the RemediationReady condition.
type recoverySummary struct {
	recovering, exhausted, dryRun, recovered []string
}

// recoveryLadder returns the enabled actions in escalation order and the attempt bound.
func recoveryLadder(hc *gryviav1.GryviaHealthCheck) ([]string, int) {
	rc := hc.Spec.Remediation
	if rc == nil || hc.Spec.OnFailure == nil || !hc.Spec.OnFailure.AutoRemediate {
		return nil, 0
	}
	var steps []string
	if rc.GpuReset {
		steps = append(steps, agent.ActionReset)
	}
	if rc.DriverReload {
		steps = append(steps, agent.ActionDriverReload)
	}
	if rc.NodeReboot {
		steps = append(steps, agent.ActionReboot)
	}
	limit := len(steps)
	if rc.MaxAttempts > 0 && int(rc.MaxAttempts) < limit {
		limit = int(rc.MaxAttempts)
	}
	return steps, limit
}

// reconcileRecovery drives each node this health check quarantined through reset -> driver reload ->
// reboot by writing reset-agent requests, and lifts the quarantine once a check run after a finished
// action passes. bad holds the nodes that failed (or warned in) this run, the same set handleFailure quarantines.
func (r *GryviaHealthCheckReconciler) reconcileRecovery(ctx context.Context, hc *gryviav1.GryviaHealthCheck, nodes []corev1.Node, bad map[string]bool, now time.Time) (recoverySummary, error) {
	var sum recoverySummary
	steps, limit := recoveryLadder(hc)
	if !r.EnableRemediation || len(steps) == 0 {
		return sum, nil
	}
	for i := range nodes {
		node := &corev1.Node{}
		if err := r.Get(ctx, types.NamespacedName{Name: nodes[i].Name}, node); err != nil {
			return sum, err
		}
		if node.Annotations[quarantineOwner] != string(hc.UID) {
			continue
		}
		if err := r.recoverNode(ctx, hc, node, bad[node.Name], steps, limit, now, &sum); err != nil {
			return sum, err
		}
	}
	return sum, nil
}

func (r *GryviaHealthCheckReconciler) recoverNode(ctx context.Context, hc *gryviav1.GryviaHealthCheck, node *corev1.Node, bad bool, steps []string, limit int, now time.Time, sum *recoverySummary) error {
	ann := node.Annotations
	attempts, _ := strconv.Atoi(ann[annRecoveryAttempts])
	reqID := ann[agent.AnnRequest]
	if attempts == 0 || reqID == "" {
		if !bad {
			return nil // healthy without any action from us: leave the quarantine to an operator
		}
		return r.requestRecovery(ctx, node, steps[0], 1, now, sum)
	}

	var res agent.Result
	_ = json.Unmarshal([]byte(ann[agent.AnnResult]), &res)
	action := ann[agent.AnnAction]
	if res.ID != reqID || res.State == agent.StateBlocked || res.State == agent.StateInProgress {
		sum.recovering = append(sum.recovering, fmt.Sprintf("%s: %s pending", node.Name, action))
		return nil
	}
	if ann[annRecoveryRecorded] != reqID {
		r.recordRecovery(hc, node.Name, action, res, now)
		if err := r.patchAnnotations(ctx, node, map[string]string{annRecoveryRecorded: reqID}); err != nil {
			return err
		}
	}
	if res.State == agent.StateDryRun {
		sum.dryRun = append(sum.dryRun, fmt.Sprintf("%s: %s (dry-run)", node.Name, action))
		return nil // a dry-run agent changes nothing, so escalating would only repeat dry-runs
	}
	at, err := time.Parse(time.RFC3339, res.At)
	if err != nil || !now.After(at) {
		sum.recovering = append(sum.recovering, fmt.Sprintf("%s: waiting for a check after %s", node.Name, action))
		return nil
	}
	if res.State == agent.StateDone && !bad {
		if err := r.releaseQuarantine(ctx, node); err != nil {
			return err
		}
		hc.Status.RemediationHistory = append(hc.Status.RemediationHistory, gryviav1.RemediationEvent{
			Timestamp: &metav1.Time{Time: now}, Action: "uncordon", Success: true,
			Message: fmt.Sprintf("%s passed checks after %s; quarantine lifted", node.Name, action)})
		sum.recovered = append(sum.recovered, node.Name)
		return nil
	}
	if attempts >= limit {
		sum.exhausted = append(sum.exhausted, fmt.Sprintf("%s: still unhealthy after %d attempt(s), last %s %s", node.Name, attempts, action, res.State))
		return nil
	}
	return r.requestRecovery(ctx, node, steps[attempts], attempts+1, now, sum)
}

func (r *GryviaHealthCheckReconciler) requestRecovery(ctx context.Context, node *corev1.Node, action string, attempt int, now time.Time, sum *recoverySummary) error {
	id := fmt.Sprintf("gryvia-%d-%d", now.Unix(), attempt)
	if err := r.patchAnnotations(ctx, node, map[string]string{
		agent.AnnRequest:    id,
		agent.AnnAction:     action,
		annRecoveryAttempts: strconv.Itoa(attempt),
	}); err != nil {
		return err
	}
	sum.recovering = append(sum.recovering, fmt.Sprintf("%s: requested %s (attempt %d)", node.Name, action, attempt))
	return nil
}

func (r *GryviaHealthCheckReconciler) recordRecovery(hc *gryviav1.GryviaHealthCheck, node, action string, res agent.Result, now time.Time) {
	hc.Status.RemediationHistory = append(hc.Status.RemediationHistory, gryviav1.RemediationEvent{
		Timestamp: &metav1.Time{Time: now}, Action: action, Success: res.State == agent.StateDone,
		Message: fmt.Sprintf("%s: %s %s", node, res.State, res.Message)})
}

func (r *GryviaHealthCheckReconciler) patchAnnotations(ctx context.Context, node *corev1.Node, set map[string]string) error {
	base := node.DeepCopy()
	if node.Annotations == nil {
		node.Annotations = map[string]string{}
	}
	for k, v := range set {
		node.Annotations[k] = v
	}
	return r.Patch(ctx, node, client.MergeFrom(base))
}

// releaseQuarantine undoes quarantine() and clears the recovery bookkeeping.
func (r *GryviaHealthCheckReconciler) releaseQuarantine(ctx context.Context, node *corev1.Node) error {
	base := node.DeepCopy()
	node.Spec.Unschedulable = false
	taints := node.Spec.Taints[:0]
	for _, t := range node.Spec.Taints {
		if t.Key != unhealthyTaint {
			taints = append(taints, t)
		}
	}
	node.Spec.Taints = taints
	for _, k := range []string{quarantineOwner, annRecoveryAttempts, annRecoveryRecorded,
		agent.AnnRequest, agent.AnnAction, agent.AnnResult, agent.AnnGPUs, agent.AnnObserved} {
		delete(node.Annotations, k)
	}
	return r.Patch(ctx, node, client.MergeFrom(base))
}
