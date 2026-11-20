// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controllers

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/ai-operator/pkg/tuner"
)

const (
	// Condition types for auto-tuner
	ConditionTrialsRunning = "TrialsRunning"
	ConditionBestFound     = "BestTrialFound"

	// Defaults for the caps below (used when the reconciler fields are zero).
	DefaultMaxTrialsCap      = 1000
	DefaultMaxParallelismCap = 32
	maxGridCombinations      = 100000
)

var metricKeyUnsafe = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

// metricAnnotation is the annotation on a trial's GryviaAIJob that carries the trial's result for the objective
// metric, for example gryvia.io/metric-accuracy: "0.93". Whatever runs the trial (or an operator watching it)
// sets it; the tuner reads it when the trial job succeeds. For an objective named "loss" the job's
// status.metrics.loss is used when the annotation is absent.
func metricAnnotation(metric string) string {
	return "gryvia.io/metric-" + metricKeyUnsafe.ReplaceAllString(metric, "_")
}

// GryviaAutoTunerReconciler reconciles a GryviaAutoTuner object.
//
// Every trial is a child GryviaAIJob "<tuner>-trial-<n>" owned by the tuner, created from spec.jobTemplate with the
// trial's hyperparameters as HP_<name> environment variables. At most min(spec.parallelism, MaxParallelismCap)
// trials run at once and at most spec.maxTrials (up to MaxTrialsCap) are ever created. The trials need the AIJob
// controller to actually run the workload.
type GryviaAutoTunerReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Log    logr.Logger

	// MaxTrialsCap rejects a tuner asking for more trials (DefaultMaxTrialsCap when 0): every trial is a status
	// entry and an object, so the count is bounded.
	MaxTrialsCap int32
	// MaxParallelismCap lowers spec.parallelism (DefaultMaxParallelismCap when 0).
	MaxParallelismCap int32
	// Clock returns the current time (time.Now when nil); tests move it.
	Clock func() time.Time
}

//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaautotuners,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaautotuners/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaautotuners/finalizers,verbs=update
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaaijobs,verbs=get;list;watch;create;update;patch;delete

func (r *GryviaAutoTunerReconciler) maxTrials() int32 {
	if r.MaxTrialsCap > 0 {
		return r.MaxTrialsCap
	}
	return DefaultMaxTrialsCap
}

func (r *GryviaAutoTunerReconciler) maxParallelism() int32 {
	if r.MaxParallelismCap > 0 {
		return r.MaxParallelismCap
	}
	return DefaultMaxParallelismCap
}

// Reconcile drives one GryviaAutoTuner.
func (r *GryviaAutoTunerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("gryviaautotuner", req.NamespacedName)

	at := &gryviav1.GryviaAutoTuner{}
	if err := r.Get(ctx, req.NamespacedName, at); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !at.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil // trial jobs are garbage-collected through their owner reference
	}
	if at.Status.Phase == PhaseSucceeded || at.Status.Phase == PhaseFailed {
		return ctrl.Result{}, nil
	}

	orig := at.DeepCopy()
	res, err := r.reconcileTuner(ctx, at)
	if perr := patchStatus(ctx, r.Client, at, orig); perr != nil {
		log.Error(perr, "Failed to patch auto-tuner status")
		if err == nil {
			err = perr
		}
	}
	return res, err
}

func (r *GryviaAutoTunerReconciler) validate(at *gryviav1.GryviaAutoTuner) error {
	s := &at.Spec
	if s.MaxTrials < 1 {
		return fmt.Errorf("spec.maxTrials must be at least 1")
	}
	if s.MaxTrials > r.maxTrials() {
		return fmt.Errorf("spec.maxTrials %d exceeds the operator limit of %d", s.MaxTrials, r.maxTrials())
	}
	if len(s.ParameterSpace) == 0 {
		return fmt.Errorf("spec.parameterSpace is empty")
	}
	for _, p := range s.ParameterSpace {
		if p.Name == "" {
			return fmt.Errorf("a parameter has no name")
		}
		if p.Type != gryviav1.ParameterTypeCategorical && (len(p.Values) == 0 && (p.Min == nil || p.Max == nil)) {
			return fmt.Errorf("parameter %q needs min and max (or values)", p.Name)
		}
		if p.Type == gryviav1.ParameterTypeCategorical && len(p.Values) == 0 {
			return fmt.Errorf("parameter %q needs values", p.Name)
		}
	}
	if s.Objective.MetricName == "" {
		return fmt.Errorf("spec.objective.metricName is required")
	}
	if s.Objective.Direction != gryviav1.ObjectiveMinimize && s.Objective.Direction != gryviav1.ObjectiveMaximize {
		return fmt.Errorf("spec.objective.direction must be minimize or maximize")
	}
	if s.JobTemplate.Image == "" {
		return fmt.Errorf("spec.jobTemplate.image is required")
	}
	if s.SearchAlgorithm == gryviav1.SearchAlgorithmGrid {
		if n := tuner.GridSize(s.ParameterSpace, maxGridCombinations); n > maxGridCombinations {
			return fmt.Errorf("the grid has more than %d combinations", maxGridCombinations)
		}
	}
	return nil
}

func (r *GryviaAutoTunerReconciler) finish(at *gryviav1.GryviaAutoTuner, phase, msg string, now time.Time) {
	at.Status.Phase = phase
	at.Status.Message = msg
	t := metav1.NewTime(now)
	at.Status.CompletionTime = &t
}

func (r *GryviaAutoTunerReconciler) reconcileTuner(ctx context.Context, at *gryviav1.GryviaAutoTuner) (ctrl.Result, error) {
	log := r.Log.WithValues("gryviaautotuner", at.Name)
	now := clock(r.Clock)

	if at.Status.StartTime == nil {
		t := metav1.NewTime(now)
		at.Status.StartTime = &t
	}
	if at.Status.Phase == "" {
		at.Status.Phase = PhasePending
	}
	if err := r.validate(at); err != nil {
		r.finish(at, PhaseFailed, fmt.Sprintf("Invalid tuner: %v", err), now)
		return ctrl.Result{}, nil
	}

	if err := r.syncTrials(ctx, at, now); err != nil {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, err
	}

	// ASHA: stop trials that fall behind at their rung.
	if at.Spec.SearchAlgorithm == gryviav1.SearchAlgorithmASHA && at.Spec.ASHAConfig != nil {
		if err := r.applyASHAEarlyStopping(ctx, at, now); err != nil {
			log.Error(err, "Failed to apply ASHA early stopping")
		}
	}
	r.updateCounts(at)
	r.updateBestTrial(at)

	// Early stopping: no improvement for N trials after the best one.
	if at.Spec.EarlyStoppingRounds > 0 && r.shouldStopEarly(at) {
		if err := r.stopRunning(ctx, at, "Stopped: early stopping", now); err != nil {
			return ctrl.Result{RequeueAfter: 15 * time.Second}, err
		}
		r.updateCounts(at)
		r.finish(at, PhaseSucceeded, fmt.Sprintf("Early stopping: no improvement in %d trials", at.Spec.EarlyStoppingRounds), now)
		return ctrl.Result{}, nil
	}

	launched := int32(len(at.Status.Trials))
	running := at.Status.TrialsRunning

	// Launch trials while there are slots and trials left.
	exhausted := false
	parallelism := at.Spec.Parallelism
	if parallelism <= 0 {
		parallelism = 1
	}
	if parallelism > r.maxParallelism() {
		parallelism = r.maxParallelism()
	}
	toLaunch := parallelism - running
	if left := at.Spec.MaxTrials - launched; toLaunch > left {
		toLaunch = left
	}
	if toLaunch > 0 {
		strategy := tuner.NewSearchStrategy(at.Spec.SearchAlgorithm, at.Spec.Objective.Direction)
		candidates := strategy.GenerateCandidates(at.Spec.ParameterSpace, at.Status.Trials, int(toLaunch))
		if len(candidates) == 0 {
			exhausted = true
		}
		for _, params := range candidates {
			if err := r.launchTrial(ctx, at, params, now); err != nil {
				r.updateCounts(at)
				return ctrl.Result{RequeueAfter: 15 * time.Second}, err
			}
		}
		r.updateCounts(at)
	}

	running = at.Status.TrialsRunning
	launched = int32(len(at.Status.Trials))
	if running == 0 && (launched >= at.Spec.MaxTrials || exhausted) {
		msg := fmt.Sprintf("All %d trials completed", launched)
		if exhausted && launched < at.Spec.MaxTrials {
			msg = fmt.Sprintf("Search space exhausted after %d trials", launched)
		}
		switch {
		case at.Status.TrialsCompleted == 0 && at.Status.TrialsFailed > 0:
			r.finish(at, PhaseFailed, fmt.Sprintf("All %d trials failed", launched), now)
		default:
			if at.Status.BestTrial == nil {
				msg += fmt.Sprintf("; no trial reported the objective metric %q (annotation %s on the trial job)",
					at.Spec.Objective.MetricName, metricAnnotation(at.Spec.Objective.MetricName))
			}
			r.finish(at, PhaseSucceeded, msg, now)
		}
		setCondition(&at.Status.Conditions, at.Generation, ConditionTrialsRunning, metav1.ConditionFalse, "Done", at.Status.Message)
		return ctrl.Result{}, nil
	}

	// "Running" is the phase word the dashboard and gateway know for work in progress.
	at.Status.Phase = PhaseRunning
	setCondition(&at.Status.Conditions, at.Generation, ConditionTrialsRunning, metav1.ConditionTrue, "TrialsActive",
		fmt.Sprintf("%d running, %d completed, %d failed", at.Status.TrialsRunning, at.Status.TrialsCompleted, at.Status.TrialsFailed))
	return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
}

func (r *GryviaAutoTunerReconciler) updateCounts(at *gryviav1.GryviaAutoTuner) {
	var running, completed, failed int32
	for _, t := range at.Status.Trials {
		switch t.Phase {
		case gryviav1.TrialPhaseRunning, gryviav1.TrialPhasePending:
			running++
		case gryviav1.TrialPhaseSucceeded, gryviav1.TrialPhaseStopped:
			completed++
		case gryviav1.TrialPhaseFailed:
			failed++
		}
	}
	at.Status.TrialsRunning, at.Status.TrialsCompleted, at.Status.TrialsFailed = running, completed, failed
}

// launchTrial creates the GryviaAIJob for one trial and records it. Failures that retrying cannot fix (the
// object is rejected) are recorded as a failed trial, so a bad template cannot make the tuner loop forever.
func (r *GryviaAutoTunerReconciler) launchTrial(ctx context.Context, at *gryviav1.GryviaAutoTuner, params map[string]string, now time.Time) error {
	idx := len(at.Status.Trials)
	trialName := childName(at.Name, "trial", strconv.Itoa(idx))
	if params == nil {
		params = map[string]string{}
	}

	job := &gryviav1.GryviaAIJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      trialName,
			Namespace: at.Namespace,
			Labels: map[string]string{
				"gryvia.io/tuner":     childName(at.Name),
				"gryvia.io/component": "trial",
			},
		},
		Spec: *at.Spec.JobTemplate.DeepCopy(),
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	env := job.Spec.Env
	for _, k := range keys {
		env = append(env, corev1.EnvVar{Name: "HP_" + k, Value: params[k]})
	}
	env = append(env,
		corev1.EnvVar{Name: "TRIAL_NAME", Value: trialName},
		corev1.EnvVar{Name: "TUNER_NAME", Value: at.Name})
	job.Spec.Env = env
	if err := controllerutil.SetControllerReference(at, job, r.Scheme); err != nil {
		return err
	}

	t := metav1.NewTime(now)
	trial := gryviav1.TrialResult{Name: trialName, Parameters: params, Phase: gryviav1.TrialPhaseRunning, JobName: trialName, StartTime: &t}
	if err := r.Create(ctx, job); err != nil {
		switch {
		case errors.IsAlreadyExists(err):
			existing := &gryviav1.GryviaAIJob{}
			if gerr := r.Get(ctx, types.NamespacedName{Namespace: at.Namespace, Name: trialName}, existing); gerr != nil {
				return gerr
			}
			if !metav1.IsControlledBy(existing, at) {
				trial.Phase = gryviav1.TrialPhaseFailed
				trial.Message = fmt.Sprintf("job %q already exists and is not owned by this tuner", trialName)
				trial.CompletionTime = &t
			} // else adopt it: a previous pass created it but lost the status write
		case errors.IsInvalid(err) || errors.IsForbidden(err) || errors.IsBadRequest(err):
			trial.Phase = gryviav1.TrialPhaseFailed
			trial.Message = fmt.Sprintf("cannot create the trial job: %v", err)
			trial.CompletionTime = &t
		default:
			return err
		}
	}
	at.Status.Trials = append(at.Status.Trials, trial)
	return nil
}

// syncTrials updates trial results from their GryviaAIJobs.
func (r *GryviaAutoTunerReconciler) syncTrials(ctx context.Context, at *gryviav1.GryviaAutoTuner, now time.Time) error {
	for i := range at.Status.Trials {
		trial := &at.Status.Trials[i]
		switch trial.Phase {
		case gryviav1.TrialPhaseFailed, gryviav1.TrialPhaseStopped:
			continue
		case gryviav1.TrialPhaseSucceeded:
			if trial.MetricValue != nil {
				continue
			}
		}

		job := &gryviav1.GryviaAIJob{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: at.Namespace, Name: trial.JobName}, job); err != nil {
			if errors.IsNotFound(err) {
				if trial.Phase != gryviav1.TrialPhaseSucceeded {
					trial.Phase = gryviav1.TrialPhaseFailed
					trial.Message = "Trial job not found"
					t := metav1.NewTime(now)
					trial.CompletionTime = &t
				}
				continue
			}
			return err
		}

		switch job.Status.Phase {
		case PhaseSucceeded:
			if trial.Phase != gryviav1.TrialPhaseSucceeded {
				trial.Phase = gryviav1.TrialPhaseSucceeded
				t := metav1.NewTime(now)
				trial.CompletionTime = &t
			}
			if v, ok := r.trialMetric(at, job); ok {
				trial.MetricValue = &v
			}
		case PhaseFailed:
			trial.Phase = gryviav1.TrialPhaseFailed
			trial.Message = job.Status.Message
			t := metav1.NewTime(now)
			trial.CompletionTime = &t
		default:
			trial.Phase = gryviav1.TrialPhaseRunning
			// Intermediate metrics feed ASHA (one per epoch).
			if m := job.Status.Metrics; m != nil && m.Epoch > 0 {
				seen := false
				for _, e := range trial.IntermediateMetrics {
					if e.Step == m.Epoch {
						seen = true
						break
					}
				}
				if !seen {
					trial.IntermediateMetrics = append(trial.IntermediateMetrics, gryviav1.IntermediateMetric{Step: m.Epoch, Value: m.Loss})
				}
			}
		}
	}
	return nil
}

func (r *GryviaAutoTunerReconciler) trialMetric(at *gryviav1.GryviaAutoTuner, job *gryviav1.GryviaAIJob) (float64, bool) {
	if val, ok := job.Annotations[metricAnnotation(at.Spec.Objective.MetricName)]; ok {
		if parsed, err := strconv.ParseFloat(val, 64); err == nil {
			return parsed, true
		}
	}
	if at.Spec.Objective.MetricName == "loss" && job.Status.Metrics != nil {
		return job.Status.Metrics.Loss, true
	}
	return 0, false
}

// updateBestTrial selects the best finished trial for the objective.
func (r *GryviaAutoTunerReconciler) updateBestTrial(at *gryviav1.GryviaAutoTuner) {
	var best *gryviav1.TrialResult
	for i := range at.Status.Trials {
		t := &at.Status.Trials[i]
		if t.MetricValue == nil {
			continue
		}
		if best == nil ||
			(at.Spec.Objective.Direction == gryviav1.ObjectiveMinimize && *t.MetricValue < *best.MetricValue) ||
			(at.Spec.Objective.Direction != gryviav1.ObjectiveMinimize && *t.MetricValue > *best.MetricValue) {
			best = t
		}
	}
	if best != nil {
		c := best.DeepCopy()
		at.Status.BestTrial = c
		setCondition(&at.Status.Conditions, at.Generation, ConditionBestFound, metav1.ConditionTrue, "BestTrialFound",
			fmt.Sprintf("Best trial %s: %s = %v", best.Name, at.Spec.Objective.MetricName, *best.MetricValue))
	}
}

// shouldStopEarly is true when the last EarlyStoppingRounds trials with a result, after the best one, did not
// beat it.
func (r *GryviaAutoTunerReconciler) shouldStopEarly(at *gryviav1.GryviaAutoTuner) bool {
	best := at.Status.BestTrial
	if best == nil || best.MetricValue == nil || at.Spec.EarlyStoppingRounds <= 0 {
		return false
	}
	found, since := false, int32(0)
	for _, t := range at.Status.Trials {
		if t.Name == best.Name {
			found = true
			continue
		}
		if !found || t.MetricValue == nil {
			continue
		}
		if (at.Spec.Objective.Direction == gryviav1.ObjectiveMinimize && *t.MetricValue < *best.MetricValue) ||
			(at.Spec.Objective.Direction != gryviav1.ObjectiveMinimize && *t.MetricValue > *best.MetricValue) {
			return false
		}
		since++
	}
	return since >= at.Spec.EarlyStoppingRounds
}

// stopRunning stops every running trial (deletes its job) and marks it Stopped.
func (r *GryviaAutoTunerReconciler) stopRunning(ctx context.Context, at *gryviav1.GryviaAutoTuner, why string, now time.Time) error {
	for i := range at.Status.Trials {
		t := &at.Status.Trials[i]
		if t.Phase != gryviav1.TrialPhaseRunning && t.Phase != gryviav1.TrialPhasePending {
			continue
		}
		if err := r.deleteTrialJob(ctx, at, t); err != nil {
			return err
		}
		t.Phase = gryviav1.TrialPhaseStopped
		t.Message = why
		ts := metav1.NewTime(now)
		t.CompletionTime = &ts
	}
	return nil
}

func (r *GryviaAutoTunerReconciler) deleteTrialJob(ctx context.Context, at *gryviav1.GryviaAutoTuner, t *gryviav1.TrialResult) error {
	job := &gryviav1.GryviaAIJob{}
	err := r.Get(ctx, types.NamespacedName{Namespace: at.Namespace, Name: t.JobName}, job)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !metav1.IsControlledBy(job, at) {
		return nil
	}
	if derr := r.Delete(ctx, job); derr != nil && !errors.IsNotFound(derr) {
		return derr
	}
	return nil
}

// applyASHAEarlyStopping stops running trials that fall behind at their rung (asynchronous successive halving).
func (r *GryviaAutoTunerReconciler) applyASHAEarlyStopping(ctx context.Context, at *gryviav1.GryviaAutoTuner, now time.Time) error {
	sched := &tuner.ASHAScheduler{
		MaxEpochs:       at.Spec.ASHAConfig.MaxEpochs,
		ReductionFactor: at.Spec.ASHAConfig.ReductionFactor,
		MinResource:     at.Spec.ASHAConfig.MinResource,
	}
	for i := range at.Status.Trials {
		t := &at.Status.Trials[i]
		if t.Phase != gryviav1.TrialPhaseRunning {
			continue
		}
		if !sched.ShouldStop(*t, at.Status.Trials, at.Spec.Objective.Direction) {
			continue
		}
		if err := r.deleteTrialJob(ctx, at, t); err != nil {
			return err
		}
		t.Phase = gryviav1.TrialPhaseStopped
		t.Message = "Stopped by ASHA early stopping"
		ts := metav1.NewTime(now)
		t.CompletionTime = &ts
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *GryviaAutoTunerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gryviav1.GryviaAutoTuner{}).
		Owns(&gryviav1.GryviaAIJob{}).
		Complete(r)
}
