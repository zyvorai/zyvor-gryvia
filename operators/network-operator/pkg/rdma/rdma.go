// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package rdma

import (
	"context"
	"encoding/json"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gryviav1 "github.com/zyvorai/gryvia/operators/network-operator/api/v1"
)

const (
	RDMADevicePluginNamespace = "kube-system"
)

// RDMADevicePluginDaemonSetName returns a DaemonSet name scoped to the owning GryviaNetwork.
func RDMADevicePluginDaemonSetName(networkName string) string {
	return fmt.Sprintf("rdma-device-plugin-%s", networkName)
}

// RDMAConfigMapName returns a ConfigMap name scoped to the owning GryviaNetwork.
func RDMAConfigMapName(networkName string) string {
	return fmt.Sprintf("rdma-devices-%s", networkName)
}

// InstallDevicePlugin installs the RDMA device plugin DaemonSet
func InstallDevicePlugin(ctx context.Context, k8sClient client.Client, owner *gryviav1.GryviaNetwork, scheme *runtime.Scheme) error {
	// Create ConfigMap first
	if err := CreateRDMAConfigMap(ctx, k8sClient, owner, scheme, owner.Spec.RDMA); err != nil {
		return fmt.Errorf("failed to create RDMA ConfigMap: %w", err)
	}

	dsName := RDMADevicePluginDaemonSetName(owner.Name)
	cmName := RDMAConfigMapName(owner.Name)

	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dsName,
			Namespace: RDMADevicePluginNamespace,
			Labels: map[string]string{
				"app":                          dsName,
				"app.kubernetes.io/managed-by": "gryvia",
				"gryvia.io/network":            owner.Name,
			},
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": dsName,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": dsName,
					},
				},
				Spec: corev1.PodSpec{
					HostNetwork: true,
					Containers: []corev1.Container{
						{
							Name:  "rdma-device-plugin",
							Image: "ghcr.io/mellanox/k8s-rdma-shared-dev-plugin:v1.5.1",
							SecurityContext: &corev1.SecurityContext{
								Privileged: func() *bool { b := true; return &b }(),
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "device-plugin",
									MountPath: "/var/lib/kubelet/device-plugins",
								},
								{
									Name:      "config",
									MountPath: "/k8s-rdma-shared-dev-plugin",
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "device-plugin",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/var/lib/kubelet/device-plugins",
								},
							},
						},
						{
							Name: "config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: cmName,
									},
								},
							},
						},
					},
				},
			},
		},
	}

	// Set owner reference for garbage collection
	// Cross-namespace owner references are not allowed (DaemonSet in kube-system, owner may be cluster-scoped)
	// so we rely on finalizer-based cleanup instead
	_ = controllerutil.SetControllerReference(owner, ds, scheme)

	// Check if DaemonSet already exists
	existing := &appsv1.DaemonSet{}
	err := k8sClient.Get(ctx, types.NamespacedName{Name: ds.Name, Namespace: ds.Namespace}, existing)
	if err != nil {
		if errors.IsNotFound(err) {
			return k8sClient.Create(ctx, ds)
		}
		return err
	}

	// Update if exists
	existing.Spec = ds.Spec
	return k8sClient.Update(ctx, existing)
}

// ConfigureNode configures RDMA on a specific node
func ConfigureNode(ctx context.Context, k8sClient client.Client, node *corev1.Node, rdmaConfig *gryviav1.RDMAConfig) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: node.Name}, node); err != nil {
			return err
		}

		if node.Labels == nil {
			node.Labels = make(map[string]string)
		}
		node.Labels["gryvia.io/rdma"] = "true"
		node.Labels["gryvia.io/rdma-mode"] = rdmaConfig.Mode

		if node.Annotations == nil {
			node.Annotations = make(map[string]string)
		}
		if len(rdmaConfig.Devices) > 0 {
			devicesJSON, err := json.Marshal(rdmaConfig.Devices)
			if err == nil {
				node.Annotations["gryvia.io/rdma-devices"] = string(devicesJSON)
			}
		}

		return k8sClient.Update(ctx, node)
	})
}

// CreateRDMAConfigMap creates a ConfigMap for RDMA device configuration
func CreateRDMAConfigMap(ctx context.Context, k8sClient client.Client, owner *gryviav1.GryviaNetwork, scheme *runtime.Scheme, rdmaConfig *gryviav1.RDMAConfig) error {
	// RDMA device plugin configuration
	config := `{
  "configList": [
    {
      "resourceName": "rdma_shared_device_a",
      "rdmaHcaMax": 1000,
      "selectors": {
        "vendors": ["15b3"],
        "deviceIDs": ["1017", "1019", "101b"]
      }
    }
  ]
}`

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      RDMAConfigMapName(owner.Name),
			Namespace: RDMADevicePluginNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "gryvia",
				"gryvia.io/network":            owner.Name,
			},
		},
		Data: map[string]string{
			"config.json": config,
		},
	}

	// Cross-namespace owner refs not allowed; rely on finalizer cleanup
	_ = controllerutil.SetControllerReference(owner, cm, scheme)

	existing := &corev1.ConfigMap{}
	err := k8sClient.Get(ctx, types.NamespacedName{Name: cm.Name, Namespace: cm.Namespace}, existing)
	if err != nil {
		if errors.IsNotFound(err) {
			return k8sClient.Create(ctx, cm)
		}
		return err
	}

	existing.Data = cm.Data
	return k8sClient.Update(ctx, existing)
}

// ValidateRDMASupport checks if node has RDMA capable hardware
func ValidateRDMASupport(node *corev1.Node) bool {
	// Check for Mellanox/NVIDIA network adapters
	if val, exists := node.Labels["feature.node.kubernetes.io/pci-15b3.present"]; exists && val == "true" {
		return true
	}
	return false
}
