// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controllers

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

func fabricMergeFixture(t *testing.T, status gryviav1.GryviaFabricSignalStatus, now time.Time) (*GryviaFabricSignalReconciler, client.Client) {
	t.Helper()
	s := runtime.NewScheme()
	_ = gryviav1.AddToScheme(s)
	sig := &gryviav1.GryviaFabricSignal{
		ObjectMeta: metav1.ObjectMeta{Name: "train-fabric", Namespace: "ml"},
		Spec:       gryviav1.GryviaFabricSignalSpec{JobRef: "train"},
		Status:     status,
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(sig).WithStatusSubresource(&gryviav1.GryviaFabricSignal{}).Build()
	return &GryviaFabricSignalReconciler{Client: c, Scheme: s, Now: func() time.Time { return now }}, c
}

func fp(v float64) *float64 { return &v }

func reconcileFabric(t *testing.T, r *GryviaFabricSignalReconciler) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ml", Name: "train-fabric"}})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func getFabric(t *testing.T, c client.Client) *gryviav1.GryviaFabricSignal {
	t.Helper()
	out := &gryviav1.GryviaFabricSignal{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ml", Name: "train-fabric"}, out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestFabricSignalMergerFoldsFreshNodes(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	rank3 := int32(3)
	st := gryviav1.GryviaFabricSignalStatus{
		NCCLP99Ms: 1, ScoreDelta: 0.9, // stale merged values from an earlier cycle are replaced
		Nodes: []gryviav1.FabricNodeStatus{
			{Node: "a", MeasuredAt: metav1.NewTime(now.Add(-10 * time.Second)), SampleCount: 10, NCCLP99Ms: fp(80), StragglerRank: &rank3, GDSHitRatio: fp(0.5)},
			{Node: "b", MeasuredAt: metav1.NewTime(now.Add(-20 * time.Second)), SampleCount: 30, RDMARetryRate: fp(0.05), GDSHitRatio: fp(1)},
			{Node: "dead", MeasuredAt: metav1.NewTime(now.Add(-time.Hour)), NCCLP99Ms: fp(999), ScoreDelta: fp(1)},
		},
	}
	r, c := fabricMergeFixture(t, st, now)
	res := reconcileFabric(t, r)
	got := getFabric(t, c)
	if got.Status.NCCLP99Ms != 80 || got.Status.RDMARetryRate != 0.05 || got.Status.StragglerRank != 3 || got.Status.ScoreDelta != 0 {
		t.Errorf("%+v", got.Status)
	}
	if want := (0.5*10 + 1*30) / 40; got.Status.GDSHitRatio != want {
		t.Errorf("gds %v", got.Status.GDSHitRatio)
	}
	if got.Status.UpdatedAt == nil || !got.Status.UpdatedAt.Time.Equal(now.Add(-10*time.Second)) || len(got.Status.Nodes) != 3 {
		t.Errorf("%+v", got.Status)
	}
	if res.RequeueAfter < 5*time.Second || res.RequeueAfter > 5*time.Minute {
		t.Errorf("requeue %v", res.RequeueAfter)
	}
	// Idempotent.
	rv := got.ResourceVersion
	reconcileFabric(t, r)
	if getFabric(t, c).ResourceVersion != rv {
		t.Error("second reconcile wrote again")
	}
}

func TestFabricSignalMergerLeavesStatusWhenAllStaleOrNoNodes(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	for name, st := range map[string]gryviav1.GryviaFabricSignalStatus{
		"no nodes (legacy collectors)": {NCCLP99Ms: 42},
		"all stale": {NCCLP99Ms: 42, Nodes: []gryviav1.FabricNodeStatus{
			{Node: "a", MeasuredAt: metav1.NewTime(now.Add(-time.Hour)), NCCLP99Ms: fp(1)}}},
	} {
		t.Run(name, func(t *testing.T) {
			r, c := fabricMergeFixture(t, st, now)
			rv := getFabric(t, c).ResourceVersion
			reconcileFabric(t, r)
			got := getFabric(t, c)
			if got.Status.NCCLP99Ms != 42 || got.ResourceVersion != rv {
				t.Errorf("status changed: %+v", got.Status)
			}
		})
	}
}

func TestFabricSignalMergerMissingObject(t *testing.T) {
	r, c := fabricMergeFixture(t, gryviav1.GryviaFabricSignalStatus{}, time.Now())
	_ = c.Delete(context.Background(), getFabric(t, c))
	reconcileFabric(t, r)
}
