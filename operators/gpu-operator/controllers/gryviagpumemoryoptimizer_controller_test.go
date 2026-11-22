// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gryviav1 "github.com/zyvorai/gryvia/operators/gpu-operator/api/v1"
)

func newMemoryOptimizerTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = gryviav1.AddToScheme(s)
	return s
}

func newMemoryOptimizerReconciler(objs ...client.Object) (*GryviaGpuMemoryOptimizerReconciler, client.Client) {
	scheme := newMemoryOptimizerTestScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&gryviav1.GryviaGpuMemoryOptimizer{}).
		Build()
	r := &GryviaGpuMemoryOptimizerReconciler{
		Client: fakeClient,
		Scheme: scheme,
		Log:    ctrl.Log.WithName("test"),
	}
	return r, fakeClient
}

func newTestOptimizer(name string) *gryviav1.GryviaGpuMemoryOptimizer {
	return &gryviav1.GryviaGpuMemoryOptimizer{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: gryviav1.GryviaGpuMemoryOptimizerSpec{
			Scope: gryviav1.OptimizerScope{
				Type: "cluster",
			},
			OomPrevention: &gryviav1.OomPreventionConfig{
				Enabled: true,
			},
			RightSizing: &gryviav1.RightSizingConfig{
				Enabled: true,
			},
		},
	}
}

func TestMemoryOptimizer_Reconcile_NotFound(t *testing.T) {
	r, _ := newMemoryOptimizerReconciler()

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "nonexistent"}}
	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("expected no error for not-found resource, got %v", err)
	}
	if result.Requeue || result.RequeueAfter > 0 {
		t.Errorf("expected no requeue for not-found resource")
	}
}

func TestMemoryOptimizer_Reconcile_DeletionTimestamp(t *testing.T) {
	now := metav1.Now()
	optimizer := newTestOptimizer("test-optimizer")
	optimizer.DeletionTimestamp = &now
	optimizer.Finalizers = []string{"keep-for-test"}

	r, _ := newMemoryOptimizerReconciler(optimizer)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-optimizer"}}
	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("expected no error during deletion, got %v", err)
	}
	if result.Requeue || result.RequeueAfter > 0 {
		t.Error("expected no requeue for deleted resource")
	}
}

func TestMemoryOptimizer_Reconcile_CollectsGpuNodes(t *testing.T) {
	optimizer := newTestOptimizer("test-optimizer")

	gpuNode1 := &gryviav1.GryviaGpuNode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "gpu-node-1",
		},
		Spec: gryviav1.GryviaGpuNodeSpec{
			NodeName: "node-1",
			GpuType:  "H100",
			GpuCount: 8,
		},
		Status: gryviav1.GryviaGpuNodeStatus{
			Phase: "Ready",
			GpuStatus: []gryviav1.GpuStatus{
				{
					Index:       0,
					UUID:        "GPU-1234",
					Health:      "Healthy",
					MemoryUsed:  30000,
					MemoryTotal: 80000,
					Utilization: 75,
				},
			},
		},
	}

	gpuNode2 := &gryviav1.GryviaGpuNode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "gpu-node-2",
		},
		Spec: gryviav1.GryviaGpuNodeSpec{
			NodeName: "node-2",
			GpuType:  "A100",
			GpuCount: 4,
		},
		Status: gryviav1.GryviaGpuNodeStatus{
			Phase: "Ready",
			GpuStatus: []gryviav1.GpuStatus{
				{
					Index:       0,
					UUID:        "GPU-5678",
					Health:      "Healthy",
					MemoryUsed:  20000,
					MemoryTotal: 40000,
					Utilization: 50,
				},
			},
		},
	}

	r, _ := newMemoryOptimizerReconciler(optimizer, gpuNode1, gpuNode2)

	nodes, err := r.collectGpuNodes(context.Background(), optimizer)
	if err != nil {
		t.Fatalf("expected no error collecting GPU nodes, got %v", err)
	}
	if len(nodes) != 2 {
		t.Errorf("expected 2 GPU nodes, got %d", len(nodes))
	}
}

func TestMemoryOptimizer_NodeMatchesScope(t *testing.T) {
	r, _ := newMemoryOptimizerReconciler()

	node := gryviav1.GryviaGpuNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-1",
			Labels: map[string]string{"team": "ml"},
		},
		Spec: gryviav1.GryviaGpuNodeSpec{
			NodeName: "node-1",
			GpuType:  "H100",
		},
	}

	tests := []struct {
		name     string
		scope    gryviav1.OptimizerScope
		expected bool
	}{
		{
			name:     "cluster scope matches all",
			scope:    gryviav1.OptimizerScope{Type: "cluster"},
			expected: true,
		},
		{
			name:     "namespace scope without label matches (cluster-scoped nodes serve all)",
			scope:    gryviav1.OptimizerScope{Type: "namespace", Namespace: "ml-team"},
			expected: true,
		},
		{
			name: "job-selector matches",
			scope: gryviav1.OptimizerScope{
				Type: "job-selector",
				JobSelector: &gryviav1.JobSelector{
					MatchLabels: map[string]string{"team": "ml"},
				},
			},
			expected: true,
		},
		{
			name: "job-selector does not match",
			scope: gryviav1.OptimizerScope{
				Type: "job-selector",
				JobSelector: &gryviav1.JobSelector{
					MatchLabels: map[string]string{"team": "other"},
				},
			},
			expected: false,
		},
		{
			name:     "unknown scope matches all",
			scope:    gryviav1.OptimizerScope{Type: "unknown"},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := r.nodeMatchesScope(node, tt.scope)
			if result != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, result)
			}
		})
	}
}

func TestMemoryOptimizer_ExtractMemorySamples(t *testing.T) {
	r, _ := newMemoryOptimizerReconciler()

	nodes := []gryviav1.GryviaGpuNode{
		{
			Spec: gryviav1.GryviaGpuNodeSpec{
				NodeName: "node-1",
				GpuType:  "H100",
			},
			Status: gryviav1.GryviaGpuNodeStatus{
				GpuStatus: []gryviav1.GpuStatus{
					{Index: 0, MemoryUsed: 30000, MemoryTotal: 80000, Utilization: 75},
					{Index: 1, MemoryUsed: 0, MemoryTotal: 0, Utilization: 0}, // Should be skipped
				},
			},
		},
		{
			Spec: gryviav1.GryviaGpuNodeSpec{
				NodeName: "node-2",
				GpuType:  "A100",
			},
			Status: gryviav1.GryviaGpuNodeStatus{
				GpuStatus: []gryviav1.GpuStatus{
					{Index: 0, MemoryUsed: 20000, MemoryTotal: 40000, Utilization: 50},
				},
			},
		},
	}

	samples := r.extractMemorySamples(nodes)
	// GPU with MemoryTotal <= 0 should be skipped
	if len(samples) != 2 {
		t.Errorf("expected 2 samples (one skipped with 0 memory), got %d", len(samples))
	}

	if samples[0].NodeName != "node-1" {
		t.Errorf("expected first sample from node-1, got %s", samples[0].NodeName)
	}
	if samples[0].GpuType != "H100" {
		t.Errorf("expected GPU type H100, got %s", samples[0].GpuType)
	}
}

func TestMemoryOptimizer_UpdateMemoryEfficiency(t *testing.T) {
	r, _ := newMemoryOptimizerReconciler()
	optimizer := newTestOptimizer("test")

	// Empty samples - no update
	r.updateMemoryEfficiency(optimizer, nil)
	if optimizer.Status.MemoryEfficiency != nil {
		t.Error("expected nil MemoryEfficiency with empty samples")
	}

	// This test doesn't use the memory package, it tests the local updateMemoryEfficiency directly
	// We can't construct memory.GpuMemorySample without importing the package,
	// but updateMemoryEfficiency is called with extracted samples during reconciliation.
}

func TestMemoryOptimizer_UpdateCondition(t *testing.T) {
	r, _ := newMemoryOptimizerReconciler()
	optimizer := newTestOptimizer("test")

	// Add a condition
	r.updateCondition(optimizer, ConditionAnalysisRunning, metav1.ConditionTrue, "Running", "Analysis active")
	if len(optimizer.Status.Conditions) != 1 {
		t.Fatalf("expected 1 condition, got %d", len(optimizer.Status.Conditions))
	}

	// Update same condition type
	r.updateCondition(optimizer, ConditionAnalysisRunning, metav1.ConditionFalse, "Stopped", "Analysis stopped")
	if len(optimizer.Status.Conditions) != 1 {
		t.Fatalf("expected 1 condition after update, got %d", len(optimizer.Status.Conditions))
	}
	if optimizer.Status.Conditions[0].Status != metav1.ConditionFalse {
		t.Error("expected condition to be False after update")
	}

	// Add different condition type
	r.updateCondition(optimizer, ConditionOomMonitoring, metav1.ConditionTrue, "Active", "OOM monitoring")
	if len(optimizer.Status.Conditions) != 2 {
		t.Fatalf("expected 2 conditions, got %d", len(optimizer.Status.Conditions))
	}
}
