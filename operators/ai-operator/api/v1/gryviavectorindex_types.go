// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Vector index phases.
const (
	VectorIndexPending      = "Pending"
	VectorIndexProvisioning = "Provisioning"
	VectorIndexIngesting    = "Ingesting"
	VectorIndexReady        = "Ready"
	VectorIndexFailed       = "Failed"
)

// Vector store types.
const (
	VectorStoreManaged  = "managed"
	VectorStoreExternal = "external"
)

// GryviaVectorIndexSpec defines the desired state of GryviaVectorIndex
type GryviaVectorIndexSpec struct {
	// DatasetRef is the GryviaDataset whose files are indexed. It must be materialized (state ready) in this
	// index's namespace; a new dataset version is ingested again
	// +kubebuilder:validation:MinLength=1
	DatasetRef string `json:"datasetRef"`

	// Embedding is how chunks and queries are embedded: through the LLM gateway's /v1/embeddings
	Embedding VectorEmbedding `json:"embedding"`

	// Chunking splits each file into overlapping pieces of text
	Chunking *VectorChunking `json:"chunking,omitempty"`

	// Store is where the vectors live
	Store VectorStore `json:"store"`

	// Collection is the name queries use (a Qdrant alias); default: the index name
	// +kubebuilder:validation:Pattern=`^([A-Za-z0-9][A-Za-z0-9_-]{0,62})?$`
	Collection string `json:"collection,omitempty"`

	// Schedule re-ingests on a cron schedule (UTC, five fields) even when the dataset did not change
	Schedule string `json:"schedule,omitempty"`

	// Suspend stops ingestion; the store keeps serving queries
	Suspend bool `json:"suspend,omitempty"`

	// IngestImage overrides the operator's ingestion image (--rag-ingest-image)
	IngestImage string `json:"ingestImage,omitempty"`
}

// VectorEmbedding names the embedding model.
type VectorEmbedding struct {
	// Model is a model name published by the LLM gateway (annotation gryvia.io/llm-model) that serves
	// /v1/embeddings and that keys of this namespace may call
	// +kubebuilder:validation:MinLength=1
	Model string `json:"model"`

	// BatchSize is how many chunks are embedded per request (default 32)
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1024
	BatchSize int32 `json:"batchSize,omitempty"`
}

// VectorChunking sets the chunk size and overlap in characters. Without a chunking block: size 1000, overlap 200.
type VectorChunking struct {
	// Size of a chunk in characters (default 1000)
	// +kubebuilder:validation:Minimum=50
	// +kubebuilder:validation:Maximum=20000
	Size int32 `json:"size,omitempty"`

	// Overlap between consecutive chunks in characters (0: none; less than size)
	// +kubebuilder:validation:Minimum=0
	Overlap int32 `json:"overlap,omitempty"`
}

// VectorStore is a managed Qdrant run by the operator or an external Qdrant-compatible URL.
type VectorStore struct {
	// Type is managed (a Qdrant StatefulSet, Service and PVC owned by the index) or external
	// +kubebuilder:validation:Enum=managed;external
	Type string `json:"type"`

	// Managed sets the managed store's image and storage
	Managed *ManagedVectorStore `json:"managed,omitempty"`

	// External is the URL of a Qdrant HTTP API (and an optional API key)
	External *ExternalVectorStore `json:"external,omitempty"`
}

// ManagedVectorStore configures the operator-run Qdrant.
type ManagedVectorStore struct {
	// Image overrides the operator's Qdrant image (--rag-qdrant-image). The pod runs as uid 1000, as the -unprivileged Qdrant images do
	Image string `json:"image,omitempty"`

	// StorageSize of the PVC (default 10Gi)
	StorageSize string `json:"storageSize,omitempty"`

	// StorageClass of the PVC (default: the cluster default)
	StorageClass string `json:"storageClass,omitempty"`
}

// ExternalVectorStore points at a Qdrant HTTP API outside the operator's control.
type ExternalVectorStore struct {
	// URL of the Qdrant HTTP API, for example http://qdrant.vector.svc:6333
	// +kubebuilder:validation:Pattern=`^https?://[^\s]+$`
	URL string `json:"url"`

	// APIKeySecretRef is a Secret in the index's namespace holding the api-key header value (key defaults to "token")
	APIKeySecretRef *SecretKeyRef `json:"apiKeySecretRef,omitempty"`
}

// GryviaVectorIndexStatus defines the observed state of GryviaVectorIndex
type GryviaVectorIndexStatus struct {
	// Phase is Pending, Provisioning, Ingesting, Ready or Failed
	Phase string `json:"phase,omitempty"`

	// Message explains the phase
	Message string `json:"message,omitempty"`

	// StoreURL is the Qdrant HTTP API the index uses
	StoreURL string `json:"storeURL,omitempty"`

	// Collection is the alias queries use
	Collection string `json:"collection,omitempty"`

	// Documents is the number of files in the last ingestion
	Documents int64 `json:"documents,omitempty"`

	// Chunks is the number of vectors in the last ingestion
	Chunks int64 `json:"chunks,omitempty"`

	// Dimensions of the embedding vectors
	Dimensions int64 `json:"dimensions,omitempty"`

	// DatasetVersion is the dataset version the served collection was built from
	DatasetVersion string `json:"datasetVersion,omitempty"`

	// LastIngested is when the last successful ingestion finished
	LastIngested *metav1.Time `json:"lastIngested,omitempty"`

	// IngestJob is the name of the current or last ingestion Job
	IngestJob string `json:"ingestJob,omitempty"`

	// IngestedGeneration is the spec generation of the last successful ingestion
	IngestedGeneration int64 `json:"ingestedGeneration,omitempty"`

	// NextScheduleTime is the next scheduled re-ingestion
	NextScheduleTime *metav1.Time `json:"nextScheduleTime,omitempty"`

	// KeySecret is the Secret in this namespace holding the LLM gateway key the ingestion uses
	KeySecret string `json:"keySecret,omitempty"`

	// ObservedGeneration is the spec generation the status describes
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions: StoreReady, Ingested
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:path=gryviavectorindexes,scope=Namespaced,shortName=gvi
//+kubebuilder:printcolumn:name="Dataset",type=string,JSONPath=`.spec.datasetRef`
//+kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.spec.embedding.model`
//+kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
//+kubebuilder:printcolumn:name="Chunks",type=integer,JSONPath=`.status.chunks`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// GryviaVectorIndex is a retrieval index: a dataset chunked, embedded through the LLM gateway and stored in Qdrant,
// queried with the gateway's POST /v1/retrieve
type GryviaVectorIndex struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GryviaVectorIndexSpec   `json:"spec,omitempty"`
	Status GryviaVectorIndexStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaVectorIndexList contains a list of GryviaVectorIndex
type GryviaVectorIndexList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaVectorIndex `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GryviaVectorIndex{}, &GryviaVectorIndexList{})
}
