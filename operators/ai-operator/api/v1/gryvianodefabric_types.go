// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GryviaNodeFabricSpec is the fabric health of ONE node as measured by the
// eBPF collector running on it. The collector is the only writer (opt-in flag
// -publish-node-fabric); the ai-operator only reads it, and only when
// -fabric-aware-scheduling is on. The whole object is advisory: a reader must
// ignore it once measuredAt + ttlSeconds has passed.
type GryviaNodeFabricSpec struct {
	// NodeName is the Kubernetes node the measurement is about (equals metadata.name)
	NodeName string `json:"nodeName"`

	// ScoreDelta is the fabric penalty in [0,1] (0 = healthy, 1 = worst)
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1
	ScoreDelta float64 `json:"scoreDelta"`

	// Reasons are the signals that contribute most to scoreDelta (bounded, most significant first)
	// +kubebuilder:validation:MaxItems=8
	Reasons []string `json:"reasons,omitempty"`

	// MeasuredAt is when the collector computed this value
	MeasuredAt metav1.Time `json:"measuredAt"`

	// TTLSeconds is how long after measuredAt the value may be used (default 300)
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=86400
	// +kubebuilder:default=300
	TTLSeconds int32 `json:"ttlSeconds,omitempty"`

	// ExpiresAt is measuredAt + ttlSeconds, for humans; readers recompute it
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:resource:scope=Cluster,shortName=gnf
//+kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.spec.nodeName`
//+kubebuilder:printcolumn:name="Delta",type=number,JSONPath=`.spec.scoreDelta`
//+kubebuilder:printcolumn:name="Measured",type=date,JSONPath=`.spec.measuredAt`

// GryviaNodeFabric is the Schema for the gryvianodefabrics API: one object per
// node (metadata.name = node name), written by that node's collector.
type GryviaNodeFabric struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec GryviaNodeFabricSpec `json:"spec,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaNodeFabricList contains a list of GryviaNodeFabric
type GryviaNodeFabricList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaNodeFabric `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GryviaNodeFabric{}, &GryviaNodeFabricList{})
}
