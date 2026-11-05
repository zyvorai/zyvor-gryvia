package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GryviaUsageRecordSpec is the metered usage of one GryviaAIJob. The quota operator
// owns and rewrites it until final is true. Values are estimates from job wall-clock
// time (start to end); they are not invoices. Records of kind tokens are written by the
// LLM gateway instead (job "llm:<model>", gpus and gpuHours 0, cost from token prices).
type GryviaUsageRecordSpec struct {
	// Tenant the usage is attributed to
	Tenant string `json:"tenant"`

	// Job is the GryviaAIJob name
	Job string `json:"job"`

	// JobUID is the GryviaAIJob UID (the record is named usage-<jobUID>, or usage-<jobUID>-<n> for
	// the nth run after a Kueue eviction)
	JobUID string `json:"jobUID"`

	// GpuType of the job
	GpuType string `json:"gpuType,omitempty"`

	// Sku is the name of the matching GryviaGpuSku, empty when the default price table was used
	Sku string `json:"sku,omitempty"`

	// Gpus is the whole GPU unit count (TotalGPUs); kept for compatibility and dashboards that
	// expect an integer slot count.
	Gpus int32 `json:"gpus"`

	// EffectiveGpus is the physical GPU fraction billed (fractional share or MIG slice). When
	// unset or zero on older records, consumers should fall back to gpus.
	EffectiveGpus float64 `json:"effectiveGpus,omitempty"`

	// Start is when the run started (the job first ran, or Kueue admitted it again)
	Start metav1.Time `json:"start"`

	// End is when the run finished (the job ended or Kueue evicted it); nil while it is running
	End *metav1.Time `json:"end,omitempty"`

	// GpuHours is wall-clock hours multiplied by effectiveGpus (or gpus when effectiveGpus is 0)
	GpuHours float64 `json:"gpuHours"`

	// Rate is the price per GPU-hour used for cost
	Rate float64 `json:"rate"`

	// Cost is gpuHours multiplied by rate
	Cost float64 `json:"cost"`

	// Currency of rate and cost
	Currency string `json:"currency,omitempty"`

	// Final is true once the run finished; a final record is never modified again
	Final bool `json:"final"`

	// Kind is what the record meters: gpu (a GryviaAIJob; also when empty), tokens (the LLM gateway's requests
	// of one tenant to one model during one hour, from start to end) or slurm (one finished Slurm job in the
	// tenant's partition, named slurm-<cluster>-<job id>)
	// +kubebuilder:validation:Enum=gpu;tokens;slurm
	Kind string `json:"kind,omitempty"`

	// Model is the LLM gateway model of a tokens record
	Model string `json:"model,omitempty"`

	// InputTokens is the prompt tokens of a tokens record
	InputTokens int64 `json:"inputTokens,omitempty"`

	// OutputTokens is the completion tokens of a tokens record
	OutputTokens int64 `json:"outputTokens,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:resource:scope=Namespaced
//+kubebuilder:printcolumn:name="Tenant",type=string,JSONPath=`.spec.tenant`
//+kubebuilder:printcolumn:name="Job",type=string,JSONPath=`.spec.job`
//+kubebuilder:printcolumn:name="GPU-Hours",type=number,JSONPath=`.spec.gpuHours`
//+kubebuilder:printcolumn:name="Cost",type=number,JSONPath=`.spec.cost`
//+kubebuilder:printcolumn:name="Final",type=boolean,JSONPath=`.spec.final`

// GryviaUsageRecord is the Schema for the gryviausagerecords API
type GryviaUsageRecord struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec GryviaUsageRecordSpec `json:"spec,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaUsageRecordList contains a list of GryviaUsageRecord
type GryviaUsageRecordList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaUsageRecord `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GryviaUsageRecord{}, &GryviaUsageRecordList{})
}
