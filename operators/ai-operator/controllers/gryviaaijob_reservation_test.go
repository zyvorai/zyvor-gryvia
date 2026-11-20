// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controllers

import (
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

func reservationObj(name, ownerType, ownerName, state string) *unstructured.Unstructured {
	u := uobj("GryviaReservation", name, map[string]interface{}{"owner": map[string]interface{}{"type": ownerType, "name": ownerName}})
	if state != "" {
		u.Object["status"] = map[string]interface{}{"state": state}
	}
	return u
}

func reservedJob(name, reservationName string) *gryviav1.GryviaAIJob {
	j := newTestAIJob(name, "tenant-acme")
	if reservationName != "" {
		j.Annotations = map[string]string{"gryvia.io/reservation": reservationName}
	}
	return j
}

func podSpecFor(t *testing.T, r *GryviaAIJobReconciler, j *gryviav1.GryviaAIJob) corev1.PodSpec {
	t.Helper()
	job, err := r.buildJob(j)
	if err != nil {
		t.Fatal(err)
	}
	return job.Spec.Template.Spec
}

func hasTol(spec corev1.PodSpec, value string) bool {
	for _, t := range spec.Tolerations {
		if t.Key == "gryvia.io/reserved" && t.Value == value && t.Effect == corev1.TaintEffectNoSchedule && t.Operator == corev1.TolerationOpEqual {
			return true
		}
	}
	return false
}

func TestReservation_OwnerJobGetsTolerationAndNodeSelector(t *testing.T) {
	j := reservedJob("train", "res-a")
	j.Spec.Tolerations = []corev1.Toleration{{Key: "nvidia.com/gpu", Operator: corev1.TolerationOpExists}}
	r, _, _ := gateReconciler(false, nil, reservationObj("res-a", "tenant", "acme", "active"))
	spec := podSpecFor(t, r, j)
	if !hasTol(spec, "acme") || spec.NodeSelector["gryvia.io/reserved-for"] != "res-a" {
		t.Errorf("spec = %+v / %v", spec.Tolerations, spec.NodeSelector)
	}
	if len(spec.Tolerations) != 2 || spec.Tolerations[0].Key != "nvidia.com/gpu" {
		t.Errorf("the job's own toleration must be kept: %+v", spec.Tolerations)
	}
	if len(j.Spec.Tolerations) != 1 {
		t.Error("the job spec must not be mutated")
	}
	if c := meta.FindStatusCondition(j.Status.Conditions, ConditionReservation); c == nil || c.Status != "True" {
		t.Errorf("condition = %+v", c)
	}
}

func TestReservation_JobWithoutAnnotationGetsNothing(t *testing.T) {
	r, _, _ := gateReconciler(false, nil, reservationObj("res-a", "tenant", "acme", "active"))
	spec := podSpecFor(t, r, reservedJob("plain", ""))
	if hasTol(spec, "acme") || spec.NodeSelector["gryvia.io/reserved-for"] != "" {
		t.Errorf("no annotation, no reservation placement: %+v %v", spec.Tolerations, spec.NodeSelector)
	}
	for _, tol := range spec.Tolerations {
		if tol.Key == "gryvia.io/reserved" || tol.Operator == corev1.TolerationOpExists && tol.Key == "" {
			t.Errorf("unexpected toleration %+v", tol)
		}
	}
}

func TestReservation_LabelAlsoWorks(t *testing.T) {
	j := reservedJob("train", "")
	j.Labels = map[string]string{"gryvia.io/reservation": "res-a"}
	r, _, _ := gateReconciler(false, nil, reservationObj("res-a", "namespace", "tenant-acme", "pending"))
	if spec := podSpecFor(t, r, j); spec.NodeSelector["gryvia.io/reserved-for"] != "res-a" {
		t.Errorf("selector = %v", spec.NodeSelector)
	}
}

func TestReservation_OwnerMismatchIsIgnoredWithCondition(t *testing.T) {
	j := reservedJob("thief", "res-b")
	r, _, rec := gateReconciler(false, nil, reservationObj("res-b", "tenant", "someone-else", "active"))
	spec := podSpecFor(t, r, j)
	if hasTol(spec, "someone-else") || spec.NodeSelector["gryvia.io/reserved-for"] != "" {
		t.Errorf("a job must not use another owner's reservation: %+v %v", spec.Tolerations, spec.NodeSelector)
	}
	c := meta.FindStatusCondition(j.Status.Conditions, ConditionReservation)
	if c == nil || c.Status != "False" || c.Reason != "OwnerMismatch" {
		t.Errorf("condition = %+v", c)
	}
	if e := <-rec.Events; !strings.Contains(e, "ReservationOwnerMismatch") {
		t.Errorf("event = %s", e)
	}
}

func TestReservation_MissingOrEndedIsIgnored(t *testing.T) {
	for name, tc := range map[string]struct {
		obj    *unstructured.Unstructured
		reason string
	}{
		"missing": {nil, "NotFound"},
		"expired": {reservationObj("res-a", "tenant", "acme", "expired"), "Ended"},
	} {
		j := reservedJob("j-"+name, "res-a")
		var r *GryviaAIJobReconciler
		if tc.obj != nil {
			r, _, _ = gateReconciler(false, nil, tc.obj)
		} else {
			r, _, _ = gateReconciler(false, nil)
		}
		spec := podSpecFor(t, r, j)
		if spec.NodeSelector["gryvia.io/reserved-for"] != "" {
			t.Errorf("%s: selector applied", name)
		}
		if c := meta.FindStatusCondition(j.Status.Conditions, ConditionReservation); c == nil || c.Reason != tc.reason {
			t.Errorf("%s: condition = %+v", name, c)
		}
	}
}

func TestReservation_EndToEndReconcileCreatesPinnedJob(t *testing.T) {
	j := reservedJob("pinned", "res-a")
	j.Namespace = ns
	r, c, _ := gateReconciler(false, nil, j, gpuNode("n1"), reservationObj("res-a", "namespace", ns, "active"))
	reconcileN(t, r, "pinned", 4)
	bj := &batchv1.Job{}
	if !exists(t, c, bj, "pinned") {
		t.Fatal("no Job created")
	}
	if bj.Spec.Template.Spec.NodeSelector["gryvia.io/reserved-for"] != "res-a" || !hasTol(bj.Spec.Template.Spec, ns) {
		t.Errorf("pod template = %+v", bj.Spec.Template.Spec)
	}
}
