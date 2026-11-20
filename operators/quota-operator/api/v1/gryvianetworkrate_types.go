// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GryviaNetworkRateSpec is the provider's network egress price list (price per GB, 1 GB = 10^9 bytes).
type GryviaNetworkRateSpec struct {
	// SameZone is the price per GB of egress to a peer in the same zone
	// +kubebuilder:validation:Minimum=0
	SameZone float64 `json:"sameZone,omitempty"`

	// CrossZone is the price per GB of egress to a peer in another zone
	// +kubebuilder:validation:Minimum=0
	CrossZone float64 `json:"crossZone,omitempty"`

	// InternetEgress is the price per GB of egress to a peer outside the cluster
	// +kubebuilder:validation:Minimum=0
	InternetEgress float64 `json:"internetEgress,omitempty"`

	// Currency is the ISO currency code of the prices
	// +kubebuilder:default=USD
	Currency string `json:"currency,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:resource:scope=Cluster
//+kubebuilder:printcolumn:name="SameZone",type=number,JSONPath=`.spec.sameZone`
//+kubebuilder:printcolumn:name="CrossZone",type=number,JSONPath=`.spec.crossZone`
//+kubebuilder:printcolumn:name="Internet",type=number,JSONPath=`.spec.internetEgress`
//+kubebuilder:printcolumn:name="Currency",type=string,JSONPath=`.spec.currency`

// GryviaNetworkRate is the Schema for the gryvianetworkrates API. It is pure data (no controller); the
// gateway uses the first object by name. Traffic to a peer whose zone is unknown is never priced.
type GryviaNetworkRate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec GryviaNetworkRateSpec `json:"spec,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaNetworkRateList contains a list of GryviaNetworkRate
type GryviaNetworkRateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaNetworkRate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GryviaNetworkRate{}, &GryviaNetworkRateList{})
}
