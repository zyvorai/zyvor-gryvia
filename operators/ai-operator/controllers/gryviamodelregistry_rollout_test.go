// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

func scoredModel(name, stage, score string, mutate ...func(*gryviav1.GryviaModelRegistry)) *gryviav1.GryviaModelRegistry {
	return newModel(name, append([]func(*gryviav1.GryviaModelRegistry){func(m *gryviav1.GryviaModelRegistry) {
		m.Spec.ModelName = "chat"
		m.Spec.Version = name
		m.Spec.Stage = gryviav1.ModelStage(stage)
		if score != "" {
			m.Spec.Metadata = map[string]string{"eval_score": score}
		}
	}}, mutate...)...)
}

func withPolicy(p gryviav1.PromotionPolicy) func(*gryviav1.GryviaModelRegistry) {
	return func(m *gryviav1.GryviaModelRegistry) { m.Spec.PromotionPolicy = &p }
}

func TestModel_PromotionPolicyDecisions(t *testing.T) {
	cases := []struct {
		name       string
		candidate  *gryviav1.GryviaModelRegistry
		production []*gryviav1.GryviaModelRegistry
		want       string
		wantStage  gryviav1.ModelStage
		msg        string
	}{
		{"first version", scoredModel("v1", "staging", "0.7", withPolicy(gryviav1.PromotionPolicy{Metric: "eval_score"})),
			nil, PromotionPromoted, gryviav1.ModelStageProduction, "no production version"},
		{"beats production", scoredModel("v2", "staging", "0.8", withPolicy(gryviav1.PromotionPolicy{Metric: "eval_score", MinDelta: "0.05"})),
			[]*gryviav1.GryviaModelRegistry{scoredModel("v1", "production", "0.7")}, PromotionPromoted, gryviav1.ModelStageProduction, "beats v1"},
		{"not by enough", scoredModel("v2", "staging", "0.72", withPolicy(gryviav1.PromotionPolicy{Metric: "eval_score", MinDelta: "0.05"})),
			[]*gryviav1.GryviaModelRegistry{scoredModel("v1", "production", "0.7")}, PromotionRejected, gryviav1.ModelStageStaging, "does not beat v1"},
		{"equal is not better", scoredModel("v2", "staging", "0.7", withPolicy(gryviav1.PromotionPolicy{Metric: "eval_score"})),
			[]*gryviav1.GryviaModelRegistry{scoredModel("v1", "production", "0.7")}, PromotionRejected, gryviav1.ModelStageStaging, "does not beat"},
		{"compares with the best production", scoredModel("v3", "staging", "0.75", withPolicy(gryviav1.PromotionPolicy{Metric: "eval_score"})),
			[]*gryviav1.GryviaModelRegistry{scoredModel("v1", "production", "0.7"), scoredModel("v2", "production", "0.8")},
			PromotionRejected, gryviav1.ModelStageStaging, "v2"},
		{"minimize", scoredModel("v2", "staging", "1.9", withPolicy(gryviav1.PromotionPolicy{Metric: "eval_score", Direction: "minimize"})),
			[]*gryviav1.GryviaModelRegistry{scoredModel("v1", "production", "2.1")}, PromotionPromoted, gryviav1.ModelStageProduction, "beats"},
		{"threshold", scoredModel("v1", "staging", "0.4", withPolicy(gryviav1.PromotionPolicy{Metric: "eval_score", Threshold: "0.5"})),
			nil, PromotionRejected, gryviav1.ModelStageStaging, "threshold"},
		{"metric missing", scoredModel("v1", "staging", "", withPolicy(gryviav1.PromotionPolicy{Metric: "eval_score"})),
			nil, PromotionWaiting, gryviav1.ModelStageStaging, "not set yet"},
		{"metric not a number", scoredModel("v1", "staging", "high", withPolicy(gryviav1.PromotionPolicy{Metric: "eval_score"})),
			nil, PromotionRejected, gryviav1.ModelStageStaging, "not a number"},
		{"production without metric", scoredModel("v2", "staging", "0.9", withPolicy(gryviav1.PromotionPolicy{Metric: "eval_score"})),
			[]*gryviav1.GryviaModelRegistry{scoredModel("v1", "production", "")}, PromotionRejected, gryviav1.ModelStageStaging, "promote by hand"},
		{"other model names ignored", scoredModel("v2", "staging", "0.1", withPolicy(gryviav1.PromotionPolicy{Metric: "eval_score"})),
			[]*gryviav1.GryviaModelRegistry{scoredModel("o1", "production", "0.9", func(m *gryviav1.GryviaModelRegistry) { m.Spec.ModelName = "other" })},
			PromotionPromoted, gryviav1.ModelStageProduction, "no production version"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objs := []client.Object{tc.candidate}
			for _, p := range tc.production {
				objs = append(objs, p)
			}
			c := mlClient(objs...)
			reconcileOnce(t, newModelReconciler(c, 0), "ns", tc.candidate.Name)
			m := getModel(t, c, tc.candidate.Name)
			cond := findCond(m.Status.Conditions, ConditionPromotionGate)
			if m.Status.PromotionDecision != tc.want || m.Spec.Stage != tc.wantStage || cond == nil || !strings.Contains(cond.Message, tc.msg) {
				t.Errorf("decision %q stage %q condition %+v", m.Status.PromotionDecision, m.Spec.Stage, cond)
			}
		})
	}
}

func TestModel_NoPolicyOrNotStagingIsUntouched(t *testing.T) {
	c := mlClient(scoredModel("v1", "dev", "0.9", withPolicy(gryviav1.PromotionPolicy{Metric: "eval_score"})), scoredModel("v2", "staging", "0.9"))
	r := newModelReconciler(c, 0)
	reconcileOnce(t, r, "ns", "v1")
	reconcileOnce(t, r, "ns", "v2")
	if m := getModel(t, c, "v1"); m.Spec.Stage != gryviav1.ModelStageDev || m.Status.PromotionDecision != "" {
		t.Errorf("dev entry: %+v", m.Spec.Stage)
	}
	if m := getModel(t, c, "v2"); m.Spec.Stage != gryviav1.ModelStageStaging {
		t.Errorf("entry without policy: %+v", m.Spec.Stage)
	}
}

func sharedServing(m *gryviav1.GryviaModelRegistry) {
	m.Spec.AutoServe = true
	m.Spec.ServingConfig = &gryviav1.ServingConfig{ServiceName: "chat", CanaryWeight: 20, PromoteAfterSeconds: 60, Args: []string{"--max-model-len=8192"}}
	m.Spec.Artifacts = gryviav1.ModelArtifacts{PVCName: "models", SubPath: m.Name}
}

func envOf(d *appsv1.Deployment) map[string]string {
	env := map[string]string{}
	for _, e := range d.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	return env
}

// TestModel_SharedServiceCanaryRollout drives the registry and inference controllers together: v1 creates the
// shared service, v2 (auto-promoted by its score) becomes its canary, is promoted, and v1 is archived.
func TestModel_SharedServiceCanaryRollout(t *testing.T) {
	ctx := context.Background()
	v1 := scoredModel("v1", "production", "0.7", sharedServing)
	c := mlClient(v1)
	clk := newClock()
	reg := newModelReconciler(c, 0)
	inf := newInferReconciler(c, clk)

	reconcileOnce(t, reg, "ns", "v1")
	svc := getInfer(t, c, "chat")
	if svc.Spec.ModelRef != "v1" || svc.Labels[labelManagedBy] != managedByRegistry || metav1.GetControllerOf(svc) != nil ||
		svc.Spec.HealthCheck == nil || !svc.Spec.HealthCheck.AutoRollback || len(svc.Spec.Args) != 1 {
		t.Fatalf("shared service: %+v labels %v owners %v", svc.Spec, svc.Labels, svc.OwnerReferences)
	}
	reconcileOnce(t, inf, "ns", "chat")
	setReady(t, c, "chat-inference", 1)
	reconcileOnce(t, inf, "ns", "chat")
	reconcileOnce(t, reg, "ns", "v1")
	if m := getModel(t, c, "v1"); m.Status.Phase != PhaseServing || m.Status.InferenceServiceName != "chat" {
		t.Fatalf("v1: %+v", m.Status)
	}

	// v2 is registered in staging with a better score: promoted, then started as canary.
	v2 := scoredModel("v2", "staging", "0.8", sharedServing, withPolicy(gryviav1.PromotionPolicy{Metric: "eval_score"}))
	if err := c.Create(ctx, v2); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, reg, "ns", "v2")
	m2 := getModel(t, c, "v2")
	if m2.Spec.Stage != gryviav1.ModelStageProduction || m2.Status.Phase != PhaseCanary {
		t.Fatalf("v2: stage %s status %+v", m2.Spec.Stage, m2.Status)
	}
	svc = getInfer(t, c, "chat")
	if cc := svc.Spec.Canary; cc == nil || cc.ModelVersion != "v2" || cc.Weight != 20 || !cc.AutoPromote || cc.PromoteAfterSeconds != 60 {
		t.Fatalf("canary spec: %+v", svc.Spec.Canary)
	}
	// v1 reconciling in the meantime keeps being the stable version.
	reconcileOnce(t, reg, "ns", "v1")
	if getInfer(t, c, "chat").Spec.ModelRef != "v1" {
		t.Fatal("v1 must stay modelRef while v2 is the canary")
	}

	reconcileOnce(t, inf, "ns", "chat")
	cd := &appsv1.Deployment{}
	mustGet(t, c, "ns", "chat-canary", cd)
	if cd.Spec.Template.Spec.Containers[0].VolumeMounts[0].SubPath != "v2" {
		t.Errorf("the canary must mount v2's artifacts: %+v", cd.Spec.Template.Spec.Containers[0].VolumeMounts)
	}

	// A third version waits for the running canary.
	v3 := scoredModel("v3", "production", "0.9", sharedServing)
	if err := c.Create(ctx, v3); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, reg, "ns", "v3")
	if m := getModel(t, c, "v3"); m.Status.Phase != PhaseDeploying || !strings.Contains(m.Status.Message, "v2 is the canary") {
		t.Errorf("v3 should wait: %+v", m.Status)
	}
	if err := c.Delete(ctx, v3); err != nil {
		t.Fatal(err)
	}

	setReady(t, c, "chat-canary", 1)
	reconcileOnce(t, inf, "ns", "chat")
	clk.Add(61 * time.Second)
	reconcileOnce(t, inf, "ns", "chat")
	primary := &appsv1.Deployment{}
	mustGet(t, c, "ns", "chat-inference", primary)
	if primary.Annotations[annotationPromoted] != "v2" {
		t.Fatalf("canary not promoted: %+v", getInfer(t, c, "chat").Status)
	}

	reconcileOnce(t, reg, "ns", "v2")
	svc = getInfer(t, c, "chat")
	if svc.Spec.ModelRef != "v2" || svc.Spec.Canary != nil {
		t.Fatalf("after promotion: %+v", svc.Spec)
	}
	if m := getModel(t, c, "v1"); m.Spec.Stage != gryviav1.ModelStageArchived {
		t.Errorf("v1 must be archived, is %s", m.Spec.Stage)
	}
	if m := getModel(t, c, "v2"); m.Status.PreviousVersion != "v1" {
		t.Errorf("previousVersion %q", m.Status.PreviousVersion)
	}

	// The archived v1 leaves the shared service alone.
	reconcileOnce(t, reg, "ns", "v1")
	if !mlObjectExists(c, "ns", "chat", &gryviav1.GryviaInferenceService{}) {
		t.Fatal("archiving the replaced version must not delete the shared service")
	}
	reconcileOnce(t, inf, "ns", "chat")
	mustGet(t, c, "ns", "chat-inference", primary)
	if env := envOf(primary); env["MODEL_NAME"] != "v2" || env["MODEL_VERSION"] != "v2" {
		t.Errorf("stable env after promotion: %v", env)
	}
	if primary.Spec.Template.Spec.Containers[0].VolumeMounts[0].SubPath != "v2" {
		t.Errorf("the stable pods must mount v2's artifacts")
	}
}

func TestModel_SharedServiceRollbackAndForeignService(t *testing.T) {
	v1 := scoredModel("v1", "production", "0.7", sharedServing)
	v2 := scoredModel("v2", "production", "0.8", sharedServing)
	c := mlClient(v1, v2)
	clk := newClock()
	reg := newModelReconciler(c, 0)
	inf := newInferReconciler(c, clk)
	inf.CanaryStartupGrace = time.Minute

	reconcileOnce(t, reg, "ns", "v1")
	reconcileOnce(t, inf, "ns", "chat")
	reconcileOnce(t, reg, "ns", "v2")
	if getInfer(t, c, "chat").Spec.Canary == nil {
		t.Fatal("v2 should be the canary")
	}
	// The canary never becomes ready: three failed checks after the grace period roll it back.
	for i := 0; i < 5; i++ {
		clk.Add(time.Minute)
		reconcileOnce(t, inf, "ns", "chat")
	}
	primary := &appsv1.Deployment{}
	mustGet(t, c, "ns", "chat-inference", primary)
	if primary.Annotations[annotationRolledBack] != "v2" {
		t.Fatalf("not rolled back: %+v", getInfer(t, c, "chat").Status)
	}
	reconcileOnce(t, reg, "ns", "v2")
	if m := getModel(t, c, "v2"); m.Status.Phase != PhaseRolledBack || getInfer(t, c, "chat").Spec.ModelRef != "v1" {
		t.Errorf("v2 after rollback: %+v", m.Status)
	}
	if m := getModel(t, c, "v1"); m.Spec.Stage != gryviav1.ModelStageProduction {
		t.Error("a rollback must not archive the stable version")
	}

	// A service of that name not created by the registry is never adopted.
	foreign := newInfer("other")
	m := scoredModel("x1", "production", "1", sharedServing, func(m *gryviav1.GryviaModelRegistry) { m.Spec.ServingConfig.ServiceName = "other" })
	c = mlClient(foreign, m)
	reconcileOnce(t, newModelReconciler(c, 0), "ns", "x1")
	if got := getModel(t, c, "x1"); got.Status.Phase != PhaseFailed || !strings.Contains(got.Status.Message, "not managed") {
		t.Errorf("foreign service: %+v", got.Status)
	}
}
