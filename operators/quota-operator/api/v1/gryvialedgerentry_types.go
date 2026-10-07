package v1

import (
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LedgerHashVersion names the hash input layout of GryviaLedgerEntrySpec.Hash (see LedgerHashInput).
const LedgerHashVersion = "v1"

// UsageRecordRef identifies the sealed GryviaUsageRecord a ledger entry was copied from.
type UsageRecordRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}

// GryviaLedgerEntrySpec is an append-only copy of one sealed usage record, chained per tenant: entry N carries the
// hash of entry N-1. Every field that enters the hash is a string or an integer (amounts are decimal strings), so
// any verifier can recompute it byte for byte. The quota operator writes entries with --billing-ledger; the
// webhook rejects every update and delete.
type GryviaLedgerEntrySpec struct {
	// Tenant the usage is attributed to.
	Tenant string `json:"tenant"`

	// Sequence is the 1-based position in the tenant's chain.
	// +kubebuilder:validation:Minimum=1
	Sequence int64 `json:"sequence"`

	// UsageRecord is the sealed record this entry copies.
	UsageRecord UsageRecordRef `json:"usageRecord"`

	// Kind of usage: gpu, tokens or slurm.
	Kind string `json:"kind"`

	// Job is the job (or llm:<model>, or Slurm job) the usage belongs to.
	Job string `json:"job"`

	// Sku or GPU type the price came from.
	Sku string `json:"sku,omitempty"`

	// Start and End of the metered run, RFC 3339 UTC seconds.
	Start string `json:"start"`
	End   string `json:"end"`

	// GpuHours, Rate and Cost as decimal strings (6 decimals).
	GpuHours string `json:"gpuHours"`
	Rate     string `json:"rate"`
	Cost     string `json:"cost"`

	// Currency of rate and cost.
	Currency string `json:"currency"`

	// RecordedAt is when the entry was appended, RFC 3339 UTC seconds.
	RecordedAt string `json:"recordedAt"`

	// PrevHash is the hash of the tenant's previous entry, empty for sequence 1.
	PrevHash string `json:"prevHash,omitempty"`

	// Hash is the hex SHA-256 of LedgerHashInput(spec).
	Hash string `json:"hash"`
}

// LedgerHashInput is the exact byte string hashed into Hash: the version and every field except Hash, one per line.
func LedgerHashInput(s GryviaLedgerEntrySpec) string {
	return strings.Join([]string{
		LedgerHashVersion, s.Tenant, strconv.FormatInt(s.Sequence, 10), s.PrevHash,
		s.UsageRecord.Namespace, s.UsageRecord.Name, s.UsageRecord.UID,
		s.Kind, s.Job, s.Sku, s.Start, s.End, s.GpuHours, s.Rate, s.Cost, s.Currency, s.RecordedAt,
	}, "\n")
}

//+kubebuilder:object:root=true
//+kubebuilder:resource:scope=Cluster
//+kubebuilder:printcolumn:name="Tenant",type=string,JSONPath=`.spec.tenant`
//+kubebuilder:printcolumn:name="Seq",type=integer,JSONPath=`.spec.sequence`
//+kubebuilder:printcolumn:name="Job",type=string,JSONPath=`.spec.job`
//+kubebuilder:printcolumn:name="Cost",type=string,JSONPath=`.spec.cost`
//+kubebuilder:printcolumn:name="Currency",type=string,JSONPath=`.spec.currency`

// GryviaLedgerEntry is one immutable, hash-chained billing ledger entry.
type GryviaLedgerEntry struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec GryviaLedgerEntrySpec `json:"spec,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaLedgerEntryList contains a list of GryviaLedgerEntry
type GryviaLedgerEntryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaLedgerEntry `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GryviaLedgerEntry{}, &GryviaLedgerEntryList{})
}
