// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
)

func TestPriorityCreatesOwnedClassWithoutInventingPreemption(t *testing.T) {
	p := &gryviav1.GryviaPriority{ObjectMeta: metav1.ObjectMeta{Name: "urgent", UID: "p"}, Spec: gryviav1.GryviaPrioritySpec{Value: 100, PreemptionPolicy: "Never"}}
	scheme := mlScheme()
	_ = schedulingv1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(p).WithStatusSubresource(p).Build()
	r := &GryviaPriorityReconciler{Client: c, Scheme: scheme}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: p.Name}}); err != nil {
		t.Fatal(err)
	}
	out := &schedulingv1.PriorityClass{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: p.Name}, out); err != nil {
		t.Fatal(err)
	}
	if out.Value != 100 || *out.PreemptionPolicy != corev1.PreemptNever || !metav1.IsControlledBy(out, p) {
		t.Fatal("priority contract not applied")
	}
}
func TestUnsupportedAPIReportsFailureWithoutActing(t *testing.T) {
	p := &gryviav1.GryviaJobHook{ObjectMeta: metav1.ObjectMeta{Name: "legacy", Namespace: "ns", Generation: 2}}
	c := fake.NewClientBuilder().WithScheme(mlScheme()).WithObjects(p).WithStatusSubresource(p).Build()
	r := &apiContract{Client: c, Prototype: &gryviav1.GryviaJobHook{}}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: p.Name, Namespace: p.Namespace}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), req.NamespacedName, p); err != nil {
		t.Fatal(err)
	}
	if cond := findCond(p.Status.Conditions, "Ready"); cond == nil || cond.Reason != "UnsupportedAPI" || cond.Status != metav1.ConditionFalse {
		t.Fatal("unsupported API silently accepted")
	}
}
func TestTopologyValidationAndPropagation(t *testing.T) {
	j := cpuJob("train")
	j.Annotations = map[string]string{"gryvia.io/required-topology": "topology.kubernetes.io/zone"}
	r := &GryviaAIJobReconciler{}
	b, err := r.buildJob(j)
	if err != nil {
		t.Fatal(err)
	}
	if b.Spec.Template.Annotations["kueue.x-k8s.io/podset-required-topology"] != "topology.kubernetes.io/zone" {
		t.Fatal("topology lost")
	}
	j.Annotations["gryvia.io/preferred-topology"] = "kubernetes.io/hostname"
	if _, err := r.buildJob(j); err == nil {
		t.Fatal("conflicting topology accepted")
	}
}
