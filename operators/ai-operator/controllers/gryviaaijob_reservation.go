// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controllers

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/ai-operator/pkg/reservation"
)

// ConditionReservation reports what happened to a job's gryvia.io/reservation request.
const ConditionReservation = "Reservation"

// applyReservation is called from buildPodTemplate. A job that names a reservation
// (annotation or label gryvia.io/reservation) whose owner is the job's tenant, namespace or
// team gets a toleration for the reservation's node taint and a nodeSelector on the
// reservation's node label, so its pods can land on (and only on) the reserved nodes. A job
// that names a reservation it does not own, one that does not exist or one that has ended is
// left alone (the annotation is ignored) and the job carries a Reservation=False condition
// saying why. A job WITHOUT the annotation gets nothing: the reserved nodes are tainted, so it
// simply cannot land there, which is the point of a reservation.
//
// The lookup reads the (cached) client; a read error leaves the pod spec unchanged. The
// condition is set in memory and persisted by the reconcile's next status update.
func (r *GryviaAIJobReconciler) applyReservation(job *gryviav1.GryviaAIJob, spec *corev1.PodSpec) {
	name := reservation.Named(job.Annotations, job.Labels)
	if name == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	info, found, err := reservation.Get(ctx, r.Client, name)
	switch {
	case err != nil:
		r.reservationCondition(job, metav1.ConditionFalse, "LookupFailed", fmt.Sprintf("reservation %q could not be read: %v", name, err))
		return
	case !found:
		r.reservationCondition(job, metav1.ConditionFalse, "NotFound", fmt.Sprintf("reservation %q does not exist; the annotation is ignored", name))
		return
	case !info.Usable():
		r.reservationCondition(job, metav1.ConditionFalse, "Ended", fmt.Sprintf("reservation %q is %s; the annotation is ignored", name, info.State))
		return
	}
	id := reservation.Identity{Namespace: job.Namespace, Team: reservation.TeamOf(ctx, r.Client, job.Namespace, job.Labels)}
	if !info.MayUse(id) {
		r.reservationCondition(job, metav1.ConditionFalse, "OwnerMismatch",
			fmt.Sprintf("reservation %q belongs to %s %q, not to this job's namespace/tenant/team; the annotation is ignored", name, info.OwnerType, info.OwnerName))
		return
	}
	reservation.Apply(spec, info)
	r.reservationCondition(job, metav1.ConditionTrue, "Reserved", fmt.Sprintf("pods are pinned to the nodes of reservation %q", name))
}

func (r *GryviaAIJobReconciler) reservationCondition(job *gryviav1.GryviaAIJob, status metav1.ConditionStatus, reason, msg string) {
	if c := meta.FindStatusCondition(job.Status.Conditions, ConditionReservation); c != nil && c.Status == status && c.Reason == reason && c.Message == msg {
		return
	}
	meta.SetStatusCondition(&job.Status.Conditions, metav1.Condition{
		Type: ConditionReservation, Status: status, Reason: reason, Message: msg, LastTransitionTime: metav1.Now(),
	})
	if status == metav1.ConditionFalse && r.Recorder != nil {
		r.Recorder.Event(job, corev1.EventTypeWarning, "Reservation"+reason, msg)
	}
}
