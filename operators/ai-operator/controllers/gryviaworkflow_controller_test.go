// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

func newWFReconciler(c client.Client, clk *fakeClock) *GryviaWorkflowReconciler {
	return &GryviaWorkflowReconciler{Client: c, Scheme: mlScheme(), Log: ctrl.Log.WithName("test"), Clock: clk.Now}
}

func jobStep(name string, deps ...string) gryviav1.WorkflowStep {
	return gryviav1.WorkflowStep{
		Name: name, Type: gryviav1.StepTypeJob, DependsOn: deps,
		JobTemplate: &gryviav1.GryviaAIJobSpec{Type: "training", Image: "busybox:1", Command: []string{"true"}},
	}
}

func newWF(name string, steps ...gryviav1.WorkflowStep) *gryviav1.GryviaWorkflow {
	return &gryviav1.GryviaWorkflow{ObjectMeta: objMeta("ns", name), Spec: gryviav1.GryviaWorkflowSpec{Steps: steps}}
}

func getWF(t *testing.T, c client.Client, name string) *gryviav1.GryviaWorkflow {
	t.Helper()
	w := &gryviav1.GryviaWorkflow{}
	mustGet(t, c, "ns", name, w)
	return w
}

func stepOf(w *gryviav1.GryviaWorkflow, name string) gryviav1.StepStatus {
	for _, s := range w.Status.StepStatuses {
		if s.Name == name {
			return s
		}
	}
	return gryviav1.StepStatus{}
}

func setJobPhase(t *testing.T, c client.Client, name, phase, msg string) {
	t.Helper()
	j := &gryviav1.GryviaAIJob{}
	mustGet(t, c, "ns", name, j)
	j.Status.Phase, j.Status.Message = phase, msg
	if err := c.Status().Update(context.Background(), j); err != nil {
		t.Fatal(err)
	}
}

func jobsOf(t *testing.T, c client.Client) map[string]bool {
	t.Helper()
	l := &gryviav1.GryviaAIJobList{}
	if err := c.List(context.Background(), l); err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, j := range l.Items {
		out[j.Name] = true
	}
	return out
}

func TestWorkflow_ValidationFailures(t *testing.T) {
	long := strings.Repeat("s", 64)
	many := make([]gryviav1.WorkflowStep, 0, 101)
	for i := 0; i < 101; i++ {
		many = append(many, jobStep(fmt.Sprintf("s%d", i)))
	}
	noPayload := gryviav1.WorkflowStep{Name: "x", Type: gryviav1.StepTypeJob}
	for name, tc := range map[string]struct {
		steps []gryviav1.WorkflowStep
		want  string
	}{
		"cycle":       {[]gryviav1.WorkflowStep{jobStep("a", "b"), jobStep("b", "a")}, "cycle"},
		"long cycle":  {[]gryviav1.WorkflowStep{jobStep("a", "c"), jobStep("b", "a"), jobStep("c", "b"), jobStep("d")}, "cycle"},
		"self":        {[]gryviav1.WorkflowStep{jobStep("a", "a")}, "depends on itself"},
		"unknown dep": {[]gryviav1.WorkflowStep{jobStep("a", "zzz")}, "non-existent"},
		"duplicate":   {[]gryviav1.WorkflowStep{jobStep("a"), jobStep("a")}, "duplicate"},
		"bad name":    {[]gryviav1.WorkflowStep{jobStep("Bad_Name")}, "DNS label"},
		"long name":   {[]gryviav1.WorkflowStep{jobStep(long)}, "DNS label"},
		"too many":    {many, "exceeds the limit"},
		"no payload":  {[]gryviav1.WorkflowStep{noPayload}, "no jobTemplate"},
		"no steps":    {nil, "no steps"},
		"bad condition": {[]gryviav1.WorkflowStep{jobStep("a"),
			{Name: "b", DependsOn: []string{"a"}, Condition: "1 == 1", JobTemplate: jobStep("b").JobTemplate}}, "unsupported condition"},
		"condition on non-dependency": {[]gryviav1.WorkflowStep{jobStep("a"),
			{Name: "b", Condition: "steps.a.status == 'Failed'", JobTemplate: jobStep("b").JobTemplate}}, "not in dependsOn"},
		"too many retries": {[]gryviav1.WorkflowStep{func() gryviav1.WorkflowStep { s := jobStep("a"); s.Retries = 99; return s }()}, "retries"},
	} {
		t.Run(name, func(t *testing.T) {
			c := mlClient(newWF("wf", tc.steps...))
			r := newWFReconciler(c, newClock())
			reconcileOnce(t, r, "ns", "wf")
			w := getWF(t, c, "wf")
			if w.Status.Phase != "Failed" || !strings.Contains(w.Status.Message, tc.want) {
				t.Errorf("phase %s message %q, want it to contain %q", w.Status.Phase, w.Status.Message, tc.want)
			}
			if w.Status.CompletionTime == nil {
				t.Error("a failed workflow has a completion time")
			}
			if n := len(jobsOf(t, c)); n != 0 {
				t.Errorf("an invalid workflow created %d jobs", n)
			}
		})
	}
}

func TestWorkflow_DAGOrderAndStatusFields(t *testing.T) {
	c := mlClient(newWF("wf", jobStep("prep"), jobStep("train-a", "prep"), jobStep("train-b", "prep"), jobStep("eval", "train-a", "train-b")))
	clk := newClock()
	r := newWFReconciler(c, clk)

	reconcileOnce(t, r, "ns", "wf")
	if got := jobsOf(t, c); len(got) != 1 || !got["wf-prep"] {
		t.Fatalf("only the root step may start first: %v", got)
	}
	w := getWF(t, c, "wf")
	if w.Status.Phase != "Running" || w.Status.StartTime == nil {
		t.Errorf("phase %s start %v", w.Status.Phase, w.Status.StartTime)
	}
	if s := stepOf(w, "prep"); s.Phase != "Running" || s.JobName != "wf-prep" || s.StartTime == nil {
		t.Errorf("prep: %+v", s)
	}
	if stepOf(w, "eval").Phase != "Pending" {
		t.Error("eval must wait")
	}
	job := &gryviav1.GryviaAIJob{}
	mustGet(t, c, "ns", "wf-prep", job)
	if ref := metav1.GetControllerOf(job); ref == nil || ref.Kind != "GryviaWorkflow" {
		t.Errorf("job owner: %v", job.OwnerReferences)
	}

	// Reconciling again without progress changes nothing.
	before := getWF(t, c, "wf").ResourceVersion
	reconcileOnce(t, r, "ns", "wf")
	if len(jobsOf(t, c)) != 1 || getWF(t, c, "wf").ResourceVersion != before {
		t.Error("an idempotent reconcile created or rewrote something")
	}

	clk.Add(10 * time.Second)
	setJobPhase(t, c, "wf-prep", PhaseSucceeded, "")
	reconcileOnce(t, r, "ns", "wf")
	if got := jobsOf(t, c); len(got) != 3 || !got["wf-train-a"] || !got["wf-train-b"] || got["wf-eval"] {
		t.Fatalf("both trainings start together, eval waits: %v", got)
	}
	w = getWF(t, c, "wf")
	if s := stepOf(w, "prep"); s.Phase != "Succeeded" || s.CompletionTime == nil {
		t.Errorf("prep: %+v", s)
	}

	setJobPhase(t, c, "wf-train-a", PhaseSucceeded, "")
	reconcileOnce(t, r, "ns", "wf")
	if jobsOf(t, c)["wf-eval"] {
		t.Fatal("eval needs both trainings")
	}
	setJobPhase(t, c, "wf-train-b", PhaseSucceeded, "")
	reconcileOnce(t, r, "ns", "wf")
	if !jobsOf(t, c)["wf-eval"] {
		t.Fatal("eval must start once both trainings are done")
	}
	setJobPhase(t, c, "wf-eval", PhaseSucceeded, "")
	reconcileOnce(t, r, "ns", "wf")

	w = getWF(t, c, "wf")
	st := statusJSON(t, w)
	wantKeys(t, st, "phase", "startTime", "completionTime", "stepStatuses")
	if st["phase"] != "Succeeded" {
		t.Errorf("phase %v (%s)", st["phase"], w.Status.Message)
	}
	steps := st["stepStatuses"].([]interface{})
	for _, s := range steps {
		m := s.(map[string]interface{})
		if m["phase"] != "Succeeded" || m["jobName"] == nil || m["startTime"] == nil || m["completionTime"] == nil {
			t.Errorf("step status: %v", m)
		}
	}
	// A finished workflow is left alone.
	rv := w.ResourceVersion
	reconcileOnce(t, r, "ns", "wf")
	if getWF(t, c, "wf").ResourceVersion != rv {
		t.Error("a terminal workflow was modified")
	}
}

func TestWorkflow_FailurePropagation(t *testing.T) {
	c := mlClient(newWF("wf", jobStep("a"), jobStep("b", "a"), jobStep("c"), jobStep("d", "b", "c"), jobStep("e", "d")))
	clk := newClock()
	r := newWFReconciler(c, clk)
	reconcileOnce(t, r, "ns", "wf")
	if got := jobsOf(t, c); len(got) != 2 || !got["wf-a"] || !got["wf-c"] {
		t.Fatalf("independent roots start together: %v", got)
	}
	setJobPhase(t, c, "wf-a", PhaseFailed, "OOMKilled")
	reconcileOnce(t, r, "ns", "wf")
	w := getWF(t, c, "wf")
	if s := stepOf(w, "a"); s.Phase != "Failed" || s.Message != "OOMKilled" {
		t.Errorf("a: %+v", s)
	}
	if s := stepOf(w, "b"); s.Phase != "Skipped" || !strings.Contains(s.Message, "dependency") {
		t.Errorf("b: %+v", s)
	}
	if w.Status.Phase != "Running" {
		t.Errorf("c is still running: phase %s", w.Status.Phase)
	}
	if jobsOf(t, c)["wf-b"] {
		t.Error("a skipped step must not run")
	}

	setJobPhase(t, c, "wf-c", PhaseSucceeded, "")
	reconcileOnce(t, r, "ns", "wf")
	w = getWF(t, c, "wf")
	if stepOf(w, "d").Phase != "Skipped" || stepOf(w, "e").Phase != "Skipped" {
		t.Errorf("skips must cascade: d=%+v e=%+v", stepOf(w, "d"), stepOf(w, "e"))
	}
	if w.Status.Phase != "Failed" || !strings.Contains(w.Status.Message, "a") || w.Status.CompletionTime == nil {
		t.Errorf("phase %s message %q", w.Status.Phase, w.Status.Message)
	}
	if len(jobsOf(t, c)) != 2 {
		t.Errorf("jobs: %v", jobsOf(t, c))
	}
}

func TestWorkflow_RetriesWithBackoff(t *testing.T) {
	step := jobStep("a")
	step.Retries = 2
	step.RetryBackoffSeconds = 30
	c := mlClient(newWF("wf", step))
	clk := newClock()
	r := newWFReconciler(c, clk)
	reconcileOnce(t, r, "ns", "wf")

	setJobPhase(t, c, "wf-a", PhaseFailed, "boom")
	reconcileOnce(t, r, "ns", "wf")
	w := getWF(t, c, "wf")
	if s := stepOf(w, "a"); s.Phase != "Pending" || s.RetriesAttempted != 1 || !strings.Contains(s.Message, "Retrying (1/2)") {
		t.Fatalf("after first failure: %+v", s)
	}
	if w.Status.Phase != "Running" {
		t.Errorf("phase %s", w.Status.Phase)
	}
	if jobsOf(t, c)["wf-a-r1"] {
		t.Fatal("must wait for the backoff")
	}

	clk.Add(31 * time.Second)
	reconcileOnce(t, r, "ns", "wf")
	if !jobsOf(t, c)["wf-a-r1"] || !jobsOf(t, c)["wf-a"] {
		t.Fatalf("attempt 2 uses a new job and keeps the old one: %v", jobsOf(t, c))
	}
	if s := stepOf(getWF(t, c, "wf"), "a"); s.Phase != "Running" || s.JobName != "wf-a-r1" {
		t.Errorf("attempt 2: %+v", s)
	}
	setJobPhase(t, c, "wf-a-r1", PhaseFailed, "boom again")
	reconcileOnce(t, r, "ns", "wf")
	clk.Add(31 * time.Second)
	reconcileOnce(t, r, "ns", "wf")
	if !jobsOf(t, c)["wf-a-r2"] {
		t.Fatal("attempt 3 missing")
	}
	setJobPhase(t, c, "wf-a-r2", PhaseFailed, "still broken")
	reconcileOnce(t, r, "ns", "wf")
	w = getWF(t, c, "wf")
	if s := stepOf(w, "a"); s.Phase != "Failed" || s.RetriesAttempted != 2 || s.Message != "still broken" {
		t.Errorf("retries exhausted: %+v", s)
	}
	if w.Status.Phase != "Failed" {
		t.Errorf("workflow phase %s", w.Status.Phase)
	}
	if len(jobsOf(t, c)) != 3 {
		t.Errorf("exactly one job per attempt: %v", jobsOf(t, c))
	}
}

func TestWorkflow_RetrySucceeds(t *testing.T) {
	step := jobStep("a")
	step.Retries = 1
	c := mlClient(newWF("wf", step))
	r := newWFReconciler(c, newClock())
	reconcileOnce(t, r, "ns", "wf")
	setJobPhase(t, c, "wf-a", PhaseFailed, "flaky")
	reconcileOnce(t, r, "ns", "wf") // no backoff: the retry starts in the same pass
	setJobPhase(t, c, "wf-a-r1", PhaseSucceeded, "")
	reconcileOnce(t, r, "ns", "wf")
	if w := getWF(t, c, "wf"); w.Status.Phase != "Succeeded" || stepOf(w, "a").RetriesAttempted != 1 {
		t.Errorf("phase %s step %+v", w.Status.Phase, stepOf(w, "a"))
	}
}

func TestWorkflow_StepTimeout(t *testing.T) {
	step := jobStep("a")
	step.TimeoutSeconds = 60
	c := mlClient(newWF("wf", step))
	clk := newClock()
	r := newWFReconciler(c, clk)
	res := reconcileOnce(t, r, "ns", "wf")
	if res.RequeueAfter == 0 || res.RequeueAfter > 61*time.Second {
		t.Errorf("must requeue for the timeout: %v", res.RequeueAfter)
	}
	clk.Add(30 * time.Second)
	reconcileOnce(t, r, "ns", "wf")
	if getWF(t, c, "wf").Status.Phase != "Running" {
		t.Fatal("not timed out yet")
	}
	clk.Add(40 * time.Second)
	reconcileOnce(t, r, "ns", "wf")
	w := getWF(t, c, "wf")
	if s := stepOf(w, "a"); s.Phase != "Failed" || !strings.Contains(s.Message, "timed out") {
		t.Errorf("step: %+v", s)
	}
	if jobsOf(t, c)["wf-a"] {
		t.Error("the timed-out job must be deleted")
	}
	if w.Status.Phase != "Failed" {
		t.Errorf("phase %s", w.Status.Phase)
	}
}

func TestWorkflow_MissingJobFailsStep(t *testing.T) {
	c := mlClient(newWF("wf", jobStep("a")))
	r := newWFReconciler(c, newClock())
	reconcileOnce(t, r, "ns", "wf")
	j := &gryviav1.GryviaAIJob{ObjectMeta: objMeta("ns", "wf-a")}
	if err := c.Delete(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "ns", "wf")
	if s := stepOf(getWF(t, c, "wf"), "a"); s.Phase != "Failed" || !strings.Contains(s.Message, "not found") {
		t.Errorf("step: %+v", s)
	}
}

func TestWorkflow_ParallelismIsCapped(t *testing.T) {
	steps := []gryviav1.WorkflowStep{}
	for i := 0; i < 5; i++ {
		steps = append(steps, jobStep(fmt.Sprintf("s%d", i)))
	}
	c := mlClient(newWF("wf", steps...))
	r := newWFReconciler(c, newClock())
	r.MaxParallelSteps = 2
	reconcileOnce(t, r, "ns", "wf")
	if n := len(jobsOf(t, c)); n != 2 {
		t.Fatalf("jobs = %d, want 2", n)
	}
	setJobPhase(t, c, "wf-s0", PhaseSucceeded, "")
	reconcileOnce(t, r, "ns", "wf")
	if n := len(jobsOf(t, c)); n != 3 {
		t.Errorf("a finished step frees a slot: jobs = %d", n)
	}
	// Default cap applies when unset.
	if got := (&GryviaWorkflowReconciler{}).MaxParallelSteps; got != 0 {
		t.Fatal("zero value expected")
	}
}

func TestWorkflow_ParametersAndEnv(t *testing.T) {
	step := jobStep("a")
	step.JobTemplate.Env = []corev1.EnvVar{{Name: "KEEP", Value: "mine"}, {Name: "LR", Value: "template"}}
	w := newWF("wf", step)
	w.Spec.Parameters = map[string]string{"LR": "0.1", "EPOCHS": "3"}
	c := mlClient(w)
	r := newWFReconciler(c, newClock())
	reconcileOnce(t, r, "ns", "wf")
	job := &gryviav1.GryviaAIJob{}
	mustGet(t, c, "ns", "wf-a", job)
	env := map[string]string{}
	for _, e := range job.Spec.Env {
		env[e.Name] = e.Value
	}
	if env["KEEP"] != "mine" || env["LR"] != "template" || env["EPOCHS"] != "3" || env["WORKFLOW_NAME"] != "wf" || env["WORKFLOW_STEP"] != "a" {
		t.Errorf("env: %v", env)
	}
	if job.Spec.Image != "busybox:1" || job.Spec.Type != "training" {
		t.Errorf("template not copied: %+v", job.Spec)
	}
}

func scriptStep(name string, deps ...string) gryviav1.WorkflowStep {
	return gryviav1.WorkflowStep{
		Name: name, Type: gryviav1.StepTypeScript, DependsOn: deps,
		Script: &gryviav1.ScriptStep{Image: "busybox:1", Command: []string{"sh", "-c"}, Args: []string{"echo hi"}},
	}
}

func TestWorkflow_ScriptStepRunsInAHardenedPod(t *testing.T) {
	s := scriptStep("hello")
	s.TimeoutSeconds = 120
	w := newWF("wf", s, jobStep("after", "hello"))
	w.Spec.Parameters = map[string]string{"X": "1"}
	c := mlClient(w)
	r := newWFReconciler(c, newClock())
	reconcileOnce(t, r, "ns", "wf")

	pod := &corev1.Pod{}
	mustGet(t, c, "ns", "wf-hello", pod)
	if ref := metav1.GetControllerOf(pod); ref == nil || ref.Kind != "GryviaWorkflow" {
		t.Errorf("owner: %v", pod.OwnerReferences)
	}
	psc, csc := pod.Spec.SecurityContext, pod.Spec.Containers[0].SecurityContext
	if psc == nil || !*psc.RunAsNonRoot || psc.RunAsUser == nil || *psc.RunAsUser == 0 {
		t.Errorf("pod security context: %+v", psc)
	}
	if csc == nil || *csc.AllowPrivilegeEscalation || *csc.Privileged || len(csc.Capabilities.Drop) != 1 || csc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("container security context: %+v", csc)
	}
	if *pod.Spec.AutomountServiceAccountToken {
		t.Error("script pods get no service-account token")
	}
	c0 := pod.Spec.Containers[0]
	if c0.Resources.Limits.Cpu().IsZero() || c0.Resources.Limits.Memory().IsZero() {
		t.Error("script pods have resource limits")
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever || *pod.Spec.ActiveDeadlineSeconds != 120 {
		t.Errorf("restart %s deadline %v", pod.Spec.RestartPolicy, pod.Spec.ActiveDeadlineSeconds)
	}
	if c0.Env[0].Name != "X" {
		t.Errorf("env: %v", c0.Env)
	}
	if got := jobsOf(t, c); len(got) != 0 {
		t.Errorf("a script step must not create an AIJob: %v", got)
	}

	pod.Status.Phase = corev1.PodSucceeded
	if err := c.Status().Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "ns", "wf")
	if !jobsOf(t, c)["wf-after"] {
		t.Error("the dependent job step must start after the script")
	}
	if got := stepOf(getWF(t, c, "wf"), "hello"); got.Phase != "Succeeded" {
		t.Errorf("script step: %+v", got)
	}
}

func TestWorkflow_ScriptFailure(t *testing.T) {
	c := mlClient(newWF("wf", scriptStep("s")))
	r := newWFReconciler(c, newClock())
	reconcileOnce(t, r, "ns", "wf")
	pod := &corev1.Pod{}
	mustGet(t, c, "ns", "wf-s", pod)
	pod.Status.Phase = corev1.PodFailed
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 3, Reason: "Error"}}}}
	if err := c.Status().Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "ns", "wf")
	w := getWF(t, c, "wf")
	if s := stepOf(w, "s"); s.Phase != "Failed" || !strings.Contains(s.Message, "code 3") {
		t.Errorf("step: %+v", s)
	}
	if w.Status.Phase != "Failed" {
		t.Errorf("phase %s", w.Status.Phase)
	}
}

func TestWorkflow_WebhookSteps(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		got = append(got, req.Method+" "+req.URL.Path+" "+req.Header.Get("X-Test"))
		if req.URL.Path == "/bad" {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(204)
	}))
	defer srv.Close()
	hook := func(name, path string, deps ...string) gryviav1.WorkflowStep {
		return gryviav1.WorkflowStep{Name: name, Type: gryviav1.StepTypeWebhook, DependsOn: deps,
			Webhook: &gryviav1.WebhookStep{URL: srv.URL + path, Method: "PUT", Headers: map[string]string{"X-Test": "yes"}, Body: "{}"}}
	}

	t.Run("disabled by default", func(t *testing.T) {
		c := mlClient(newWF("wf", hook("h", "/ok")))
		r := newWFReconciler(c, newClock())
		reconcileOnce(t, r, "ns", "wf")
		w := getWF(t, c, "wf")
		if s := stepOf(w, "h"); s.Phase != "Failed" || !strings.Contains(s.Message, "disabled") || w.Status.Phase != "Failed" {
			t.Errorf("step %+v phase %s", s, w.Status.Phase)
		}
		if len(got) != 0 {
			t.Error("no request may be sent while webhooks are disabled")
		}
	})
	t.Run("enabled", func(t *testing.T) {
		c := mlClient(newWF("wf", hook("h", "/ok"), hook("g", "/bad", "h")))
		r := newWFReconciler(c, newClock())
		r.AllowWebhooks = true
		reconcileOnce(t, r, "ns", "wf")
		w := getWF(t, c, "wf")
		if stepOf(w, "h").Phase != "Succeeded" || stepOf(w, "g").Phase != "Failed" || w.Status.Phase != "Failed" {
			t.Errorf("steps: %+v phase %s", w.Status.StepStatuses, w.Status.Phase)
		}
		if len(got) != 2 || got[0] != "PUT /ok yes" {
			t.Errorf("requests: %v", got)
		}
	})
}

func TestWorkflow_Conditions(t *testing.T) {
	handler := jobStep("notify", "a")
	handler.Condition = "steps.a.status == 'Failed'"
	onlyOK := jobStep("publish", "a")
	onlyOK.Condition = "steps.a.status != 'Failed' && steps.a.status == 'Succeeded'"

	t.Run("a fails: handler runs, publish is skipped", func(t *testing.T) {
		c := mlClient(newWF("wf", jobStep("a"), handler, onlyOK))
		r := newWFReconciler(c, newClock())
		reconcileOnce(t, r, "ns", "wf")
		setJobPhase(t, c, "wf-a", PhaseFailed, "x")
		reconcileOnce(t, r, "ns", "wf")
		w := getWF(t, c, "wf")
		if stepOf(w, "notify").Phase != "Running" || stepOf(w, "publish").Phase != "Skipped" {
			t.Fatalf("steps: %+v", w.Status.StepStatuses)
		}
		setJobPhase(t, c, "wf-notify", PhaseSucceeded, "")
		reconcileOnce(t, r, "ns", "wf")
		if w := getWF(t, c, "wf"); w.Status.Phase != "Failed" {
			t.Errorf("a failed step still fails the workflow: %s", w.Status.Phase)
		}
	})
	t.Run("a succeeds: handler skipped, publish runs", func(t *testing.T) {
		c := mlClient(newWF("wf", jobStep("a"), handler, onlyOK))
		r := newWFReconciler(c, newClock())
		reconcileOnce(t, r, "ns", "wf")
		setJobPhase(t, c, "wf-a", PhaseSucceeded, "")
		reconcileOnce(t, r, "ns", "wf")
		w := getWF(t, c, "wf")
		if stepOf(w, "notify").Phase != "Skipped" || stepOf(w, "publish").Phase != "Running" {
			t.Fatalf("steps: %+v", w.Status.StepStatuses)
		}
		setJobPhase(t, c, "wf-publish", PhaseSucceeded, "")
		reconcileOnce(t, r, "ns", "wf")
		if w := getWF(t, c, "wf"); w.Status.Phase != "Succeeded" {
			t.Errorf("phase %s: %s", w.Status.Phase, w.Status.Message)
		}
	})
}

func TestWorkflow_ExistingForeignJobFailsTheStep(t *testing.T) {
	foreign := &gryviav1.GryviaAIJob{ObjectMeta: objMeta("ns", "wf-a"), Spec: gryviav1.GryviaAIJobSpec{Type: "training", Image: "x"}}
	c := mlClient(newWF("wf", jobStep("a")), foreign)
	r := newWFReconciler(c, newClock())
	reconcileOnce(t, r, "ns", "wf")
	if s := stepOf(getWF(t, c, "wf"), "a"); s.Phase != "Failed" || !strings.Contains(s.Message, "not owned") {
		t.Errorf("step: %+v", s)
	}
}

func TestWorkflow_NotFoundAndDeleted(t *testing.T) {
	c := mlClient()
	r := newWFReconciler(c, newClock())
	reconcileOnce(t, r, "ns", "nothing")
	w := newWF("wf", jobStep("a"))
	now := metav1.Now()
	w.DeletionTimestamp = &now
	w.Finalizers = []string{"hold"}
	c = mlClient(w)
	r = newWFReconciler(c, newClock())
	reconcileOnce(t, r, "ns", "wf")
	if len(jobsOf(t, c)) != 0 {
		t.Error("nothing to create for a deleted workflow")
	}
}
