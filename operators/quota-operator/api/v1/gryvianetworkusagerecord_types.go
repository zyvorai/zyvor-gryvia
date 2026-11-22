// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GryviaNetworkUsageRecordSpec is one fragment of a tenant's measured network usage: the bytes one node's
// collector attributed to (tenant, UTC hour, peerClass, zoneClass). The gateway sums the fragments of all
// nodes. Only egress is billable (see docs/network-cost-attribution.md); ingress is informational.
// Values are measurements from a tcx byte counter, not invoices.
type GryviaNetworkUsageRecordSpec struct {
	// Tenant the traffic is attributed to (the namespace is tenant-<tenant>)
	Tenant string `json:"tenant"`

	// Node is the node whose collector measured the fragment
	Node string `json:"node"`

	// Source of the measurement; the gateway uses exactly one source per tenant and period
	// +kubebuilder:default=collector
	// +kubebuilder:validation:Enum=collector;netra
	Source string `json:"source,omitempty"`

	// Hour is the start of the UTC hour bucket
	Hour metav1.Time `json:"hour"`

	// PeerClass classifies the other end of the traffic
	// +kubebuilder:validation:Enum=same-tenant;other-tenant;cluster;external;unknown
	PeerClass string `json:"peerClass"`

	// ZoneClass is the zone relation between the tenant pod and the peer. internet is the zone class of
	// every external peer; same-node is only recorded for cross-tenant traffic
	// +kubebuilder:validation:Enum=same-zone;cross-zone;same-node;unknown-zone;internet
	ZoneClass string `json:"zoneClass"`

	// EgressBytes sent by the tenant's pods to the peer (billable)
	// +kubebuilder:validation:Minimum=0
	EgressBytes int64 `json:"egressBytes"`

	// IngressBytes received by the tenant's pods from the peer (informational, never billed)
	// +kubebuilder:validation:Minimum=0
	IngressBytes int64 `json:"ingressBytes,omitempty"`

	// Final is true once the hour is closed; a final record is never modified again
	Final bool `json:"final"`
}

//+kubebuilder:object:root=true
//+kubebuilder:resource:scope=Namespaced
//+kubebuilder:printcolumn:name="Tenant",type=string,JSONPath=`.spec.tenant`
//+kubebuilder:printcolumn:name="Peer",type=string,JSONPath=`.spec.peerClass`
//+kubebuilder:printcolumn:name="Zone",type=string,JSONPath=`.spec.zoneClass`
//+kubebuilder:printcolumn:name="Egress",type=integer,JSONPath=`.spec.egressBytes`
//+kubebuilder:printcolumn:name="Final",type=boolean,JSONPath=`.spec.final`

// GryviaNetworkUsageRecord is the Schema for the gryvianetworkusagerecords API. It is written by the
// collector (opt-in), never by a controller.
type GryviaNetworkUsageRecord struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec GryviaNetworkUsageRecordSpec `json:"spec,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaNetworkUsageRecordList contains a list of GryviaNetworkUsageRecord
type GryviaNetworkUsageRecordList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaNetworkUsageRecord `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GryviaNetworkUsageRecord{}, &GryviaNetworkUsageRecordList{})
}
