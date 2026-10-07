package controllers

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

const (
	defaultCheckpointDir = "/data/checkpoints"
	defaultNodeLossGrace = 60 * time.Second

	// ConditionRecoveredFromNodeLoss is True after pods stuck on a lost node were replaced.
	ConditionRecoveredFromNodeLoss = "RecoveredFromNodeLoss"

	// Keys of the status ConfigMap. The trainer writes the step keys; the guard writes the request keys.
	cmCommittedStep = "committedStep"
	cmCommittedAt   = "committedAt"
	cmCurrentStep   = "currentStep"
	cmRequestedAt   = "requestedAt"
	cmRequestReason = "requestReason"
)

func checkpointStatusName(job *gryviav1.GryviaAIJob) string {
	return job.Name + "-checkpoint-status"
}

// matchingGuard returns the first guard (by name) in the job's namespace whose jobSelector matches the job;
// an empty selector matches every job, as in the guard controller.
func (r *GryviaAIJobReconciler) matchingGuard(ctx context.Context, job *gryviav1.GryviaAIJob) (*gryviav1.GryviaCheckpointGuard, error) {
	guards := &gryviav1.GryviaCheckpointGuardList{}
	if err := r.List(ctx, guards, client.InNamespace(job.Namespace)); err != nil {
		return nil, err
	}
	sort.Slice(guards.Items, func(i, j int) bool { return guards.Items[i].Name < guards.Items[j].Name })
	for i := range guards.Items {
		g := &guards.Items[i]
		if g.DeletionTimestamp != nil {
			continue
		}
		match := true
		for k, v := range g.Spec.JobSelector.MatchLabels {
			if job.Labels[k] != v {
				match = false
				break
			}
		}
		if match {
			return g, nil
		}
	}
	return nil, nil
}

// checkpointIneligible says why a guard cannot manage the job's checkpoints, or "".
func checkpointIneligible(job *gryviav1.GryviaAIJob) string {
	switch {
	case job.Spec.Type == "inference":
		return "inference jobs are not checkpointed"
	case job.Spec.WorkloadKind == gryviav1.WorkloadKindStatefulSet:
		return "checkpoint guards manage batch Jobs only"
	case job.Spec.Storage == "":
		return "needs spec.storage: checkpoints go to the persistent /data volume"
	}
	return ""
}

// guardDirectory is the job's checkpoint-directory annotation when checkpoint hooks are configured (the
// preStop hook and the guard must agree), else the guard's directory.
func guardDirectory(job *gryviav1.GryviaAIJob, guard *gryviav1.GryviaCheckpointGuard) (string, error) {
	if o, err := parseRecoveryOptions(job); err == nil && len(o.command) > 0 {
		return o.directory, nil
	}
	dir := guard.Spec.CheckpointPolicy.Directory
	if dir == "" {
		dir = defaultCheckpointDir
	}
	if !strings.HasPrefix(dir, "/data/") || path.Clean(dir) != dir || strings.ContainsRune(dir, 0) {
		return "", fmt.Errorf("guard %s: checkpoint directory must be a clean absolute path below /data", guard.Name)
	}
	return dir, nil
}

// applyCheckpointGuard gives the trainer container the guard's checkpoint environment. Reserved names
// override what the job set, so every pod saves to and resumes from the same place.
func applyCheckpointGuard(job *gryviav1.GryviaAIJob, guard *gryviav1.GryviaCheckpointGuard, dir string, spec *corev1.PodSpec) {
	c := &spec.Containers[0]
	p := guard.Spec.CheckpointPolicy
	c.Env = replaceEnv(c.Env, "GRYVIA_CHECKPOINT_DIR", dir)
	c.Env = replaceEnv(c.Env, "GRYVIA_RESUME_IF_PRESENT", "true")
	c.Env = replaceEnv(c.Env, "GRYVIA_CHECKPOINT_STATUS_CONFIGMAP", checkpointStatusName(job))
	if p.IntervalMinutes > 0 {
		c.Env = replaceEnv(c.Env, "GRYVIA_CHECKPOINT_INTERVAL_SECONDS", strconv.Itoa(int(p.IntervalMinutes)*60))
	}
	if p.EverySteps > 0 {
		c.Env = replaceEnv(c.Env, "GRYVIA_CHECKPOINT_EVERY", strconv.Itoa(int(p.EverySteps)))
	}
	if p.Replication != nil && p.Replication.Target != "" {
		c.Env = replaceEnv(c.Env, "GRYVIA_CHECKPOINT_REPLICA", p.Replication.Target)
	}
	if !hasEnv(c.Env, "POD_NAMESPACE") {
		c.Env = append(c.Env, corev1.EnvVar{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}})
	}
}

// ensureCheckpointStatus creates the job's status ConfigMap and a Role that lets the pods' service account
// (the namespace's default one) read and update exactly that ConfigMap. All three are owned by the job.
func (r *GryviaAIJobReconciler) ensureCheckpointStatus(ctx context.Context, job *gryviav1.GryviaAIJob) error {
	name := checkpointStatusName(job)
	labels := map[string]string{"gryvia.io/job": job.Name}
	objs := []client.Object{
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: job.Namespace, Labels: labels}},
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: job.Namespace, Labels: labels},
			Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{name}, Verbs: []string{"get", "update"}}}},
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: job.Namespace, Labels: labels},
			RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name},
			Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "default", Namespace: job.Namespace}}},
	}
	for _, obj := range objs {
		if err := controllerutil.SetControllerReference(job, obj, r.Scheme); err != nil {
			return err
		}
		if err := r.Create(ctx, obj); err != nil && !errors.IsAlreadyExists(err) {
			return fmt.Errorf("checkpoint status %T: %w", obj, err)
		}
	}
	return nil
}

// syncCheckpointStatus copies what the trainer reported into status.checkpoint.
func (r *GryviaAIJobReconciler) syncCheckpointStatus(ctx context.Context, job *gryviav1.GryviaAIJob) {
	cp := job.Status.Checkpoint
	cm := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: job.Namespace, Name: cp.StatusConfigMap}, cm); err != nil {
		return
	}
	if n, err := strconv.ParseInt(cm.Data[cmCommittedStep], 10, 64); err == nil && n >= 0 {
		cp.LastCommittedStep = n
		if t, err := time.Parse(time.RFC3339, cm.Data[cmCommittedAt]); err == nil {
			cp.LastCommittedAt = &metav1.Time{Time: t}
		}
	}
	if n, err := strconv.ParseInt(cm.Data[cmCurrentStep], 10, 64); err == nil && n >= 0 {
		cp.CurrentStep = n
	}
}

// startCheckpointGuard runs once, when the batch Job is created: it injects the matching guard's policy into
// the pod template and records the guard in status.checkpoint. Jobs created before a guard keep running
// unguarded (a Job's pod template is immutable).
func (r *GryviaAIJobReconciler) startCheckpointGuard(ctx context.Context, job *gryviav1.GryviaAIJob, spec *corev1.PodSpec) error {
	if !r.CheckpointGuard {
		return nil
	}
	guard, err := r.matchingGuard(ctx, job)
	if err != nil || guard == nil || checkpointIneligible(job) != "" {
		return err
	}
	dir, err := guardDirectory(job, guard)
	if err != nil {
		return err
	}
	if err := r.ensureCheckpointStatus(ctx, job); err != nil {
		return err
	}
	applyCheckpointGuard(job, guard, dir, spec)
	job.Status.Checkpoint = &gryviav1.AIJobCheckpoint{Guard: guard.Name, Directory: dir, StatusConfigMap: checkpointStatusName(job)}
	return nil
}

// recoverFromNodeLoss force-deletes the pods of a guarded, non-elastic job that sit on a node that is gone or
// has not been Ready for the guard's grace period, so the Indexed Job recreates those indexes on healthy nodes
// and the trainer resumes from the last committed step. A pod on an unreachable node otherwise stays
// Terminating until the node returns. Elastic jobs are left alone: their group shrinks instead.
func (r *GryviaAIJobReconciler) recoverFromNodeLoss(ctx context.Context, job *gryviav1.GryviaAIJob, now time.Time) error {
	cp := job.Status.Checkpoint
	if !r.CheckpointGuard || cp == nil || isTerminalPhase(job.Status.Phase) {
		return nil
	}
	if _, _, elastic := job.Spec.Distributed.ElasticBounds(); elastic {
		return nil
	}
	guard := &gryviav1.GryviaCheckpointGuard{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: job.Namespace, Name: cp.Guard}, guard); err != nil {
		return client.IgnoreNotFound(err)
	}
	if guard.Spec.Restore == nil || !guard.Spec.Restore.AutoRestore {
		return nil
	}
	grace := defaultNodeLossGrace
	if g := guard.Spec.Restore.NodeLossGraceSeconds; g > 0 {
		grace = time.Duration(g) * time.Second
	}
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(job.Namespace), client.MatchingLabels{"gryvia.io/job": job.Name}); err != nil {
		return err
	}
	lost := map[string][]corev1.Pod{}
	nodeLost := map[string]bool{}
	for _, pod := range pods.Items {
		if pod.Spec.NodeName == "" || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		gone, seen := nodeLost[pod.Spec.NodeName]
		if !seen {
			var err error
			if gone, err = r.nodeLost(ctx, pod.Spec.NodeName, grace, now); err != nil {
				return err
			}
			nodeLost[pod.Spec.NodeName] = gone
		}
		if gone {
			lost[pod.Spec.NodeName] = append(lost[pod.Spec.NodeName], pod)
		}
	}
	nodes := make([]string, 0, len(lost))
	for n := range lost {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	for _, node := range nodes {
		var names []string
		for i := range lost[node] {
			pod := &lost[node][i]
			uid := pod.UID
			err := r.Delete(ctx, pod, client.GracePeriodSeconds(0), client.Preconditions{UID: &uid})
			if err != nil && !errors.IsNotFound(err) {
				return fmt.Errorf("replace pod %s on lost node %s: %w", pod.Name, node, err)
			}
			names = append(names, pod.Name)
		}
		var lostSteps int64
		if cp.CurrentStep > cp.LastCommittedStep {
			lostSteps = cp.CurrentStep - cp.LastCommittedStep
		}
		cp.NodeLossRecoveries++
		cp.LostSteps += lostSteps
		cp.LastNodeLoss = &gryviav1.NodeLossEvent{Node: node, At: metav1.Time{Time: now}, Pods: names, ResumeStep: cp.LastCommittedStep, LostSteps: lostSteps}
		r.updateCondition(job, ConditionRecoveredFromNodeLoss, metav1.ConditionTrue, "PodsReplaced",
			fmt.Sprintf("node %s lost: replaced %s; resuming from committed step %d", node, strings.Join(names, ", "), cp.LastCommittedStep))
		if r.Recorder != nil {
			r.Recorder.Eventf(job, corev1.EventTypeWarning, "NodeLost", "node %s lost: replaced %s, resuming from step %d", node, strings.Join(names, ", "), cp.LastCommittedStep)
		}
	}
	return nil
}

// nodeLost is true when the node no longer exists or its Ready condition has not been True for grace.
func (r *GryviaAIJobReconciler) nodeLost(ctx context.Context, name string, grace time.Duration, now time.Time) (bool, error) {
	node := &corev1.Node{}
	if err := r.Get(ctx, types.NamespacedName{Name: name}, node); err != nil {
		if errors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status != corev1.ConditionTrue && now.Sub(c.LastTransitionTime.Time) >= grace, nil
		}
	}
	return false, nil
}
