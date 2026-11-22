// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GryviaFabricSignalSpec defines the desired state of GryviaFabricSignal
type GryviaFabricSignalSpec struct {
	// JobRef is the name of the GryviaAIJob in the same namespace whose fabric
	// signals (NCCL stragglers, RDMA retries, GPUDirect Storage) are tracked
	JobRef string `json:"jobRef,omitempty"`

	// ObserveOnly keeps the signals advisory: nothing is enforced (no lease
	// revocation, no fail-open) based on them
	// +kubebuilder:default=true
	ObserveOnly *bool `json:"observeOnly,omitempty"`
}

// FabricNodeStatus is what ONE node's collector measured for the job (collector flag
// -fabric-status-per-node). Each collector owns only its entry (server-side apply, field manager
// gryvia-collector-<node>), so nodes cannot overwrite each other. A nil field means the node did
// not measure it. With the ai-operator's --merge-fabric-signals the fresh entries are folded into
// the top-level status fields.
type FabricNodeStatus struct {
	// Node is the Kubernetes node the collector runs on (list map key)
	Node string `json:"node"`

	// MeasuredAt is when the collector wrote this entry
	MeasuredAt metav1.Time `json:"measuredAt"`

	// TTLSeconds is how long the entry stays valid after measuredAt; older entries are ignored
	// +kubebuilder:default=300
	// +kubebuilder:validation:Minimum=1
	TTLSeconds int32 `json:"ttlSeconds,omitempty"`

	// SampleCount is the number of signals folded into the entry (the weight of its ratios)
	// +kubebuilder:validation:Minimum=0
	SampleCount int64 `json:"sampleCount,omitempty"`

	StragglerRank          *int32   `json:"stragglerRank,omitempty"`
	NCCLP99Ms              *float64 `json:"ncclP99ms,omitempty"`
	RDMARetryRate          *float64 `json:"rdmaRetryRate,omitempty"`
	GDSHitRatio            *float64 `json:"gdsHitRatio,omitempty"`
	OverlapIdleRatio       *float64 `json:"overlapIdleRatio,omitempty"`
	CNPRate                *float64 `json:"cnpRate,omitempty"`
	InferWaitP99Ms         *float64 `json:"inferWaitP99ms,omitempty"`
	PFCRate                *float64 `json:"pfcRate,omitempty"`
	ExfilEvents            *int64   `json:"exfilEvents,omitempty"`
	CollectiveMaxSkewMs    *float64 `json:"collectiveMaxSkewMs,omitempty"`
	GPUIdleDuringCommRatio *float64 `json:"gpuIdleDuringCommRatio,omitempty"`
	SMActiveDuringCompute  *float64 `json:"smActiveDuringCompute,omitempty"`
	GPUCorrelationCoverage *float64 `json:"gpuCorrelationCoverage,omitempty"`
	ScoreDelta             *float64 `json:"scoreDelta,omitempty"`
	Engine                 *string  `json:"engine,omitempty"`
	TTFTP99Ms              *float64 `json:"ttftP99ms,omitempty"`
	ITLP99Ms               *float64 `json:"itlP99ms,omitempty"`
	QueueTimeP99Ms         *float64 `json:"queueTimeP99ms,omitempty"`
	E2EP99Ms               *float64 `json:"e2eP99ms,omitempty"`
	QueueTimeMeanMs        *float64 `json:"queueTimeMeanMs,omitempty"`
	E2EMeanMs              *float64 `json:"e2eMeanMs,omitempty"`
	RequestsWaiting        *int64   `json:"requestsWaiting,omitempty"`
	KVCacheUsage           *float64 `json:"kvCacheUsage,omitempty"`
}

// GryviaFabricSignalStatus defines the observed state of GryviaFabricSignal
type GryviaFabricSignalStatus struct {
	// StragglerRank is the rank of the most recent NCCL straggler
	StragglerRank int32 `json:"stragglerRank,omitempty"`

	// NCCLP99Ms is the p99 latency in milliseconds of straggler-flagged NCCL collectives
	NCCLP99Ms float64 `json:"ncclP99ms,omitempty"`

	// RDMARetryRate is the ratio of RDMA retry/RNR error completions to posted sends
	RDMARetryRate float64 `json:"rdmaRetryRate,omitempty"`

	// GDSHitRatio is the fraction of cuFile bytes served over the direct GPUDirect Storage path
	GDSHitRatio float64 `json:"gdsHitRatio,omitempty"`

	// OverlapIdleRatio is the fraction of the window the job spent in cudaDeviceSynchronize
	// inside an in-flight ncclAllReduce (GPU idle while communicating)
	OverlapIdleRatio float64 `json:"overlapIdleRatio,omitempty"`

	// CNPRate is the RoCEv2 congestion notification packets per second seen on the node
	CNPRate float64 `json:"cnpRate,omitempty"`

	// InferWaitP99Ms is the p99 NETWORK wait in milliseconds from accept to first read on
	// inference ports (eBPF). It is not engine queue time, time to first token or inter-token
	// latency: see the engine fields below
	InferWaitP99Ms float64 `json:"inferWaitP99ms,omitempty"`

	// Engine names the serving engine whose own metrics fill the fields below (vllm, triton,
	// tgi, or mixed). Empty when the collector's opt-in metrics scraper (-infer-metrics) is off.
	// Fields the engine does not export are left unset, meaning not measured, never zero
	Engine string `json:"engine,omitempty"`

	// TTFTP99Ms is the p99 time to first token in milliseconds over the collector window (vLLM)
	TTFTP99Ms *float64 `json:"ttftP99ms,omitempty"`

	// ITLP99Ms is the p99 inter-token latency (time per output token) in milliseconds (vLLM); for
	// TGI it is the p99 of the per-request mean time per token
	ITLP99Ms *float64 `json:"itlP99ms,omitempty"`

	// QueueTimeP99Ms is the p99 time in milliseconds a request waited in the engine's queue
	QueueTimeP99Ms *float64 `json:"queueTimeP99ms,omitempty"`

	// E2EP99Ms is the p99 end-to-end request latency in milliseconds inside the engine
	E2EP99Ms *float64 `json:"e2eP99ms,omitempty"`

	// QueueTimeMeanMs is the mean engine queue time in milliseconds, for engines that export only
	// cumulative duration counters (Triton without summary latencies); not a percentile
	QueueTimeMeanMs *float64 `json:"queueTimeMeanMs,omitempty"`

	// E2EMeanMs is the mean end-to-end request latency in milliseconds for counter-only engines;
	// not a percentile
	E2EMeanMs *float64 `json:"e2eMeanMs,omitempty"`

	// RequestsWaiting is the number of requests queued in the engine (summed over the scraped
	// replicas on the reporting node)
	RequestsWaiting *int64 `json:"requestsWaiting,omitempty"`

	// KVCacheUsage is the KV cache usage in [0,1] reported by the engine (worst replica)
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1
	KVCacheUsage *float64 `json:"kvCacheUsage,omitempty"`

	// PFCRate is the 802.1Qbb priority-flow-control pause frames per second seen on the node
	PFCRate float64 `json:"pfcRate,omitempty"`

	// ExfilEvents is the number of large model-file reads followed by a connect to a non-internal
	// address from the same process in the window (observe only, never part of scoreDelta)
	ExfilEvents int64 `json:"exfilEvents,omitempty"`

	// CollectiveMaxSkewMs is the largest slowest-minus-fastest host-side NCCL call duration in
	// milliseconds among collectives matched by (communicator ordinal, sequence, op) across the
	// job's ranks on the publishing node (0 = none matched; informational, never part of scoreDelta)
	CollectiveMaxSkewMs float64 `json:"collectiveMaxSkewMs,omitempty"`

	// GPUIdleDuringCommRatio is the fraction of NCCL communication time (covered by DCGM samples)
	// during which SM_ACTIVE (else GPU_UTIL) was below the idle threshold. Set only when the
	// collector runs with -dcgm-correlate and measured it; informational, never part of scoreDelta
	GPUIdleDuringCommRatio float64 `json:"gpuIdleDuringCommRatio,omitempty"`

	// SMActiveDuringCompute is the mean DCGM SM activity (0-1) outside the NCCL call windows,
	// between the job's first and last observed collective (set only when measured)
	SMActiveDuringCompute float64 `json:"smActiveDuringCompute,omitempty"`

	// GPUCorrelationCoverage is the fraction of NCCL communication time covered by a DCGM sample;
	// a low value means the dcgm-exporter collect interval is too coarse for the ratios above
	GPUCorrelationCoverage float64 `json:"gpuCorrelationCoverage,omitempty"`

	// ScoreDelta is the penalty in [0,1] for the topology scorer (0 = healthy fabric)
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1
	ScoreDelta float64 `json:"scoreDelta,omitempty"`

	// UpdatedAt is the time the status was last refreshed from collector signals
	UpdatedAt *metav1.Time `json:"updatedAt,omitempty"`

	// Nodes holds one entry per collector node (collector flag -fabric-status-per-node); empty in
	// the default mode. The ai-operator (--merge-fabric-signals) folds the fresh entries into the
	// fields above: max for degradation metrics, sample-weighted means for ratios
	// +listType=map
	// +listMapKey=node
	// +kubebuilder:validation:MaxItems=256
	Nodes []FabricNodeStatus `json:"nodes,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Namespaced,shortName=gfs
//+kubebuilder:printcolumn:name="Job",type=string,JSONPath=`.spec.jobRef`
//+kubebuilder:printcolumn:name="Straggler",type=integer,JSONPath=`.status.stragglerRank`
//+kubebuilder:printcolumn:name="NCCL p99 ms",type=number,JSONPath=`.status.ncclP99ms`
//+kubebuilder:printcolumn:name="RDMA retry",type=number,JSONPath=`.status.rdmaRetryRate`
//+kubebuilder:printcolumn:name="GDS hit",type=number,JSONPath=`.status.gdsHitRatio`

// GryviaFabricSignal is the Schema for the gryviafabricsignals API
type GryviaFabricSignal struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GryviaFabricSignalSpec   `json:"spec,omitempty"`
	Status GryviaFabricSignalStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaFabricSignalList contains a list of GryviaFabricSignal
type GryviaFabricSignalList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaFabricSignal `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GryviaFabricSignal{}, &GryviaFabricSignalList{})
}
