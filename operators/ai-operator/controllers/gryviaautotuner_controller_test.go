// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

func newTunerReconciler(c client.Client, clk *fakeClock) *GryviaAutoTunerReconciler {
	return &GryviaAutoTunerReconciler{Client: c, Scheme: mlScheme(), Log: ctrl.Log.WithName("test"), Clock: clk.Now}
}

func f64(v float64) *float64 { return &v }

func newTuner(name string, mutate ...func(*gryviav1.GryviaAutoTuner)) *gryviav1.GryviaAutoTuner {
	a := &gryviav1.GryviaAutoTuner{
		ObjectMeta: objMeta("ns", name),
		Spec: gryviav1.GryviaAutoTunerSpec{
			SearchAlgorithm: gryviav1.SearchAlgorithmRandom,
			ParameterSpace: []gryviav1.ParameterSpec{
				{Name: "lr", Type: gryviav1.ParameterTypeFloat, Min: f64(0.001), Max: f64(0.1)},
				{Name: "opt", Type: gryviav1.ParameterTypeCategorical, Values: []string{"adam", "sgd"}},
			},
			Objective:   gryviav1.ObjectiveSpec{MetricName: "accuracy", Direction: gryviav1.ObjectiveMaximize},
			MaxTrials:   5,
			Parallelism: 2,
			JobTemplate: gryviav1.GryviaAIJobSpec{Type: "training", Image: "busybox:1", Command: []string{"true"}},
		},
	}
	for _, m := range mutate {
		m(a)
	}
	return a
}

func getTuner(t *testing.T, c client.Client, name string) *gryviav1.GryviaAutoTuner {
	t.Helper()
	a := &gryviav1.GryviaAutoTuner{}
	mustGet(t, c, "ns", name, a)
	return a
}

// finishTrial completes a trial job, reporting the metric through the documented annotation.
func finishTrial(t *testing.T, c client.Client, job, phase string, metric *float64) {
	t.Helper()
	j := &gryviav1.GryviaAIJob{}
	mustGet(t, c, "ns", job, j)
	if metric != nil {
		if j.Annotations == nil {
			j.Annotations = map[string]string{}
		}
		j.Annotations[metricAnnotation("accuracy")] = fmt.Sprintf("%g", *metric)
		if err := c.Update(context.Background(), j); err != nil {
			t.Fatal(err)
		}
	}
	setJobPhase(t, c, job, phase, "")
}

func TestTuner_LaunchesUpToParallelismWithEnv(t *testing.T) {
	c := mlClient(newTuner("hp"))
	r := newTunerReconciler(c, newClock())
	res := reconcileOnce(t, r, "ns", "hp")
	if res.RequeueAfter == 0 {
		t.Error("expected a periodic requeue while tuning")
	}
	jobs := jobsOf(t, c)
	if len(jobs) != 2 || !jobs["hp-trial-0"] || !jobs["hp-trial-1"] {
		t.Fatalf("jobs: %v", jobs)
	}
	job := &gryviav1.GryviaAIJob{}
	mustGet(t, c, "ns", "hp-trial-0", job)
	if ref := metav1.GetControllerOf(job); ref == nil || ref.Kind != "GryviaAutoTuner" {
		t.Errorf("owner: %v", job.OwnerReferences)
	}
	env := map[string]string{}
	for _, e := range job.Spec.Env {
		env[e.Name] = e.Value
	}
	if env["HP_lr"] == "" || env["HP_opt"] == "" || env["TRIAL_NAME"] != "hp-trial-0" || env["TUNER_NAME"] != "hp" {
		t.Errorf("env: %v", env)
	}
	if job.Spec.Image != "busybox:1" || job.Labels["gryvia.io/tuner"] != "hp" {
		t.Errorf("job: %+v %v", job.Spec, job.Labels)
	}

	a := getTuner(t, c, "hp")
	if a.Status.Phase != "Running" || a.Status.TrialsRunning != 2 || len(a.Status.Trials) != 2 || a.Status.StartTime == nil {
		t.Errorf("status: %+v", a.Status)
	}
	for _, tr := range a.Status.Trials {
		if tr.Phase != gryviav1.TrialPhaseRunning || tr.JobName == "" || len(tr.Parameters) != 2 {
			t.Errorf("trial: %+v", tr)
		}
	}

	// Idempotent while nothing finishes.
	rv := a.ResourceVersion
	reconcileOnce(t, r, "ns", "hp")
	if len(jobsOf(t, c)) != 2 || getTuner(t, c, "hp").ResourceVersion != rv {
		t.Error("an idempotent reconcile created or rewrote something")
	}
}

func TestTuner_FullRunSelectsBestTrialAndStatusFields(t *testing.T) {
	c := mlClient(newTuner("hp", func(a *gryviav1.GryviaAutoTuner) { a.Spec.MaxTrials = 3 }))
	r := newTunerReconciler(c, newClock())
	reconcileOnce(t, r, "ns", "hp")
	finishTrial(t, c, "hp-trial-0", PhaseSucceeded, f64(0.71))
	finishTrial(t, c, "hp-trial-1", PhaseSucceeded, f64(0.93))
	reconcileOnce(t, r, "ns", "hp")

	// Only maxTrials in total: the third trial is launched, never a fourth.
	jobs := jobsOf(t, c)
	if len(jobs) != 3 || !jobs["hp-trial-2"] {
		t.Fatalf("jobs: %v", jobs)
	}
	a := getTuner(t, c, "hp")
	if a.Status.TrialsCompleted != 2 || a.Status.TrialsRunning != 1 || a.Status.BestTrial == nil || a.Status.BestTrial.Name != "hp-trial-1" {
		t.Errorf("mid-run status: completed %d running %d best %+v", a.Status.TrialsCompleted, a.Status.TrialsRunning, a.Status.BestTrial)
	}

	finishTrial(t, c, "hp-trial-2", PhaseSucceeded, f64(0.5))
	reconcileOnce(t, r, "ns", "hp")
	if len(jobsOf(t, c)) != 3 {
		t.Errorf("jobs: %v", jobsOf(t, c))
	}
	a = getTuner(t, c, "hp")
	st := statusJSON(t, a)
	// The fields the gateway's tuner to_ui and trials endpoint read.
	wantKeys(t, st, "phase", "trialsCompleted", "bestTrial", "trials", "completionTime")
	if st["phase"] != "Succeeded" || st["trialsCompleted"].(float64) != 3 {
		t.Errorf("status: %v", st)
	}
	best := st["bestTrial"].(map[string]interface{})
	if best["name"] != "hp-trial-1" || best["metricValue"].(float64) != 0.93 {
		t.Errorf("bestTrial: %v", best)
	}
	for _, x := range st["trials"].([]interface{}) {
		m := x.(map[string]interface{})
		if m["name"] == nil || m["parameters"] == nil || m["phase"] != "Succeeded" || m["metricValue"] == nil || m["startTime"] == nil || m["completionTime"] == nil {
			t.Errorf("trial: %v", m)
		}
	}
	// A finished tuner is left alone.
	rv := a.ResourceVersion
	reconcileOnce(t, r, "ns", "hp")
	if getTuner(t, c, "hp").ResourceVersion != rv {
		t.Error("a terminal tuner was modified")
	}
}

func TestTuner_MinimizeAndLossFromJobStatus(t *testing.T) {
	c := mlClient(newTuner("hp", func(a *gryviav1.GryviaAutoTuner) {
		a.Spec.Objective = gryviav1.ObjectiveSpec{MetricName: "loss", Direction: gryviav1.ObjectiveMinimize}
		a.Spec.MaxTrials = 2
	}))
	r := newTunerReconciler(c, newClock())
	reconcileOnce(t, r, "ns", "hp")
	for i, loss := range []float64{0.8, 0.3} {
		j := &gryviav1.GryviaAIJob{}
		name := fmt.Sprintf("hp-trial-%d", i)
		mustGet(t, c, "ns", name, j)
		j.Status.Phase = PhaseSucceeded
		j.Status.Metrics = &gryviav1.JobMetrics{Loss: loss}
		if err := c.Status().Update(context.Background(), j); err != nil {
			t.Fatal(err)
		}
	}
	reconcileOnce(t, r, "ns", "hp")
	a := getTuner(t, c, "hp")
	if a.Status.BestTrial == nil || a.Status.BestTrial.Name != "hp-trial-1" || *a.Status.BestTrial.MetricValue != 0.3 {
		t.Errorf("best: %+v", a.Status.BestTrial)
	}
}

func TestTuner_NoMetricReportedIsSaid(t *testing.T) {
	c := mlClient(newTuner("hp", func(a *gryviav1.GryviaAutoTuner) { a.Spec.MaxTrials = 1 }))
	r := newTunerReconciler(c, newClock())
	reconcileOnce(t, r, "ns", "hp")
	setJobPhase(t, c, "hp-trial-0", PhaseSucceeded, "")
	reconcileOnce(t, r, "ns", "hp")
	a := getTuner(t, c, "hp")
	if a.Status.Phase != "Succeeded" || a.Status.BestTrial != nil || !strings.Contains(a.Status.Message, "gryvia.io/metric-accuracy") {
		t.Errorf("status: %q %q %+v", a.Status.Phase, a.Status.Message, a.Status.BestTrial)
	}
}

func TestTuner_FailedTrialsAndAllFailed(t *testing.T) {
	c := mlClient(newTuner("hp", func(a *gryviav1.GryviaAutoTuner) { a.Spec.MaxTrials = 2 }))
	r := newTunerReconciler(c, newClock())
	reconcileOnce(t, r, "ns", "hp")
	setJobPhase(t, c, "hp-trial-0", PhaseFailed, "OOM")
	setJobPhase(t, c, "hp-trial-1", PhaseFailed, "OOM")
	reconcileOnce(t, r, "ns", "hp")
	a := getTuner(t, c, "hp")
	if a.Status.Phase != "Failed" || a.Status.TrialsFailed != 2 || !strings.Contains(a.Status.Message, "failed") {
		t.Errorf("status: %+v", a.Status)
	}
	if len(jobsOf(t, c)) != 2 {
		t.Error("failed trials count towards maxTrials: no extra jobs")
	}
}

func TestTuner_SomeFailedStillSucceeds(t *testing.T) {
	c := mlClient(newTuner("hp", func(a *gryviav1.GryviaAutoTuner) { a.Spec.MaxTrials = 2 }))
	r := newTunerReconciler(c, newClock())
	reconcileOnce(t, r, "ns", "hp")
	setJobPhase(t, c, "hp-trial-0", PhaseFailed, "OOM")
	finishTrial(t, c, "hp-trial-1", PhaseSucceeded, f64(0.6))
	reconcileOnce(t, r, "ns", "hp")
	a := getTuner(t, c, "hp")
	if a.Status.Phase != "Succeeded" || a.Status.BestTrial == nil || a.Status.TrialsFailed != 1 || a.Status.TrialsCompleted != 1 {
		t.Errorf("status: %+v", a.Status)
	}
}

func TestTuner_GridStopsWhenExhausted(t *testing.T) {
	c := mlClient(newTuner("hp", func(a *gryviav1.GryviaAutoTuner) {
		a.Spec.SearchAlgorithm = gryviav1.SearchAlgorithmGrid
		a.Spec.ParameterSpace = []gryviav1.ParameterSpec{{Name: "opt", Type: gryviav1.ParameterTypeCategorical, Values: []string{"a", "b", "c"}}}
		a.Spec.MaxTrials = 50
		a.Spec.Parallelism = 10
	}))
	r := newTunerReconciler(c, newClock())
	reconcileOnce(t, r, "ns", "hp")
	if n := len(jobsOf(t, c)); n != 3 {
		t.Fatalf("a 3-point grid runs 3 trials, got %d", n)
	}
	for i := 0; i < 3; i++ {
		finishTrial(t, c, fmt.Sprintf("hp-trial-%d", i), PhaseSucceeded, f64(float64(i)))
	}
	reconcileOnce(t, r, "ns", "hp")
	a := getTuner(t, c, "hp")
	if a.Status.Phase != "Succeeded" || !strings.Contains(a.Status.Message, "exhausted") || len(jobsOf(t, c)) != 3 {
		t.Errorf("status: %q %q jobs %d", a.Status.Phase, a.Status.Message, len(jobsOf(t, c)))
	}
}

func TestTuner_CapsAndValidation(t *testing.T) {
	t.Run("parallelism cap", func(t *testing.T) {
		c := mlClient(newTuner("hp", func(a *gryviav1.GryviaAutoTuner) { a.Spec.Parallelism = 500; a.Spec.MaxTrials = 100 }))
		r := newTunerReconciler(c, newClock())
		r.MaxParallelismCap = 3
		reconcileOnce(t, r, "ns", "hp")
		if n := len(jobsOf(t, c)); n != 3 {
			t.Errorf("jobs = %d, want the cap of 3", n)
		}
	})
	t.Run("default parallelism is 1", func(t *testing.T) {
		c := mlClient(newTuner("hp", func(a *gryviav1.GryviaAutoTuner) { a.Spec.Parallelism = 0 }))
		r := newTunerReconciler(c, newClock())
		reconcileOnce(t, r, "ns", "hp")
		if n := len(jobsOf(t, c)); n != 1 {
			t.Errorf("jobs = %d", n)
		}
	})
	for name, tc := range map[string]struct {
		mutate func(*gryviav1.GryviaAutoTuner)
		want   string
	}{
		"too many trials": {func(a *gryviav1.GryviaAutoTuner) { a.Spec.MaxTrials = 5000 }, "exceeds the operator limit"},
		"zero trials":     {func(a *gryviav1.GryviaAutoTuner) { a.Spec.MaxTrials = 0 }, "at least 1"},
		"no parameters":   {func(a *gryviav1.GryviaAutoTuner) { a.Spec.ParameterSpace = nil }, "parameterSpace is empty"},
		"no image":        {func(a *gryviav1.GryviaAutoTuner) { a.Spec.JobTemplate.Image = "" }, "image is required"},
		"no metric":       {func(a *gryviav1.GryviaAutoTuner) { a.Spec.Objective.MetricName = "" }, "metricName"},
		"bad direction":   {func(a *gryviav1.GryviaAutoTuner) { a.Spec.Objective.Direction = "sideways" }, "direction"},
		"empty categorical": {func(a *gryviav1.GryviaAutoTuner) {
			a.Spec.ParameterSpace = []gryviav1.ParameterSpec{{Name: "x", Type: gryviav1.ParameterTypeCategorical}}
		}, "needs values"},
		"float without range": {func(a *gryviav1.GryviaAutoTuner) {
			a.Spec.ParameterSpace = []gryviav1.ParameterSpec{{Name: "x", Type: gryviav1.ParameterTypeFloat}}
		}, "min and max"},
		"huge grid": {func(a *gryviav1.GryviaAutoTuner) {
			a.Spec.SearchAlgorithm = gryviav1.SearchAlgorithmGrid
			a.Spec.ParameterSpace = []gryviav1.ParameterSpec{
				{Name: "a", Type: gryviav1.ParameterTypeInt, Min: f64(0), Max: f64(1e9)},
				{Name: "b", Type: gryviav1.ParameterTypeInt, Min: f64(0), Max: f64(1e9)},
			}
		}, "combinations"},
	} {
		t.Run(name, func(t *testing.T) {
			c := mlClient(newTuner("hp", tc.mutate))
			r := newTunerReconciler(c, newClock())
			reconcileOnce(t, r, "ns", "hp")
			a := getTuner(t, c, "hp")
			if a.Status.Phase != "Failed" || !strings.Contains(a.Status.Message, tc.want) {
				t.Errorf("phase %s message %q, want %q", a.Status.Phase, a.Status.Message, tc.want)
			}
			if len(jobsOf(t, c)) != 0 {
				t.Error("an invalid tuner must not create jobs")
			}
		})
	}
}

func TestTuner_ASHAStopsLaggingTrials(t *testing.T) {
	c := mlClient(newTuner("hp", func(a *gryviav1.GryviaAutoTuner) {
		a.Spec.SearchAlgorithm = gryviav1.SearchAlgorithmASHA
		a.Spec.ASHAConfig = &gryviav1.ASHAConfig{MaxEpochs: 9, ReductionFactor: 3, MinResource: 1}
		a.Spec.Objective = gryviav1.ObjectiveSpec{MetricName: "loss", Direction: gryviav1.ObjectiveMinimize}
		a.Spec.MaxTrials = 3
		a.Spec.Parallelism = 3
	}))
	r := newTunerReconciler(c, newClock())
	reconcileOnce(t, r, "ns", "hp")
	for i, loss := range []float64{0.1, 0.5, 0.9} {
		j := &gryviav1.GryviaAIJob{}
		mustGet(t, c, "ns", fmt.Sprintf("hp-trial-%d", i), j)
		j.Status.Phase = PhaseRunning
		j.Status.Metrics = &gryviav1.JobMetrics{Epoch: 3, Loss: loss}
		if err := c.Status().Update(context.Background(), j); err != nil {
			t.Fatal(err)
		}
	}
	reconcileOnce(t, r, "ns", "hp")
	a := getTuner(t, c, "hp")
	byName := map[string]gryviav1.TrialResult{}
	for _, tr := range a.Status.Trials {
		byName[tr.Name] = tr
	}
	if byName["hp-trial-0"].Phase != gryviav1.TrialPhaseRunning {
		t.Errorf("the best trial keeps running: %+v", byName["hp-trial-0"])
	}
	for _, n := range []string{"hp-trial-1", "hp-trial-2"} {
		tr := byName[n]
		if tr.Phase != gryviav1.TrialPhaseStopped || !strings.Contains(tr.Message, "ASHA") || tr.CompletionTime == nil {
			t.Errorf("%s: %+v", n, tr)
		}
		if jobsOf(t, c)[n] {
			t.Errorf("the job of %s must be deleted", n)
		}
	}
	if !jobsOf(t, c)["hp-trial-0"] {
		t.Error("the surviving trial's job must stay")
	}
	if a.Status.TrialsRunning != 1 || a.Status.TrialsCompleted != 2 {
		t.Errorf("counts: running %d completed %d", a.Status.TrialsRunning, a.Status.TrialsCompleted)
	}
}

func TestTuner_EarlyStopping(t *testing.T) {
	c := mlClient(newTuner("hp", func(a *gryviav1.GryviaAutoTuner) {
		a.Spec.EarlyStoppingRounds = 2
		a.Spec.MaxTrials = 10
		a.Spec.Parallelism = 4
	}))
	r := newTunerReconciler(c, newClock())
	reconcileOnce(t, r, "ns", "hp")
	finishTrial(t, c, "hp-trial-0", PhaseSucceeded, f64(0.9))
	finishTrial(t, c, "hp-trial-1", PhaseSucceeded, f64(0.5))
	finishTrial(t, c, "hp-trial-2", PhaseSucceeded, f64(0.6))
	reconcileOnce(t, r, "ns", "hp")
	a := getTuner(t, c, "hp")
	if a.Status.Phase != "Succeeded" || !strings.Contains(a.Status.Message, "Early stopping") || a.Status.BestTrial.Name != "hp-trial-0" {
		t.Fatalf("status: %q %q", a.Status.Phase, a.Status.Message)
	}
	// The trial that was still running is stopped and its job removed.
	for _, tr := range a.Status.Trials {
		if tr.Phase == gryviav1.TrialPhaseRunning {
			t.Errorf("a trial is still running: %+v", tr)
		}
	}
	if jobsOf(t, c)["hp-trial-3"] {
		t.Error("the job of the stopped trial must be deleted")
	}
}

func TestTuner_LateMetricIsPickedUp(t *testing.T) {
	c := mlClient(newTuner("hp", func(a *gryviav1.GryviaAutoTuner) { a.Spec.MaxTrials = 2; a.Spec.Parallelism = 1 }))
	r := newTunerReconciler(c, newClock())
	reconcileOnce(t, r, "ns", "hp")
	setJobPhase(t, c, "hp-trial-0", PhaseSucceeded, "")
	reconcileOnce(t, r, "ns", "hp") // trial 0 done without a metric; trial 1 starts
	if a := getTuner(t, c, "hp"); a.Status.BestTrial != nil {
		t.Fatal("no metric yet")
	}
	j := &gryviav1.GryviaAIJob{}
	mustGet(t, c, "ns", "hp-trial-0", j)
	j.Annotations = map[string]string{metricAnnotation("accuracy"): "0.8"}
	if err := c.Update(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "ns", "hp")
	if a := getTuner(t, c, "hp"); a.Status.BestTrial == nil || a.Status.BestTrial.Name != "hp-trial-0" {
		t.Errorf("best: %+v", a.Status.BestTrial)
	}
}

func TestTuner_RejectedJobBecomesAFailedTrial(t *testing.T) {
	base := fake.NewClientBuilder().WithScheme(mlScheme()).WithObjects(newTuner("hp", func(a *gryviav1.GryviaAutoTuner) { a.Spec.MaxTrials = 3 })).
		WithStatusSubresource(&gryviav1.GryviaAutoTuner{}, &gryviav1.GryviaAIJob{}).
		WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*gryviav1.GryviaAIJob); ok {
				return apierrors.NewInvalid(schema.GroupKind{Group: "gryvia.io", Kind: "GryviaAIJob"}, obj.GetName(), nil)
			}
			return c.Create(ctx, obj, opts...)
		}}).Build()
	r := newTunerReconciler(base, newClock())
	r.MaxParallelismCap = 3
	for i := 0; i < 3; i++ {
		reconcileOnce(t, r, "ns", "hp")
	}
	a := getTuner(t, base, "hp")
	if a.Status.Phase != "Failed" || len(a.Status.Trials) != 3 || a.Status.TrialsFailed != 3 {
		t.Errorf("a template the API rejects must fail the trials, bounded by maxTrials: %q trials %d failed %d",
			a.Status.Phase, len(a.Status.Trials), a.Status.TrialsFailed)
	}
	if !strings.Contains(a.Status.Trials[0].Message, "cannot create") {
		t.Errorf("message: %q", a.Status.Trials[0].Message)
	}
}

func TestTuner_AdoptsItsOwnExistingTrialJob(t *testing.T) {
	tn := newTuner("hp", func(a *gryviav1.GryviaAutoTuner) { a.Spec.MaxTrials = 1; a.Spec.Parallelism = 1 })
	// A previous pass created the job but lost the status write.
	pre := &gryviav1.GryviaAIJob{
		ObjectMeta: objMeta("ns", "hp-trial-0"),
		Spec:       gryviav1.GryviaAIJobSpec{Type: "training", Image: "busybox:1"},
	}
	pre.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(tn, gryviav1.GroupVersion.WithKind("GryviaAutoTuner"))}
	c := mlClient(tn, pre)
	r := newTunerReconciler(c, newClock())
	reconcileOnce(t, r, "ns", "hp")
	a := getTuner(t, c, "hp")
	if len(a.Status.Trials) != 1 || a.Status.Trials[0].Phase != gryviav1.TrialPhaseRunning {
		t.Errorf("trials: %+v", a.Status.Trials)
	}

	// A foreign job with the trial's name is never adopted.
	foreign := &gryviav1.GryviaAIJob{ObjectMeta: objMeta("ns", "hp2-trial-0"), Spec: gryviav1.GryviaAIJobSpec{Type: "training", Image: "x"}}
	c = mlClient(newTuner("hp2", func(a *gryviav1.GryviaAutoTuner) { a.Spec.MaxTrials = 1; a.Spec.Parallelism = 1 }), foreign)
	r = newTunerReconciler(c, newClock())
	reconcileOnce(t, r, "ns", "hp2")
	a = getTuner(t, c, "hp2")
	if len(a.Status.Trials) != 1 || a.Status.Trials[0].Phase != gryviav1.TrialPhaseFailed || !strings.Contains(a.Status.Trials[0].Message, "not owned") {
		t.Errorf("trials: %+v", a.Status.Trials)
	}
}

func TestTuner_MissingTrialJobFailsTheTrial(t *testing.T) {
	c := mlClient(newTuner("hp", func(a *gryviav1.GryviaAutoTuner) { a.Spec.MaxTrials = 1; a.Spec.Parallelism = 1 }))
	r := newTunerReconciler(c, newClock())
	clk := newClock()
	r.Clock = clk.Now
	reconcileOnce(t, r, "ns", "hp")
	if err := c.Delete(context.Background(), &gryviav1.GryviaAIJob{ObjectMeta: objMeta("ns", "hp-trial-0")}); err != nil {
		t.Fatal(err)
	}
	clk.Add(time.Minute)
	reconcileOnce(t, r, "ns", "hp")
	a := getTuner(t, c, "hp")
	if a.Status.Trials[0].Phase != gryviav1.TrialPhaseFailed || a.Status.Phase != "Failed" {
		t.Errorf("status: %+v", a.Status)
	}
}

func TestTuner_MetricAnnotationKey(t *testing.T) {
	if got := metricAnnotation("val/acc"); got != "gryvia.io/metric-val_acc" {
		t.Errorf("got %q", got)
	}
}

func TestTuner_NotFoundAndDeleted(t *testing.T) {
	c := mlClient()
	r := newTunerReconciler(c, newClock())
	reconcileOnce(t, r, "ns", "nothing")
}
