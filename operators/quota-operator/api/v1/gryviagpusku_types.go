// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GryviaGpuSkuSpec defines one entry of the provider's GPU price catalog.
type GryviaGpuSkuSpec struct {
	// GpuType is the GPU model this SKU prices (matches GryviaAIJob spec.gpuType), e.g. H100
	// +kubebuilder:validation:MinLength=1
	GpuType string `json:"gpuType"`

	// GpusPerUnit is the number of GPUs in one purchasable unit
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	GpusPerUnit int32 `json:"gpusPerUnit,omitempty"`

	// HourlyRate is the price per GPU-hour in the SKU currency
	// +kubebuilder:validation:Minimum=0
	HourlyRate float64 `json:"hourlyRate"`

	// Currency is the ISO currency code of hourlyRate
	// +kubebuilder:default=USD
	Currency string `json:"currency,omitempty"`

	// SpotDiscount is the percentage discount for spot capacity (0-100)
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	SpotDiscount int32 `json:"spotDiscount,omitempty"`

	// Description is shown in the catalog
	Description string `json:"description,omitempty"`

	// Enabled controls whether tenants can use and are billed with this SKU
	// +kubebuilder:default=true
	Enabled *bool `json:"enabled,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:resource:scope=Cluster
//+kubebuilder:printcolumn:name="GPU",type=string,JSONPath=`.spec.gpuType`
//+kubebuilder:printcolumn:name="Rate",type=number,JSONPath=`.spec.hourlyRate`
//+kubebuilder:printcolumn:name="Currency",type=string,JSONPath=`.spec.currency`
//+kubebuilder:printcolumn:name="Enabled",type=boolean,JSONPath=`.spec.enabled`

// GryviaGpuSku is the Schema for the gryviagpuskus API. It is pure data (no controller).
type GryviaGpuSku struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec GryviaGpuSkuSpec `json:"spec,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaGpuSkuList contains a list of GryviaGpuSku
type GryviaGpuSkuList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaGpuSku `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GryviaGpuSku{}, &GryviaGpuSkuList{})
}
