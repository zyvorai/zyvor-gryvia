// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controllers

import (
	"context"
	"fmt"
	gryviav1 "github.com/zyvorai/gryvia/operators/gpu-operator/api/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const quarantineOwner = "gryvia.io/quarantine-owner"

func (r *GryviaHealthCheckReconciler) quarantine(ctx context.Context, hc *gryviav1.GryviaHealthCheck, name string) error {
	node := &corev1.Node{}
	if err := r.Get(ctx, types.NamespacedName{Name: name}, node); err != nil {
		return err
	}
	if owner := node.Annotations[quarantineOwner]; owner != "" && owner != string(hc.UID) {
		return fmt.Errorf("node %s is quarantined by another health check", name)
	}
	base := node.DeepCopy()
	node.Spec.Unschedulable = true
	if node.Annotations == nil {
		node.Annotations = map[string]string{}
	}
	node.Annotations[quarantineOwner] = string(hc.UID)
	exists := false
	for _, taint := range node.Spec.Taints {
		if taint.Key == "gryvia.io/gpu-unhealthy" {
			exists = true
		}
	}
	if !exists {
		node.Spec.Taints = append(node.Spec.Taints, corev1.Taint{Key: "gryvia.io/gpu-unhealthy", Value: "true", Effect: corev1.TaintEffectNoSchedule})
	}
	if err := r.Patch(ctx, node, client.MergeFrom(base)); err != nil {
		return err
	}
	if base.Annotations[quarantineOwner] != string(hc.UID) {
		now := metav1.Now()
		hc.Status.RemediationHistory = append(hc.Status.RemediationHistory, gryviav1.RemediationEvent{Timestamp: &now, Action: "quarantine", Success: true, Message: "Cordoned " + name})
	}
	return nil
}
func (r *GryviaHealthCheckReconciler) drain(ctx context.Context, node string) error {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods); err != nil {
		return err
	}
	for _, pod := range pods.Items {
		if pod.Spec.NodeName != node || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed || pod.DeletionTimestamp != nil {
			continue
		}
		owner := metav1.GetControllerOf(&pod)
		if owner != nil && owner.Kind == "DaemonSet" || pod.Annotations[corev1.MirrorPodAnnotationKey] != "" {
			continue
		}
		if owner == nil {
			return fmt.Errorf("drain blocked by unmanaged pod %s/%s", pod.Namespace, pod.Name)
		}
		for _, v := range pod.Spec.Volumes {
			if v.EmptyDir != nil && v.EmptyDir.Medium != corev1.StorageMediumMemory {
				return fmt.Errorf("drain blocked by local data in pod %s/%s", pod.Namespace, pod.Name)
			}
		}
		eviction := &policyv1.Eviction{ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace}, DeleteOptions: &metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &pod.UID}}}
		if err := r.SubResource("eviction").Create(ctx, &pod, eviction); err != nil {
			return fmt.Errorf("evict %s/%s respecting PDB: %w", pod.Namespace, pod.Name, err)
		}
	}
	return nil
}
