// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

func newModelReconciler(c client.Client, gpus int32) *GryviaModelRegistryReconciler {
	return &GryviaModelRegistryReconciler{Client: c, Scheme: mlScheme(), Log: ctrl.Log.WithName("test"), AutoServeGPUCount: gpus}
}

func newModel(name string, mutate ...func(*gryviav1.GryviaModelRegistry)) *gryviav1.GryviaModelRegistry {
	m := &gryviav1.GryviaModelRegistry{
		ObjectMeta: objMeta("ns", name),
		Spec: gryviav1.GryviaModelRegistrySpec{
			ModelName: "llama", Version: "1.0", Stage: gryviav1.ModelStageDev,
			Artifacts: gryviav1.ModelArtifacts{S3Path: "s3://bucket/llama"},
		},
	}
	for _, f := range mutate {
		f(m)
	}
	return m
}

func getModel(t *testing.T, c client.Client, name string) *gryviav1.GryviaModelRegistry {
	t.Helper()
	m := &gryviav1.GryviaModelRegistry{}
	mustGet(t, c, "ns", name, m)
	return m
}

func TestModel_RegisteredWithoutServing(t *testing.T) {
	c := mlClient(newModel("llama-1"))
	r := newModelReconciler(c, 1)
	reconcileOnce(t, r, "ns", "llama-1")
	m := getModel(t, c, "llama-1")
	if m.Status.Phase != PhaseRegistered || m.Status.RegisteredAt == nil {
		t.Errorf("status: %+v", m.Status)
	}
	list := &gryviav1.GryviaInferenceServiceList{}
	if err := c.List(context.Background(), list); err != nil || len(list.Items) != 0 {
		t.Errorf("a dev model must not be served: %d services, err %v", len(list.Items), err)
	}
	// autoServe outside production does nothing either.
	m.Spec.AutoServe = true
	if err := c.Update(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "ns", "llama-1")
	if err := c.List(context.Background(), list); err != nil || len(list.Items) != 0 {
		t.Errorf("autoServe below production created a service: %d", len(list.Items))
	}
}

func TestModel_AutoServeCreatesService(t *testing.T) {
	c := mlClient(newModel("llama-1", func(m *gryviav1.GryviaModelRegistry) {
		m.Spec.Stage = gryviav1.ModelStageProduction
		m.Spec.AutoServe = true
		m.Spec.ServingConfig = &gryviav1.ServingConfig{Backend: "triton", Replicas: 3}
	}))
	r := newModelReconciler(c, 0) // CPU serving
	reconcileOnce(t, r, "ns", "llama-1")

	svc := &gryviav1.GryviaInferenceService{}
	mustGet(t, c, "ns", "llama-1-serving", svc)
	if ref := metav1.GetControllerOf(svc); ref == nil || ref.Kind != "GryviaModelRegistry" || ref.Name != "llama-1" {
		t.Errorf("owner: %v", svc.OwnerReferences)
	}
	// modelRef is the registry object name: the gateway and the inference controller look the model up by it.
	if svc.Spec.ModelRef != "llama-1" || svc.Spec.Backend != gryviav1.BackendTriton || svc.Spec.Replicas != 3 || svc.Spec.GPUCount != 0 {
		t.Errorf("spec: %+v", svc.Spec)
	}
	m := getModel(t, c, "llama-1")
	if m.Status.Phase != PhaseDeploying || m.Status.InferenceServiceName != "llama-1-serving" {
		t.Errorf("status: %+v", m.Status)
	}
	if m.Status.ServingEndpoint != "" {
		t.Error("no endpoint before the service is ready")
	}

	// Idempotent.
	reconcileOnce(t, r, "ns", "llama-1")
	list := &gryviav1.GryviaInferenceServiceList{}
	if err := c.List(context.Background(), list); err != nil || len(list.Items) != 1 {
		t.Fatalf("services = %d, err %v", len(list.Items), err)
	}

	// The inference controller reports Ready with an endpoint: the model serves.
	svc.Status.Phase = PhaseReady
	svc.Status.Endpoint = "http://llama-1-serving-inference.ns.svc.cluster.local:8000"
	if err := c.Status().Update(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "ns", "llama-1")
	m = getModel(t, c, "llama-1")
	st := statusJSON(t, m)
	wantKeys(t, st, "servingEndpoint", "phase", "deployedAt")
	if st["servingEndpoint"] != svc.Status.Endpoint || m.Status.Phase != PhaseServing || m.Status.Health != "Healthy" {
		t.Errorf("status: %v", st)
	}
}

func TestModel_ServingDefaultsFromFlag(t *testing.T) {
	c := mlClient(newModel("m", func(m *gryviav1.GryviaModelRegistry) {
		m.Spec.Stage = gryviav1.ModelStageProduction
		m.Spec.AutoServe = true
	}))
	r := newModelReconciler(c, 1)
	reconcileOnce(t, r, "ns", "m")
	svc := &gryviav1.GryviaInferenceService{}
	mustGet(t, c, "ns", "m-serving", svc)
	if svc.Spec.GPUCount != 1 || svc.Spec.Backend != gryviav1.BackendVLLM || svc.Spec.Replicas != 1 {
		t.Errorf("defaults: %+v", svc.Spec)
	}
}

func TestModel_ServingConfigChangeIsApplied(t *testing.T) {
	c := mlClient(newModel("m", func(m *gryviav1.GryviaModelRegistry) {
		m.Spec.Stage = gryviav1.ModelStageProduction
		m.Spec.AutoServe = true
	}))
	r := newModelReconciler(c, 0)
	reconcileOnce(t, r, "ns", "m")
	m := getModel(t, c, "m")
	m.Spec.ServingConfig = &gryviav1.ServingConfig{Replicas: 4, GPUCount: 2}
	if err := c.Update(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "ns", "m")
	svc := &gryviav1.GryviaInferenceService{}
	mustGet(t, c, "ns", "m-serving", svc)
	if svc.Spec.Replicas != 4 || svc.Spec.GPUCount != 2 {
		t.Errorf("spec: %+v", svc.Spec)
	}
}

// A server moved off its backend default port (vLLM --port=8080) must be probed and served on that port.
func TestModel_ServicePortIsPassedThrough(t *testing.T) {
	c := mlClient(newModel("m", func(m *gryviav1.GryviaModelRegistry) {
		m.Spec.Stage = gryviav1.ModelStageProduction
		m.Spec.AutoServe = true
		m.Spec.ServingConfig = &gryviav1.ServingConfig{Args: []string{"--port=8080"}, ServicePort: 8080}
	}))
	r := newModelReconciler(c, 0)
	reconcileOnce(t, r, "ns", "m")
	svc := &gryviav1.GryviaInferenceService{}
	mustGet(t, c, "ns", "m-serving", svc)
	if svc.Spec.ServicePort != 8080 {
		t.Fatalf("servicePort = %d, want 8080", svc.Spec.ServicePort)
	}
	m := getModel(t, c, "m")
	m.Spec.ServingConfig.ServicePort = 9000
	if err := c.Update(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "ns", "m")
	mustGet(t, c, "ns", "m-serving", svc)
	if svc.Spec.ServicePort != 9000 {
		t.Errorf("servicePort change not applied: %d", svc.Spec.ServicePort)
	}

	// Shared service: the first version creates it with the port.
	c = mlClient(newModel("v1", func(m *gryviav1.GryviaModelRegistry) {
		m.Spec.Stage = gryviav1.ModelStageProduction
		m.Spec.AutoServe = true
		m.Spec.ServingConfig = &gryviav1.ServingConfig{ServiceName: "chat", ServicePort: 8080}
	}))
	reconcileOnce(t, newModelReconciler(c, 0), "ns", "v1")
	mustGet(t, c, "ns", "chat", svc)
	if svc.Spec.ServicePort != 8080 {
		t.Errorf("shared servicePort = %d, want 8080", svc.Spec.ServicePort)
	}
}

func TestModel_ArchivingStopsServing(t *testing.T) {
	c := mlClient(newModel("m", func(m *gryviav1.GryviaModelRegistry) {
		m.Spec.Stage = gryviav1.ModelStageProduction
		m.Spec.AutoServe = true
	}))
	r := newModelReconciler(c, 0)
	reconcileOnce(t, r, "ns", "m")
	svc := &gryviav1.GryviaInferenceService{}
	mustGet(t, c, "ns", "m-serving", svc)
	svc.Status.Phase, svc.Status.Endpoint = PhaseReady, "http://x"
	if err := c.Status().Update(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "ns", "m")

	m := getModel(t, c, "m")
	m.Spec.Stage = gryviav1.ModelStageArchived
	if err := c.Update(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "ns", "m")
	if mlObjectExists(c, "ns", "m-serving", &gryviav1.GryviaInferenceService{}) {
		t.Error("archiving must remove the serving service")
	}
	m = getModel(t, c, "m")
	if m.Status.ServingEndpoint != "" || m.Status.InferenceServiceName != "" || m.Status.Phase != PhaseRegistered {
		t.Errorf("status: %+v", m.Status)
	}
}

func TestModel_ServiceFailureIsReported(t *testing.T) {
	c := mlClient(newModel("m", func(m *gryviav1.GryviaModelRegistry) {
		m.Spec.Stage = gryviav1.ModelStageProduction
		m.Spec.AutoServe = true
	}))
	r := newModelReconciler(c, 0)
	reconcileOnce(t, r, "ns", "m")
	svc := &gryviav1.GryviaInferenceService{}
	mustGet(t, c, "ns", "m-serving", svc)
	svc.Status.Phase, svc.Status.Message = PhaseFailed, "boom"
	if err := c.Status().Update(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "ns", "m")
	m := getModel(t, c, "m")
	if m.Status.Phase != PhaseFailed || m.Status.Message != "boom" || m.Status.Health != "Unhealthy" {
		t.Errorf("status: %+v", m.Status)
	}
}

func TestModel_ServingNameCollision(t *testing.T) {
	foreign := &gryviav1.GryviaInferenceService{ObjectMeta: objMeta("ns", "m-serving"), Spec: gryviav1.GryviaInferenceServiceSpec{ModelRef: "x", Backend: "vllm"}}
	c := mlClient(newModel("m", func(m *gryviav1.GryviaModelRegistry) {
		m.Spec.Stage = gryviav1.ModelStageProduction
		m.Spec.AutoServe = true
	}), foreign)
	r := newModelReconciler(c, 0)
	reconcileOnce(t, r, "ns", "m")
	m := getModel(t, c, "m")
	if m.Status.Phase != PhaseFailed || !strings.Contains(m.Status.Message, "not owned") {
		t.Errorf("status: %+v", m.Status)
	}
}

func TestModel_PreviousVersionTracked(t *testing.T) {
	old := newModel("llama-0", func(m *gryviav1.GryviaModelRegistry) {
		m.Spec.Version = "0.9"
		m.Spec.Stage = gryviav1.ModelStageProduction
	})
	cur := newModel("llama-1", func(m *gryviav1.GryviaModelRegistry) { m.Spec.Stage = gryviav1.ModelStageProduction })
	other := newModel("other", func(m *gryviav1.GryviaModelRegistry) {
		m.Spec.ModelName = "other"
		m.Spec.Stage = gryviav1.ModelStageProduction
	})
	c := mlClient(old, cur, other)
	r := newModelReconciler(c, 0)
	reconcileOnce(t, r, "ns", "llama-1")
	if pv := getModel(t, c, "llama-1").Status.PreviousVersion; pv != "0.9" {
		t.Errorf("previousVersion = %q", pv)
	}
}

func TestModel_NotFoundAndIdempotentRegistered(t *testing.T) {
	c := mlClient(newModel("m"))
	r := newModelReconciler(c, 0)
	reconcileOnce(t, r, "ns", "nothing")
	reconcileOnce(t, r, "ns", "m")
	a := getModel(t, c, "m")
	reconcileOnce(t, r, "ns", "m")
	b := getModel(t, c, "m")
	if a.ResourceVersion != b.ResourceVersion {
		t.Error("a no-op reconcile rewrote the object")
	}
}
