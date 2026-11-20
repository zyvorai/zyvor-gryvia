// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package weka

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
	WekaCSINamespace      = "kube-system"
	WekaCSIDriverName     = "csi.weka.io"
	WekaCSIControllerName = "weka-csi-controller"
	WekaCSINodeName       = "weka-csi-node"
)

// InstallCSIDriver installs the Weka CSI driver
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
			Name:      "weka-csi-controller-sa",
			Namespace: WekaCSINamespace,
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
			Name: "weka-csi-controller-role",
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
				Resources: []string{"volumesnapshots", "volumesnapshotcontents"},
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
			Name: "weka-csi-controller-binding",
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     "weka-csi-controller-role",
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      "weka-csi-controller-sa",
				Namespace: WekaCSINamespace,
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
			Namespace: WekaCSINamespace,
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
	err := k8sClient.Get(ctx, types.NamespacedName{Name: secretName, Namespace: WekaCSINamespace}, existing)
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
			Name:      WekaCSIControllerName,
			Namespace: WekaCSINamespace,
			Labels: map[string]string{
				"app": WekaCSIControllerName,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": WekaCSIControllerName,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": WekaCSIControllerName,
					},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: "weka-csi-controller-sa",
					Containers: []corev1.Container{
						{
							Name:  "weka-csi-plugin",
							Image: "quay.io/weka.io/csi-wekafs:v2.6.0",
							Args: []string{
								"--drivername=$(CSI_DRIVER_NAME)",
								"--endpoint=$(CSI_ENDPOINT)",
								"--nodeid=$(KUBE_NODE_NAME)",
								"--dynamic-path=csi-volumes",
							},
							Env: []corev1.EnvVar{
								{
									Name:  "CSI_DRIVER_NAME",
									Value: WekaCSIDriverName,
								},
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
								"--extra-create-metadata",
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
						{
							Name:  "csi-attacher",
							Image: "registry.k8s.io/sig-storage/csi-attacher:v4.7.0",
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
									MountPath: "/var/lib/csi/sockets/pluginproxy/",
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
	secretName := fmt.Sprintf("%s-endpoint", storage.Name)

	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      WekaCSINodeName,
			Namespace: WekaCSINamespace,
			Labels: map[string]string{
				"app": WekaCSINodeName,
			},
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": WekaCSINodeName,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": WekaCSINodeName,
					},
				},
				Spec: corev1.PodSpec{
					HostNetwork: true,
					Containers: []corev1.Container{
						{
							Name:  "weka-csi-node",
							Image: "quay.io/weka.io/csi-wekafs:v2.6.0",
							SecurityContext: &corev1.SecurityContext{
								Privileged: func() *bool { b := true; return &b }(),
							},
							Args: []string{
								"--drivername=$(CSI_DRIVER_NAME)",
								"--endpoint=$(CSI_ENDPOINT)",
								"--nodeid=$(KUBE_NODE_NAME)",
								"--dynamic-path=csi-volumes",
							},
							Env: []corev1.EnvVar{
								{
									Name:  "CSI_DRIVER_NAME",
									Value: WekaCSIDriverName,
								},
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
								{
									Name: "WEKA_ENDPOINT",
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
									Value: "/var/lib/kubelet/plugins/csi.weka.io/csi.sock",
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
									Path: "/var/lib/kubelet/plugins/csi.weka.io",
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

// HealthCheck checks Weka cluster health
func HealthCheck(ctx context.Context, endpoint string) error {
	httpClient := &http.Client{
		Timeout: 5 * time.Second,
	}

	healthURL := fmt.Sprintf("https://%s/api/v2/healthcheck", endpoint)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return fmt.Errorf("Weka health check request creation failed: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("Weka health check failed: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Weka health check returned status %d", resp.StatusCode)
	}

	return nil
}
