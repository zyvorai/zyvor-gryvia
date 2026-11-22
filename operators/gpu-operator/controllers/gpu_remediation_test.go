// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	gryviav1 "github.com/zyvorai/gryvia/operators/gpu-operator/api/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
	"time"
)

type pdbClient struct {
	client.Client
	attempts int
}
type pdbResource struct {
	client.SubResourceClient
	c *pdbClient
}

func (c *pdbClient) SubResource(name string) client.SubResourceClient {
	return &pdbResource{SubResourceClient: c.Client.SubResource(name), c: c}
}
func (r *pdbResource) Create(ctx context.Context, obj client.Object, sub client.Object, opts ...client.SubResourceCreateOption) error {
	r.c.attempts++
	return apierrors.NewTooManyRequests("PDB blocks eviction", 1)
}
func TestQuarantineOptInAndPDB(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu"}}
	controller := true
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "ns", UID: "pod", OwnerReferences: []metav1.OwnerReference{{Kind: "Job", Name: "training", Controller: &controller}}}, Spec: corev1.PodSpec{NodeName: "gpu"}}
	base := fake.NewClientBuilder().WithScheme(newGpuNodeTestScheme()).WithObjects(node, pod).Build()
	c := &pdbClient{Client: base}
	r := &GryviaHealthCheckReconciler{Client: c}
	hc := &gryviav1.GryviaHealthCheck{ObjectMeta: metav1.ObjectMeta{Name: "health", UID: "h"}, Spec: gryviav1.GryviaHealthCheckSpec{OnFailure: &gryviav1.FailureActions{Cordon: true, Drain: true}}}
	affected := []gryviav1.AffectedResource{{Type: "node", Name: "gpu"}}
	if err := r.handleFailure(context.Background(), hc, []corev1.Node{*node}, affected); err != nil {
		t.Fatal(err)
	}
	if c.attempts != 0 {
		t.Fatal("default mutated workloads")
	}
	r.EnableRemediation = true
	if err := r.handleFailure(context.Background(), hc, []corev1.Node{*node}, affected); err == nil {
		t.Fatal("ignored PDB denial")
	}
	if c.attempts != 1 {
		t.Fatal("did not use eviction subresource")
	}
	if err := base.Get(context.Background(), types.NamespacedName{Name: "gpu"}, node); err != nil {
		t.Fatal(err)
	}
	if !node.Spec.Unschedulable || len(node.Spec.Taints) != 1 {
		t.Fatal("failed GPU not quarantined")
	}
	if err := base.Get(context.Background(), types.NamespacedName{Name: "worker", Namespace: "ns"}, pod); err != nil {
		t.Fatal("force deleted pod")
	}
}
func TestNoMetricsCannotPassHealth(t *testing.T) {
	r := &GryviaHealthCheckReconciler{}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu"}}
	gpu := &gryviav1.GryviaGpuNode{}
	check := gryviav1.HealthCheck{Type: "gpu-temperature", Enabled: true}
	if result := r.runCheck(check, gpu, node); result.Status != checkStatusWarning {
		t.Fatal("missing data passed")
	}
	old := metav1.NewTime(time.Now().Add(-time.Hour))
	gpu.Status.LastHealthCheck = &old
	if result := r.runCheck(check, gpu, node); result.Status == checkStatusFail {
		t.Fatal("stale data caused remediation")
	}
}
