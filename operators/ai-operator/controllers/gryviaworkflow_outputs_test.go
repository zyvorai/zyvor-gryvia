// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controllers

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

// succeededPod is a finished pod of a step's AIJob (or a script pod) carrying a termination message.
func succeededPod(name string, labels map[string]string, message string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name, Labels: labels},
		Status: corev1.PodStatus{
			Phase: corev1.PodSucceeded,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "main",
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: message}},
			}},
		},
	}
}

func modelFactoryWF() *gryviav1.GryviaWorkflow {
	train := jobStep("train")
	train.JobTemplate.Args = []string{"--out=/models/{{workflow.name}}", "--data={{parameters.DATASET}}"}
	eval := jobStep("eval", "train")
	eval.JobTemplate.Args = []string{"--model={{steps.train.outputs.path}}"}
	reg := gryviav1.WorkflowStep{
		Name: "register", Type: gryviav1.StepTypeRegister, DependsOn: []string{"eval"},
		Register: &gryviav1.RegisterStep{
			ModelName: "chat", Version: "{{steps.train.outputs.version}}",
			Artifacts:       gryviav1.ModelArtifacts{PVCName: "models", SubPath: "{{steps.train.outputs.path}}", Format: "safetensors"},
			Metadata:        map[string]string{"eval_score": "{{steps.eval.outputs.score}}"},
			AutoServe:       true,
			PromotionPolicy: &gryviav1.PromotionPolicy{Metric: "eval_score"},
		},
	}
	wf := newWF("factory", train, eval, reg)
	wf.Spec.Parameters = map[string]string{"DATASET": "s3://d/chat"}
	wf.Annotations = map[string]string{annotationSourceModel: "Qwen/Qwen3-8B", annotationSourceRevision: "abc1234"}
	return wf
}

func TestWorkflow_OutputsFlowIntoLaterSteps(t *testing.T) {
	c := mlClient(modelFactoryWF())
	r := newWFReconciler(c, newClock())
	ctx := context.Background()

	reconcileOnce(t, r, "ns", "factory")
	job := &gryviav1.GryviaAIJob{}
	mustGet(t, c, "ns", "factory-train", job)
	if got := strings.Join(job.Spec.Args, " "); got != "--out=/models/factory --data=s3://d/chat" {
		t.Errorf("train args %q", got)
	}

	// Rank 1 finishes first with different output; rank 0's termination message must win.
	other := succeededPod("factory-train-1", map[string]string{"gryvia.io/job": "factory-train", "batch.kubernetes.io/job-completion-index": "1"}, `{"path":"wrong"}`)
	rank0 := succeededPod("factory-train-0", map[string]string{"gryvia.io/job": "factory-train", "batch.kubernetes.io/job-completion-index": "0"},
		`{"path":"qwen3-8b/run1","version":"2026.10.01","steps":1200,"ok":true,"nested":{"x":1},"bad key":"x"}`)
	for _, p := range []*corev1.Pod{other, rank0} {
		if err := c.Create(ctx, p); err != nil {
			t.Fatal(err)
		}
		p.Status = succeededPod(p.Name, nil, p.Status.ContainerStatuses[0].State.Terminated.Message).Status
		if err := c.Status().Update(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	// An annotation on the AIJob wins over the termination message.
	mustGet(t, c, "ns", "factory-train", job)
	job.Annotations = map[string]string{"gryvia.io/output-version": "v7"}
	if err := c.Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	setJobPhase(t, c, "factory-train", PhaseSucceeded, "")
	reconcileOnce(t, r, "ns", "factory")

	wf := getWF(t, c, "factory")
	out := stepOf(wf, "train").Outputs
	want := map[string]string{"path": "qwen3-8b/run1", "version": "v7", "steps": "1200", "ok": "true"}
	if len(out) != len(want) {
		t.Errorf("outputs %v", out)
	}
	for k, v := range want {
		if out[k] != v {
			t.Errorf("output %s = %q, want %q", k, out[k], v)
		}
	}
	mustGet(t, c, "ns", "factory-eval", job)
	if got := strings.Join(job.Spec.Args, " "); got != "--model=qwen3-8b/run1" {
		t.Errorf("eval args %q", got)
	}

	job.Annotations = map[string]string{"gryvia.io/output-score": "0.81"}
	if err := c.Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	setJobPhase(t, c, "factory-eval", PhaseSucceeded, "")
	reconcileOnce(t, r, "ns", "factory")

	wf = getWF(t, c, "factory")
	if wf.Status.Phase != PhaseSucceeded {
		t.Fatalf("workflow: %s %s %+v", wf.Status.Phase, wf.Status.Message, stepOf(wf, "register"))
	}
	if s := stepOf(wf, "register"); s.Outputs["name"] != "factory-register" || s.JobName != "factory-register" {
		t.Errorf("register step: %+v", s)
	}
	m := &gryviav1.GryviaModelRegistry{}
	mustGet(t, c, "ns", "factory-register", m)
	if m.Spec.ModelName != "chat" || m.Spec.Version != "v7" || m.Spec.Stage != gryviav1.ModelStageStaging ||
		m.Spec.Artifacts.SubPath != "qwen3-8b/run1" || m.Spec.Source.WorkflowRef != "factory" || !m.Spec.AutoServe ||
		m.Spec.PromotionPolicy == nil {
		t.Errorf("registry spec: %+v", m.Spec)
	}
	if m.Spec.Metadata["eval_score"] != "0.81" || m.Spec.Metadata["base_model"] != "Qwen/Qwen3-8B" || m.Spec.Metadata["base_revision"] != "abc1234" {
		t.Errorf("metadata: %v", m.Spec.Metadata)
	}
	if metav1.GetControllerOf(m) != nil {
		t.Error("a registered model must not be owned by the workflow")
	}
}

func TestWorkflow_UnresolvedOutputFailsStep(t *testing.T) {
	c := mlClient(modelFactoryWF())
	r := newWFReconciler(c, newClock())
	reconcileOnce(t, r, "ns", "factory")
	setJobPhase(t, c, "factory-train", PhaseSucceeded, "") // no outputs reported
	reconcileOnce(t, r, "ns", "factory")

	wf := getWF(t, c, "factory")
	s := stepOf(wf, "eval")
	if s.Phase != gryviav1.StepPhaseFailed || !strings.Contains(s.Message, "{{steps.train.outputs.path}}") {
		t.Errorf("eval step: %+v", s)
	}
	if jobsOf(t, c)["factory-eval"] {
		t.Error("a job with an unresolved placeholder must not be created")
	}
}

func TestWorkflow_ScriptStepOutputs(t *testing.T) {
	s := scriptStep("probe")
	next := scriptStep("use", "probe")
	next.Script.Args = []string{"{{steps.probe.outputs.url}}"}
	c := mlClient(newWF("scripts", s, next))
	r := newWFReconciler(c, newClock())
	ctx := context.Background()
	reconcileOnce(t, r, "ns", "scripts")

	pod := &corev1.Pod{}
	mustGet(t, c, "ns", "scripts-probe", pod)
	pod.Status = succeededPod("", nil, `{"url":"http://x/y"}`).Status
	if err := c.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "ns", "scripts")
	mustGet(t, c, "ns", "scripts-use", pod)
	if got := pod.Spec.Containers[0].Args; len(got) != 1 || got[0] != "http://x/y" {
		t.Errorf("script args %v", got)
	}
}

func TestWorkflow_RegisterValidationAndConflicts(t *testing.T) {
	bad := []gryviav1.RegisterStep{
		{Version: "1", Artifacts: gryviav1.ModelArtifacts{S3Path: "s3://x"}},
		{ModelName: "m", Version: "1"},
		{ModelName: "m", Version: "1", Artifacts: gryviav1.ModelArtifacts{S3Path: "s3://x"}, Stage: gryviav1.ModelStageProduction},
	}
	for i := range bad {
		wf := newWF("w", gryviav1.WorkflowStep{Name: "r", Register: &bad[i]})
		if err := validateWorkflow(wf); err == nil {
			t.Errorf("case %d must be invalid", i)
		}
	}

	// An entry with the same name from somewhere else fails the step instead of being adopted.
	foreign := newModel("w-r")
	c := mlClient(foreign, newWF("w", gryviav1.WorkflowStep{Name: "r", Register: &gryviav1.RegisterStep{
		ModelName: "m", Version: "1", Artifacts: gryviav1.ModelArtifacts{S3Path: "s3://x"},
	}}))
	r := newWFReconciler(c, newClock())
	reconcileOnce(t, r, "ns", "w")
	wf := getWF(t, c, "w")
	if s := stepOf(wf, "r"); s.Phase != gryviav1.StepPhaseFailed || !strings.Contains(s.Message, "not registered by this run") {
		t.Errorf("step: %+v", s)
	}
}

func TestWorkflow_Schedule(t *testing.T) {
	wf := newWF("nightly", jobStep("train"))
	wf.Spec.Schedule = "0 2 * * *"
	clk := newClock() // 2026-01-01 12:00 UTC
	wf.CreationTimestamp = metav1.NewTime(clk.Now())
	c := mlClient(wf)
	r := newWFReconciler(c, clk)

	res := reconcileOnce(t, r, "ns", "nightly")
	got := getWF(t, c, "nightly")
	if got.Status.Phase != PhaseScheduled || got.Status.NextScheduleTime == nil ||
		!got.Status.NextScheduleTime.Time.Equal(time.Date(2026, 1, 2, 2, 0, 0, 0, time.UTC)) {
		t.Fatalf("status: %+v", got.Status)
	}
	if res.RequeueAfter < 13*time.Hour || len(jobsOf(t, c)) != 0 {
		t.Errorf("requeue %s, jobs %v", res.RequeueAfter, jobsOf(t, c))
	}

	clk.Add(14 * time.Hour) // past 02:00
	reconcileOnce(t, r, "ns", "nightly")
	got = getWF(t, c, "nightly")
	if got.Status.Run != 1 || got.Status.Phase != PhaseRunning || !jobsOf(t, c)["nightly-train-n1"] {
		t.Fatalf("first run: run %d phase %s jobs %v", got.Status.Run, got.Status.Phase, jobsOf(t, c))
	}
	j := &gryviav1.GryviaAIJob{}
	mustGet(t, c, "ns", "nightly-train-n1", j)
	if j.Labels["gryvia.io/run"] != "1" {
		t.Errorf("labels %v", j.Labels)
	}

	setJobPhase(t, c, "nightly-train-n1", PhaseSucceeded, "")
	res = reconcileOnce(t, r, "ns", "nightly")
	got = getWF(t, c, "nightly")
	if got.Status.Phase != PhaseSucceeded || res.RequeueAfter < 20*time.Hour {
		t.Fatalf("after run: %s requeue %s", got.Status.Phase, res.RequeueAfter)
	}

	// Still before the next fire time: nothing happens.
	clk.Add(time.Hour)
	reconcileOnce(t, r, "ns", "nightly")
	if got = getWF(t, c, "nightly"); got.Status.Run != 1 || got.Status.Phase != PhaseSucceeded {
		t.Errorf("early: run %d phase %s", got.Status.Run, got.Status.Phase)
	}

	clk.Add(24 * time.Hour)
	reconcileOnce(t, r, "ns", "nightly")
	got = getWF(t, c, "nightly")
	if got.Status.Run != 2 || !jobsOf(t, c)["nightly-train-n2"] || !jobsOf(t, c)["nightly-train-n1"] {
		t.Errorf("second run: run %d jobs %v", got.Status.Run, jobsOf(t, c))
	}
	if s := stepOf(got, "train"); s.Phase != gryviav1.StepPhaseRunning || s.JobName != "nightly-train-n2" {
		t.Errorf("second run step: %+v", s)
	}

	bad := newWF("bad", jobStep("train"))
	bad.Spec.Schedule = "every day"
	c = mlClient(bad)
	reconcileOnce(t, newWFReconciler(c, clk), "ns", "bad")
	if got := getWF(t, c, "bad"); got.Status.Phase != PhaseFailed || !strings.Contains(got.Status.Message, "schedule") {
		t.Errorf("invalid schedule: %+v", got.Status)
	}
}
