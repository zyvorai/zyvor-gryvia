// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package lineage

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

// CollectedProvenance holds provenance data gathered from related CRs
type CollectedProvenance struct {
	// Infrastructure details gathered from the cluster
	Infrastructure gryviav1.InfrastructureProvenance

	// Training details gathered from the GryviaAIJob
	Training gryviav1.TrainingProvenance

	// Events gathered from the GryviaAIJob
	Events gryviav1.EventsProvenance

	// Code provenance gathered from the job container image
	Code gryviav1.CodeProvenance

	// JobFound indicates whether the referenced job was found
	JobFound bool

	// JobPhase is the current phase of the referenced job
	JobPhase string
}

// Collector gathers provenance data from related Kubernetes resources
type Collector struct {
	client.Client
}

// NewCollector creates a new provenance collector
func NewCollector(c client.Client) *Collector {
	return &Collector{Client: c}
}

// CollectProvenance gathers provenance data from the referenced GryviaAIJob
// and related cluster resources.
func (c *Collector) CollectProvenance(ctx context.Context, lineage *gryviav1.GryviaModelLineage) (*CollectedProvenance, error) {
	logger := log.FromContext(ctx)
	result := &CollectedProvenance{}

	jobRef := lineage.Spec.Provenance.Training.JobRef
	if jobRef == "" {
		logger.Info("No job reference specified, skipping auto-collection")
		return result, nil
	}

	// Fetch the referenced GryviaAIJob
	job := &gryviav1.GryviaAIJob{}
	err := c.Get(ctx, types.NamespacedName{
		Name:      jobRef,
		Namespace: lineage.Namespace,
	}, job)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("Referenced GryviaAIJob not found", "jobRef", jobRef)
			return result, nil
		}
		return nil, fmt.Errorf("failed to get GryviaAIJob %s: %w", jobRef, err)
	}

	result.JobFound = true
	result.JobPhase = job.Status.Phase

	// Collect infrastructure details from the job
	result.Infrastructure = c.collectInfrastructure(job)

	// Collect training details
	result.Training = c.collectTraining(job, lineage)

	// Collect code provenance from the job spec
	result.Code = c.collectCode(job, lineage)

	// Collect events from the job
	result.Events = c.collectEvents(job)

	return result, nil
}

// collectInfrastructure gathers infrastructure details from a GryviaAIJob
func (c *Collector) collectInfrastructure(job *gryviav1.GryviaAIJob) gryviav1.InfrastructureProvenance {
	infra := gryviav1.InfrastructureProvenance{
		GPUNodes:       job.Status.NodesAllocated,
		NetworkType:    job.Spec.Network,
		StorageBackend: job.Spec.Storage,
	}

	// Calculate GPU hours if the job has start and completion times
	if job.Status.StartTime != nil && !job.Status.StartTime.IsZero() {
		var duration time.Duration
		if job.Status.CompletionTime != nil && !job.Status.CompletionTime.IsZero() {
			duration = job.Status.CompletionTime.Time.Sub(job.Status.StartTime.Time)
		} else {
			// Job still running
			duration = time.Since(job.Status.StartTime.Time)
		}
		infra.TotalGpuHours = duration.Hours() * float64(job.Spec.GPUs)
	}

	return infra
}

// collectTraining gathers training configuration from a GryviaAIJob
func (c *Collector) collectTraining(job *gryviav1.GryviaAIJob, lineage *gryviav1.GryviaModelLineage) gryviav1.TrainingProvenance {
	training := gryviav1.TrainingProvenance{
		JobRef: job.Name,
	}

	// Preserve user-specified hyperparameters
	if lineage.Spec.Provenance.Training.Hyperparameters != nil {
		training.Hyperparameters = lineage.Spec.Provenance.Training.Hyperparameters
	}

	// Build distributed config summary from the job
	if job.Spec.Distributed != nil && job.Spec.Distributed.Enabled {
		training.DistributedConfig = fmt.Sprintf("%dx%s, %s, %s over %s",
			job.Spec.Distributed.Nodes,
			job.Spec.GpuType,
			job.Spec.Distributed.Framework,
			job.Spec.Distributed.Backend,
			job.Spec.Network,
		)
	} else if lineage.Spec.Provenance.Training.DistributedConfig != "" {
		training.DistributedConfig = lineage.Spec.Provenance.Training.DistributedConfig
	}

	return training
}

// collectCode gathers code provenance from a GryviaAIJob
func (c *Collector) collectCode(job *gryviav1.GryviaAIJob, lineage *gryviav1.GryviaModelLineage) gryviav1.CodeProvenance {
	code := lineage.Spec.Provenance.Code

	// Auto-fill container image from the job spec if not already set
	if code.ContainerImage == "" {
		code.ContainerImage = job.Spec.Image
	}

	return code
}

// collectEvents gathers event data from a GryviaAIJob
func (c *Collector) collectEvents(job *gryviav1.GryviaAIJob) gryviav1.EventsProvenance {
	events := gryviav1.EventsProvenance{}

	// Detect anomalies from job conditions
	for _, condition := range job.Status.Conditions {
		if condition.Status == "False" && condition.Reason != "" {
			events.Anomalies = append(events.Anomalies, gryviav1.AnomalyEvent{
				Timestamp:   condition.LastTransitionTime,
				Type:        condition.Type,
				Description: condition.Message,
			})
		}
	}

	// Record retry events as interventions
	if job.Status.Retries > 0 {
		ts := job.CreationTimestamp
		if job.Status.StartTime != nil {
			ts = *job.Status.StartTime
		}
		events.Interventions = append(events.Interventions, gryviav1.InterventionEvent{
			Timestamp: ts,
			Action:    fmt.Sprintf("Retried %d times", job.Status.Retries),
			Reason:    "Automatic retry on failure",
		})
	}

	return events
}

// ComputeProvenanceHash computes a SHA256 hash of the lineage provenance data.
// When cryptographic chaining is enabled, it incorporates the previous hash.
func ComputeProvenanceHash(lineage *gryviav1.GryviaModelLineage, previousHash string) (string, error) {
	// Build a canonical representation of the provenance
	provenanceData := struct {
		Model      gryviav1.ModelIdentity  `json:"model"`
		Provenance gryviav1.ProvenanceSpec `json:"provenance"`
		Compliance gryviav1.ComplianceSpec `json:"compliance"`
		PrevHash   string                  `json:"prevHash,omitempty"`
	}{
		Model:      lineage.Spec.Model,
		Provenance: lineage.Spec.Provenance,
		Compliance: lineage.Spec.Compliance,
	}

	if lineage.Spec.Compliance.CryptographicChain && previousHash != "" {
		provenanceData.PrevHash = previousHash
	}

	data, err := json.Marshal(provenanceData)
	if err != nil {
		return "", fmt.Errorf("failed to marshal provenance for hashing: %w", err)
	}

	hash := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", hash), nil
}

// EvaluateCompliance evaluates compliance status based on the lineage spec
func EvaluateCompliance(lineage *gryviav1.GryviaModelLineage) string {
	if len(lineage.Spec.Compliance.RegulatoryFramework) == 0 {
		return "Compliant"
	}

	// Check required fields for compliance
	issues := 0

	// Code provenance is required
	if lineage.Spec.Provenance.Code.GitRepo == "" || lineage.Spec.Provenance.Code.GitCommit == "" {
		issues++
	}

	// Training job reference is required
	if lineage.Spec.Provenance.Training.JobRef == "" {
		issues++
	}

	// Data provenance with checksums is required
	if len(lineage.Spec.Provenance.Data.Datasets) == 0 {
		issues++
	} else {
		for _, ds := range lineage.Spec.Provenance.Data.Datasets {
			if ds.Checksum == "" {
				issues++
				break
			}
		}
	}

	// Check data privacy requirements
	if lineage.Spec.Compliance.DataPrivacy.PIIScanned && lineage.Spec.Compliance.DataPrivacy.PIIFound {
		// PII found but not addressed
		if !lineage.Spec.Compliance.DataPrivacy.DPIACompleted {
			issues++
		}
	}

	// Check signing requirement
	if lineage.Spec.Compliance.SignedBy == "" {
		issues++
	}

	if issues == 0 {
		return "Compliant"
	}
	return "NonCompliant"
}

// CheckLineageCompleteness determines if all required provenance data is present
func CheckLineageCompleteness(lineage *gryviav1.GryviaModelLineage) bool {
	// Model identity must be complete
	if lineage.Spec.Model.Name == "" || lineage.Spec.Model.Version == "" {
		return false
	}

	// Code provenance
	if lineage.Spec.Provenance.Code.GitRepo == "" {
		return false
	}

	// Training reference
	if lineage.Spec.Provenance.Training.JobRef == "" {
		return false
	}

	// Infrastructure must be populated
	if len(lineage.Spec.Provenance.Infrastructure.GPUNodes) == 0 {
		return false
	}

	return true
}

// CheckReproducibility checks if the model can be reproduced from recorded provenance
func CheckReproducibility(lineage *gryviav1.GryviaModelLineage) bool {
	// Need code commit
	if lineage.Spec.Provenance.Code.GitCommit == "" {
		return false
	}

	// Need container image
	if lineage.Spec.Provenance.Code.ContainerImage == "" {
		return false
	}

	// Need hyperparameters
	if len(lineage.Spec.Provenance.Training.Hyperparameters) == 0 {
		return false
	}

	// Need dataset checksums
	for _, ds := range lineage.Spec.Provenance.Data.Datasets {
		if ds.Checksum == "" {
			return false
		}
	}

	return true
}
