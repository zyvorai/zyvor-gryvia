// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controllers

import (
	"context"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gryviav1 "github.com/zyvorai/gryvia/operators/quota-operator/api/v1"
)

// patchJobPhase sets a GryviaAIJob's status.phase (and, when given, one condition and
// status.message) with a JSON merge patch on the status subresource.
//
// The quota operator holds only a narrow copy of the job status type. Writing that copy back
// with Status().Update would replace the whole status and silently drop every field the AI
// operator owns (gpusAllocated, replicasReady, message, placementExplanation, metrics, ...).
// A merge patch computed from a snapshot sends only the fields that changed here, so the
// others are never touched. It is deliberately not optimistic-locked: if the AI operator moved
// the job on in the meantime, the AI operator sees the new phase (Rejected, Queued) and acts
// on it (see docs/aijob-lifecycle.md).
func patchJobPhase(ctx context.Context, c client.Client, job *gryviav1.GryviaAIJob, phase, message string, cond *metav1.Condition) error {
	base := job.DeepCopy()
	job.Status.Phase = phase
	if message != "" {
		job.Status.Message = message
	}
	if cond != nil {
		meta.SetStatusCondition(&job.Status.Conditions, *cond)
	}
	return c.Status().Patch(ctx, job, client.MergeFrom(base))
}
