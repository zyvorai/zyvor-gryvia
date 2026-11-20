// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GryviaNetworkSpec defines the desired state of GryviaNetwork
type GryviaNetworkSpec struct {
	// NetworkType is the type of network (rdma, sriov, standard)
	NetworkType string `json:"networkType"`

	// RDMA configuration for InfiniBand/RoCE
	RDMA *RDMAConfig `json:"rdma,omitempty"`

	// SRIOV configuration
	SRIOV *SRIOVConfig `json:"sriov,omitempty"`

	// NodeSelector for network configuration
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// MTU for network interfaces
	MTU int `json:"mtu,omitempty"`

	// TargetNamespace is the namespace where NetworkAttachmentDefinitions will be created
	TargetNamespace string `json:"targetNamespace,omitempty"`
}

// RDMAConfig defines RDMA network configuration
type RDMAConfig struct {
	// Mode is the RDMA mode (infiniband, roce)
	Mode string `json:"mode"`

	// Devices is the list of RDMA devices
	Devices []string `json:"devices,omitempty"`

	// Subnet for IP assignment
	Subnet string `json:"subnet,omitempty"`

	// Gateway for the network
	Gateway string `json:"gateway,omitempty"`
}

// SRIOVConfig defines SR-IOV configuration
type SRIOVConfig struct {
	// PhysicalInterface is the physical NIC
	PhysicalInterface string `json:"physicalInterface"`

	// NumVFs is the number of virtual functions
	NumVFs int `json:"numVfs"`

	// VFDriver is the driver for VFs
	VFDriver string `json:"vfDriver,omitempty"`

	// ResourceName is the Kubernetes resource name
	ResourceName string `json:"resourceName"`

	// Subnet for IP assignment
	Subnet string `json:"subnet,omitempty"`

	// Gateway for the network
	Gateway string `json:"gateway,omitempty"`
}

// GryviaNetworkStatus defines the observed state of GryviaNetwork
type GryviaNetworkStatus struct {
	// Phase is the current phase (Pending, Ready, Failed)
	Phase string `json:"phase,omitempty"`

	// Conditions represent the latest available observations
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ConfiguredNodes is the number of nodes configured
	ConfiguredNodes int `json:"configuredNodes,omitempty"`

	// TotalNodes is the total number of matching nodes
	TotalNodes int `json:"totalNodes,omitempty"`

	// LastUpdated is the timestamp of last status update
	LastUpdated metav1.Time `json:"lastUpdated,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Cluster

// GryviaNetwork is the Schema for the gryvianetworks API
type GryviaNetwork struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GryviaNetworkSpec   `json:"spec,omitempty"`
	Status GryviaNetworkStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaNetworkList contains a list of GryviaNetwork
type GryviaNetworkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaNetwork `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GryviaNetwork{}, &GryviaNetworkList{})
}
