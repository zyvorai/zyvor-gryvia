// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
	"testing"
)

func TestTelemetryUIDRevisionAndRolloutStability(t *testing.T) {
	svc := analyzedCanary()
	svc.Annotations[AnnotationInferenceTelemetry] = "true"
	c := mlClient(svc)
	r := newInferReconciler(c, newClock())
	r.TelemetryImage = "example/operator:test"
	reconcileOnce(t, r, "ns", "chat")
	d := &appsv1.Deployment{}
	mustGet(t, c, "ns", "chat-canary", d)
	d.UID = types.UID("incarnation-one")
	if err := c.Update(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "ns", "chat")
	mustGet(t, c, "ns", "chat-canary", d)
	if len(d.Spec.Template.Spec.Containers) != 2 || d.Spec.Template.Spec.Containers[1].Ports[0].Name != "http" {
		t.Fatal("service does not route through telemetry")
	}
	env := map[string]string{}
	for _, e := range d.Spec.Template.Spec.Containers[1].Env {
		env[e.Name] = e.Value
	}
	if env["GRYVIA_REVISION"] != d.Annotations[annotationSpecHash] || env["GRYVIA_DEPLOYMENT_UID"] != "incarnation-one" || env["GRYVIA_MODEL_VERSION"] != "v2" {
		t.Fatalf("identity mismatch %v", env)
	}
	hash := d.Annotations[annotationSpecHash]
	reconcileOnce(t, r, "ns", "chat")
	mustGet(t, c, "ns", "chat-canary", d)
	if hash != d.Annotations[annotationSpecHash] {
		t.Fatal("perpetual rollout")
	}
}
func TestTelemetryRequiresAdministratorImage(t *testing.T) {
	svc := newInfer("chat", func(s *gryviav1.GryviaInferenceService) {
		s.Annotations = map[string]string{AnnotationInferenceTelemetry: "true"}
	})
	c := mlClient(svc)
	r := newInferReconciler(c, newClock())
	reconcileOnce(t, r, "ns", "chat")
	if getInfer(t, c, "chat").Status.Phase != PhaseFailed {
		t.Fatal("silently bypassed telemetry")
	}
}
