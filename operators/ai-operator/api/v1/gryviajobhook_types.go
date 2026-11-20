// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GryviaJobHookSpec selects job phase transitions in the hook's namespace and the webhook they are delivered to.
type GryviaJobHookSpec struct {
	// Events are the phases that trigger a delivery when a job reaches them.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=8
	// +listType=set
	Events []JobHookEvent `json:"events"`

	// Kinds restricts the watched kinds; empty means both.
	// +kubebuilder:validation:MaxItems=2
	// +listType=set
	// +optional
	Kinds []JobHookKind `json:"kinds,omitempty"`

	// Selector restricts the jobs by label; empty matches every job in the namespace.
	// +optional
	Selector *metav1.LabelSelector `json:"selector,omitempty"`

	// Webhook is where deliveries are POSTed.
	Webhook JobHookWebhook `json:"webhook"`

	// Retry bounds redelivery of a failed POST.
	// +optional
	Retry *JobHookRetry `json:"retry,omitempty"`

	// Suspend stops new deliveries; transitions that happen while suspended are not delivered later.
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// JobHookEvent is a job phase.
// +kubebuilder:validation:Enum=Running;Succeeded;Failed;Cancelled;Preempted;Rejected
type JobHookEvent string

// JobHookKind is a watched job kind.
// +kubebuilder:validation:Enum=GryviaAIJob;GryviaWorkflow
type JobHookKind string

// JobHookWebhook is the receiver.
type JobHookWebhook struct {
	// URL is an absolute http(s) URL without credentials. Plain http is only accepted for addresses the operator
	// allows (--job-hook-allowed-cidrs), typically in-cluster receivers.
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^https?://`
	URL string `json:"url"`

	// Format of the body: gryvia (the delivery JSON) or slack (a {"text": ...} message for an incoming webhook).
	// +kubebuilder:validation:Enum=gryvia;slack
	// +kubebuilder:default=gryvia
	// +optional
	Format string `json:"format,omitempty"`

	// Headers are extra, non-secret request headers.
	// +kubebuilder:validation:MaxProperties=16
	// +optional
	Headers map[string]string `json:"headers,omitempty"`

	// SecretRef names a Secret in the hook's namespace whose "secret" key signs each body
	// (X-Gryvia-Signature: sha256=HMAC(secret, "<X-Gryvia-Timestamp>.<body>")).
	// +optional
	SecretRef *JobHookSecretRef `json:"secretRef,omitempty"`

	// TimeoutSeconds bounds one POST.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=30
	// +kubebuilder:default=10
	// +optional
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
}

// JobHookSecretRef names a Secret in the hook's namespace.
type JobHookSecretRef struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// JobHookRetry bounds redelivery.
type JobHookRetry struct {
	// Attempts is the total number of POSTs for one transition, including the first.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	// +kubebuilder:default=3
	// +optional
	Attempts int32 `json:"attempts,omitempty"`

	// BackoffSeconds is the wait before the second attempt; it doubles for each later one.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=3600
	// +kubebuilder:default=10
	// +optional
	BackoffSeconds int32 `json:"backoffSeconds,omitempty"`
}

// JobHookDelivery describes one delivery.
type JobHookDelivery struct {
	Kind      string      `json:"kind"`
	Name      string      `json:"name"`
	Event     string      `json:"event"`
	Time      metav1.Time `json:"time"`
	Attempt   int32       `json:"attempt"`
	Succeeded bool        `json:"succeeded"`
	// +optional
	Error string `json:"error,omitempty"`
}

// GryviaJobHookStatus defines the observed state of GryviaJobHook
type GryviaJobHookStatus struct {
	// Conditions: Ready (the spec is valid and the signing Secret, if any, exists).
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Deliveries counts successful deliveries.
	// +optional
	Deliveries int64 `json:"deliveries,omitempty"`

	// Failures counts transitions given up on after the last attempt.
	// +optional
	Failures int64 `json:"failures,omitempty"`

	// LastDelivery is the most recent attempt.
	// +optional
	LastDelivery *JobHookDelivery `json:"lastDelivery,omitempty"`

	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:path=gryviajobhooks,scope=Namespaced,shortName=gjh
//+kubebuilder:printcolumn:name="Events",type=string,JSONPath=`.spec.events`
//+kubebuilder:printcolumn:name="Deliveries",type=integer,JSONPath=`.status.deliveries`
//+kubebuilder:printcolumn:name="Failures",type=integer,JSONPath=`.status.failures`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// GryviaJobHook delivers a webhook when a GryviaAIJob or GryviaWorkflow in its namespace reaches one of its events.
type GryviaJobHook struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GryviaJobHookSpec   `json:"spec,omitempty"`
	Status GryviaJobHookStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaJobHookList contains a list of GryviaJobHook
type GryviaJobHookList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaJobHook `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GryviaJobHook{}, &GryviaJobHookList{})
}
