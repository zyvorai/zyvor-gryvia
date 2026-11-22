// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"strings"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	AnnotationCheckpointCommand   = "gryvia.io/checkpoint-command"
	AnnotationCheckpointDirectory = "gryvia.io/checkpoint-directory"
	AnnotationCheckpointGrace     = "gryvia.io/checkpoint-grace-seconds"
	AnnotationSpreadWorkers       = "gryvia.io/spread-workers"
)

type recoveryOptions struct {
	command   []string
	directory string
	grace     int64
	spread    bool
}

// Training code owns the checkpoint format and must atomically save/load it.
// The command is an argv array; a preStop hook cannot checkpoint a failed node.
func parseRecoveryOptions(job *gryviav1.GryviaAIJob) (recoveryOptions, error) {
	o := recoveryOptions{grace: 120}
	if _, _, err := topologyAnnotation(job); err != nil {
		return o, err
	}
	a := job.Annotations
	if v := a[AnnotationSpreadWorkers]; v != "" {
		if v != "true" && v != "false" {
			return o, fmt.Errorf("%s must be true or false", AnnotationSpreadWorkers)
		}
		o.spread = v == "true"
	}
	raw := a[AnnotationCheckpointCommand]
	if raw == "" {
		if a[AnnotationCheckpointDirectory] != "" || a[AnnotationCheckpointGrace] != "" {
			return o, fmt.Errorf("checkpoint directory/grace requires %s", AnnotationCheckpointCommand)
		}
		return o, nil
	}
	if len(raw) > 4096 {
		return o, fmt.Errorf("checkpoint command exceeds 4096 bytes")
	}
	if err := json.Unmarshal([]byte(raw), &o.command); err != nil || len(o.command) == 0 || len(o.command) > 32 {
		return o, fmt.Errorf("checkpoint command must be a JSON array of 1-32 arguments")
	}
	for i, arg := range o.command {
		if strings.ContainsRune(arg, 0) || (i == 0 && strings.TrimSpace(arg) == "") {
			return o, fmt.Errorf("invalid checkpoint command argument")
		}
	}
	if job.Spec.Storage == "" {
		return o, fmt.Errorf("checkpoint hooks require spec.storage for a persistent /data volume")
	}
	if job.Spec.Type == "inference" || job.Spec.WorkloadKind == gryviav1.WorkloadKindStatefulSet {
		return o, fmt.Errorf("checkpoint hooks support batch training jobs only")
	}
	o.directory = a[AnnotationCheckpointDirectory]
	if o.directory == "" {
		o.directory = "/data/checkpoints"
	}
	if !strings.HasPrefix(o.directory, "/data/") || path.Clean(o.directory) != o.directory || strings.ContainsRune(o.directory, 0) {
		return o, fmt.Errorf("checkpoint directory must be a clean absolute path below /data")
	}
	if v := a[AnnotationCheckpointGrace]; v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 30 || n > 3600 {
			return o, fmt.Errorf("checkpoint grace must be 30-3600 seconds")
		}
		o.grace = n
	}
	return o, nil
}

func validateRecoveryOptions(job *gryviav1.GryviaAIJob) error {
	_, err := parseRecoveryOptions(job)
	return err
}

func applyRecoveryOptions(job *gryviav1.GryviaAIJob, spec *corev1.PodSpec) {
	o, err := parseRecoveryOptions(job)
	if err != nil {
		return
	} // reconcile/buildJob reject malformed configuration before creation
	if len(o.command) > 0 {
		spec.TerminationGracePeriodSeconds = &o.grace
		c := &spec.Containers[0]
		c.Lifecycle = &corev1.Lifecycle{PreStop: &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: o.command}}}
		// Reserved values override user env, so saving and restoring use the same directory.
		env := c.Env[:0]
		for _, e := range c.Env {
			if e.Name != "GRYVIA_CHECKPOINT_DIR" && e.Name != "GRYVIA_RESUME_IF_PRESENT" {
				env = append(env, e)
			}
		}
		c.Env = append(env, corev1.EnvVar{Name: "GRYVIA_CHECKPOINT_DIR", Value: o.directory}, corev1.EnvVar{Name: "GRYVIA_RESUME_IF_PRESENT", Value: "true"})
	}
	if o.spread && job.Spec.Distributed != nil && job.Spec.Distributed.Enabled && job.Spec.Distributed.Nodes > 1 {
		aff := spec.Affinity.DeepCopy()
		if aff == nil {
			aff = &corev1.Affinity{}
		}
		if aff.PodAntiAffinity == nil {
			aff.PodAntiAffinity = &corev1.PodAntiAffinity{}
		}
		aff.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution = append(aff.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution,
			corev1.PodAffinityTerm{LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"gryvia.io/job": job.Name}}, Namespaces: []string{job.Namespace}, TopologyKey: "kubernetes.io/hostname"})
		spec.Affinity = aff
	}
}

// Kueue owns topology placement; annotations refer to administrator-defined topology labels.
func topologyAnnotation(job *gryviav1.GryviaAIJob) (string, string, error) {
	required, preferred := job.Annotations["gryvia.io/required-topology"], job.Annotations["gryvia.io/preferred-topology"]
	if required != "" && preferred != "" {
		return "", "", fmt.Errorf("required and preferred topology are mutually exclusive")
	}
	key, val := "kueue.x-k8s.io/podset-required-topology", required
	if preferred != "" {
		key, val = "kueue.x-k8s.io/podset-preferred-topology", preferred
	}
	if val != "" && len(validation.IsQualifiedName(val)) != 0 {
		return "", "", fmt.Errorf("invalid topology label")
	}
	return key, val, nil
}
