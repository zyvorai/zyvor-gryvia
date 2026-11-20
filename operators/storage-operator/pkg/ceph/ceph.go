// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package ceph

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gryviav1 "github.com/zyvorai/gryvia/operators/storage-operator/api/v1"
)

const (
	CephCSINamespace      = "kube-system"
	CephCSIDriverName     = "rook-ceph.cephfs.csi.ceph.com"
	CephCSIControllerName = "ceph-csi-controller"
	CephCSINodeName       = "ceph-csi-node"
)

// InstallCSIDriver installs the CephFS CSI driver
func InstallCSIDriver(ctx context.Context, k8sClient client.Client, storage *gryviav1.GryviaStorage) error {
	if err := ensureServiceAccount(ctx, k8sClient); err != nil {
		return fmt.Errorf("failed to create ServiceAccount: %w", err)
	}

	if err := ensureRBAC(ctx, k8sClient); err != nil {
		return fmt.Errorf("failed to create RBAC: %w", err)
	}

	if err := ensureCephConfigMap(ctx, k8sClient, storage); err != nil {
		return fmt.Errorf("failed to create Ceph config: %w", err)
	}

	if err := ensureController(ctx, k8sClient, storage); err != nil {
		return fmt.Errorf("failed to deploy CSI controller: %w", err)
	}

	if err := ensureNodeDaemonSet(ctx, k8sClient); err != nil {
		return fmt.Errorf("failed to deploy CSI node DaemonSet: %w", err)
	}

	return nil
}

func ensureServiceAccount(ctx context.Context, k8sClient client.Client) error {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ceph-csi-controller-sa",
			Namespace: CephCSINamespace,
		},
	}

	err := k8sClient.Get(ctx, types.NamespacedName{Name: sa.Name, Namespace: sa.Namespace}, &corev1.ServiceAccount{})
	if errors.IsNotFound(err) {
		return k8sClient.Create(ctx, sa)
	}
	return err
}

func ensureRBAC(ctx context.Context, k8sClient client.Client) error {
	cr := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ceph-csi-controller-role",
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"persistentvolumes"},
				Verbs:     []string{"get", "list", "watch", "create", "delete", "patch"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"persistentvolumeclaims"},
				Verbs:     []string{"get", "list", "watch", "update"},
			},
			{
				APIGroups: []string{"storage.k8s.io"},
				Resources: []string{"storageclasses"},
				Verbs:     []string{"get", "list", "watch"},
			},
			{
				APIGroups: []string{"storage.k8s.io"},
				Resources: []string{"csinodes"},
				Verbs:     []string{"get", "list", "watch"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"events"},
				Verbs:     []string{"list", "watch", "create", "update", "patch"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"nodes"},
				Verbs:     []string{"get", "list", "watch"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"secrets"},
				Verbs:     []string{"get", "list"},
			},
			{
				APIGroups: []string{"snapshot.storage.k8s.io"},
				Resources: []string{"volumesnapshots", "volumesnapshotcontents", "volumesnapshotclasses"},
				Verbs:     []string{"get", "list", "watch", "create", "delete"},
			},
		},
	}

	err := k8sClient.Get(ctx, types.NamespacedName{Name: cr.Name}, &rbacv1.ClusterRole{})
	if errors.IsNotFound(err) {
		if err := k8sClient.Create(ctx, cr); err != nil {
			return err
		}
	} else if err != nil {
		return fmt.Errorf("failed to check ClusterRole: %w", err)
	}

	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ceph-csi-controller-binding",
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     "ceph-csi-controller-role",
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      "ceph-csi-controller-sa",
				Namespace: CephCSINamespace,
			},
		},
	}

	err = k8sClient.Get(ctx, types.NamespacedName{Name: crb.Name}, &rbacv1.ClusterRoleBinding{})
	if errors.IsNotFound(err) {
		return k8sClient.Create(ctx, crb)
	}
	return err
}

func ensureCephConfigMap(ctx context.Context, k8sClient client.Client, storage *gryviav1.GryviaStorage) error {
	configData := []map[string]interface{}{
		{
			"clusterID": storage.Name,
			"monitors":  []string{storage.Spec.Endpoint},
		},
	}
	configJSON, err := json.Marshal(configData)
	if err != nil {
		return fmt.Errorf("failed to marshal Ceph config: %w", err)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ceph-csi-config",
			Namespace: CephCSINamespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "gryvia",
			},
		},
		Data: map[string]string{
			"config.json": string(configJSON),
		},
	}

	existing := &corev1.ConfigMap{}
	err = k8sClient.Get(ctx, types.NamespacedName{Name: cm.Name, Namespace: CephCSINamespace}, existing)
	if errors.IsNotFound(err) {
		return k8sClient.Create(ctx, cm)
	}
	if err != nil {
		return err
	}
	// Merge new data into existing, preserving manually-added keys
	if existing.Data == nil {
		existing.Data = make(map[string]string)
	}
	for k, v := range cm.Data {
		existing.Data[k] = v
	}
	return k8sClient.Update(ctx, existing)
}

func ensureController(ctx context.Context, k8sClient client.Client, storage *gryviav1.GryviaStorage) error {
	replicas := int32(1)

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      CephCSIControllerName,
			Namespace: CephCSINamespace,
			Labels: map[string]string{
				"app": CephCSIControllerName,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": CephCSIControllerName,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": CephCSIControllerName,
					},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: "ceph-csi-controller-sa",
					Containers: []corev1.Container{
						{
							Name:  "csi-cephfsplugin",
							Image: "quay.io/cephcsi/cephcsi:v3.12.2",
							Args: []string{
								"--nodeid=$(NODE_ID)",
								"--type=cephfs",
								"--controllerserver=true",
								"--endpoint=$(CSI_ENDPOINT)",
								"--drivername=" + CephCSIDriverName,
							},
							Env: []corev1.EnvVar{
								{
									Name:  "CSI_ENDPOINT",
									Value: "unix:///csi/csi-provisioner.sock",
								},
								{
									Name: "NODE_ID",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{
											FieldPath: "spec.nodeName",
										},
									},
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "socket-dir",
									MountPath: "/csi",
								},
								{
									Name:      "ceph-csi-config",
									MountPath: "/etc/ceph-csi-config",
								},
							},
						},
						{
							Name:  "csi-provisioner",
							Image: "registry.k8s.io/sig-storage/csi-provisioner:v5.1.0",
							Args: []string{
								"--csi-address=$(ADDRESS)",
								"--leader-election",
								"--extra-create-metadata",
							},
							Env: []corev1.EnvVar{
								{
									Name:  "ADDRESS",
									Value: "unix:///csi/csi-provisioner.sock",
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "socket-dir",
									MountPath: "/csi",
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "socket-dir",
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
							},
						},
						{
							Name: "ceph-csi-config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: "ceph-csi-config",
									},
								},
							},
						},
					},
				},
			},
		},
	}

	err := k8sClient.Get(ctx, types.NamespacedName{Name: deployment.Name, Namespace: deployment.Namespace}, &appsv1.Deployment{})
	if errors.IsNotFound(err) {
		return k8sClient.Create(ctx, deployment)
	}
	return err
}

func ensureNodeDaemonSet(ctx context.Context, k8sClient client.Client) error {
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      CephCSINodeName,
			Namespace: CephCSINamespace,
			Labels: map[string]string{
				"app": CephCSINodeName,
			},
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": CephCSINodeName,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": CephCSINodeName,
					},
				},
				Spec: corev1.PodSpec{
					HostNetwork: true,
					Containers: []corev1.Container{
						{
							Name:  "csi-cephfsplugin",
							Image: "quay.io/cephcsi/cephcsi:v3.12.2",
							SecurityContext: &corev1.SecurityContext{
								Privileged: func() *bool { b := true; return &b }(),
							},
							Args: []string{
								"--nodeid=$(NODE_ID)",
								"--type=cephfs",
								"--nodeserver=true",
								"--endpoint=$(CSI_ENDPOINT)",
								"--drivername=" + CephCSIDriverName,
							},
							Env: []corev1.EnvVar{
								{
									Name:  "CSI_ENDPOINT",
									Value: "unix:///csi/csi.sock",
								},
								{
									Name: "NODE_ID",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{
											FieldPath: "spec.nodeName",
										},
									},
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "plugin-dir",
									MountPath: "/csi",
								},
								{
									Name:      "pods-mount-dir",
									MountPath: "/var/lib/kubelet/pods",
									MountPropagation: func() *corev1.MountPropagationMode {
										m := corev1.MountPropagationBidirectional
										return &m
									}(),
								},
								{
									Name:      "registration-dir",
									MountPath: "/registration",
								},
								{
									Name:      "ceph-csi-config",
									MountPath: "/etc/ceph-csi-config",
								},
							},
						},
						{
							Name:  "csi-node-driver-registrar",
							Image: "registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.12.0",
							Args: []string{
								"--csi-address=$(ADDRESS)",
								"--kubelet-registration-path=$(DRIVER_REG_SOCK_PATH)",
							},
							Env: []corev1.EnvVar{
								{
									Name:  "ADDRESS",
									Value: "/csi/csi.sock",
								},
								{
									Name:  "DRIVER_REG_SOCK_PATH",
									Value: "/var/lib/kubelet/plugins/rook-ceph.cephfs.csi.ceph.com/csi.sock",
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "plugin-dir",
									MountPath: "/csi",
								},
								{
									Name:      "registration-dir",
									MountPath: "/registration",
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "plugin-dir",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/var/lib/kubelet/plugins/rook-ceph.cephfs.csi.ceph.com",
									Type: func() *corev1.HostPathType {
										t := corev1.HostPathDirectoryOrCreate
										return &t
									}(),
								},
							},
						},
						{
							Name: "pods-mount-dir",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/var/lib/kubelet/pods",
									Type: func() *corev1.HostPathType {
										t := corev1.HostPathDirectory
										return &t
									}(),
								},
							},
						},
						{
							Name: "registration-dir",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/var/lib/kubelet/plugins_registry/",
									Type: func() *corev1.HostPathType {
										t := corev1.HostPathDirectory
										return &t
									}(),
								},
							},
						},
						{
							Name: "ceph-csi-config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: "ceph-csi-config",
									},
								},
							},
						},
					},
				},
			},
		},
	}

	err := k8sClient.Get(ctx, types.NamespacedName{Name: ds.Name, Namespace: ds.Namespace}, &appsv1.DaemonSet{})
	if errors.IsNotFound(err) {
		return k8sClient.Create(ctx, ds)
	}
	return err
}

// HealthCheck checks Ceph cluster health via the management endpoint
func HealthCheck(ctx context.Context, endpoint string) error {
	httpClient := &http.Client{
		Timeout: 10 * time.Second,
	}

	healthURL := fmt.Sprintf("https://%s/api/health", endpoint)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return fmt.Errorf("Ceph health check request creation failed: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("Ceph health check failed: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Ceph health check returned status %d", resp.StatusCode)
	}

	return nil
}
