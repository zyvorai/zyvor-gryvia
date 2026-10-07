package controllers

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

func guardFor(labels map[string]string) *gryviav1.GryviaCheckpointGuard {
	return &gryviav1.GryviaCheckpointGuard{
		ObjectMeta: metav1.ObjectMeta{Name: "guard", Namespace: "default"},
		Spec: gryviav1.GryviaCheckpointGuardSpec{
			JobSelector: gryviav1.JobSelector{MatchLabels: labels},
			CheckpointPolicy: gryviav1.CheckpointPolicy{IntervalMinutes: 10, EverySteps: 50, Directory: "/data/ckpt",
				Replication: &gryviav1.ReplicationConfig{Target: "file:///replica/run"}},
			Restore: &gryviav1.RestorePolicy{AutoRestore: true, NodeLossGraceSeconds: 30},
		},
	}
}

func guardedJob(name string) *gryviav1.GryviaAIJob {
	job := cpuJob(name)
	job.Labels = map[string]string{"team": "ml"}
	job.Spec.Storage = "nfs"
	job.Spec.Distributed = &gryviav1.DistributedConfig{Enabled: true, Nodes: 2}
	return job
}

func TestCheckpointGuardInjectsEnvAndStatusRBAC(t *testing.T) {
	r, c := newAIJobReconciler(guardedJob("run"), guardFor(map[string]string{"team": "ml"}))
	r.CheckpointGuard = true
	reconcileN(t, r, "run", 3)

	bj := &batchv1.Job{}
	if !exists(t, c, bj, "run") {
		t.Fatal("no batch Job")
	}
	env := envMap(bj.Spec.Template.Spec.Containers[0].Env)
	for k, want := range map[string]string{
		"GRYVIA_CHECKPOINT_DIR": "/data/ckpt", "GRYVIA_RESUME_IF_PRESENT": "true", "GRYVIA_CHECKPOINT_EVERY": "50",
		"GRYVIA_CHECKPOINT_INTERVAL_SECONDS": "600", "GRYVIA_CHECKPOINT_REPLICA": "file:///replica/run",
		"GRYVIA_CHECKPOINT_STATUS_CONFIGMAP": "run-checkpoint-status",
	} {
		if env[k].Value != want {
			t.Errorf("%s = %q, want %q", k, env[k].Value, want)
		}
	}
	if env["POD_NAMESPACE"].ValueFrom == nil {
		t.Error("POD_NAMESPACE must come from the downward API")
	}
	role := &rbacv1.Role{}
	if !exists(t, c, role, "run-checkpoint-status") || role.Rules[0].ResourceNames[0] != "run-checkpoint-status" ||
		strings.Join(role.Rules[0].Verbs, ",") != "get,update" {
		t.Fatalf("role must grant get/update on exactly the status ConfigMap: %+v", role.Rules)
	}
	rb := &rbacv1.RoleBinding{}
	if !exists(t, c, rb, "run-checkpoint-status") || rb.Subjects[0].Name != "default" || !exists(t, c, &corev1.ConfigMap{}, "run-checkpoint-status") {
		t.Fatal("status ConfigMap and binding to the default service account expected")
	}
	got := getAIJob(t, c, "run")
	if got.Status.Checkpoint == nil || got.Status.Checkpoint.Guard != "guard" || got.Status.Checkpoint.Directory != "/data/ckpt" {
		t.Fatalf("status.checkpoint = %+v", got.Status.Checkpoint)
	}
}

func TestCheckpointGuardOffOrUnmatchedOrIneligibleInjectsNothing(t *testing.T) {
	cases := map[string]func(*GryviaAIJobReconciler, *gryviav1.GryviaAIJob){
		"capability off": func(r *GryviaAIJobReconciler, j *gryviav1.GryviaAIJob) { r.CheckpointGuard = false },
		"no match":       func(r *GryviaAIJobReconciler, j *gryviav1.GryviaAIJob) { j.Labels = map[string]string{"team": "other"} },
		"no storage":     func(r *GryviaAIJobReconciler, j *gryviav1.GryviaAIJob) { j.Spec.Storage = "" },
	}
	for name, mutate := range cases {
		job := guardedJob("run")
		r, c := newAIJobReconciler(job, guardFor(map[string]string{"team": "ml"}))
		r.CheckpointGuard = true
		mutate(r, job)
		_ = c.Update(context.Background(), job)
		reconcileN(t, r, "run", 3)
		bj := &batchv1.Job{}
		exists(t, c, bj, "run")
		if _, ok := envMap(bj.Spec.Template.Spec.Containers[0].Env)["GRYVIA_CHECKPOINT_STATUS_CONFIGMAP"]; ok || getAIJob(t, c, "run").Status.Checkpoint != nil {
			t.Errorf("%s: guard applied", name)
		}
	}
}

func TestCheckpointStatusIsCopiedFromTheConfigMap(t *testing.T) {
	r, c := newAIJobReconciler(guardedJob("run"), guardFor(map[string]string{"team": "ml"}))
	r.CheckpointGuard = true
	reconcileN(t, r, "run", 3)
	cm := &corev1.ConfigMap{}
	exists(t, c, cm, "run-checkpoint-status")
	cm.Data = map[string]string{"committedStep": "40", "committedAt": "2026-10-07T12:00:00Z", "currentStep": "47"}
	if err := c.Update(context.Background(), cm); err != nil {
		t.Fatal(err)
	}
	reconcileN(t, r, "run", 1)
	cp := getAIJob(t, c, "run").Status.Checkpoint
	if cp.LastCommittedStep != 40 || cp.CurrentStep != 47 || cp.LastCommittedAt == nil || cp.LastCommittedAt.UTC().Hour() != 12 {
		t.Fatalf("checkpoint status %+v", cp)
	}
}

func nodeWithReady(name string, status corev1.ConditionStatus, since time.Time) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
		{Type: corev1.NodeReady, Status: status, LastTransitionTime: metav1.NewTime(since)}}}}
}

func trainerPod(name, node string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(name), Labels: map[string]string{"gryvia.io/job": "run"}},
		Spec: corev1.PodSpec{NodeName: node}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
}

func runningGuardedJob() *gryviav1.GryviaAIJob {
	job := guardedJob("run")
	job.Status.Phase = PhaseRunning
	job.Status.Checkpoint = &gryviav1.AIJobCheckpoint{Guard: "guard", StatusConfigMap: "run-checkpoint-status", LastCommittedStep: 40, CurrentStep: 47}
	return job
}

func TestNodeLossReplacesPodsAfterGrace(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	job := runningGuardedJob()
	r, c := newAIJobReconciler(job, guardFor(map[string]string{"team": "ml"}),
		nodeWithReady("bad", corev1.ConditionUnknown, now.Add(-10*time.Second)), nodeWithReady("good", corev1.ConditionTrue, now.Add(-time.Hour)),
		trainerPod("run-0", "good"), trainerPod("run-1", "bad"), trainerPod("run-2", "gone"))
	r.CheckpointGuard = true

	if err := r.recoverFromNodeLoss(context.Background(), job, now); err != nil {
		t.Fatal(err)
	}
	if !exists(t, c, &corev1.Pod{}, "run-1") || exists(t, c, &corev1.Pod{}, "run-2") || !exists(t, c, &corev1.Pod{}, "run-0") {
		t.Fatal("within grace only the pod of the deleted node goes")
	}
	cp := job.Status.Checkpoint
	if cp.NodeLossRecoveries != 1 || cp.LastNodeLoss.Node != "gone" || cp.LastNodeLoss.ResumeStep != 40 || cp.LostSteps != 7 {
		t.Fatalf("accounting %+v %+v", cp, cp.LastNodeLoss)
	}

	if err := r.recoverFromNodeLoss(context.Background(), job, now.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if exists(t, c, &corev1.Pod{}, "run-1") || !exists(t, c, &corev1.Pod{}, "run-0") {
		t.Fatal("after the grace the NotReady node's pod is replaced, the healthy one stays")
	}
	if cp.NodeLossRecoveries != 2 || cp.LostSteps != 14 || !conditionTrue(job, ConditionRecoveredFromNodeLoss) {
		t.Fatalf("second loss %+v", cp)
	}
}

func TestNodeLossLeavesElasticAndNonAutoRestoreJobsAlone(t *testing.T) {
	now := time.Now()
	for name, mutate := range map[string]func(*gryviav1.GryviaAIJob, *gryviav1.GryviaCheckpointGuard){
		"elastic":        func(j *gryviav1.GryviaAIJob, g *gryviav1.GryviaCheckpointGuard) { j.Spec.Distributed.Elastic = &gryviav1.ElasticConfig{MinNodes: 1} },
		"no autoRestore": func(j *gryviav1.GryviaAIJob, g *gryviav1.GryviaCheckpointGuard) { g.Spec.Restore.AutoRestore = false },
	} {
		job, guard := runningGuardedJob(), guardFor(nil)
		mutate(job, guard)
		r, c := newAIJobReconciler(job, guard, trainerPod("run-1", "gone"))
		r.CheckpointGuard = true
		if err := r.recoverFromNodeLoss(context.Background(), job, now); err != nil {
			t.Fatal(err)
		}
		if !exists(t, c, &corev1.Pod{}, "run-1") {
			t.Errorf("%s: pod deleted", name)
		}
	}
}

