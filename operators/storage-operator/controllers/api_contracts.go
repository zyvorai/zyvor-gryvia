// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controllers

import (
	"context"
	gryviav1 "github.com/zyvorai/gryvia/operators/storage-operator/api/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Report legacy APIs as unsupported rather than activating dormant simulations.
// Objects remain readable so administrators can migrate without losing data.
type apiContract struct {
	client.Client
	Prototype client.Object
}

func (r *apiContract) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	obj := r.Prototype.DeepCopyObject().(client.Object)
	if err := r.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !obj.GetDeletionTimestamp().IsZero() {
		return ctrl.Result{}, nil
	}
	base := obj.DeepCopyObject().(client.Object)
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return ctrl.Result{}, err
	}
	status, _ := raw["status"].(map[string]interface{})
	if status == nil {
		status = map[string]interface{}{}
	}
	var conditions []metav1.Condition
	if cs, ok := status["conditions"]; ok {
		for _, item := range cs.([]interface{}) {
			var c metav1.Condition
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.(map[string]interface{}), &c); err != nil {
				return ctrl.Result{}, err
			}
			conditions = append(conditions, c)
		}
	}
	prior := meta.FindStatusCondition(conditions, "Ready")
	if prior != nil && prior.Status == metav1.ConditionFalse && prior.Reason == "UnsupportedAPI" && prior.ObservedGeneration == obj.GetGeneration() {
		return ctrl.Result{}, nil
	}
	meta.SetStatusCondition(&conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: "UnsupportedAPI", Message: "Legacy API has no supported runtime implementation; see docs/platform-completion.md for replacements", ObservedGeneration: obj.GetGeneration()})
	out := []interface{}{}
	for _, c := range conditions {
		v, e := runtime.DefaultUnstructuredConverter.ToUnstructured(&c)
		if e != nil {
			return ctrl.Result{}, e
		}
		out = append(out, v)
	}
	status["conditions"] = out
	raw["status"] = status
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, obj); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, r.Status().Patch(ctx, obj, client.MergeFrom(base))
}
func RegisterAPIContracts(mgr ctrl.Manager) error {
	for _, obj := range []client.Object{&gryviav1.GryviaDataset{}} {
		if err := ctrl.NewControllerManagedBy(mgr).For(obj).Complete(&apiContract{Client: mgr.GetClient(), Prototype: obj}); err != nil {
			return err
		}
	}
	return nil
}
