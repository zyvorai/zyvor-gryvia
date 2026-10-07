package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Invoice states. Draft -> Finalized -> Paid or Void; nothing leaves Paid or Void.
const (
	InvoiceDraft     = "Draft"
	InvoiceFinalized = "Finalized"
	InvoicePaid      = "Paid"
	InvoiceVoid      = "Void"
)

// InvoiceLine is one frozen line of an invoice: the ledger entries of one SKU (or GPU type) in the period.
type InvoiceLine struct {
	Sku      string `json:"sku"`
	GpuType  string `json:"gpuType,omitempty"`
	Jobs     int32  `json:"jobs"`
	GpuHours string `json:"gpuHours"`
	Rate     string `json:"rate"`
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// InvoiceLedgerRange names the ledger entries an invoice was built from.
type InvoiceLedgerRange struct {
	// Entries is the number of ledger entries.
	Entries int32 `json:"entries"`
	// FirstSequence and LastSequence bound the entries' sequences (not necessarily contiguous: another month's
	// entries may sit between them).
	FirstSequence int64 `json:"firstSequence,omitempty"`
	LastSequence  int64 `json:"lastSequence,omitempty"`
	// HeadHash is the hash of the tenant's newest ledger entry when the invoice was finalized.
	HeadHash string `json:"headHash,omitempty"`
}

// InvoicePayment is the payment provider side of an invoice (Stripe test mode only).
type InvoicePayment struct {
	// Provider is "stripe".
	Provider string `json:"provider"`
	// InvoiceID is the provider's invoice id.
	InvoiceID string `json:"invoiceID"`
	// HostedURL is the provider's page for the invoice.
	HostedURL string `json:"hostedURL,omitempty"`
	// LiveMode is always false: only test-mode keys are accepted.
	LiveMode bool `json:"liveMode"`
}

// GryviaInvoiceSpec is a tenant's invoice for one month. Created as Draft by the gateway's finalize call, then
// Finalized with frozen lines; the webhook rejects any change to a non-Draft invoice other than the allowed state
// transitions, and deleting one.
type GryviaInvoiceSpec struct {
	// Tenant billed.
	Tenant string `json:"tenant"`

	// Period is the billed UTC month, YYYY-MM.
	// +kubebuilder:validation:Pattern=`^[0-9]{4}-(0[1-9]|1[0-2])$`
	Period string `json:"period"`

	// Number is the tenant's sequential invoice number, starting at 1.
	// +kubebuilder:validation:Minimum=1
	Number int64 `json:"number"`

	// State is Draft, Finalized, Paid or Void.
	// +kubebuilder:validation:Enum=Draft;Finalized;Paid;Void
	State string `json:"state"`

	// Currency of every line, or MIXED.
	Currency string `json:"currency"`

	// Lines are frozen at finalization.
	Lines []InvoiceLine `json:"lines,omitempty"`

	// Subtotal is the sum of the line amounts, a decimal string.
	Subtotal string `json:"subtotal"`

	// Jobs is the number of distinct jobs billed.
	Jobs int32 `json:"jobs,omitempty"`

	// Ledger is the ledger range the lines were built from.
	Ledger InvoiceLedgerRange `json:"ledger"`

	// FinalizedAt, FinalizedBy, PaidAt, VoidedAt, VoidedBy and VoidReason record the transitions.
	FinalizedAt *metav1.Time `json:"finalizedAt,omitempty"`
	FinalizedBy string       `json:"finalizedBy,omitempty"`
	PaidAt      *metav1.Time `json:"paidAt,omitempty"`
	VoidedAt    *metav1.Time `json:"voidedAt,omitempty"`
	VoidedBy    string       `json:"voidedBy,omitempty"`
	VoidReason  string       `json:"voidReason,omitempty"`

	// Payment is set once when the invoice is sent to the payment provider.
	Payment *InvoicePayment `json:"payment,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:resource:scope=Cluster
//+kubebuilder:printcolumn:name="Tenant",type=string,JSONPath=`.spec.tenant`
//+kubebuilder:printcolumn:name="Period",type=string,JSONPath=`.spec.period`
//+kubebuilder:printcolumn:name="Number",type=integer,JSONPath=`.spec.number`
//+kubebuilder:printcolumn:name="State",type=string,JSONPath=`.spec.state`
//+kubebuilder:printcolumn:name="Subtotal",type=string,JSONPath=`.spec.subtotal`

// GryviaInvoice is a finalized (or draft, paid, void) monthly invoice of a tenant.
type GryviaInvoice struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec GryviaInvoiceSpec `json:"spec,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaInvoiceList contains a list of GryviaInvoice
type GryviaInvoiceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaInvoice `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GryviaInvoice{}, &GryviaInvoiceList{})
}
