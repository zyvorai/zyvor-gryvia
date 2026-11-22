// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/ai-operator/pkg/modelhub"
)

// fakeHub serves a fixed model list per author and records the token it was given.
type fakeHub struct {
	models map[string][]modelhub.Model
	err    error
	calls  int
	token  string
}

func (h *fakeHub) List(_ context.Context, q modelhub.Query, token string) ([]modelhub.Model, error) {
	h.calls++
	h.token = token
	if h.err != nil {
		return nil, h.err
	}
	return h.models[q.Author], nil
}

func hfModel(id, sha, license string, params int64) modelhub.Model {
	return modelhub.Model{ID: id, SHA: sha, Tags: []string{"license:" + license}, Params: params, PipelineTag: "text-generation"}
}

func watchTemplate() gryviav1.GryviaWorkflowSpec {
	return gryviav1.GryviaWorkflowSpec{
		Parameters: map[string]string{"DATASET": "s3://data/chat"},
		Steps: []gryviav1.WorkflowStep{
			{
				Name: "finetune", Type: gryviav1.StepTypeJob,
				JobTemplate: &gryviav1.GryviaAIJobSpec{
					Type: "fine-tuning", Image: "trainer:1", Model: "{{model.id}}",
					Args: []string{"--base={{model.id}}", "--revision={{model.revision}}", "--out=/models/{{model.slug}}"},
				},
			},
			{
				Name: "register", Type: gryviav1.StepTypeRegister, DependsOn: []string{"finetune"},
				Register: &gryviav1.RegisterStep{
					ModelName: "{{model.slug}}-chat", Version: "{{model.revision}}",
					Artifacts: gryviav1.ModelArtifacts{S3Path: "{{steps.finetune.outputs.path}}"},
				},
			},
		},
	}
}

func newWatch(name string, mutate ...func(*gryviav1.GryviaModelWatch)) *gryviav1.GryviaModelWatch {
	w := &gryviav1.GryviaModelWatch{
		ObjectMeta: objMeta("ns", name),
		Spec: gryviav1.GryviaModelWatchSpec{
			Sources:          []gryviav1.ModelWatchSource{{Provider: gryviav1.ProviderHuggingFace, Author: "Qwen"}},
			WorkflowTemplate: watchTemplate(),
		},
	}
	for _, f := range mutate {
		f(w)
	}
	return w
}

func newWatchReconciler(c client.Client, hub ModelLister, clk *fakeClock) *GryviaModelWatchReconciler {
	return &GryviaModelWatchReconciler{Client: c, Scheme: mlScheme(), Log: ctrl.Log.WithName("test"), Hub: hub, Clock: clk.Now}
}

func getWatch(t *testing.T, c client.Client, name string) *gryviav1.GryviaModelWatch {
	t.Helper()
	w := &gryviav1.GryviaModelWatch{}
	mustGet(t, c, "ns", name, w)
	return w
}

func candidate(w *gryviav1.GryviaModelWatch, id string) *gryviav1.ModelCandidate {
	for i := range w.Status.Candidates {
		if w.Status.Candidates[i].ID == id {
			return &w.Status.Candidates[i]
		}
	}
	return nil
}

func listWorkflows(t *testing.T, c client.Client) []gryviav1.GryviaWorkflow {
	t.Helper()
	l := &gryviav1.GryviaWorkflowList{}
	if err := c.List(context.Background(), l); err != nil {
		t.Fatal(err)
	}
	return l.Items
}

func TestModelWatch_BaselineThenNewRelease(t *testing.T) {
	hub := &fakeHub{models: map[string][]modelhub.Model{"Qwen": {hfModel("Qwen/Qwen3-4B", "aaaaaaa1", "apache-2.0", 4e9)}}}
	c := mlClient(newWatch("qwen"))
	clk := newClock()
	r := newWatchReconciler(c, hub, clk)

	reconcileOnce(t, r, "ns", "qwen")
	w := getWatch(t, c, "qwen")
	if w.Status.Phase != PhaseWatching || w.Status.LastPollTime == nil || w.Status.NextPollTime == nil {
		t.Fatalf("status: %+v", w.Status)
	}
	if cand := candidate(w, "Qwen/Qwen3-4B"); cand == nil || cand.Phase != gryviav1.CandidateBaseline {
		t.Fatalf("existing model must be baseline: %+v", w.Status.Candidates)
	}
	if n := len(listWorkflows(t, c)); n != 0 {
		t.Fatalf("baseline must not start runs, got %d workflows", n)
	}

	// Before the next poll nothing is listed again.
	reconcileOnce(t, r, "ns", "qwen")
	if hub.calls != 1 {
		t.Errorf("polled %d times before the interval elapsed", hub.calls)
	}

	// A new release appears.
	hub.models["Qwen"] = append([]modelhub.Model{hfModel("Qwen/Qwen3.5-8B", "bbbbbbbbbb", "apache-2.0", 8e9)}, hub.models["Qwen"]...)
	clk.Add(time.Hour)
	reconcileOnce(t, r, "ns", "qwen")
	w = getWatch(t, c, "qwen")
	cand := candidate(w, "Qwen/Qwen3.5-8B")
	if cand == nil || cand.Phase != gryviav1.CandidateRunning || cand.GPUs != 1 || cand.ParamsB != "8" || cand.Revision != "bbbbbbbbbb" {
		t.Fatalf("new model candidate: %+v", cand)
	}
	if w.Status.ActiveRuns != 1 {
		t.Errorf("activeRuns %d", w.Status.ActiveRuns)
	}

	wf := &gryviav1.GryviaWorkflow{}
	mustGet(t, c, "ns", cand.Workflow, wf)
	if cand.Workflow != "qwen-qwen3-5-8b-bbbbbbb" {
		t.Errorf("workflow name %q", cand.Workflow)
	}
	if ref := metav1.GetControllerOf(wf); ref == nil || ref.Kind != "GryviaModelWatch" || ref.Name != "qwen" {
		t.Errorf("owner: %v", wf.OwnerReferences)
	}
	if wf.Annotations[annotationSourceModel] != "Qwen/Qwen3.5-8B" || wf.Labels[labelModelWatch] != "qwen" {
		t.Errorf("metadata: %v %v", wf.Annotations, wf.Labels)
	}
	jt := wf.Spec.Steps[0].JobTemplate
	if jt.Model != "Qwen/Qwen3.5-8B" || strings.Join(jt.Args, " ") != "--base=Qwen/Qwen3.5-8B --revision=bbbbbbbbbb --out=/models/qwen3-5-8b" {
		t.Errorf("rendered job: %+v", jt)
	}
	reg := wf.Spec.Steps[1].Register
	if reg.ModelName != "qwen3-5-8b-chat" || reg.Artifacts.S3Path != "{{steps.finetune.outputs.path}}" {
		t.Errorf("model placeholders must be filled and step placeholders kept: %+v", reg)
	}
	if wf.Spec.Parameters["MODEL_ID"] != "Qwen/Qwen3.5-8B" || wf.Spec.Parameters["MODEL_GPUS"] != "1" || wf.Spec.Parameters["DATASET"] != "s3://data/chat" {
		t.Errorf("parameters: %v", wf.Spec.Parameters)
	}

	// The workflow finishes: the candidate follows.
	wf.Status.Phase = PhaseSucceeded
	if err := c.Status().Update(context.Background(), wf); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "ns", "qwen")
	w = getWatch(t, c, "qwen")
	if cand := candidate(w, "Qwen/Qwen3.5-8B"); cand.Phase != gryviav1.CandidateSucceeded || w.Status.ActiveRuns != 0 {
		t.Errorf("after success: %+v active %d", cand, w.Status.ActiveRuns)
	}
}

func TestModelWatch_Filters(t *testing.T) {
	gated := hfModel("Qwen/Gated-7B", "c000001", "apache-2.0", 7e9)
	gated.Gated = true
	hub := &fakeHub{models: map[string][]modelhub.Model{"Qwen": {
		hfModel("Qwen/Qwen3-8B", "a000001", "apache-2.0", 8e9),
		hfModel("Qwen/Qwen3-235B", "a000002", "apache-2.0", 235e9),
		hfModel("Qwen/Other-8B", "a000003", "cc-by-nc-4.0", 8e9),
		hfModel("Qwen/Qwen3-8B-GGUF", "a000004", "apache-2.0", 8e9),
		{ID: "Qwen/NoSize", SHA: "a000005", Tags: []string{"license:apache-2.0"}},
		{ID: "Other/Qwen-7B", SHA: "a000006", Tags: []string{"license:apache-2.0"}},
		{ID: "Qwen/Low-7B", SHA: "a000007", Tags: []string{"license:apache-2.0"}, Downloads: 1},
		gated,
	}}}
	c := mlClient(newWatch("qwen", func(w *gryviav1.GryviaModelWatch) {
		w.Spec.IncludeExisting = true
		w.Spec.MaxConcurrentRuns = 10
		w.Spec.LicenseAllowlist = []string{"apache-2.0", "mit"}
		w.Spec.MaxParamsB = 100
		w.Spec.Sources[0].NameRegex = `^(?!.*GGUF)`
	}))
	r := newWatchReconciler(c, hub, newClock())
	reconcileOnce(t, r, "ns", "qwen")
	w := getWatch(t, c, "qwen")
	if w.Status.Phase != PhaseFailed || !strings.Contains(w.Status.Message, "nameRegex") {
		t.Fatalf("Go regexp has no lookahead; the watch must be Failed: %+v", w.Status)
	}

	w.Spec.Sources[0].NameRegex = `^[A-Za-z0-9.-]+-\d+B$|^NoSize$|^Low-7B$`
	w.Spec.Sources[0].MinDownloads = 0
	if err := c.Update(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "ns", "qwen")
	w = getWatch(t, c, "qwen")

	want := map[string]string{
		"Qwen/Qwen3-8B":   gryviav1.CandidateRunning,
		"Qwen/Qwen3-235B": gryviav1.CandidateRejected, // maxParamsB
		"Qwen/Other-8B":   gryviav1.CandidateRejected, // license
		"Qwen/NoSize":     gryviav1.CandidateRejected, // unknown size with maxParamsB
		"Qwen/Low-7B":     gryviav1.CandidateRunning,
		"Qwen/Gated-7B":   gryviav1.CandidateRejected, // no token
	}
	for id, phase := range want {
		cand := candidate(w, id)
		if cand == nil || cand.Phase != phase {
			t.Errorf("%s: want %s, got %+v", id, phase, cand)
		}
	}
	for _, id := range []string{"Qwen/Qwen3-8B-GGUF", "Other/Qwen-7B"} {
		if candidate(w, id) != nil {
			t.Errorf("%s must be filtered out", id)
		}
	}
	if m := candidate(w, "Qwen/Other-8B").Message; !strings.Contains(m, "cc-by-nc-4.0") {
		t.Errorf("license message %q", m)
	}

	// minDownloads drops a model before it becomes a candidate.
	hub.models["Qwen"] = append(hub.models["Qwen"], modelhub.Model{ID: "Qwen/Tiny-1B", SHA: "a000008", Tags: []string{"license:mit"}, Downloads: 3})
	w.Spec.Sources[0].MinDownloads = 10
	w.Spec.Sources[0].NameRegex = ""
	_ = c.Update(context.Background(), w)
	w.Status.NextPollTime = nil
	_ = c.Status().Update(context.Background(), w)
	reconcileOnce(t, r, "ns", "qwen")
	if candidate(getWatch(t, c, "qwen"), "Qwen/Tiny-1B") != nil {
		t.Error("minDownloads not applied")
	}
}

func TestModelWatch_ConcurrencyAndTokenAndSizing(t *testing.T) {
	hub := &fakeHub{models: map[string][]modelhub.Model{"Qwen": {
		hfModel("Qwen/C-70B", "c000003", "apache-2.0", 70e9),
		hfModel("Qwen/B-32B", "c000002", "apache-2.0", 32e9),
		hfModel("Qwen/A-7B", "c000001", "apache-2.0", 7e9),
	}}}
	secret := &corev1.Secret{ObjectMeta: objMeta("ns", "hf"), Data: map[string][]byte{"token": []byte("hf_secret\n")}}
	c := mlClient(secret, newWatch("qwen", func(w *gryviav1.GryviaModelWatch) {
		w.Spec.IncludeExisting = true
		w.Spec.TokenSecretRef = &gryviav1.SecretKeyRef{Name: "hf"}
		w.Spec.Sizing = &gryviav1.ModelSizing{AutoSizeSteps: []string{"finetune"}}
	}))
	r := newWatchReconciler(c, hub, newClock())
	reconcileOnce(t, r, "ns", "qwen")
	if hub.token != "hf_secret" {
		t.Errorf("token %q", hub.token)
	}
	w := getWatch(t, c, "qwen")
	// Oldest first (the hub lists newest first), one at a time.
	if a := candidate(w, "Qwen/A-7B"); a.Phase != gryviav1.CandidateRunning {
		t.Errorf("A: %+v", a)
	}
	for _, id := range []string{"Qwen/B-32B", "Qwen/C-70B"} {
		if cand := candidate(w, id); cand.Phase != gryviav1.CandidateQueued {
			t.Errorf("%s should wait: %+v", id, cand)
		}
	}
	if b, cc := candidate(w, "Qwen/B-32B"), candidate(w, "Qwen/C-70B"); b.GPUs != 2 || cc.GPUs != 4 {
		t.Errorf("sizing: B %d C %d", b.GPUs, cc.GPUs)
	}
	wf := &gryviav1.GryviaWorkflow{}
	mustGet(t, c, "ns", candidate(w, "Qwen/A-7B").Workflow, wf)
	if wf.Spec.Steps[0].JobTemplate.GPUs != 1 {
		t.Errorf("auto-sized gpus %d", wf.Spec.Steps[0].JobTemplate.GPUs)
	}

	wf.Status.Phase = PhaseFailed
	wf.Status.Message = "boom"
	_ = c.Status().Update(context.Background(), wf)
	reconcileOnce(t, r, "ns", "qwen")
	w = getWatch(t, c, "qwen")
	if a := candidate(w, "Qwen/A-7B"); a.Phase != gryviav1.CandidateFailed || a.Message != "boom" {
		t.Errorf("A after failure: %+v", a)
	}
	if b := candidate(w, "Qwen/B-32B"); b.Phase != gryviav1.CandidateRunning {
		t.Errorf("B should start next: %+v", b)
	}
	mustGet(t, c, "ns", candidate(w, "Qwen/B-32B").Workflow, wf)
	if wf.Spec.Steps[0].JobTemplate.GPUs != 2 {
		t.Errorf("auto-sized gpus %d", wf.Spec.Steps[0].JobTemplate.GPUs)
	}
}

func TestModelWatch_RetrainOnNewRevision(t *testing.T) {
	hub := &fakeHub{models: map[string][]modelhub.Model{"Qwen": {hfModel("Qwen/A-7B", "d000001", "apache-2.0", 7e9)}}}
	c := mlClient(newWatch("qwen", func(w *gryviav1.GryviaModelWatch) { w.Spec.IncludeExisting = true }))
	clk := newClock()
	r := newWatchReconciler(c, hub, clk)
	reconcileOnce(t, r, "ns", "qwen")
	w := getWatch(t, c, "qwen")
	first := candidate(w, "Qwen/A-7B").Workflow
	wf := &gryviav1.GryviaWorkflow{}
	mustGet(t, c, "ns", first, wf)
	wf.Status.Phase = PhaseSucceeded
	_ = c.Status().Update(context.Background(), wf)

	hub.models["Qwen"][0].SHA = "d000002"
	clk.Add(time.Hour)
	reconcileOnce(t, r, "ns", "qwen")
	if cand := candidate(getWatch(t, c, "qwen"), "Qwen/A-7B"); cand.Phase != gryviav1.CandidateSucceeded || cand.Revision != "d000001" {
		t.Errorf("without retrainOnNewRevision a new commit is ignored: %+v", cand)
	}

	w = getWatch(t, c, "qwen")
	w.Spec.RetrainOnNewRevision = true
	_ = c.Update(context.Background(), w)
	clk.Add(time.Hour)
	reconcileOnce(t, r, "ns", "qwen")
	cand := candidate(getWatch(t, c, "qwen"), "Qwen/A-7B")
	if cand.Phase != gryviav1.CandidateRunning || cand.Revision != "d000002" || cand.Workflow == first {
		t.Errorf("retrain: %+v", cand)
	}
}

func TestModelWatch_PollErrorsAndSuspend(t *testing.T) {
	hub := &fakeHub{err: fmt.Errorf("hub answered 503")}
	c := mlClient(newWatch("qwen", func(w *gryviav1.GryviaModelWatch) {
		w.Spec.Sources = append(w.Spec.Sources, gryviav1.ModelWatchSource{Provider: gryviav1.ProviderNGC, Author: "nvidia"})
	}))
	clk := newClock()
	r := newWatchReconciler(c, hub, clk)
	res := reconcileOnce(t, r, "ns", "qwen")
	w := getWatch(t, c, "qwen")
	if !strings.Contains(w.Status.Message, "503") || w.Status.LastPollTime != nil {
		t.Errorf("poll error: %+v", w.Status)
	}
	if res.RequeueAfter > MinModelWatchPollInterval {
		t.Errorf("a failed poll retries within %s, got %s", MinModelWatchPollInterval, res.RequeueAfter)
	}

	hub.err = nil
	clk.Add(MinModelWatchPollInterval)
	reconcileOnce(t, r, "ns", "qwen")
	w = getWatch(t, c, "qwen")
	if !strings.Contains(w.Status.Message, "ngc") || w.Status.LastPollTime == nil {
		t.Errorf("ngc must be reported unsupported while huggingface is polled: %+v", w.Status)
	}

	w.Spec.Suspend = true
	_ = c.Update(context.Background(), w)
	calls := hub.calls
	clk.Add(2 * time.Hour)
	reconcileOnce(t, r, "ns", "qwen")
	w = getWatch(t, c, "qwen")
	if w.Status.Phase != PhaseSuspended || hub.calls != calls {
		t.Errorf("suspended watch polled: phase %s calls %d->%d", w.Status.Phase, calls, hub.calls)
	}
}

func TestModelWatch_InvalidTemplate(t *testing.T) {
	c := mlClient(newWatch("bad", func(w *gryviav1.GryviaModelWatch) {
		w.Spec.WorkflowTemplate.Steps[1].DependsOn = []string{"missing"}
	}))
	r := newWatchReconciler(c, &fakeHub{}, newClock())
	reconcileOnce(t, r, "ns", "bad")
	w := getWatch(t, c, "bad")
	if w.Status.Phase != PhaseFailed || !strings.Contains(w.Status.Message, "workflowTemplate") {
		t.Errorf("status: %+v", w.Status)
	}
}

func TestEstimateGPUs(t *testing.T) {
	cases := []struct {
		params int64
		sizing *gryviav1.ModelSizing
		want   int32
		err    bool
	}{
		{0, nil, 1, false},
		{135e6, nil, 1, false},
		{8e9, nil, 1, false},  // 24GB
		{32e9, nil, 2, false}, // 96GB
		{70e9, nil, 4, false}, // 210GB
		{235e9, nil, 0, true}, // 705GB -> 16 GPUs > 8
		{235e9, &gryviav1.ModelSizing{MaxGPUs: 16}, 16, false},
		{8e9, &gryviav1.ModelSizing{GPUMemoryGB: 24}, 1, false},
		{8e9, &gryviav1.ModelSizing{GPUMemoryGB: 16}, 2, false},
	}
	for _, tc := range cases {
		got, err := estimateGPUs(tc.params, tc.sizing)
		if (err != nil) != tc.err || got != tc.want {
			t.Errorf("params %d sizing %+v: got %d err %v", tc.params, tc.sizing, got, err)
		}
	}
}

func TestModelSlug(t *testing.T) {
	for in, want := range map[string]string{
		"Qwen3.5-8B-Instruct":   "qwen3-5-8b-instruct",
		"Llama_3.1--70B":        "llama-3-1--70b",
		"___":                   "model",
		strings.Repeat("a", 60): strings.Repeat("a", 40),
	} {
		if got := modelSlug(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}
