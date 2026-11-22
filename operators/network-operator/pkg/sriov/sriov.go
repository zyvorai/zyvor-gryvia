// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package sriov

import (
	"context"
	"encoding/json"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gryviav1 "github.com/zyvorai/gryvia/operators/network-operator/api/v1"
)

const (
	SRIOVDevicePluginNamespace = "kube-system"
)

// SRIOVDevicePluginDaemonSetName returns a DaemonSet name scoped to the owning GryviaNetwork.
func SRIOVDevicePluginDaemonSetName(networkName string) string {
	return fmt.Sprintf("sriov-device-plugin-%s", networkName)
}

// SRIOVCNIDaemonSetName returns the SR-IOV CNI DaemonSet name scoped to the owning GryviaNetwork.
func SRIOVCNIDaemonSetName(networkName string) string {
	return fmt.Sprintf("sriov-cni-%s", networkName)
}

// SRIOVConfigMapName returns a ConfigMap name scoped to the owning GryviaNetwork.
func SRIOVConfigMapName(networkName string) string {
	return fmt.Sprintf("sriov-config-%s", networkName)
}

// InstallDevicePlugin installs the SR-IOV device plugin.
// networkName is the owning GryviaNetwork's name, used to scope resource names
// so that multiple GryviaNetworks don't conflict.
func InstallDevicePlugin(ctx context.Context, k8sClient client.Client, networkName string, sriovConfig *gryviav1.SRIOVConfig) error {
	// Install SR-IOV CNI first
	if err := installSRIOVCNI(ctx, k8sClient, networkName); err != nil {
		return fmt.Errorf("failed to install SR-IOV CNI: %w", err)
	}

	// Create ConfigMap for SR-IOV device plugin
	if err := createSRIOVConfigMap(ctx, k8sClient, networkName, sriovConfig); err != nil {
		return fmt.Errorf("failed to create SR-IOV ConfigMap: %w", err)
	}

	dsName := SRIOVDevicePluginDaemonSetName(networkName)
	cmName := SRIOVConfigMapName(networkName)

	// Install device plugin DaemonSet
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dsName,
			Namespace: SRIOVDevicePluginNamespace,
			Labels: map[string]string{
				"app":               dsName,
				"gryvia.io/network": networkName,
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
							Name:  "sriov-device-plugin",
							Image: "ghcr.io/k8snetworkplumbingwg/sriov-network-device-plugin:v3.7.0",
							Args: []string{
								"--log-level=10",
							},
							SecurityContext: &corev1.SecurityContext{
								Privileged: func() *bool { b := true; return &b }(),
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "devicesock",
									MountPath: "/var/lib/kubelet/device-plugins",
								},
								{
									Name:      "config",
									MountPath: "/etc/pcidp/config.json",
									SubPath:   "config.json",
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "devicesock",
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

	existing := &appsv1.DaemonSet{}
	err := k8sClient.Get(ctx, types.NamespacedName{Name: ds.Name, Namespace: ds.Namespace}, existing)
	if err != nil {
		if errors.IsNotFound(err) {
			return k8sClient.Create(ctx, ds)
		}
		return err
	}

	existing.Spec = ds.Spec
	return k8sClient.Update(ctx, existing)
}

func installSRIOVCNI(ctx context.Context, k8sClient client.Client, networkName string) error {
	cniName := SRIOVCNIDaemonSetName(networkName)

	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cniName,
			Namespace: SRIOVDevicePluginNamespace,
			Labels: map[string]string{
				"app":               cniName,
				"gryvia.io/network": networkName,
			},
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": cniName,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": cniName,
					},
				},
				Spec: corev1.PodSpec{
					HostNetwork: true,
					Containers: []corev1.Container{
						{
							Name:  "sriov-cni",
							Image: "ghcr.io/k8snetworkplumbingwg/sriov-cni:v2.8.0",
							SecurityContext: &corev1.SecurityContext{
								Privileged: func() *bool { b := true; return &b }(),
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "cnibin",
									MountPath: "/host/opt/cni/bin",
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "cnibin",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/opt/cni/bin",
								},
							},
						},
					},
				},
			},
		},
	}

	existing := &appsv1.DaemonSet{}
	err := k8sClient.Get(ctx, types.NamespacedName{Name: ds.Name, Namespace: ds.Namespace}, existing)
	if err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("failed to get SRIOV CNI DaemonSet: %w", err)
		}
		// NotFound - create it
		return k8sClient.Create(ctx, ds)
	}

	return nil
}

func createSRIOVConfigMap(ctx context.Context, k8sClient client.Client, networkName string, sriovConfig *gryviav1.SRIOVConfig) error {
	resourceName := sriovConfig.ResourceName
	if resourceName == "" {
		resourceName = "intel_sriov_netdevice"
	}

	configObj := map[string]interface{}{
		"resourceList": []map[string]interface{}{
			{
				"resourceName": resourceName,
				"selectors": map[string]interface{}{
					"pfNames": []string{sriovConfig.PhysicalInterface},
				},
			},
		},
	}
	configBytes, err := json.MarshalIndent(configObj, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal SR-IOV config: %w", err)
	}
	config := string(configBytes)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SRIOVConfigMapName(networkName),
			Namespace: SRIOVDevicePluginNamespace,
			Labels: map[string]string{
				"gryvia.io/network": networkName,
			},
		},
		Data: map[string]string{
			"config.json": config,
		},
	}

	existing := &corev1.ConfigMap{}
	err = k8sClient.Get(ctx, types.NamespacedName{Name: cm.Name, Namespace: cm.Namespace}, existing)
	if err != nil {
		if errors.IsNotFound(err) {
			return k8sClient.Create(ctx, cm)
		}
		return err
	}

	existing.Data = cm.Data
	return k8sClient.Update(ctx, existing)
}

// ConfigureNode configures SR-IOV on a specific node
func ConfigureNode(ctx context.Context, k8sClient client.Client, node *corev1.Node, sriovConfig *gryviav1.SRIOVConfig) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: node.Name}, node); err != nil {
			return err
		}

		if node.Labels == nil {
			node.Labels = make(map[string]string)
		}

		node.Labels["gryvia.io/sriov"] = "true"
		node.Labels[fmt.Sprintf("gryvia.io/sriov-%s", sriovConfig.ResourceName)] = "true"

		if node.Annotations == nil {
			node.Annotations = make(map[string]string)
		}

		node.Annotations["gryvia.io/sriov-interface"] = sriovConfig.PhysicalInterface
		node.Annotations["gryvia.io/sriov-numvfs"] = fmt.Sprintf("%d", sriovConfig.NumVFs)

		return k8sClient.Update(ctx, node)
	})
}

// EnsureVFConfigDaemonSet deploys a DaemonSet that enables Virtual Functions
// on matching nodes by writing to the sysfs sriov_numvfs file.
func EnsureVFConfigDaemonSet(ctx context.Context, k8sClient client.Client, sriovConfig *gryviav1.SRIOVConfig) error {
	name := fmt.Sprintf("sriov-vf-config-%s", sriovConfig.ResourceName)
	enableScript := fmt.Sprintf(
		`#!/bin/sh
set -e
IFACE="%s"
NUM_VFS=%d
CURRENT=$(cat /sys/class/net/$IFACE/device/sriov_numvfs 2>/dev/null || echo 0)
if [ "$CURRENT" != "$NUM_VFS" ]; then
  echo 0 > /sys/class/net/$IFACE/device/sriov_numvfs
  echo $NUM_VFS > /sys/class/net/$IFACE/device/sriov_numvfs
  echo "Configured $NUM_VFS VFs on $IFACE"
else
  echo "VFs already configured ($CURRENT) on $IFACE"
fi
sleep infinity`,
		sriovConfig.PhysicalInterface, sriovConfig.NumVFs,
	)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + "-script",
			Namespace: SRIOVDevicePluginNamespace,
		},
		Data: map[string]string{
			"enable-vfs.sh": enableScript,
		},
	}

	existingCM := &corev1.ConfigMap{}
	err := k8sClient.Get(ctx, types.NamespacedName{Name: cm.Name, Namespace: cm.Namespace}, existingCM)
	if errors.IsNotFound(err) {
		if err := k8sClient.Create(ctx, cm); err != nil {
			return fmt.Errorf("failed to create VF config script: %w", err)
		}
	} else if err != nil {
		return err
	} else {
		existingCM.Data = cm.Data
		if err := k8sClient.Update(ctx, existingCM); err != nil {
			return fmt.Errorf("failed to update VF config script: %w", err)
		}
	}

	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: SRIOVDevicePluginNamespace,
			Labels: map[string]string{
				"app":                      name,
				"gryvia.io/component":      "sriov-vf-config",
				"gryvia.io/sriov-resource": sriovConfig.ResourceName,
			},
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": name,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": name,
					},
				},
				Spec: corev1.PodSpec{
					HostNetwork: true,
					HostPID:     true,
					NodeSelector: map[string]string{
						"gryvia.io/sriov": "true",
					},
					Containers: []corev1.Container{
						{
							Name:  "vf-config",
							Image: "busybox:1.36",
							Command: []string{
								"/bin/sh",
								"/scripts/enable-vfs.sh",
							},
							SecurityContext: &corev1.SecurityContext{
								Privileged: func() *bool { b := true; return &b }(),
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "scripts",
									MountPath: "/scripts",
								},
								{
									Name:      "sys",
									MountPath: "/sys",
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "scripts",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: cm.Name,
									},
									DefaultMode: func() *int32 { m := int32(0755); return &m }(),
								},
							},
						},
						{
							Name: "sys",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/sys",
								},
							},
						},
					},
				},
			},
		},
	}

	existingDS := &appsv1.DaemonSet{}
	err = k8sClient.Get(ctx, types.NamespacedName{Name: ds.Name, Namespace: ds.Namespace}, existingDS)
	if errors.IsNotFound(err) {
		return k8sClient.Create(ctx, ds)
	} else if err != nil {
		return err
	}

	existingDS.Spec = ds.Spec
	return k8sClient.Update(ctx, existingDS)
}
