// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controllers

import (
	"context"
	"fmt"
	gryviav1 "github.com/zyvorai/gryvia/operators/storage-operator/api/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
)

func TestLegacyCapabilityConditionsSurviveTypedConversion(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := gryviav1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	for _, obj := range []client.Object{&gryviav1.GryviaDataset{}} {
		t.Run(string(obj.GetObjectKind().GroupVersionKind().Kind)+reflectName(obj), func(t *testing.T) {
			obj.SetName("legacy")
			obj.SetNamespace("test")
			obj.SetGeneration(3)
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(obj).WithStatusSubresource(obj).Build()
			r := &apiContract{Client: c, Prototype: obj.DeepCopyObject().(client.Object)}
			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}}
			for i := 0; i < 2; i++ {
				if _, err := r.Reconcile(context.Background(), req); err != nil {
					t.Fatal(err)
				}
			}
			if err := c.Get(context.Background(), req.NamespacedName, obj); err != nil {
				t.Fatal(err)
			}
			raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
			if err != nil {
				t.Fatal(err)
			}
			cs, _, err := unstructured.NestedSlice(raw, "status", "conditions")
			if err != nil || len(cs) != 1 {
				t.Fatalf("condition lost: %v", cs)
			}
			condition := cs[0].(map[string]interface{})
			if condition["reason"] != "UnsupportedAPI" || condition["status"] != "False" {
				t.Fatalf("unexpected condition: %v", condition)
			}
		})
	}
}
func reflectName(obj client.Object) string { return fmt.Sprintf("%T", obj) }
