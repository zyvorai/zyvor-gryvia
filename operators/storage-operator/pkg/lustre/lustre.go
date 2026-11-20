// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package lustre

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
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
	LustreCSINamespace      = "kube-system"
	LustreCSIDriverName     = "csi.lustre.org"
	LustreCSIControllerName = "lustre-csi-controller"
	LustreCSINodeName       = "lustre-csi-node"
)

// InstallCSIDriver installs the Lustre CSI driver
func InstallCSIDriver(ctx context.Context, k8sClient client.Client, storage *gryviav1.GryviaStorage) error {
	if err := ensureServiceAccount(ctx, k8sClient); err != nil {
		return fmt.Errorf("failed to create ServiceAccount: %w", err)
	}

	if err := ensureRBAC(ctx, k8sClient); err != nil {
		return fmt.Errorf("failed to create RBAC: %w", err)
	}

	if err := ensureController(ctx, k8sClient, storage); err != nil {
		return fmt.Errorf("failed to deploy CSI controller: %w", err)
	}

	if err := ensureNodeDaemonSet(ctx, k8sClient, storage); err != nil {
		return fmt.Errorf("failed to deploy CSI node DaemonSet: %w", err)
	}

	return nil
}

func ensureServiceAccount(ctx context.Context, k8sClient client.Client) error {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "lustre-csi-controller-sa",
			Namespace: LustreCSINamespace,
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
			Name: "lustre-csi-controller-role",
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
				APIGroups: []string{""},
				Resources: []string{"events"},
				Verbs:     []string{"list", "watch", "create", "update", "patch"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"nodes"},
				Verbs:     []string{"get", "list", "watch"},
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
			Name: "lustre-csi-controller-binding",
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     "lustre-csi-controller-role",
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      "lustre-csi-controller-sa",
				Namespace: LustreCSINamespace,
			},
		},
	}

	err = k8sClient.Get(ctx, types.NamespacedName{Name: crb.Name}, &rbacv1.ClusterRoleBinding{})
	if errors.IsNotFound(err) {
		return k8sClient.Create(ctx, crb)
	}
	return err
}

func validateEndpoint(endpoint string) error {
	if endpoint == "" {
		return fmt.Errorf("endpoint must not be empty")
	}
	if strings.ContainsAny(endpoint, "\n\r\t;|&$`") {
		return fmt.Errorf("endpoint contains invalid characters")
	}
	return nil
}

func ensureEndpointSecret(ctx context.Context, k8sClient client.Client, secretName, endpoint string) error {
	if err := validateEndpoint(endpoint); err != nil {
		return fmt.Errorf("invalid endpoint: %w", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: LustreCSINamespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "gryvia",
			},
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			"endpoint": endpoint,
		},
	}

	existing := &corev1.Secret{}
	err := k8sClient.Get(ctx, types.NamespacedName{Name: secretName, Namespace: LustreCSINamespace}, existing)
	if errors.IsNotFound(err) {
		return k8sClient.Create(ctx, secret)
	}
	if err != nil {
		return err
	}
	existing.StringData = secret.StringData
	return k8sClient.Update(ctx, existing)
}

func ensureController(ctx context.Context, k8sClient client.Client, storage *gryviav1.GryviaStorage) error {
	secretName := fmt.Sprintf("%s-endpoint", storage.Name)
	if err := ensureEndpointSecret(ctx, k8sClient, secretName, storage.Spec.Endpoint); err != nil {
		return fmt.Errorf("failed to create endpoint secret: %w", err)
	}

	replicas := int32(1)

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      LustreCSIControllerName,
			Namespace: LustreCSINamespace,
			Labels: map[string]string{
				"app": LustreCSIControllerName,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": LustreCSIControllerName,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": LustreCSIControllerName,
					},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: "lustre-csi-controller-sa",
					Containers: []corev1.Container{
						{
							Name:  "lustre-csi-plugin",
							Image: "ghcr.io/kubernetes-sigs/lustre-csi-driver:v0.3.0",
							Args: []string{
								"--endpoint=$(CSI_ENDPOINT)",
								"--nodeid=$(KUBE_NODE_NAME)",
								"--drivername=" + LustreCSIDriverName,
							},
							Env: []corev1.EnvVar{
								{
									Name:  "CSI_ENDPOINT",
									Value: "unix:///var/lib/csi/sockets/pluginproxy/csi.sock",
								},
								{
									Name: "KUBE_NODE_NAME",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{
											FieldPath: "spec.nodeName",
										},
									},
								},
								{
									Name: "LUSTRE_ENDPOINT",
									ValueFrom: &corev1.EnvVarSource{
										SecretKeyRef: &corev1.SecretKeySelector{
											LocalObjectReference: corev1.LocalObjectReference{
												Name: secretName,
											},
											Key: "endpoint",
										},
									},
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "socket-dir",
									MountPath: "/var/lib/csi/sockets/pluginproxy",
								},
							},
						},
						{
							Name:  "csi-provisioner",
							Image: "registry.k8s.io/sig-storage/csi-provisioner:v5.1.0",
							Args: []string{
								"--csi-address=$(ADDRESS)",
								"--leader-election",
							},
							Env: []corev1.EnvVar{
								{
									Name:  "ADDRESS",
									Value: "/var/lib/csi/sockets/pluginproxy/csi.sock",
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "socket-dir",
									MountPath: "/var/lib/csi/sockets/pluginproxy",
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

func ensureNodeDaemonSet(ctx context.Context, k8sClient client.Client, storage *gryviav1.GryviaStorage) error {
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      LustreCSINodeName,
			Namespace: LustreCSINamespace,
			Labels: map[string]string{
				"app": LustreCSINodeName,
			},
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": LustreCSINodeName,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": LustreCSINodeName,
					},
				},
				Spec: corev1.PodSpec{
					HostNetwork: true,
					Containers: []corev1.Container{
						{
							Name:  "lustre-csi-node",
							Image: "ghcr.io/kubernetes-sigs/lustre-csi-driver:v0.3.0",
							SecurityContext: &corev1.SecurityContext{
								Privileged: func() *bool { b := true; return &b }(),
							},
							Args: []string{
								"--endpoint=$(CSI_ENDPOINT)",
								"--nodeid=$(KUBE_NODE_NAME)",
								"--drivername=" + LustreCSIDriverName,
							},
							Env: []corev1.EnvVar{
								{
									Name:  "CSI_ENDPOINT",
									Value: "unix:///csi/csi.sock",
								},
								{
									Name: "KUBE_NODE_NAME",
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
									Name:      "lustre-mount",
									MountPath: "/lustre",
									MountPropagation: func() *corev1.MountPropagationMode {
										m := corev1.MountPropagationBidirectional
										return &m
									}(),
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
									Value: "/var/lib/kubelet/plugins/csi.lustre.org/csi.sock",
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
									Path: "/var/lib/kubelet/plugins/csi.lustre.org",
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
							Name: "lustre-mount",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/lustre",
									Type: func() *corev1.HostPathType {
										t := corev1.HostPathDirectoryOrCreate
										return &t
									}(),
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

// HealthCheck checks Lustre filesystem health via the management endpoint
func HealthCheck(ctx context.Context, endpoint string) error {
	httpClient := &http.Client{
		Timeout: 10 * time.Second,
	}

	healthURL := fmt.Sprintf("https://%s/api/health", endpoint)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return fmt.Errorf("Lustre health check request creation failed: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("Lustre health check failed: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Lustre health check returned status %d", resp.StatusCode)
	}

	return nil
}
