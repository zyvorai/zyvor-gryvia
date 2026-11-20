// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package timemachine

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

// ForkHandler creates new GryviaAIJob resources from checkpoint state
type ForkHandler struct {
	Client client.Client
}

// NewForkHandler creates a new ForkHandler
func NewForkHandler(c client.Client) *ForkHandler {
	return &ForkHandler{Client: c}
}

// CreateForkedJob creates a new GryviaAIJob from a source job and a fork specification.
// It clones the source job spec, applies overrides from the fork, and sets environment
// variables to resume from the specified checkpoint step.
func (f *ForkHandler) CreateForkedJob(ctx context.Context, sourceJob *gryviav1.GryviaAIJob, fork gryviav1.ForkSpec, checkpointStep int) (string, error) {
	// Determine the new job name
	newJobName := fork.NewJobName
	if newJobName == "" {
		newJobName = fmt.Sprintf("%s-fork-%s", sourceJob.Name, fork.Name)
	}

	// Build the forked job spec by cloning the source
	forkedSpec := buildForkedSpec(sourceJob.Spec, fork, checkpointStep)

	// Create the forked GryviaAIJob
	forkedJob := &gryviav1.GryviaAIJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      newJobName,
			Namespace: sourceJob.Namespace,
			Labels:    buildForkLabels(sourceJob, fork),
			Annotations: map[string]string{
				"gryvia.io/forked-from": sourceJob.Name,
				"gryvia.io/fork-name":   fork.Name,
				"gryvia.io/fork-step":   fmt.Sprintf("%d", checkpointStep),
			},
		},
		Spec: forkedSpec,
	}

	if err := f.Client.Create(ctx, forkedJob); err != nil {
		return "", fmt.Errorf("failed to create forked job %s: %w", newJobName, err)
	}

	return newJobName, nil
}

// buildForkedSpec creates a new GryviaAIJobSpec by cloning the source and applying overrides
func buildForkedSpec(sourceSpec gryviav1.GryviaAIJobSpec, fork gryviav1.ForkSpec, checkpointStep int) gryviav1.GryviaAIJobSpec {
	// Deep clone the source spec
	spec := cloneJobSpec(sourceSpec)

	// Add checkpoint resume environment variables
	resumeEnvVars := []corev1.EnvVar{
		{
			Name:  "GRYVIA_RESUME_FROM_CHECKPOINT",
			Value: "true",
		},
		{
			Name:  "GRYVIA_CHECKPOINT_STEP",
			Value: fmt.Sprintf("%d", checkpointStep),
		},
		{
			Name:  "GRYVIA_FORK_NAME",
			Value: fork.Name,
		},
	}
	spec.Env = append(spec.Env, resumeEnvVars...)

	// Apply overrides if specified
	if fork.Overrides != nil {
		// Apply environment variable overrides
		for _, envOverride := range fork.Overrides.Env {
			spec.Env = applyEnvOverride(spec.Env, envOverride)
		}

		// Apply argument overrides
		if len(fork.Overrides.Args) > 0 {
			spec.Args = append(spec.Args, fork.Overrides.Args...)
		}
	}

	return spec
}

// cloneJobSpec creates a deep copy of a GryviaAIJobSpec
func cloneJobSpec(src gryviav1.GryviaAIJobSpec) gryviav1.GryviaAIJobSpec {
	spec := gryviav1.GryviaAIJobSpec{
		Type:            src.Type,
		Model:           src.Model,
		GPUs:            src.GPUs,
		GpuType:         src.GpuType,
		Storage:         src.Storage,
		StorageRequest:  src.StorageRequest,
		Network:         src.Network,
		Image:           src.Image,
		ImagePullPolicy: src.ImagePullPolicy,
		WorkingDir:      src.WorkingDir,
		Priority:        src.Priority,
		RetryLimit:      src.RetryLimit,
		Timeout:         src.Timeout,
	}

	// Clone slices
	if src.Command != nil {
		spec.Command = make([]string, len(src.Command))
		copy(spec.Command, src.Command)
	}
	if src.Args != nil {
		spec.Args = make([]string, len(src.Args))
		copy(spec.Args, src.Args)
	}
	if src.Env != nil {
		spec.Env = make([]corev1.EnvVar, len(src.Env))
		copy(spec.Env, src.Env)
	}

	// Clone distributed config
	if src.Distributed != nil {
		spec.Distributed = &gryviav1.DistributedConfig{
			Enabled:     src.Distributed.Enabled,
			Framework:   src.Distributed.Framework,
			Nodes:       src.Distributed.Nodes,
			GpusPerNode: src.Distributed.GpusPerNode,
			Backend:     src.Distributed.Backend,
		}
	}

	// Clone resource requirements
	src.Resources.DeepCopyInto(&spec.Resources)

	// Clone volumes and volume mounts
	if src.Volumes != nil {
		spec.Volumes = make([]corev1.Volume, len(src.Volumes))
		for i := range src.Volumes {
			src.Volumes[i].DeepCopyInto(&spec.Volumes[i])
		}
	}
	if src.VolumeMounts != nil {
		spec.VolumeMounts = make([]corev1.VolumeMount, len(src.VolumeMounts))
		for i := range src.VolumeMounts {
			src.VolumeMounts[i].DeepCopyInto(&spec.VolumeMounts[i])
		}
	}

	// Clone node selector
	if src.NodeSelector != nil {
		spec.NodeSelector = make(map[string]string, len(src.NodeSelector))
		for k, v := range src.NodeSelector {
			spec.NodeSelector[k] = v
		}
	}

	// Clone tolerations
	if src.Tolerations != nil {
		spec.Tolerations = make([]corev1.Toleration, len(src.Tolerations))
		for i := range src.Tolerations {
			src.Tolerations[i].DeepCopyInto(&spec.Tolerations[i])
		}
	}

	// Clone affinity
	if src.Affinity != nil {
		spec.Affinity = new(corev1.Affinity)
		src.Affinity.DeepCopyInto(spec.Affinity)
	}

	return spec
}

// applyEnvOverride applies an environment variable override. If the variable
// already exists, its value is updated. Otherwise, the variable is appended.
func applyEnvOverride(envVars []corev1.EnvVar, override gryviav1.EnvOverride) []corev1.EnvVar {
	for i, env := range envVars {
		if env.Name == override.Name {
			envVars[i].Value = override.Value
			return envVars
		}
	}
	// Append new env var
	return append(envVars, corev1.EnvVar{
		Name:  override.Name,
		Value: override.Value,
	})
}

// buildForkLabels creates labels for a forked job
func buildForkLabels(sourceJob *gryviav1.GryviaAIJob, fork gryviav1.ForkSpec) map[string]string {
	labels := map[string]string{
		"gryvia.io/forked-from": sourceJob.Name,
		"gryvia.io/fork":        fork.Name,
		"gryvia.io/type":        sourceJob.Spec.Type,
	}

	// Copy relevant labels from the source job
	if sourceJob.Labels != nil {
		if model, ok := sourceJob.Labels["gryvia.io/model"]; ok {
			labels["gryvia.io/model"] = model
		}
		if team, ok := sourceJob.Labels["gryvia.io/team"]; ok {
			labels["gryvia.io/team"] = team
		}
	}

	return labels
}

// ValidateForkSpec validates a fork specification
func ValidateForkSpec(fork gryviav1.ForkSpec) error {
	if fork.Name == "" {
		return fmt.Errorf("fork name is required")
	}

	// Validate checkpoint selector has at least one criterion
	selector := fork.FromCheckpoint
	hasStep := selector.Step != nil
	hasEpoch := selector.Epoch != nil
	hasMetric := selector.Metric != nil

	if !hasStep && !hasEpoch && !hasMetric {
		return fmt.Errorf("fork %q must specify at least one checkpoint selector (step, epoch, or metric)", fork.Name)
	}

	// Validate metric selector
	if hasMetric {
		if selector.Metric.Name == "" {
			return fmt.Errorf("fork %q metric selector must specify a metric name", fork.Name)
		}
		if selector.Metric.Selector != "min" && selector.Metric.Selector != "max" {
			return fmt.Errorf("fork %q metric selector must be 'min' or 'max', got %q", fork.Name, selector.Metric.Selector)
		}
	}

	return nil
}
