// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type GryviaStorageSpec struct {
	Backend      string            `json:"backend"`
	Capacity     string            `json:"capacity"`
	IOPS         string            `json:"iops,omitempty"`
	Throughput   string            `json:"throughput,omitempty"`
	RDMA         bool              `json:"rdma,omitempty"`
	Protocol     string            `json:"protocol,omitempty"`
	Endpoint     string            `json:"endpoint"`
	MountOptions []string          `json:"mountOptions,omitempty"`
	Credentials  *CredentialsRef   `json:"credentials,omitempty"`
	StorageClass *StorageClassSpec `json:"storageClass,omitempty"`
	Quotas       map[string]string `json:"quotas,omitempty"`
	Performance  *PerformanceSpec  `json:"performance,omitempty"`
}

type CredentialsRef struct {
	SecretName      string `json:"secretName"`
	SecretNamespace string `json:"secretNamespace,omitempty"`
}

type StorageClassSpec struct {
	Name                 string            `json:"name"`
	ReclaimPolicy        string            `json:"reclaimPolicy,omitempty"`
	VolumeBindingMode    string            `json:"volumeBindingMode,omitempty"`
	AllowVolumeExpansion bool              `json:"allowVolumeExpansion,omitempty"`
	Parameters           map[string]string `json:"parameters,omitempty"`
}

type PerformanceSpec struct {
	Tier          string `json:"tier,omitempty"`
	Caching       bool   `json:"caching,omitempty"`
	Compression   bool   `json:"compression,omitempty"`
	Deduplication bool   `json:"deduplication,omitempty"`
	Encryption    bool   `json:"encryption,omitempty"`
}

type GryviaStorageStatus struct {
	Phase               string             `json:"phase,omitempty"`
	Conditions          []metav1.Condition `json:"conditions,omitempty"`
	TotalCapacity       string             `json:"totalCapacity,omitempty"`
	UsedCapacity        string             `json:"usedCapacity,omitempty"`
	AvailableCapacity   string             `json:"availableCapacity,omitempty"`
	CurrentIOPS         string             `json:"currentIOPS,omitempty"`
	CurrentThroughput   string             `json:"currentThroughput,omitempty"`
	StorageClassCreated bool               `json:"storageClassCreated,omitempty"`
	CSIDriverInstalled  bool               `json:"csiDriverInstalled,omitempty"`
	LastHealthCheck     *metav1.Time       `json:"lastHealthCheck,omitempty"`
	Metrics             *StorageMetrics    `json:"metrics,omitempty"`
}

type StorageMetrics struct {
	ReadLatency  string `json:"readLatency,omitempty"`
	WriteLatency string `json:"writeLatency,omitempty"`
	Utilization  int    `json:"utilization,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Cluster
//+kubebuilder:printcolumn:name="Backend",type=string,JSONPath=`.spec.backend`
//+kubebuilder:printcolumn:name="Capacity",type=string,JSONPath=`.spec.capacity`
//+kubebuilder:printcolumn:name="RDMA",type=boolean,JSONPath=`.spec.rdma`
//+kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

type GryviaStorage struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GryviaStorageSpec   `json:"spec,omitempty"`
	Status GryviaStorageStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

type GryviaStorageList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaStorage `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GryviaStorage{}, &GryviaStorageList{})
}
