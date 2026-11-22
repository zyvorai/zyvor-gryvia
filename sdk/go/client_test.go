// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package gryvia

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

func newTestClient(t *testing.T) *GryviaClient {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := gryviav1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return NewClientFromExisting(fake.NewClientBuilder().WithScheme(scheme).Build())
}

func testJob(namespace, name string) *GryviaAIJob {
	return &GryviaAIJob{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       gryviav1.GryviaAIJobSpec{Type: "training", GPUs: 2, Image: "busybox:1.36"},
	}
}

func TestJobLifecycle(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)

	if err := c.CreateJob(ctx, testJob("ml", "train")); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := c.GetJob(ctx, "ml", "train")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Spec.GPUs != 2 || got.Spec.Type != "training" {
		t.Fatalf("unexpected spec: %+v", got.Spec)
	}

	if err := c.CreateJob(ctx, testJob("other", "elsewhere")); err != nil {
		t.Fatalf("create other: %v", err)
	}
	list, err := c.ListJobs(ctx, "ml")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Items) != 1 || list.Items[0].Name != "train" {
		t.Fatalf("list must be scoped to the namespace, got %d items", len(list.Items))
	}

	if err := c.DeleteJob(ctx, "ml", "train"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := c.GetJob(ctx, "ml", "train"); !apierrors.IsNotFound(err) {
		t.Fatalf("expected NotFound after delete, got %v", err)
	}
}

func TestGetMissingJobIsNotFound(t *testing.T) {
	if _, err := newTestClient(t).GetJob(context.Background(), "ml", "nope"); !apierrors.IsNotFound(err) {
		t.Fatalf("expected NotFound, got %v", err)
	}
}

func TestWatchJobExplainsTheLimitation(t *testing.T) {
	if _, err := newTestClient(t).WatchJob(context.Background(), "ml"); err == nil {
		t.Fatal("WatchJob must return an error explaining that an informer-backed client is needed")
	}
}
