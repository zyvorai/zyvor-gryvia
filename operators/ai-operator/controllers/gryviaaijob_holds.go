// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"time"

	"k8s.io/apimachinery/pkg/types"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/ai-operator/pkg/scheduler"
)

// placementHoldTTL is the longest a placement holds GPUs: long enough for pods to be bound and
// counted, short enough that a missed release cannot starve other jobs for long.
const placementHoldTTL = 5 * time.Minute

func (r *GryviaAIJobReconciler) holds() *scheduler.GPUHolds {
	r.gpuHoldsOnce.Do(func() { r.gpuHolds = scheduler.NewGPUHolds() })
	return r.gpuHolds
}

func placementKey(job *gryviav1.GryviaAIJob) string { return job.Namespace + "/" + job.Name }

// placementHeld returns the GPUs other jobs hold on each node (nil when holds are off).
func (r *GryviaAIJobReconciler) placementHeld(job *gryviav1.GryviaAIJob) map[string]int64 {
	if !r.PlacementHolds {
		return nil
	}
	return r.holds().Held(placementKey(job), time.Now())
}

// holdPlacement holds the GPUs of the nodes a job was just placed on.
func (r *GryviaAIJobReconciler) holdPlacement(job *gryviav1.GryviaAIJob, nodes []string) {
	if !r.PlacementHolds {
		return
	}
	r.holds().Hold(placementKey(job), nodes, int64(scheduler.GPUsPerPlacedNode(job)), placementHoldTTL, time.Now())
}

// releasePlacement drops a job's hold by name (the job object may already be gone).
func (r *GryviaAIJobReconciler) releasePlacement(nn types.NamespacedName) {
	if !r.PlacementHolds {
		return
	}
	r.holds().Release(nn.Namespace + "/" + nn.Name)
}

// releaseIfSettled drops the hold once the job's own pods are up (they now count as used GPUs) or the
// job reached an end state, whichever comes first.
func (r *GryviaAIJobReconciler) releaseIfSettled(job *gryviav1.GryviaAIJob) {
	if !r.PlacementHolds {
		return
	}
	switch job.Status.Phase {
	case PhaseSucceeded, PhaseFailed, PhaseCancelled, PhasePreempted, PhaseRejected:
		r.holds().Release(placementKey(job))
		return
	}
	if want := r.getReplicaCount(job); want > 0 && job.Status.ReplicasReady >= want {
		r.holds().Release(placementKey(job))
	}
}
