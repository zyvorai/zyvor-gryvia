package controllers

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

func newAIJobTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = gryviav1.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	_ = appsv1.AddToScheme(s)
	_ = batchv1.AddToScheme(s)
	_ = rbacv1.AddToScheme(s)
	return s
}

func newAIJobReconciler(objs ...client.Object) (*GryviaAIJobReconciler, client.Client) {
	scheme := newAIJobTestScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&gryviav1.GryviaAIJob{}, &appsv1.StatefulSet{}, &batchv1.Job{}).
		Build()
	r := &GryviaAIJobReconciler{
		Client: fakeClient,
		Scheme: scheme,
		Log:    ctrl.Log.WithName("test"),
	}
	return r, fakeClient
}

func newTestAIJob(name, namespace string) *gryviav1.GryviaAIJob {
	return &gryviav1.GryviaAIJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: gryviav1.GryviaAIJobSpec{
			Type:    "training",
			Image:   "pytorch/pytorch:latest",
			GPUs:    4,
			GpuType: "H100",
		},
	}
}

func TestAIJob_Reconcile_NotFound(t *testing.T) {
	r, _ := newAIJobReconciler()

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "nonexistent", Namespace: "default"}}
	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("expected no error for not-found resource, got %v", err)
	}
	if result.Requeue || result.RequeueAfter > 0 {
		t.Errorf("expected no requeue for not-found resource")
	}
}

func TestAIJob_Reconcile_InitializesStatus(t *testing.T) {
	job := newTestAIJob("test-job", "default")
	r, fakeClient := newAIJobReconciler(job)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-job", Namespace: "default"}}
	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if !result.Requeue {
		t.Error("expected requeue after status initialization")
	}

	updated := &gryviav1.GryviaAIJob{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-job", Namespace: "default"}, updated); err != nil {
		t.Fatalf("failed to get updated job: %v", err)
	}
	if updated.Status.Phase != PhasePending {
		t.Errorf("expected phase %s, got %s", PhasePending, updated.Status.Phase)
	}
}

func TestAIJob_Reconcile_DeletionTimestamp(t *testing.T) {
	now := metav1.Now()
	job := newTestAIJob("test-job", "default")
	job.DeletionTimestamp = &now
	job.Finalizers = []string{"keep-for-test"}

	r, _ := newAIJobReconciler(job)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-job", Namespace: "default"}}
	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("expected no error during deletion, got %v", err)
	}
	if result.Requeue || result.RequeueAfter > 0 {
		t.Error("expected no requeue for deleted resource")
	}
}

func TestAIJob_GetReplicaCount(t *testing.T) {
	r, _ := newAIJobReconciler()

	tests := []struct {
		name     string
		job      *gryviav1.GryviaAIJob
		expected int32
	}{
		{
			name:     "non-distributed returns 1",
			job:      newTestAIJob("test", "default"),
			expected: 1,
		},
		{
			name: "distributed with nodes",
			job: &gryviav1.GryviaAIJob{
				Spec: gryviav1.GryviaAIJobSpec{
					Distributed: &gryviav1.DistributedConfig{
						Enabled: true,
						Nodes:   4,
					},
				},
			},
			expected: 4,
		},
		{
			name: "distributed without nodes defaults to 1",
			job: &gryviav1.GryviaAIJob{
				Spec: gryviav1.GryviaAIJobSpec{
					Distributed: &gryviav1.DistributedConfig{
						Enabled: true,
					},
				},
			},
			expected: 1,
		},
		{
			name: "distributed disabled returns 1",
			job: &gryviav1.GryviaAIJob{
				Spec: gryviav1.GryviaAIJobSpec{
					Distributed: &gryviav1.DistributedConfig{
						Enabled: false,
						Nodes:   4,
					},
				},
			},
			expected: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := r.getReplicaCount(tt.job)
			if result != tt.expected {
				t.Errorf("expected %d, got %d", tt.expected, result)
			}
		})
	}
}

func TestAIJob_GetGPUsPerPod(t *testing.T) {
	r, _ := newAIJobReconciler()

	tests := []struct {
		name     string
		job      *gryviav1.GryviaAIJob
		expected int32
	}{
		{
			name:     "non-distributed uses spec GPUs",
			job:      &gryviav1.GryviaAIJob{Spec: gryviav1.GryviaAIJobSpec{GPUs: 4}},
			expected: 4,
		},
		{
			name:     "non-distributed zero GPUs is CPU-only",
			job:      &gryviav1.GryviaAIJob{Spec: gryviav1.GryviaAIJobSpec{GPUs: 0}},
			expected: 0,
		},
		{
			name:     "negative GPUs falls back to 1",
			job:      &gryviav1.GryviaAIJob{Spec: gryviav1.GryviaAIJobSpec{GPUs: -1}},
			expected: 1,
		},
		{
			name: "distributed uses GpusPerNode",
			job: &gryviav1.GryviaAIJob{
				Spec: gryviav1.GryviaAIJobSpec{
					GPUs: 8,
					Distributed: &gryviav1.DistributedConfig{
						Enabled:     true,
						GpusPerNode: 4,
					},
				},
			},
			expected: 4,
		},
		{
			name: "distributed without GpusPerNode defaults to 1",
			job: &gryviav1.GryviaAIJob{
				Spec: gryviav1.GryviaAIJobSpec{
					GPUs: 8,
					Distributed: &gryviav1.DistributedConfig{
						Enabled: true,
					},
				},
			},
			expected: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := r.getGPUsPerPod(tt.job)
			if result != tt.expected {
				t.Errorf("expected %d, got %d", tt.expected, result)
			}
		})
	}
}

func TestAIJob_BuildNodeSelector(t *testing.T) {
	r, _ := newAIJobReconciler()

	job := newTestAIJob("test", "default")
	job.Spec.GpuType = "H100"
	job.Spec.Network = "rdma"
	job.Spec.NodeSelector = map[string]string{"zone": "us-east-1a"}

	selector := r.buildNodeSelector(job)
	if selector["gryvia.io/gpu"] != "H100" {
		t.Errorf("expected GPU selector H100, got %s", selector["gryvia.io/gpu"])
	}
	if selector["gryvia.io/rdma"] != "true" {
		t.Errorf("expected RDMA selector true, got %s", selector["gryvia.io/rdma"])
	}
	if selector["zone"] != "us-east-1a" {
		t.Errorf("expected zone selector us-east-1a, got %s", selector["zone"])
	}

	// Test "any" GPU type - should not set GPU selector
	job.Spec.GpuType = "any"
	job.Spec.Network = "standard"
	selector = r.buildNodeSelector(job)
	if _, ok := selector["gryvia.io/gpu"]; ok {
		t.Error("expected no GPU selector for 'any' GPU type")
	}
	if _, ok := selector["gryvia.io/rdma"]; ok {
		t.Error("expected no RDMA selector for standard network")
	}
}

func TestAIJob_BuildEnvVars(t *testing.T) {
	r, _ := newAIJobReconciler()

	// Non-distributed: only user env vars
	job := newTestAIJob("test", "default")
	job.Spec.Env = []corev1.EnvVar{
		{Name: "MY_VAR", Value: "my_value"},
	}
	envVars := r.buildEnvVars(job)
	if len(envVars) != 1 {
		t.Errorf("expected 1 env var for non-distributed job, got %d", len(envVars))
	}

	// Distributed: should add MASTER_ADDR, MASTER_PORT, WORLD_SIZE, NCCL_DEBUG
	job.Spec.Distributed = &gryviav1.DistributedConfig{
		Enabled:     true,
		Nodes:       2,
		GpusPerNode: 4,
	}
	envVars = r.buildEnvVars(job)
	foundMasterAddr := false
	foundWorldSize := false
	for _, ev := range envVars {
		if ev.Name == "MASTER_ADDR" {
			foundMasterAddr = true
		}
		if ev.Name == "WORLD_SIZE" {
			foundWorldSize = true
			if ev.Value != "8" { // 2 nodes * 4 GPUs
				t.Errorf("expected WORLD_SIZE=8, got %s", ev.Value)
			}
		}
	}
	if !foundMasterAddr {
		t.Error("expected MASTER_ADDR env var for distributed job")
	}
	if !foundWorldSize {
		t.Error("expected WORLD_SIZE env var for distributed job")
	}

	// RDMA network should add NCCL IB vars
	job.Spec.Network = "rdma"
	envVars = r.buildEnvVars(job)
	foundNCCLIB := false
	for _, ev := range envVars {
		if ev.Name == "NCCL_IB_DISABLE" && ev.Value == "0" {
			foundNCCLIB = true
		}
	}
	if !foundNCCLIB {
		t.Error("expected NCCL_IB_DISABLE=0 for RDMA network")
	}
}

func TestAIJob_BuildVolumeMounts(t *testing.T) {
	r, _ := newAIJobReconciler()

	// With storage - should add data volume mount
	job := newTestAIJob("test", "default")
	job.Spec.Storage = "fast-storage"
	mounts := r.buildVolumeMounts(job)
	foundData := false
	for _, m := range mounts {
		if m.Name == "data" && m.MountPath == "/data" {
			foundData = true
		}
	}
	if !foundData {
		t.Error("expected data volume mount when storage is specified")
	}

	// Distributed - should add shared memory
	job.Spec.Distributed = &gryviav1.DistributedConfig{Enabled: true}
	mounts = r.buildVolumeMounts(job)
	foundShm := false
	for _, m := range mounts {
		if m.Name == "shm" && m.MountPath == "/dev/shm" {
			foundShm = true
		}
	}
	if !foundShm {
		t.Error("expected shared memory volume mount for distributed job")
	}
}

func TestAIJob_BuildStatefulSet(t *testing.T) {
	r, _ := newAIJobReconciler()

	job := newTestAIJob("test-job", "default")
	job.Spec.Network = "rdma"

	sts := r.buildStatefulSet(job)
	if sts.Name != "test-job-training" {
		t.Errorf("expected StatefulSet name test-job-training, got %s", sts.Name)
	}
	if *sts.Spec.Replicas != 1 {
		t.Errorf("expected 1 replica, got %d", *sts.Spec.Replicas)
	}
	if sts.Spec.Template.Spec.Containers[0].Image != "pytorch/pytorch:latest" {
		t.Errorf("expected image pytorch/pytorch:latest, got %s", sts.Spec.Template.Spec.Containers[0].Image)
	}

	// Check RDMA annotation
	if sts.Spec.Template.Annotations["gryvia.io/rdma"] != "true" {
		t.Error("expected RDMA annotation on pod template")
	}

	// Check GPU resource limits
	gpuLimit := sts.Spec.Template.Spec.Containers[0].Resources.Limits["nvidia.com/gpu"]
	if gpuLimit.Value() != 4 {
		t.Errorf("expected 4 GPU limit, got %d", gpuLimit.Value())
	}
}

func TestAIJob_UpdateCondition(t *testing.T) {
	r, _ := newAIJobReconciler()
	job := newTestAIJob("test", "default")

	// Add new condition
	r.updateCondition(job, ConditionScheduled, metav1.ConditionTrue, "Scheduled", "OK")
	if len(job.Status.Conditions) != 1 {
		t.Fatalf("expected 1 condition, got %d", len(job.Status.Conditions))
	}

	// Update same condition
	r.updateCondition(job, ConditionScheduled, metav1.ConditionFalse, "Failed", "No nodes")
	if len(job.Status.Conditions) != 1 {
		t.Fatalf("expected still 1 condition, got %d", len(job.Status.Conditions))
	}
	if job.Status.Conditions[0].Status != metav1.ConditionFalse {
		t.Error("expected condition status False")
	}

	// Add different condition
	r.updateCondition(job, ConditionReady, metav1.ConditionTrue, "Ready", "All ready")
	if len(job.Status.Conditions) != 2 {
		t.Fatalf("expected 2 conditions, got %d", len(job.Status.Conditions))
	}
}

func TestAIJob_StringSlicesEqual(t *testing.T) {
	if !stringSlicesEqual(nil, nil) {
		t.Error("nil slices should be equal")
	}
	if !stringSlicesEqual([]string{}, []string{}) {
		t.Error("empty slices should be equal")
	}
	if !stringSlicesEqual([]string{"a", "b"}, []string{"a", "b"}) {
		t.Error("equal slices should be equal")
	}
	if stringSlicesEqual([]string{"a"}, []string{"a", "b"}) {
		t.Error("different length slices should not be equal")
	}
	if stringSlicesEqual([]string{"a", "b"}, []string{"a", "c"}) {
		t.Error("different content slices should not be equal")
	}
}

func TestAIJob_MapsEqual(t *testing.T) {
	if !mapsEqual(nil, nil) {
		t.Error("nil maps should be equal")
	}
	if !mapsEqual(map[string]string{}, map[string]string{}) {
		t.Error("empty maps should be equal")
	}
	if !mapsEqual(map[string]string{"a": "1"}, map[string]string{"a": "1"}) {
		t.Error("equal maps should be equal")
	}
	if mapsEqual(map[string]string{"a": "1"}, map[string]string{"a": "2"}) {
		t.Error("maps with different values should not be equal")
	}
	if mapsEqual(map[string]string{"a": "1"}, map[string]string{"b": "1"}) {
		t.Error("maps with different keys should not be equal")
	}
}

func TestAIJob_GetStatefulSetName(t *testing.T) {
	r, _ := newAIJobReconciler()
	job := newTestAIJob("my-training-job", "default")
	name := r.getStatefulSetName(job)
	if name != "my-training-job-training" {
		t.Errorf("expected my-training-job-training, got %s", name)
	}
}
