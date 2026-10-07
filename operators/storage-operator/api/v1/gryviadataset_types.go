package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GryviaDatasetSpec defines the desired state of GryviaDataset
type GryviaDatasetSpec struct {
	// Description is a human-readable description
	Description string `json:"description,omitempty"`

	// Type is the dataset type (image, text, audio, video, tabular, multimodal)
	Type string `json:"type,omitempty"`

	// License is the dataset license
	License string `json:"license,omitempty"`

	// Tags are searchable labels
	Tags []string `json:"tags,omitempty"`

	// Source defines the data source
	Source DatasetSource `json:"source"`

	// Version is the semantic version
	Version string `json:"version,omitempty"`

	// Versioning defines versioning configuration
	Versioning *DatasetVersioning `json:"versioning,omitempty"`

	// Cache defines caching configuration
	Cache *DatasetCache `json:"cache,omitempty"`

	// Access defines access control
	Access *DatasetAccess `json:"access,omitempty"`

	// Statistics defines dataset statistics
	Statistics *DatasetStatistics `json:"statistics,omitempty"`

	// Namespace is where the dataset is materialized (its PVC and download Jobs); jobs that read it run there.
	// Empty uses the storage-operator's --dataset-namespace.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`
	Namespace string `json:"namespace,omitempty"`

	// Placement keeps an extra copy of the current version in each listed node pool, so jobs in that pool read
	// local data. Each copy is its own PVC, filled by a download Job pinned to the pool, and is verified
	// against the primary copy's digest.
	// +optional
	Placement *DatasetPlacement `json:"placement,omitempty"`
}

// DatasetPlacement lists the pools that get a copy.
type DatasetPlacement struct {
	// Pools each get one replica.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	Pools []DatasetPool `json:"pools"`

	// StorageClass for the replica PVCs; a node-local class with volumeBindingMode WaitForFirstConsumer binds each
	// replica inside its pool. Default: spec.cache.storageClass, then the cluster default.
	// +optional
	StorageClass string `json:"storageClass,omitempty"`

	// AccessMode of the replica PVCs. ReadWriteOnce (default) volumes can only be mounted by pods on one node, so
	// jobs mount a ReadWriteOnce replica only when they run on a single node; ReadOnlyMany or ReadWriteMany
	// replicas are mounted by multi-node jobs too.
	// +kubebuilder:validation:Enum=ReadWriteOnce;ReadOnlyMany;ReadWriteMany
	// +optional
	AccessMode string `json:"accessMode,omitempty"`
}

// DatasetPool is a set of nodes that shares one replica.
type DatasetPool struct {
	// Name identifies the pool (part of the replica PVC name).
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]{0,22}[a-z0-9])?$`
	Name string `json:"name"`

	// NodeSelector picks the pool's nodes (for example topology.kubernetes.io/zone or gryvia.io/rack).
	// +kubebuilder:validation:MinProperties=1
	NodeSelector map[string]string `json:"nodeSelector"`
}

// DatasetReplica is the state of one pool's copy.
type DatasetReplica struct {
	Pool         string            `json:"pool"`
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	PVCName      string            `json:"pvcName,omitempty"`
	AccessMode   string            `json:"accessMode,omitempty"`
	// Version is the version directory (PVC SubPath) the replica holds; SourceHash the source it came from.
	Version    string `json:"version,omitempty"`
	SourceHash string `json:"sourceHash,omitempty"`
	// Digest is the sha256 over the replica's files, computed like the primary copy's checksum.
	Digest string `json:"digest,omitempty"`
	// Ready is true once the replica holds the current version; Verified once its digest equals the primary's.
	Ready    bool  `json:"ready"`
	Verified bool  `json:"verified"`
	Files    int64 `json:"files,omitempty"`
	Bytes    int64 `json:"bytes,omitempty"`
	// LastSynced is when the replica last finished a download.
	LastSynced *metav1.Time `json:"lastSynced,omitempty"`
	Message    string       `json:"message,omitempty"`
}

// DatasetSource defines where the data comes from
type DatasetSource struct {
	// Type is the source type (s3, gcs, azure-blob, nfs, http, git-lfs, vast)
	Type string `json:"type"`

	// S3 is the S3 configuration
	S3 *S3Source `json:"s3,omitempty"`

	// NFS is the NFS configuration
	NFS *NFSSource `json:"nfs,omitempty"`

	// HTTP is the HTTP configuration
	HTTP *HTTPSource `json:"http,omitempty"`
}

// S3Source defines S3 data source
type S3Source struct {
	// Bucket is the S3 bucket name
	Bucket string `json:"bucket"`

	// Prefix is the S3 key prefix
	Prefix string `json:"prefix,omitempty"`

	// Region is the S3 region
	Region string `json:"region,omitempty"`

	// Endpoint is the URL of an S3-compatible service (MinIO, Ceph RGW, R2, ...), for example
	// http://minio.storage.svc:9000. Empty means AWS. Passed to the AWS CLI as AWS_ENDPOINT_URL.
	// +kubebuilder:validation:Pattern=`^(https?://[^\s/]+(/[^\s]*)?)?$`
	Endpoint string `json:"endpoint,omitempty"`

	// CredentialsSecret is the name of the secret with credentials
	CredentialsSecret string `json:"credentialsSecret,omitempty"`
}

// NFSSource defines NFS data source
type NFSSource struct {
	// Server is the NFS server address
	Server string `json:"server"`

	// Path is the NFS export path
	Path string `json:"path"`
}

// HTTPSource defines HTTP data source
type HTTPSource struct {
	// URL is the download URL
	URL string `json:"url"`

	// ChecksumURL is the URL for checksum verification
	ChecksumURL string `json:"checksumURL,omitempty"`
}

// DatasetVersioning defines versioning configuration
type DatasetVersioning struct {
	// Enabled enables versioning
	Enabled bool `json:"enabled,omitempty"`

	// Strategy is the versioning strategy (snapshot, incremental, git-lfs)
	Strategy string `json:"strategy,omitempty"`

	// RetentionPolicy defines version retention
	RetentionPolicy *RetentionPolicy `json:"retentionPolicy,omitempty"`
}

// RetentionPolicy defines version retention
type RetentionPolicy struct {
	// KeepLast is the number of versions to keep
	KeepLast int32 `json:"keepLast,omitempty"`

	// KeepDays is the number of days to keep versions
	KeepDays int32 `json:"keepDays,omitempty"`
}

// DatasetCache defines caching configuration
type DatasetCache struct {
	// Enabled enables caching
	Enabled bool `json:"enabled,omitempty"`

	// StorageClass is the storage class for cache PVCs
	StorageClass string `json:"storageClass,omitempty"`

	// Size is the cache size
	Size string `json:"size,omitempty"`

	// Warmup starts the spec.placement replica downloads together with the primary download instead of after it
	// completes. Replicas are still only marked verified once the primary copy's digest is known.
	Warmup bool `json:"warmup,omitempty"`
}

// DatasetAccess defines access control
type DatasetAccess struct {
	// Public indicates whether the dataset is publicly accessible
	Public bool `json:"public,omitempty"`

	// AllowedTeams lists teams that can access the dataset
	AllowedTeams []string `json:"allowedTeams,omitempty"`

	// AllowedUsers lists users that can access the dataset
	AllowedUsers []string `json:"allowedUsers,omitempty"`
}

// DatasetStatistics defines dataset statistics
type DatasetStatistics struct {
	// SampleCount is the number of samples
	SampleCount int64 `json:"sampleCount,omitempty"`

	// TotalSizeBytes is the total size in bytes
	TotalSizeBytes int64 `json:"totalSizeBytes,omitempty"`

	// FileCount is the number of files
	FileCount int64 `json:"fileCount,omitempty"`
}

// DatasetVersionInfo holds version metadata
type DatasetVersionInfo struct {
	// Version is the version string
	Version string `json:"version"`

	// CreatedAt is the version creation time
	CreatedAt *metav1.Time `json:"createdAt,omitempty"`

	// Size is the version size in bytes
	Size int64 `json:"size,omitempty"`

	// Checksum is the version checksum
	Checksum string `json:"checksum,omitempty"`

	// Changes describes what changed
	Changes string `json:"changes,omitempty"`
}

// DatasetCacheStatus holds cache status
type DatasetCacheStatus struct {
	// Cached indicates whether data is cached
	Cached bool `json:"cached,omitempty"`

	// HitRate is the cache hit rate
	HitRate float64 `json:"hitRate,omitempty"`

	// LastAccess is the last cache access time
	LastAccess *metav1.Time `json:"lastAccess,omitempty"`
}

// DatasetUsageStatus holds usage information
type DatasetUsageStatus struct {
	// JobsUsing is the number of jobs currently using the dataset
	JobsUsing int32 `json:"jobsUsing,omitempty"`

	// LastUsedBy is the name of the last job that used the dataset
	LastUsedBy string `json:"lastUsedBy,omitempty"`

	// TotalAccesses is the total number of accesses
	TotalAccesses int64 `json:"totalAccesses,omitempty"`
}

// GryviaDatasetStatus defines the observed state of GryviaDataset
type GryviaDatasetStatus struct {
	// Conditions report runtime capability and remediation state.
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// State is the current state (initializing, ready, error, syncing)
	State string `json:"state,omitempty"`

	// CurrentVersion is the current active version
	CurrentVersion string `json:"currentVersion,omitempty"`

	// Versions is the list of available versions
	Versions []DatasetVersionInfo `json:"versions,omitempty"`

	// CacheStatus holds cache information
	CacheStatus *DatasetCacheStatus `json:"cacheStatus,omitempty"`

	// Usage holds usage information
	Usage *DatasetUsageStatus `json:"usage,omitempty"`

	// Namespace and PVCName locate the materialized data; each version is the directory SubPath of the PVC.
	Namespace string `json:"namespace,omitempty"`
	PVCName   string `json:"pvcName,omitempty"`
	SubPath   string `json:"subPath,omitempty"`

	// FileCount and TotalSizeBytes describe the current version as downloaded.
	FileCount      int64 `json:"fileCount,omitempty"`
	TotalSizeBytes int64 `json:"totalSizeBytes,omitempty"`

	// SourceHash identifies the source and version the current data was materialized from.
	SourceHash string `json:"sourceHash,omitempty"`

	// Message explains the state (the download error, for example).
	Message string `json:"message,omitempty"`

	// Replicas reports the spec.placement copies, one per pool.
	Replicas []DatasetReplica `json:"replicas,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Cluster
//+kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
//+kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.status.currentVersion`
//+kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
//+kubebuilder:printcolumn:name="Jobs-Using",type=integer,JSONPath=`.status.usage.jobsUsing`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// GryviaDataset is the Schema for the gryviadatasets API
type GryviaDataset struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GryviaDatasetSpec   `json:"spec,omitempty"`
	Status GryviaDatasetStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// GryviaDatasetList contains a list of GryviaDataset
type GryviaDatasetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GryviaDataset `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GryviaDataset{}, &GryviaDatasetList{})
}
