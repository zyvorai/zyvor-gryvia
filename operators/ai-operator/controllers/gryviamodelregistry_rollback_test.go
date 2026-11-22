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
	"k8s.io/client-go/tools/record"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

// promotedState is v2 serving the shared service "chat" after replacing v1, which was archived.
func promotedState(v2score string, mutate ...func(*gryviav1.GryviaModelRegistry)) (*gryviav1.GryviaModelRegistry, *gryviav1.GryviaModelRegistry, *gryviav1.GryviaInferenceService) {
	v1 := scoredModel("v1", "archived", "0.7", sharedServing)
	v2 := scoredModel("v2", "production", v2score, append([]func(*gryviav1.GryviaModelRegistry){sharedServing,
		func(m *gryviav1.GryviaModelRegistry) {
			m.Spec.RollbackPolicy = &gryviav1.RollbackPolicy{Metric: "eval_score", Threshold: "0.5"}
			m.Status.PreviousVersion = "v1"
		}}, mutate...)...)
	svc := newInfer("chat", func(s *gryviav1.GryviaInferenceService) {
		s.Labels = map[string]string{labelManagedBy: managedByRegistry, labelServingGroup: "chat"}
		s.Spec.ModelRef = "v2"
		s.Spec.Canary = &gryviav1.CanaryConfig{Enabled: true, Weight: 10, ModelVersion: "v3"}
	})
	return v1, v2, svc
}

func TestModel_RollbackPolicyRestoresPreviousVersion(t *testing.T) {
	v1, v2, svc := promotedState("0.3")
	c := mlClient(v1, v2, svc)
	r := newModelReconciler(c, 0)
	rec := record.NewFakeRecorder(10)
	r.Recorder = rec

	reconcileOnce(t, r, "ns", "v2")
	s := getInfer(t, c, "chat")
	if s.Spec.ModelRef != "v1" || s.Spec.Canary != nil {
		t.Fatalf("shared service after rollback: %+v", s.Spec)
	}
	if m := getModel(t, c, "v1"); m.Spec.Stage != gryviav1.ModelStageProduction || !m.Spec.AutoServe {
		t.Errorf("v1 must be back in production: %s", m.Spec.Stage)
	}
	m := getModel(t, c, "v2")
	cond := findCond(m.Status.Conditions, ConditionRolledBack)
	if m.Spec.Stage != gryviav1.ModelStageArchived || m.Status.Phase != PhaseRolledBack || cond == nil ||
		cond.Status != metav1.ConditionTrue || cond.Reason != "PolicyBreached" || !strings.Contains(cond.Message, "below the rollback threshold 0.5") {
		t.Fatalf("v2: stage %s status %+v", m.Spec.Stage, m.Status)
	}
	if ev := <-rec.Events; !strings.Contains(ev, "Normal RolledBack") {
		t.Errorf("event %q", ev)
	}

	// The archived entry keeps saying it was rolled back; v1 serves again.
	reconcileOnce(t, r, "ns", "v2")
	if m := getModel(t, c, "v2"); m.Status.Phase != PhaseRolledBack {
		t.Errorf("v2 phase after another pass: %s", m.Status.Phase)
	}
	reconcileOnce(t, r, "ns", "v1")
	if s := getInfer(t, c, "chat"); s.Spec.ModelRef != "v1" {
		t.Errorf("v1 must stay the modelRef: %s", s.Spec.ModelRef)
	}
}

// TestModel_RollbackRollsTheStablePodsBack drives both controllers: v2 is canaried and promoted over v1, then its
// live score drops and the stable Deployment must load v1's artifacts again.
func TestModel_RollbackRollsTheStablePodsBack(t *testing.T) {
	ctx := context.Background()
	v1 := scoredModel("v1", "production", "0.7", sharedServing)
	v2 := scoredModel("v2", "production", "0.8", sharedServing, func(m *gryviav1.GryviaModelRegistry) {
		m.Spec.RollbackPolicy = &gryviav1.RollbackPolicy{Metric: "live_score", Threshold: "0.5"}
	})
	c := mlClient(v1)
	clk := newClock()
	reg := newModelReconciler(c, 0)
	inf := newInferReconciler(c, clk)
	reconcileOnce(t, reg, "ns", "v1")
	reconcileOnce(t, inf, "ns", "chat")
	setReady(t, c, "chat-inference", 1)
	if err := c.Create(ctx, v2); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, reg, "ns", "v2")
	reconcileOnce(t, inf, "ns", "chat")
	setReady(t, c, "chat-canary", 1)
	reconcileOnce(t, inf, "ns", "chat")
	clk.Add(61 * time.Second)
	reconcileOnce(t, inf, "ns", "chat")
	reconcileOnce(t, reg, "ns", "v2")
	reconcileOnce(t, inf, "ns", "chat")
	primary := &appsv1.Deployment{}
	mustGet(t, c, "ns", "chat-inference", primary)
	if envOf(primary)["MODEL_VERSION"] != "v2" || getModel(t, c, "v1").Spec.Stage != gryviav1.ModelStageArchived {
		t.Fatalf("v2 not promoted: env %v", envOf(primary))
	}

	m := getModel(t, c, "v2")
	m.Spec.Metadata["live_score"] = "0.2"
	if err := c.Update(ctx, m); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, reg, "ns", "v2")
	reconcileOnce(t, inf, "ns", "chat")
	mustGet(t, c, "ns", "chat-inference", primary)
	if env := envOf(primary); env["MODEL_VERSION"] == "v2" || env["MODEL_NAME"] != "v1" {
		t.Errorf("stable env after rollback: %v", env)
	}
	if sp := primary.Spec.Template.Spec.Containers[0].VolumeMounts[0].SubPath; sp != "v1" {
		t.Errorf("the stable pods must mount v1's artifacts again, mount %q", sp)
	}
	reconcileOnce(t, reg, "ns", "v1")
	if s := getInfer(t, c, "chat"); s.Spec.ModelRef != "v1" || s.Spec.Canary != nil {
		t.Errorf("service after rollback: %+v", s.Spec)
	}
}

func TestModel_RollbackPolicyWithinThresholdIsUntouched(t *testing.T) {
	v1, v2, svc := promotedState("0.6")
	c := mlClient(v1, v2, svc)
	reconcileOnce(t, newModelReconciler(c, 0), "ns", "v2")
	if s := getInfer(t, c, "chat"); s.Spec.ModelRef != "v2" {
		t.Errorf("modelRef %s", s.Spec.ModelRef)
	}
	if m := getModel(t, c, "v2"); m.Spec.Stage != gryviav1.ModelStageProduction || findCond(m.Status.Conditions, ConditionRolledBack) != nil {
		t.Errorf("v2: %s %+v", m.Spec.Stage, m.Status.Conditions)
	}
}

func TestModel_RollbackRequested(t *testing.T) {
	v1, v2, svc := promotedState("0.9", func(m *gryviav1.GryviaModelRegistry) {
		m.Annotations = map[string]string{annotationRollbackRequested: "admin"}
	})
	c := mlClient(v1, v2, svc)
	reconcileOnce(t, newModelReconciler(c, 0), "ns", "v2")
	m := getModel(t, c, "v2")
	cond := findCond(m.Status.Conditions, ConditionRolledBack)
	if m.Spec.Stage != gryviav1.ModelStageArchived || cond == nil || cond.Reason != "Requested" || m.Annotations[annotationRollbackRequested] != "" {
		t.Fatalf("v2: %s %+v %v", m.Spec.Stage, cond, m.Annotations)
	}
	if s := getInfer(t, c, "chat"); s.Spec.ModelRef != "v1" {
		t.Errorf("modelRef %s", s.Spec.ModelRef)
	}
}

func TestModel_RollbackRefused(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*gryviav1.GryviaModelRegistry)
		reason string
	}{
		{"no previous version", func(m *gryviav1.GryviaModelRegistry) { m.Status.PreviousVersion = "" }, "NoPreviousVersion"},
		{"previous of another service", func(m *gryviav1.GryviaModelRegistry) { m.Status.PreviousVersion = "v9" }, "NoPreviousVersion"},
		{"not in production", func(m *gryviav1.GryviaModelRegistry) { m.Spec.Stage = gryviav1.ModelStageStaging }, "NotServing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v1, v2, svc := promotedState("0.9", tc.mutate, func(m *gryviav1.GryviaModelRegistry) {
				m.Annotations = map[string]string{annotationRollbackRequested: "admin"}
			})
			c := mlClient(v1, v2, svc)
			r := newModelReconciler(c, 0)
			rec := record.NewFakeRecorder(10)
			r.Recorder = rec
			reconcileOnce(t, r, "ns", "v2")
			m := getModel(t, c, "v2")
			cond := findCond(m.Status.Conditions, ConditionRolledBack)
			if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != tc.reason || m.Annotations[annotationRollbackRequested] != "" {
				t.Fatalf("condition %+v annotations %v", cond, m.Annotations)
			}
			if m.Spec.Stage == gryviav1.ModelStageArchived {
				t.Error("a refused rollback must not archive the entry")
			}
			if ev := <-rec.Events; !strings.Contains(ev, "Warning RollbackRefused") {
				t.Errorf("event %q", ev)
			}
		})
	}
}

func TestRollbackBreach(t *testing.T) {
	cases := []struct {
		score, threshold, direction string
		want                        bool
	}{
		{"0.4", "0.5", "", true},
		{"0.5", "0.5", "", false},
		{"2.5", "2", "minimize", true},
		{"1.5", "2", "minimize", false},
		{"", "0.5", "", false},
		{"n/a", "0.5", "", false},
	}
	for _, tc := range cases {
		m := scoredModel("v", "production", tc.score, func(m *gryviav1.GryviaModelRegistry) {
			m.Spec.RollbackPolicy = &gryviav1.RollbackPolicy{Metric: "eval_score", Threshold: tc.threshold, Direction: tc.direction}
		})
		if got, _ := rollbackBreach(m); got != tc.want {
			t.Errorf("%+v: got %v", tc, got)
		}
	}
	if got, _ := rollbackBreach(scoredModel("v", "production", "0.1")); got {
		t.Error("no policy is never a breach")
	}
}

func TestWorkflow_RegistryStep(t *testing.T) {
	ctx := context.Background()
	entry := scoredModel("chat-v2", "production", "0.8")
	eval := scriptStep("eval")
	update := gryviav1.WorkflowStep{Name: "record", DependsOn: []string{"eval"}, Registry: &gryviav1.RegistryStep{
		Entry: "{{parameters.ENTRY}}", Action: "updateMetadata",
		Metadata: map[string]string{"eval_score": "{{steps.eval.outputs.score}}", "eval_run": "{{workflow.name}}"},
	}}
	rollback := gryviav1.WorkflowStep{Name: "rollback", DependsOn: []string{"record"}, Type: gryviav1.StepTypeRegistry,
		Registry: &gryviav1.RegistryStep{Entry: "chat-v2", Action: "rollback"}}
	wf := newWF("nightly-eval", eval, update, rollback)
	wf.Spec.Parameters = map[string]string{"ENTRY": "chat-v2"}
	c := mlClient(entry, wf)
	r := newWFReconciler(c, newClock())

	reconcileOnce(t, r, "ns", "nightly-eval")
	pod := succeededPod("", nil, `{"score":"0.42"}`)
	got := getWF(t, c, "nightly-eval")
	mustGet(t, c, "ns", stepOf(got, "eval").JobName, pod)
	pod.Status = succeededPod("", nil, `{"score":"0.42"}`).Status
	if err := c.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "ns", "nightly-eval")
	reconcileOnce(t, r, "ns", "nightly-eval")

	got = getWF(t, c, "nightly-eval")
	if got.Status.Phase != PhaseSucceeded {
		t.Fatalf("workflow %s %s: %+v", got.Status.Phase, got.Status.Message, got.Status.StepStatuses)
	}
	if s := stepOf(got, "record"); s.Outputs["name"] != "chat-v2" || !strings.Contains(s.Message, "eval_run, eval_score") {
		t.Errorf("record step: %+v", s)
	}
	m := getModel(t, c, "chat-v2")
	if m.Spec.Metadata["eval_score"] != "0.42" || m.Spec.Metadata["eval_run"] != "nightly-eval" {
		t.Errorf("metadata %v", m.Spec.Metadata)
	}
	if !strings.HasPrefix(m.Annotations[annotationRollbackRequested], "workflow nightly-eval/rollback at ") {
		t.Errorf("rollback annotation %v", m.Annotations)
	}
}

func TestWorkflow_RegistryStepByServiceName(t *testing.T) {
	v1, v2, svc := promotedState("0.8")
	foreign := newInfer("plain", func(s *gryviav1.GryviaInferenceService) { s.Spec.ModelRef = "" })
	// Created by hand, not by the registry: its modelRef still names the entry it serves.
	manual := newInfer("manual", func(s *gryviav1.GryviaInferenceService) { s.Spec.ModelRef = "v1" })
	step := func(service string) gryviav1.WorkflowStep {
		return gryviav1.WorkflowStep{Name: "record", Registry: &gryviav1.RegistryStep{
			ServiceName: service, Action: "updateMetadata", Metadata: map[string]string{"live_score": "0.31"}}}
	}
	c := mlClient(v1, v2, svc, foreign, manual, newWF("live", step("chat")), newWF("bad", step("plain")),
		newWF("gone", step("absent")), newWF("hand", step("manual")))
	r := newWFReconciler(c, newClock())
	reconcileOnce(t, r, "ns", "live")
	if s := stepOf(getWF(t, c, "live"), "record"); s.Phase != gryviav1.StepPhaseSucceeded || s.Outputs["name"] != "v2" {
		t.Fatalf("step: %+v", s)
	}
	if m := getModel(t, c, "v2"); m.Spec.Metadata["live_score"] != "0.31" || m.Spec.Metadata["eval_score"] != "0.8" {
		t.Errorf("serving entry metadata %v", m.Spec.Metadata)
	}
	reconcileOnce(t, r, "ns", "hand")
	if s := stepOf(getWF(t, c, "hand"), "record"); s.Phase != gryviav1.StepPhaseSucceeded || s.Outputs["name"] != "v1" {
		t.Fatalf("hand-made service step: %+v", s)
	}
	for wf, msg := range map[string]string{"bad": "no modelRef", "gone": "not found"} {
		reconcileOnce(t, r, "ns", wf)
		if s := stepOf(getWF(t, c, wf), "record"); s.Phase != gryviav1.StepPhaseFailed || !strings.Contains(s.Message, msg) {
			t.Errorf("%s: %+v", wf, s)
		}
	}
}

func TestWorkflow_RegistryStepValidationAndMissingEntry(t *testing.T) {
	bad := []gryviav1.RegistryStep{
		{Action: "rollback"},
		{Entry: "e", ServiceName: "s", Action: "rollback"},
		{Entry: "e", Action: "promote"},
		{Entry: "e", Action: "updateMetadata"},
	}
	for i := range bad {
		if err := validateWorkflow(newWF("w", gryviav1.WorkflowStep{Name: "r", Registry: &bad[i]})); err == nil {
			t.Errorf("case %d must be invalid", i)
		}
	}
	c := mlClient(newWF("w", gryviav1.WorkflowStep{Name: "r", Registry: &gryviav1.RegistryStep{Entry: "absent", Action: "rollback"}}))
	reconcileOnce(t, newWFReconciler(c, newClock()), "ns", "w")
	if s := stepOf(getWF(t, c, "w"), "r"); s.Phase != gryviav1.StepPhaseFailed || !strings.Contains(s.Message, "not found") {
		t.Errorf("step: %+v", s)
	}
}
