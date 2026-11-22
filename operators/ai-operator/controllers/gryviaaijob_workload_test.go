// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/ai-operator/pkg/scheduler"
)

const ns = "default"

func cpuJob(name string) *gryviav1.GryviaAIJob {
	j := newTestAIJob(name, ns)
	j.Spec.GPUs = 0
	j.Spec.GpuType = ""
	return j
}

func gpuNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"gryvia.io/gpu-count": "8", "gryvia.io/gpu": "H100"}},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
	}
}

func reconcileN(t *testing.T, r *GryviaAIJobReconciler, name string, n int) ctrl.Result {
	t.Helper()
	var res ctrl.Result
	var err error
	for i := 0; i < n; i++ {
		res, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
		if err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	return res
}

func getAIJob(t *testing.T, c client.Client, name string) *gryviav1.GryviaAIJob {
	t.Helper()
	j := &gryviav1.GryviaAIJob{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, j); err != nil {
		t.Fatal(err)
	}
	return j
}

func exists(t *testing.T, c client.Client, obj client.Object, name string) bool {
	t.Helper()
	err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, obj)
	if err == nil {
		return true
	}
	if errors.IsNotFound(err) {
		return false
	}
	t.Fatal(err)
	return false
}

func envMap(env []corev1.EnvVar) map[string]corev1.EnvVar {
	m := map[string]corev1.EnvVar{}
	for _, e := range env {
		m[e.Name] = e
	}
	return m
}

func TestBuildJob_Construction(t *testing.T) {
	r, _ := newAIJobReconciler()
	job := newTestAIJob("train", ns)
	job.Spec.GPUs = 8
	job.Spec.Storage = "fast"
	job.Spec.RetryLimit = 3
	job.Spec.Timeout = "2d"
	job.Spec.Network = "rdma"
	job.Spec.NodeSelector = map[string]string{"zone": "a"}
	job.Spec.Tolerations = []corev1.Toleration{{Key: "gpu", Operator: corev1.TolerationOpExists}}
	job.Spec.Distributed = &gryviav1.DistributedConfig{Enabled: true, Nodes: 3, GpusPerNode: 8, Framework: "pytorch", Backend: "nccl"}
	job.Spec.Resources = corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}}

	bj, err := r.buildJob(job)
	if err != nil {
		t.Fatal(err)
	}
	if bj.Name != "train" || bj.Namespace != ns {
		t.Errorf("name/namespace = %s/%s", bj.Namespace, bj.Name)
	}
	sp := bj.Spec
	if sp.CompletionMode == nil || *sp.CompletionMode != batchv1.IndexedCompletion {
		t.Error("completionMode must be Indexed")
	}
	if *sp.Parallelism != 3 || *sp.Completions != 3 {
		t.Errorf("parallelism/completions = %d/%d, want 3/3", *sp.Parallelism, *sp.Completions)
	}
	if *sp.BackoffLimit != 3 {
		t.Errorf("backoffLimit = %d, want retryLimit 3", *sp.BackoffLimit)
	}
	if sp.ActiveDeadlineSeconds == nil || *sp.ActiveDeadlineSeconds != 2*24*3600 {
		t.Errorf("activeDeadlineSeconds = %v", sp.ActiveDeadlineSeconds)
	}
	if *sp.Suspend {
		t.Error("must not be suspended by default")
	}
	if sp.PodFailurePolicy == nil || sp.PodFailurePolicy.Rules[0].Action != batchv1.PodFailurePolicyActionIgnore {
		t.Error("expected an Ignore podFailurePolicy for DisruptionTarget")
	}
	pod := sp.Template.Spec
	if pod.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %s", pod.RestartPolicy)
	}
	if pod.Subdomain != "train-headless" {
		t.Errorf("subdomain = %q", pod.Subdomain)
	}
	if pod.NodeSelector["zone"] != "a" || pod.NodeSelector["gryvia.io/rdma"] != "true" || pod.NodeSelector["gryvia.io/gpu"] != "H100" {
		t.Errorf("nodeSelector = %v", pod.NodeSelector)
	}
	if len(pod.Tolerations) != 1 {
		t.Errorf("tolerations = %v", pod.Tolerations)
	}
	c := pod.Containers[0]
	if g := c.Resources.Limits["nvidia.com/gpu"]; g.Value() != 8 {
		t.Errorf("gpu limit = %v", g.Value())
	}
	if _, ok := job.Spec.Resources.Limits["nvidia.com/gpu"]; ok {
		t.Error("buildJob mutated spec.resources")
	}
	vols := map[string]bool{}
	for _, v := range pod.Volumes {
		vols[v.Name] = true
	}
	if !vols["data"] || !vols["shm"] {
		t.Errorf("volumes = %v", vols)
	}
	if sp.Template.Annotations["gryvia.io/rdma"] != "true" {
		t.Error("rdma annotation missing")
	}
	env := envMap(c.Env)
	rank := env["RANK"].ValueFrom
	if rank == nil || rank.FieldRef.FieldPath != "metadata.annotations['batch.kubernetes.io/job-completion-index']" {
		t.Errorf("RANK = %+v", env["RANK"])
	}
	if env["NODE_RANK"].ValueFrom == nil {
		t.Error("NODE_RANK missing")
	}
	if got := env["MASTER_ADDR"].Value; got != "train-0.train-headless.default.svc.cluster.local" {
		t.Errorf("MASTER_ADDR = %q", got)
	}
	if env["WORLD_SIZE"].Value != "24" || env["NNODES"].Value != "3" || env["NPROC_PER_NODE"].Value != "8" {
		t.Errorf("world/nnodes/nproc = %s/%s/%s", env["WORLD_SIZE"].Value, env["NNODES"].Value, env["NPROC_PER_NODE"].Value)
	}
}

func TestBuildJob_CPUOnly(t *testing.T) {
	r, _ := newAIJobReconciler()
	job := cpuJob("cpu")
	job.Spec.GpuType = "H100" // a stale GPU type must not create a GPU selector
	job.Spec.Distributed = &gryviav1.DistributedConfig{Enabled: true, Nodes: 2}
	bj, err := r.buildJob(job)
	if err != nil {
		t.Fatal(err)
	}
	pod := bj.Spec.Template.Spec
	if _, ok := pod.Containers[0].Resources.Limits["nvidia.com/gpu"]; ok {
		t.Error("CPU-only job must not request nvidia.com/gpu")
	}
	if _, ok := pod.NodeSelector["gryvia.io/gpu"]; ok {
		t.Error("CPU-only job must not select GPU nodes")
	}
	if e := envMap(pod.Containers[0].Env); e["WORLD_SIZE"].Value != "2" || e["NPROC_PER_NODE"].Value != "1" {
		t.Errorf("CPU-only: one process per node, got WORLD_SIZE=%q NPROC_PER_NODE=%q", e["WORLD_SIZE"].Value, e["NPROC_PER_NODE"].Value)
	}
}

func TestBuildJob_SuspendKueueAndTimeout(t *testing.T) {
	r, _ := newAIJobReconciler()
	job := cpuJob("s")
	job.Spec.Suspend = true
	bj, _ := r.buildJob(job)
	if !*bj.Spec.Suspend {
		t.Error("spec.suspend must create a suspended Job")
	}
	job = cpuJob("q")
	job.Labels = map[string]string{LabelKueueQueue: "team-a"}
	bj, _ = r.buildJob(job)
	if !*bj.Spec.Suspend || bj.Labels[LabelKueueQueue] != "team-a" {
		t.Errorf("queue label must be copied and the Job created suspended: %v %v", bj.Labels, *bj.Spec.Suspend)
	}
	job = cpuJob("bad")
	job.Spec.Timeout = "forever"
	if _, err := r.buildJob(job); err == nil {
		t.Error("invalid timeout must be an error")
	}
	job = cpuJob("none")
	bj, _ = r.buildJob(job)
	if bj.Spec.ActiveDeadlineSeconds != nil || *bj.Spec.BackoffLimit != 0 {
		t.Error("no timeout means no deadline; retryLimit 0 means backoffLimit 0")
	}
}

func TestBuildJob_PreservesFabricAffinity(t *testing.T) {
	r, _ := newAIJobReconciler()
	job := newTestAIJob("f", ns)
	job.Status.NodesAllocated = []string{"n2"}
	job.Status.PlacementExplanation = []gryviav1.PlacementExplanation{{Node: "n1", FabricPenalty: 25}}
	user := &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
		NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "k", Operator: corev1.NodeSelectorOpExists}}}},
	}}}
	job.Spec.Affinity = user
	bj, _ := r.buildJob(job)
	aff := bj.Spec.Template.Spec.Affinity
	if aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		t.Error("required user terms must be kept")
	}
	if len(aff.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution) != len(scheduler.PreferredNodeTerms(job.Status.NodesAllocated, job.Status.PlacementExplanation)) ||
		len(aff.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution) == 0 {
		t.Errorf("fabric preferred terms missing: %+v", aff)
	}
	if len(user.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution) != 0 {
		t.Error("spec affinity mutated")
	}
}

// The exact environment per framework/backend is documented in docs/aijob-lifecycle.md.
func TestBuildEnvVars_Matrix(t *testing.T) {
	r, _ := newAIJobReconciler()
	mk := func(framework, backend, network string) *gryviav1.GryviaAIJob {
		j := newTestAIJob("m", "ml")
		j.Spec.Network = network
		j.Spec.Distributed = &gryviav1.DistributedConfig{Enabled: true, Nodes: 2, GpusPerNode: 4, Framework: framework, Backend: backend}
		return j
	}
	cases := []struct {
		name         string
		job          *gryviav1.GryviaAIJob
		kind         string
		version      int
		want, absent []string
	}{
		{"job nccl rdma", mk("pytorch", "nccl", "rdma"), gryviav1.WorkloadKindJob, 2,
			[]string{"MASTER_ADDR", "MASTER_PORT", "WORLD_SIZE", "RANK", "NODE_RANK", "NNODES", "NPROC_PER_NODE", "GRYVIA_DIST_FRAMEWORK", "GRYVIA_DIST_BACKEND", "NCCL_DEBUG", "NCCL_IB_DISABLE", "NCCL_NET_GDR_LEVEL"}, nil},
		{"job default backend", mk("", "", ""), gryviav1.WorkloadKindJob, 2,
			[]string{"MASTER_ADDR", "RANK", "NCCL_DEBUG"}, []string{"GRYVIA_DIST_FRAMEWORK", "GRYVIA_DIST_BACKEND", "NCCL_IB_DISABLE"}},
		{"job gloo has no NCCL env", mk("pytorch", "gloo", "rdma"), gryviav1.WorkloadKindJob, 2,
			[]string{"MASTER_ADDR", "RANK", "GRYVIA_DIST_BACKEND"}, []string{"NCCL_DEBUG", "NCCL_IB_DISABLE", "NCCL_NET_GDR_LEVEL"}},
		{"statefulset v2", mk("pytorch", "nccl", ""), gryviav1.WorkloadKindStatefulSet, 2,
			[]string{"MASTER_ADDR", "RANK", "NODE_RANK"}, nil},
		{"statefulset v1 legacy", mk("pytorch", "nccl", "rdma"), gryviav1.WorkloadKindStatefulSet, 1,
			[]string{"MASTER_ADDR", "MASTER_PORT", "WORLD_SIZE", "NCCL_DEBUG", "NCCL_IB_DISABLE", "NCCL_NET_GDR_LEVEL"}, []string{"RANK", "NODE_RANK", "NNODES"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := envMap(r.buildEnvVarsFor(c.job, c.kind, c.version))
			for _, n := range c.want {
				if _, ok := env[n]; !ok {
					t.Errorf("missing %s", n)
				}
			}
			for _, n := range c.absent {
				if _, ok := env[n]; ok {
					t.Errorf("unexpected %s", n)
				}
			}
		})
	}
	sts := envMap(r.buildEnvVarsFor(mk("", "", ""), gryviav1.WorkloadKindStatefulSet, 2))
	if sts["MASTER_ADDR"].Value != "m-training-0.m-headless.ml.svc.cluster.local" {
		t.Errorf("sts MASTER_ADDR = %q", sts["MASTER_ADDR"].Value)
	}
	if sts["RANK"].ValueFrom.FieldRef.FieldPath != "metadata.labels['apps.kubernetes.io/pod-index']" {
		t.Errorf("sts RANK = %+v", sts["RANK"])
	}
	legacy := envMap(r.buildEnvVarsFor(mk("", "", ""), gryviav1.WorkloadKindStatefulSet, 1))
	if legacy["MASTER_ADDR"].Value != "m-training-0.m-headless" {
		t.Errorf("legacy MASTER_ADDR changed: %q", legacy["MASTER_ADDR"].Value)
	}
	r.ClusterDomain = "corp.local"
	if got := envMap(r.buildEnvVarsFor(mk("", "", ""), gryviav1.WorkloadKindJob, 2))["MASTER_ADDR"].Value; got != "m-0.m-headless.ml.svc.corp.local" {
		t.Errorf("custom cluster domain: %q", got)
	}
}

func condTrue(t batchv1.JobConditionType, reason, msg string) batchv1.JobCondition {
	return batchv1.JobCondition{Type: t, Status: corev1.ConditionTrue, Reason: reason, Message: msg,
		LastTransitionTime: metav1.NewTime(time.Unix(1_700_000_100, 0))}
}

func TestSummarizeJob_PhaseMapping(t *testing.T) {
	i32 := func(v int32) *int32 { return &v }
	yes := true
	start := metav1.NewTime(time.Unix(1_700_000_000, 0))
	done := metav1.NewTime(time.Unix(1_700_000_050, 0))
	cases := []struct {
		name      string
		job       batchv1.Job
		wantPhase string
		wantReady int32
	}{
		{"nothing yet", batchv1.Job{}, PhaseScheduling, 0},
		{"pods active not ready", batchv1.Job{Status: batchv1.JobStatus{Active: 2, Ready: i32(0), StartTime: &start}}, PhaseScheduling, 0},
		{"one ready", batchv1.Job{Status: batchv1.JobStatus{Active: 2, Ready: i32(1), StartTime: &start}}, PhaseRunning, 1},
		{"one index done, others active", batchv1.Job{Status: batchv1.JobStatus{Active: 1, Succeeded: 1, Ready: i32(0)}}, PhaseRunning, 0},
		{"suspended spec", batchv1.Job{Spec: batchv1.JobSpec{Suspend: &yes}}, PhaseScheduling, 0},
		{"suspended condition", batchv1.Job{Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{condTrue(batchv1.JobSuspended, "JobSuspended", "")}}}, PhaseScheduling, 0},
		{"complete", batchv1.Job{Status: batchv1.JobStatus{Succeeded: 2, StartTime: &start, CompletionTime: &done,
			Conditions: []batchv1.JobCondition{condTrue(batchv1.JobComplete, "", "")}}}, PhaseSucceeded, 0},
		{"failed", batchv1.Job{Status: batchv1.JobStatus{Failed: 1, StartTime: &start,
			Conditions: []batchv1.JobCondition{condTrue(batchv1.JobFailed, "BackoffLimitExceeded", "Job has reached the specified backoff limit")}}}, PhaseFailed, 0},
		{"deadline", batchv1.Job{Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{condTrue(batchv1.JobFailed, "DeadlineExceeded", "Job was active longer than specified deadline")}}}, PhaseFailed, 0},
		{"failed wins over stale counts", batchv1.Job{Status: batchv1.JobStatus{Active: 1, Ready: i32(1),
			Conditions: []batchv1.JobCondition{condTrue(batchv1.JobFailed, "BackoffLimitExceeded", "")}}}, PhaseFailed, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := summarizeJob(&c.job, 2)
			if out.Phase != c.wantPhase {
				t.Errorf("phase = %s, want %s", out.Phase, c.wantPhase)
			}
			if out.Ready != c.wantReady {
				t.Errorf("ready = %d, want %d", out.Ready, c.wantReady)
			}
			if c.wantPhase == PhaseFailed && out.Message == "" {
				t.Error("failure needs a message")
			}
			if c.wantPhase == PhaseSucceeded && (out.End == nil || !out.End.Equal(&done)) {
				t.Errorf("end = %v", out.End)
			}
		})
	}
}

func TestReconcile_DefaultsToIndexedJob(t *testing.T) {
	job := cpuJob("run")
	job.Spec.Distributed = &gryviav1.DistributedConfig{Enabled: true, Nodes: 2}
	job.Spec.Storage = "std"
	r, c := newAIJobReconciler(job)
	reconcileN(t, r, "run", 3)

	bj := &batchv1.Job{}
	if !exists(t, c, bj, "run") || !metav1.IsControlledBy(bj, getAIJob(t, c, "run")) {
		t.Fatal("expected an owned batch Job named after the job")
	}
	if exists(t, c, &appsv1.StatefulSet{}, "run-training") {
		t.Error("no StatefulSet expected for a training job")
	}
	if !exists(t, c, &corev1.Service{}, "run-headless") || !exists(t, c, &corev1.PersistentVolumeClaim{}, "run-data") {
		t.Error("service and PVC expected")
	}
	got := getAIJob(t, c, "run")
	if got.Status.Phase != PhaseScheduling || !conditionTrue(got, ConditionScheduled) {
		t.Errorf("phase = %s, conditions = %v", got.Status.Phase, got.Status.Conditions)
	}
	if len(got.Status.NodesAllocated) != 0 {
		t.Errorf("CPU-only job needs no placement, got %v", got.Status.NodesAllocated)
	}
}

func TestReconcile_WorkloadKindSelection(t *testing.T) {
	cases := []struct {
		name         string
		typ, kind    string
		wantJob, sts bool
	}{
		{"training default", "training", "", true, false},
		{"fine-tuning default", "fine-tuning", "", true, false},
		{"evaluation default", "evaluation", "", true, false},
		{"inference default", "inference", "", false, true},
		{"inference forced to job", "inference", "job", true, false},
		{"training forced to statefulset", "training", "statefulset", false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			job := cpuJob("k")
			job.Spec.Type, job.Spec.WorkloadKind = c.typ, c.kind
			r, cl := newAIJobReconciler(job)
			reconcileN(t, r, "k", 3)
			if got := exists(t, cl, &batchv1.Job{}, "k"); got != c.wantJob {
				t.Errorf("batch Job exists = %v, want %v", got, c.wantJob)
			}
			if got := exists(t, cl, &appsv1.StatefulSet{}, "k-training"); got != c.sts {
				t.Errorf("StatefulSet exists = %v, want %v", got, c.sts)
			}
		})
	}
}

func TestReconcile_ExistingStatefulSetIsNeverMigrated(t *testing.T) {
	job := cpuJob("old")
	job.UID = "uid-old"
	job.Status.Phase = PhaseRunning
	job.Status.NodesAllocated = []string{"n1"}
	r, c := newAIJobReconciler(job)
	// A StatefulSet as an earlier operator version built it (v1 env, no env-version annotation).
	job.Spec.Distributed = &gryviav1.DistributedConfig{Enabled: true, Nodes: 2}
	if err := c.Update(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	legacy := r.buildStatefulSetVersion(job, 1)
	if err := controllerutil.SetControllerReference(job, legacy, r.Scheme); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(context.Background(), legacy); err != nil {
		t.Fatal(err)
	}
	reconcileN(t, r, "old", 2)

	if exists(t, c, &batchv1.Job{}, "old") {
		t.Fatal("a job with an existing StatefulSet must not get a batch Job")
	}
	sts := &appsv1.StatefulSet{}
	exists(t, c, sts, "old-training")
	env := envMap(sts.Spec.Template.Spec.Containers[0].Env)
	if _, ok := env["RANK"]; ok || env["MASTER_ADDR"].Value != "old-training-0.old-headless" {
		t.Errorf("existing StatefulSet env was changed (would restart running pods): %+v", env)
	}
	if _, ok := sts.Spec.Template.Annotations[AnnotationEnvVersion]; ok {
		t.Error("existing StatefulSet was upgraded to the v2 environment")
	}
}

func TestReconcile_NewStatefulSetGetsV2Env(t *testing.T) {
	job := cpuJob("new")
	job.Spec.WorkloadKind = "statefulset"
	job.Spec.Distributed = &gryviav1.DistributedConfig{Enabled: true, Nodes: 2}
	r, c := newAIJobReconciler(job)
	reconcileN(t, r, "new", 3)
	sts := &appsv1.StatefulSet{}
	exists(t, c, sts, "new-training")
	if _, ok := envMap(sts.Spec.Template.Spec.Containers[0].Env)["RANK"]; !ok {
		t.Error("new StatefulSet should carry RANK")
	}
	// Reconciling again must not flip-flop the template.
	before := sts.ResourceVersion
	reconcileN(t, r, "new", 2)
	exists(t, c, sts, "new-training")
	if sts.ResourceVersion != before {
		t.Error("StatefulSet was rewritten without a spec change")
	}
}

func setJobStatus(t *testing.T, c client.Client, name string, mutate func(*batchv1.Job)) {
	t.Helper()
	bj := &batchv1.Job{}
	if !exists(t, c, bj, name) {
		t.Fatal("batch Job missing")
	}
	mutate(bj)
	if err := c.Status().Update(context.Background(), bj); err != nil {
		t.Fatal(err)
	}
}

func TestReconcile_JobStatusDrivesPhase(t *testing.T) {
	job := cpuJob("life")
	job.Spec.Distributed = &gryviav1.DistributedConfig{Enabled: true, Nodes: 2}
	r, c := newAIJobReconciler(job)
	res := reconcileN(t, r, "life", 3)
	if res.RequeueAfter != 30*time.Second {
		t.Errorf("non-terminal job must requeue after 30s, got %v", res)
	}

	one := int32(1)
	start := metav1.NewTime(time.Now().Add(-time.Minute).Truncate(time.Second))
	setJobStatus(t, c, "life", func(b *batchv1.Job) {
		b.Status.Active, b.Status.Ready, b.Status.StartTime = 2, &one, &start
	})
	res = reconcileN(t, r, "life", 1)
	got := getAIJob(t, c, "life")
	if got.Status.Phase != PhaseRunning || got.Status.ReplicasReady != 1 {
		t.Fatalf("phase=%s ready=%d", got.Status.Phase, got.Status.ReplicasReady)
	}
	if got.Status.StartTime == nil || !got.Status.StartTime.Time.Equal(start.Time) {
		t.Errorf("startTime = %v, want the Job's %v", got.Status.StartTime, start)
	}
	if res.RequeueAfter == 0 {
		t.Error("running job must be requeued")
	}

	// All pods ready -> Ready condition true; a Job that lost readiness stays Running.
	two := int32(2)
	setJobStatus(t, c, "life", func(b *batchv1.Job) { b.Status.Ready = &two })
	reconcileN(t, r, "life", 1)
	got = getAIJob(t, c, "life")
	ready := false
	for _, cd := range got.Status.Conditions {
		if cd.Type == ConditionReady && cd.Status == metav1.ConditionTrue {
			ready = true
		}
	}
	if !ready {
		t.Errorf("Ready condition not true: %v", got.Status.Conditions)
	}
	zero := int32(0)
	setJobStatus(t, c, "life", func(b *batchv1.Job) { b.Status.Ready = &zero; b.Status.Active = 2 })
	reconcileN(t, r, "life", 1)
	if got = getAIJob(t, c, "life"); got.Status.Phase != PhaseRunning {
		t.Errorf("phase regressed to %s", got.Status.Phase)
	}

	// Complete -> Succeeded with start and completion times, no more requeue.
	done := metav1.NewTime(start.Add(50 * time.Second))
	setJobStatus(t, c, "life", func(b *batchv1.Job) {
		b.Status.Active, b.Status.Succeeded, b.Status.CompletionTime = 0, 2, &done
		b.Status.Conditions = []batchv1.JobCondition{condTrue(batchv1.JobComplete, "", "")}
	})
	res = reconcileN(t, r, "life", 1)
	got = getAIJob(t, c, "life")
	if got.Status.Phase != PhaseSucceeded {
		t.Fatalf("phase = %s", got.Status.Phase)
	}
	if got.Status.CompletionTime == nil || !got.Status.CompletionTime.Time.Equal(done.Time) || got.Status.StartTime == nil {
		t.Errorf("start=%v completion=%v", got.Status.StartTime, got.Status.CompletionTime)
	}
	if res.RequeueAfter != 0 || res.Requeue {
		t.Errorf("terminal job must not requeue: %+v", res)
	}
}

func TestReconcile_TerminalPhasesAreSticky(t *testing.T) {
	for _, phase := range []string{PhaseSucceeded, PhaseFailed} {
		t.Run(phase, func(t *testing.T) {
			job := cpuJob("done")
			job.Status.Phase = phase
			job.Status.Message = "kept"
			r, c := newAIJobReconciler(job)
			res := reconcileN(t, r, "done", 2)
			if res.RequeueAfter != 0 || res.Requeue {
				t.Errorf("requeue on terminal job: %+v", res)
			}
			if exists(t, c, &batchv1.Job{}, "done") || exists(t, c, &corev1.Service{}, "done-headless") {
				t.Error("a terminal job must not (re)create anything")
			}
			if got := getAIJob(t, c, "done"); got.Status.Phase != phase || got.Status.Message != "kept" {
				t.Errorf("status changed: %+v", got.Status)
			}
		})
	}
	// A Job that later reports something else cannot resurrect a terminal AIJob.
	job := cpuJob("d2")
	r, c := newAIJobReconciler(job)
	reconcileN(t, r, "d2", 3)
	setJobStatus(t, c, "d2", func(b *batchv1.Job) {
		b.Status.Conditions = []batchv1.JobCondition{condTrue(batchv1.JobFailed, "BackoffLimitExceeded", "x")}
	})
	reconcileN(t, r, "d2", 1)
	one := int32(1)
	setJobStatus(t, c, "d2", func(b *batchv1.Job) { b.Status.Conditions = nil; b.Status.Ready = &one; b.Status.Active = 1 })
	reconcileN(t, r, "d2", 2)
	if got := getAIJob(t, c, "d2"); got.Status.Phase != PhaseFailed {
		t.Errorf("Failed is sticky, got %s", got.Status.Phase)
	}
}

func TestReconcile_FailedJobCarriesMessage(t *testing.T) {
	job := cpuJob("bad")
	r, c := newAIJobReconciler(job)
	reconcileN(t, r, "bad", 3)
	setJobStatus(t, c, "bad", func(b *batchv1.Job) {
		b.Status.Failed = 1
		b.Status.Conditions = []batchv1.JobCondition{condTrue(batchv1.JobFailed, "BackoffLimitExceeded", "Job has reached the specified backoff limit")}
	})
	reconcileN(t, r, "bad", 1)
	got := getAIJob(t, c, "bad")
	if got.Status.Phase != PhaseFailed || got.Status.Retries != 1 || got.Status.CompletionTime == nil || got.Status.StartTime == nil {
		t.Errorf("status = %+v", got.Status)
	}
	if got.Status.Message == "" {
		t.Error("failed job needs a message")
	}
}

func TestReconcile_PhaseGates(t *testing.T) {
	for _, phase := range []string{PhaseQueued, PhaseRejected} {
		t.Run(phase+" creates nothing", func(t *testing.T) {
			job := newTestAIJob("held", ns) // GPU job on a cluster with a node
			job.Spec.Storage = "std"
			job.Status.Phase = phase
			r, c := newAIJobReconciler(job, gpuNode("n1"))
			res := reconcileN(t, r, "held", 3)
			if res.Requeue || res.RequeueAfter != 0 {
				t.Errorf("no requeue expected: %+v", res)
			}
			for name, obj := range map[string]client.Object{
				"held": &batchv1.Job{}, "held-training": &appsv1.StatefulSet{},
				"held-headless": &corev1.Service{}, "held-data": &corev1.PersistentVolumeClaim{},
			} {
				if exists(t, c, obj, name) {
					t.Errorf("%s exists while %s", name, phase)
				}
			}
			if got := getAIJob(t, c, "held"); got.Status.Phase != phase || len(got.Status.NodesAllocated) != 0 {
				t.Errorf("job was scheduled while %s: %+v", phase, got.Status)
			}
		})
	}
}

func TestReconcile_RejectedAfterWorkloadStartedRemovesEverything(t *testing.T) {
	job := newTestAIJob("race", ns)
	job.Spec.Storage = "std"
	r, c := newAIJobReconciler(job, gpuNode("n1"))
	reconcileN(t, r, "race", 3) // the quota operator lost the race: workload already exists
	if !exists(t, c, &batchv1.Job{}, "race") || !exists(t, c, &corev1.PersistentVolumeClaim{}, "race-data") {
		t.Fatal("setup: workload not created")
	}
	setPhase(t, c, "race", PhaseRejected)
	reconcileN(t, r, "race", 2)
	for name, obj := range map[string]client.Object{
		"race": &batchv1.Job{}, "race-headless": &corev1.Service{}, "race-data": &corev1.PersistentVolumeClaim{},
	} {
		if exists(t, c, obj, name) {
			t.Errorf("%s still exists after rejection", name)
		}
	}
}

func setPhase(t *testing.T, c client.Client, name, phase string) {
	t.Helper()
	j := getAIJob(t, c, name)
	j.Status.Phase = phase
	if err := c.Status().Update(context.Background(), j); err != nil {
		t.Fatal(err)
	}
}

func TestReconcile_CancelAndPreemptDeleteWorkloadKeepPVC(t *testing.T) {
	for _, phase := range []string{PhaseCancelled, PhasePreempted} {
		t.Run(phase, func(t *testing.T) {
			job := cpuJob("stop")
			job.Spec.Storage = "std"
			r, c := newAIJobReconciler(job)
			reconcileN(t, r, "stop", 3)
			if !exists(t, c, &batchv1.Job{}, "stop") {
				t.Fatal("setup: Job missing")
			}
			setPhase(t, c, "stop", phase)
			res := reconcileN(t, r, "stop", 2)
			if res.Requeue || res.RequeueAfter != 0 {
				t.Errorf("no requeue: %+v", res)
			}
			if exists(t, c, &batchv1.Job{}, "stop") {
				t.Error("workload must be deleted")
			}
			if !exists(t, c, &corev1.PersistentVolumeClaim{}, "stop-data") {
				t.Error("PVC must be kept")
			}
			got := getAIJob(t, c, "stop")
			if got.Status.Phase != phase || got.Status.CompletionTime == nil {
				t.Errorf("phase=%s completion=%v", got.Status.Phase, got.Status.CompletionTime)
			}
		})
	}
}

func TestReconcile_CancelAnnotation(t *testing.T) {
	job := cpuJob("ann")
	job.Spec.WorkloadKind = "statefulset"
	r, c := newAIJobReconciler(job)
	reconcileN(t, r, "ann", 3)
	if !exists(t, c, &appsv1.StatefulSet{}, "ann-training") {
		t.Fatal("setup")
	}
	j := getAIJob(t, c, "ann")
	j.Annotations = map[string]string{AnnotationCancel: "true"}
	if err := c.Update(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	reconcileN(t, r, "ann", 2)
	if exists(t, c, &appsv1.StatefulSet{}, "ann-training") {
		t.Error("StatefulSet must be deleted on cancel")
	}
	if got := getAIJob(t, c, "ann"); got.Status.Phase != PhaseCancelled {
		t.Errorf("phase = %s", got.Status.Phase)
	}

	// Cancelling a finished job changes nothing.
	fin := cpuJob("fin")
	fin.Status.Phase = PhaseSucceeded
	fin.Annotations = map[string]string{AnnotationCancel: "true"}
	r2, c2 := newAIJobReconciler(fin)
	reconcileN(t, r2, "fin", 2)
	if got := getAIJob(t, c2, "fin"); got.Status.Phase != PhaseSucceeded {
		t.Errorf("phase = %s", got.Status.Phase)
	}
}

func TestReconcile_SuspendIsCreatedAndToggled(t *testing.T) {
	job := cpuJob("sus")
	job.Spec.Suspend = true
	r, c := newAIJobReconciler(job)
	reconcileN(t, r, "sus", 3)
	bj := &batchv1.Job{}
	exists(t, c, bj, "sus")
	if !*bj.Spec.Suspend {
		t.Fatal("Job must be created suspended")
	}
	got := getAIJob(t, c, "sus")
	if got.Status.Phase != PhaseScheduling || got.Status.StartTime != nil {
		t.Errorf("suspended job: phase=%s start=%v", got.Status.Phase, got.Status.StartTime)
	}

	got.Spec.Suspend = false
	if err := c.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	reconcileN(t, r, "sus", 1)
	exists(t, c, bj, "sus")
	if *bj.Spec.Suspend {
		t.Error("clearing spec.suspend must unsuspend the Job")
	}
}

func TestReconcile_KueueLabelKeepsSuspendWithKueue(t *testing.T) {
	job := cpuJob("kq")
	job.Labels = map[string]string{LabelKueueQueue: "q"}
	r, c := newAIJobReconciler(job)
	reconcileN(t, r, "kq", 3)
	setJobStatus(t, c, "kq", func(b *batchv1.Job) {}) // no-op
	bj := &batchv1.Job{}
	exists(t, c, bj, "kq")
	f := false
	bj.Spec.Suspend = &f // Kueue admitted it
	if err := c.Update(context.Background(), bj); err != nil {
		t.Fatal(err)
	}
	reconcileN(t, r, "kq", 2)
	exists(t, c, bj, "kq")
	if *bj.Spec.Suspend {
		t.Error("the controller must not re-suspend a Job Kueue admitted")
	}
}

func TestReconcile_GPUJobStillNeedsNodes(t *testing.T) {
	job := newTestAIJob("gpu", ns)
	r, c := newAIJobReconciler(job)
	for i := 0; i < 3; i++ {
		_, _ = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "gpu"}})
	}
	if exists(t, c, &batchv1.Job{}, "gpu") {
		t.Error("GPU behaviour unchanged: no eligible node, no workload")
	}
	if got := getAIJob(t, c, "gpu"); got.Status.Phase != PhasePending {
		t.Errorf("phase = %s", got.Status.Phase)
	}
	// With a node the Job requests the GPUs.
	r, c = newAIJobReconciler(newTestAIJob("gpu2", ns), gpuNode("n1"))
	reconcileN(t, r, "gpu2", 3)
	bj := &batchv1.Job{}
	exists(t, c, bj, "gpu2")
	if g := bj.Spec.Template.Spec.Containers[0].Resources.Limits["nvidia.com/gpu"]; g.Value() != 4 {
		t.Errorf("gpu limit = %v", g.Value())
	}
	if bj.Spec.Template.Spec.NodeSelector["gryvia.io/gpu"] != "H100" {
		t.Error("GPU node selector lost")
	}
}

func TestReconcile_InvalidTimeoutFailsTheJob(t *testing.T) {
	job := cpuJob("to")
	job.Spec.Timeout = "eventually"
	r, c := newAIJobReconciler(job)
	reconcileN(t, r, "to", 3)
	got := getAIJob(t, c, "to")
	if got.Status.Phase != PhaseFailed || got.Status.Message == "" {
		t.Errorf("status = %+v", got.Status)
	}
	if exists(t, c, &batchv1.Job{}, "to") {
		t.Error("no Job for an invalid spec")
	}
}

func TestResolveWorkloadKind_ExistingJobWins(t *testing.T) {
	job := cpuJob("ex")
	job.Spec.Type = "inference" // default would be statefulset
	r, c := newAIJobReconciler(job)
	bj, _ := r.buildJob(job)
	_ = controllerutil.SetControllerReference(getAIJob(t, c, "ex"), bj, r.Scheme)
	if err := c.Create(context.Background(), bj); err != nil {
		t.Fatal(err)
	}
	kind, err := r.resolveWorkloadKind(context.Background(), getAIJob(t, c, "ex"))
	if err != nil || kind != gryviav1.WorkloadKindJob {
		t.Errorf("kind = %s, err = %v", kind, err)
	}
}

func TestTimeoutSeconds(t *testing.T) {
	cases := map[string]int64{"90m": 5400, "24h": 86400, "7d": 604800, "1d12h": 129600, "30s": 30}
	for in, want := range cases {
		got, err := gryviav1.GryviaAIJobSpec{Timeout: in}.TimeoutSeconds()
		if err != nil || got == nil || *got != want {
			t.Errorf("%q -> %v, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"soon", "-1d", "0s", "d", "xd"} {
		if _, err := (gryviav1.GryviaAIJobSpec{Timeout: bad}).TimeoutSeconds(); err == nil {
			t.Errorf("%q should be invalid", bad)
		}
	}
	if got, err := (gryviav1.GryviaAIJobSpec{}).TimeoutSeconds(); got != nil || err != nil {
		t.Error("empty timeout means none")
	}
}
