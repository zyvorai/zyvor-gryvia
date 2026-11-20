// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/ai-operator/pkg/cron"
)

const (
	// Workflow condition types
	ConditionDAGValid     = "DAGValid"
	ConditionStepsRunning = "StepsRunning"
	ConditionWorkflowDone = "WorkflowComplete"

	// Limits that bound how many child objects one workflow can create.
	maxWorkflowSteps                = 100
	DefaultWorkflowMaxParallelSteps = 10
	maxStepRetries                  = 10

	webhookTimeout = 10 * time.Second

	// PhaseScheduled is a scheduled workflow waiting for its next fire time.
	PhaseScheduled = "Scheduled"

	// annotationOutputPrefix marks a step output written on a step's GryviaAIJob.
	registryActionUpdate   = "updateMetadata"
	registryActionRollback = "rollback"

	annotationOutputPrefix = "gryvia.io/output-"
	maxStepOutputs         = 32
	maxStepOutputBytes     = 4096
)

var (
	stepNameRE     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	conditionRE    = regexp.MustCompile(`^steps\.([a-z0-9]([-a-z0-9]*[a-z0-9])?)\.status\s*(==|!=)\s*['"]([A-Za-z]+)['"]$`)
	webhookStatusR = regexp.MustCompile(`^status\s*==\s*([1-5][0-9][0-9])$`)
	outputKeyRE    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,63}$`)
	// unresolvedRE finds workflow placeholders left after rendering (an output the step never reported).
	unresolvedRE = regexp.MustCompile(`\{\{\s*(steps|parameters|workflow)\.[A-Za-z0-9_.\-]+\s*\}\}`)
)

// GryviaWorkflowReconciler reconciles a GryviaWorkflow object.
//
// Steps run as soon as everything they depend on is done:
//
//   - job steps create a child GryviaAIJob (named "<workflow>-<step>", owned by the workflow); they need the
//     AIJob controller to actually run the workload;
//   - script steps run as a Pod owned by the workflow (non-root, no privilege escalation, capabilities dropped,
//     no service-account token, bounded resources), independent of the AIJob controller;
//   - webhook steps make one HTTP call from the operator; they are off unless AllowWebhooks is set, because they
//     let anyone who can create a workflow make the operator issue requests inside the cluster network;
//   - register steps create a GryviaModelRegistry entry inline (not owned by the workflow, so deleting the workflow
//     does not remove a model that may be serving).
//
// A finished job or script step's outputs ("gryvia.io/output-<key>" annotations on the GryviaAIJob, or a JSON
// object in the termination message of its rank-0 pod) are kept in status and fill {{steps.<name>.outputs.<key>}}
// placeholders in later steps, next to {{parameters.<key>}}, {{workflow.name}} and {{workflow.run}}.
//
// A failed step is retried up to spec.retries times (new child object per attempt, optional backoff). A step whose
// dependency failed or was skipped is skipped, unless it has a condition that says otherwise. With spec.schedule the
// whole DAG runs again at each fire time after the previous run finished; each run's children carry the run number.
type GryviaWorkflowReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Log    logr.Logger

	// MaxParallelSteps caps how many steps of one workflow run at the same time (10 when 0).
	MaxParallelSteps int
	// AllowWebhooks enables webhook steps (off by default).
	AllowWebhooks bool
	// HTTPClient performs webhook calls (a client with a 10s timeout when nil).
	HTTPClient *http.Client
	// Clock returns the current time (time.Now when nil); tests move it.
	Clock func() time.Time
}

//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaworkflows,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaworkflows/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaworkflows/finalizers,verbs=update
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaaijobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviamodelregistries,verbs=get;list;watch;create;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviainferenceservices,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete

// Reconcile drives one GryviaWorkflow.
func (r *GryviaWorkflowReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("gryviaworkflow", req.NamespacedName)

	wf := &gryviav1.GryviaWorkflow{}
	if err := r.Get(ctx, req.NamespacedName, wf); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !wf.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil // children are garbage-collected through their owner reference
	}
	if wf.Spec.Schedule == "" && (wf.Status.Phase == PhaseSucceeded || wf.Status.Phase == PhaseFailed) {
		return ctrl.Result{}, nil
	}

	orig := wf.DeepCopy()
	var res ctrl.Result
	var err error
	if wf.Spec.Schedule != "" {
		res, err = r.reconcileScheduled(ctx, wf)
	} else {
		res, err = r.reconcileWorkflow(ctx, wf)
	}
	if perr := patchStatus(ctx, r.Client, wf, orig); perr != nil {
		log.Error(perr, "Failed to patch workflow status")
		if err == nil {
			err = perr
		}
	}
	return res, err
}

func (r *GryviaWorkflowReconciler) finish(wf *gryviav1.GryviaWorkflow, phase, msg string, now time.Time) {
	wf.Status.Phase = phase
	wf.Status.Message = msg
	t := metav1.NewTime(now)
	wf.Status.CompletionTime = &t
	status := metav1.ConditionTrue
	if phase == PhaseFailed {
		status = metav1.ConditionFalse
	}
	setCondition(&wf.Status.Conditions, wf.Generation, ConditionWorkflowDone, status, phase, msg)
}

// parsedCondition is one clause of a step condition.
type parsedCondition struct {
	step   string
	negate bool
	phase  string
}

// parseCondition understands "steps.<name>.status == 'Succeeded'" clauses joined with "&&" (== or !=).
func parseCondition(expr string) ([]parsedCondition, error) {
	var out []parsedCondition
	for _, part := range strings.Split(expr, "&&") {
		m := conditionRE.FindStringSubmatch(strings.TrimSpace(part))
		if m == nil {
			return nil, fmt.Errorf("unsupported condition %q (use: steps.<name>.status == 'Succeeded', joined with &&)", expr)
		}
		switch m[4] {
		case "Succeeded", "Failed", "Skipped", "Running", "Pending":
		default:
			return nil, fmt.Errorf("condition %q: unknown step phase %q", expr, m[4])
		}
		out = append(out, parsedCondition{step: m[1], negate: m[3] == "!=", phase: m[4]})
	}
	return out, nil
}

// stepKind resolves the step type, inferring it from the payload when spec.type is empty.
func stepKind(s *gryviav1.WorkflowStep) gryviav1.StepType {
	if s.Type != "" {
		return s.Type
	}
	switch {
	case s.JobTemplate != nil:
		return gryviav1.StepTypeJob
	case s.Script != nil:
		return gryviav1.StepTypeScript
	case s.Webhook != nil:
		return gryviav1.StepTypeWebhook
	case s.Register != nil:
		return gryviav1.StepTypeRegister
	case s.Registry != nil:
		return gryviav1.StepTypeRegistry
	}
	return ""
}

// validateWorkflow rejects a workflow that cannot run: too many steps, bad names, missing payloads, unknown or
// cyclic dependencies, unsupported conditions.
func validateWorkflow(wf *gryviav1.GryviaWorkflow) error {
	if len(wf.Spec.Steps) == 0 {
		return fmt.Errorf("the workflow has no steps")
	}
	if len(wf.Spec.Steps) > maxWorkflowSteps {
		return fmt.Errorf("%d steps exceeds the limit of %d", len(wf.Spec.Steps), maxWorkflowSteps)
	}
	if wf.Spec.Schedule != "" {
		if _, err := cron.Parse(wf.Spec.Schedule); err != nil {
			return fmt.Errorf("schedule: %w", err)
		}
	}
	deps := make(map[string][]string, len(wf.Spec.Steps))
	for i := range wf.Spec.Steps {
		s := &wf.Spec.Steps[i]
		if !stepNameRE.MatchString(s.Name) || len(s.Name) > maxDNSLabel {
			return fmt.Errorf("step name %q must be a lowercase DNS label (a-z, 0-9, '-'; at most %d characters)", s.Name, maxDNSLabel)
		}
		if _, dup := deps[s.Name]; dup {
			return fmt.Errorf("duplicate step name: %s", s.Name)
		}
		deps[s.Name] = s.DependsOn
		if s.Retries < 0 || s.Retries > maxStepRetries {
			return fmt.Errorf("step %q: retries must be between 0 and %d", s.Name, maxStepRetries)
		}
		switch k := stepKind(s); k {
		case gryviav1.StepTypeJob:
			if s.JobTemplate == nil {
				return fmt.Errorf("step %q has type 'job' but no jobTemplate", s.Name)
			}
		case gryviav1.StepTypeScript:
			if s.Script == nil || s.Script.Image == "" || len(s.Script.Command) == 0 {
				return fmt.Errorf("step %q has type 'script' but no script image and command", s.Name)
			}
		case gryviav1.StepTypeWebhook:
			if s.Webhook == nil || s.Webhook.URL == "" {
				return fmt.Errorf("step %q has type 'webhook' but no webhook url", s.Name)
			}
			if sc := s.Webhook.SuccessCondition; sc != "" && !webhookStatusR.MatchString(strings.TrimSpace(sc)) {
				return fmt.Errorf("step %q: unsupported successCondition %q (use: status == 200)", s.Name, sc)
			}
		case gryviav1.StepTypeRegister:
			reg := s.Register
			if reg == nil || reg.ModelName == "" || reg.Version == "" {
				return fmt.Errorf("step %q has type 'register' but no register modelName and version", s.Name)
			}
			if reg.Artifacts.S3Path == "" && reg.Artifacts.PVCName == "" {
				return fmt.Errorf("step %q: register artifacts need an s3Path or a pvcName", s.Name)
			}
			switch reg.Stage {
			case "", gryviav1.ModelStageDev, gryviav1.ModelStageStaging:
			default:
				return fmt.Errorf("step %q: register stage must be dev or staging (use a promotionPolicy to reach production)", s.Name)
			}
		case gryviav1.StepTypeRegistry:
			reg := s.Registry
			if reg == nil || (reg.Entry == "") == (reg.ServiceName == "") {
				return fmt.Errorf("step %q has type 'registry' but not exactly one of registry entry and serviceName", s.Name)
			}
			switch reg.Action {
			case registryActionUpdate:
				if len(reg.Metadata) == 0 {
					return fmt.Errorf("step %q: registry action updateMetadata needs metadata", s.Name)
				}
			case registryActionRollback:
			default:
				return fmt.Errorf("step %q: registry action must be updateMetadata or rollback", s.Name)
			}
		default:
			return fmt.Errorf("step %q has no recognised type or payload", s.Name)
		}
	}
	for _, s := range wf.Spec.Steps {
		for _, d := range s.DependsOn {
			if _, ok := deps[d]; !ok {
				return fmt.Errorf("step %q depends on non-existent step %q", s.Name, d)
			}
			if d == s.Name {
				return fmt.Errorf("step %q depends on itself", s.Name)
			}
		}
		if s.Condition != "" {
			clauses, err := parseCondition(s.Condition)
			if err != nil {
				return fmt.Errorf("step %q: %w", s.Name, err)
			}
			for _, c := range clauses {
				if !contains(s.DependsOn, c.step) {
					return fmt.Errorf("step %q: its condition refers to %q, which is not in dependsOn", s.Name, c.step)
				}
			}
		}
	}
	// Kahn's algorithm: whatever cannot be ordered is on, or behind, a cycle.
	indeg := make(map[string]int, len(deps))
	for n, ds := range deps {
		indeg[n] = len(uniq(ds))
	}
	users := map[string][]string{}
	for n, ds := range deps {
		for _, d := range uniq(ds) {
			users[d] = append(users[d], n)
		}
	}
	var queue []string
	for n, d := range indeg {
		if d == 0 {
			queue = append(queue, n)
		}
	}
	seen := 0
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		seen++
		for _, u := range users[n] {
			if indeg[u]--; indeg[u] == 0 {
				queue = append(queue, u)
			}
		}
	}
	if seen != len(deps) {
		return fmt.Errorf("cycle detected in workflow DAG")
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func stepTerminal(p gryviav1.StepPhase) bool {
	return p == gryviav1.StepPhaseSucceeded || p == gryviav1.StepPhaseFailed || p == gryviav1.StepPhaseSkipped
}

func (r *GryviaWorkflowReconciler) reconcileWorkflow(ctx context.Context, wf *gryviav1.GryviaWorkflow) (ctrl.Result, error) {
	log := r.Log.WithValues("gryviaworkflow", wf.Name)
	now := clock(r.Clock)

	if wf.Status.Phase == "" {
		if err := validateWorkflow(wf); err != nil {
			setCondition(&wf.Status.Conditions, wf.Generation, ConditionDAGValid, metav1.ConditionFalse, "Invalid", err.Error())
			r.finish(wf, PhaseFailed, fmt.Sprintf("Invalid workflow: %v", err), now)
			return ctrl.Result{}, nil
		}
		setCondition(&wf.Status.Conditions, wf.Generation, ConditionDAGValid, metav1.ConditionTrue, "Valid", "The step graph is a valid DAG")
		t := metav1.NewTime(now)
		wf.Status.StartTime = &t
		wf.Status.Phase = PhasePending
		for _, s := range wf.Spec.Steps {
			wf.Status.StepStatuses = append(wf.Status.StepStatuses, gryviav1.StepStatus{Name: s.Name, Phase: gryviav1.StepPhasePending})
		}
	} else if err := validateWorkflow(wf); err != nil {
		// The spec was edited into something invalid while running.
		r.finish(wf, PhaseFailed, fmt.Sprintf("Invalid workflow: %v", err), now)
		return ctrl.Result{}, nil
	}

	specs := make(map[string]*gryviav1.WorkflowStep, len(wf.Spec.Steps))
	for i := range wf.Spec.Steps {
		specs[wf.Spec.Steps[i].Name] = &wf.Spec.Steps[i]
	}
	byName := make(map[string]*gryviav1.StepStatus, len(wf.Status.StepStatuses))
	for i := range wf.Status.StepStatuses {
		byName[wf.Status.StepStatuses[i].Name] = &wf.Status.StepStatuses[i]
	}
	for _, s := range wf.Spec.Steps { // steps added after the first pass
		if byName[s.Name] == nil {
			wf.Status.StepStatuses = append(wf.Status.StepStatuses, gryviav1.StepStatus{Name: s.Name, Phase: gryviav1.StepPhasePending})
			byName = make(map[string]*gryviav1.StepStatus, len(wf.Status.StepStatuses))
			for i := range wf.Status.StepStatuses {
				byName[wf.Status.StepStatuses[i].Name] = &wf.Status.StepStatuses[i]
			}
		}
	}

	next := 10 * time.Second
	wake := func(d time.Duration) {
		if d < time.Second {
			d = time.Second
		}
		next = minDuration(next, d)
	}

	// 1. Follow the steps that are running.
	for i := range wf.Status.StepStatuses {
		ss := &wf.Status.StepStatuses[i]
		spec := specs[ss.Name]
		if spec == nil || ss.Phase != gryviav1.StepPhaseRunning {
			continue
		}
		done, ok, msg, err := r.pollStep(ctx, wf, spec, ss)
		if err != nil {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, err
		}
		if !done && spec.TimeoutSeconds > 0 && ss.StartTime != nil {
			if elapsed := now.Sub(ss.StartTime.Time); elapsed > time.Duration(spec.TimeoutSeconds)*time.Second {
				r.deleteStepChild(ctx, wf, spec, ss)
				done, ok, msg = true, false, fmt.Sprintf("timed out after %ds", spec.TimeoutSeconds)
			} else {
				wake(time.Duration(spec.TimeoutSeconds)*time.Second - elapsed + time.Second)
			}
		}
		if !done {
			continue
		}
		if ok {
			outputs, err := r.collectOutputs(ctx, wf, spec, ss)
			if err != nil {
				return ctrl.Result{RequeueAfter: 5 * time.Second}, err
			}
			ss.Outputs = outputs
			ss.Phase = gryviav1.StepPhaseSucceeded
			ss.Message = ""
			t := metav1.NewTime(now)
			ss.CompletionTime = &t
			continue
		}
		r.failStep(ss, spec, msg, now)
	}

	// 2. Start what is ready, skipping what can no longer run. Repeat while skips cascade.
	maxParallel := r.MaxParallelSteps
	if maxParallel <= 0 {
		maxParallel = DefaultWorkflowMaxParallelSteps
	}
	for changed := true; changed; {
		changed = false
		running := 0
		for _, ss := range wf.Status.StepStatuses {
			if ss.Phase == gryviav1.StepPhaseRunning {
				running++
			}
		}
		for i := range wf.Status.StepStatuses {
			ss := &wf.Status.StepStatuses[i]
			spec := specs[ss.Name]
			if spec == nil || ss.Phase != gryviav1.StepPhasePending {
				continue
			}
			ready, badDep := true, false
			for _, d := range spec.DependsOn {
				dp := byName[d].Phase
				if !stepTerminal(dp) {
					ready = false
					break
				}
				if dp != gryviav1.StepPhaseSucceeded {
					badDep = true
				}
			}
			if !ready {
				continue
			}
			if spec.Condition != "" {
				if !evalCondition(spec.Condition, byName) {
					ss.Phase = gryviav1.StepPhaseSkipped
					ss.Message = "Skipped: condition not met"
					changed = true
					continue
				}
			} else if badDep {
				ss.Phase = gryviav1.StepPhaseSkipped
				ss.Message = "Skipped: a dependency failed or was skipped"
				changed = true
				continue
			}
			// Retry backoff: wait out spec.retryBackoffSeconds after the failed attempt.
			if ss.RetriesAttempted > 0 && ss.CompletionTime != nil && spec.RetryBackoffSeconds > 0 {
				due := ss.CompletionTime.Add(time.Duration(spec.RetryBackoffSeconds) * time.Second)
				if now.Before(due) {
					wake(due.Sub(now) + time.Second)
					continue
				}
			}
			if running >= maxParallel {
				continue
			}
			if err := r.launchStep(ctx, wf, spec, ss, now, workflowLookup(wf, byName)); err != nil {
				log.Error(err, "Failed to launch step", "step", ss.Name)
				return ctrl.Result{RequeueAfter: 5 * time.Second}, err
			}
			if ss.Phase == gryviav1.StepPhaseRunning {
				running++
			} else {
				changed = true // a webhook step finished (or failed) inline
			}
		}
	}

	// 3. The workflow phase.
	pending, run, failed := 0, 0, []string{}
	for _, ss := range wf.Status.StepStatuses {
		switch ss.Phase {
		case gryviav1.StepPhasePending:
			pending++
		case gryviav1.StepPhaseRunning:
			run++
		case gryviav1.StepPhaseFailed:
			failed = append(failed, ss.Name)
		}
	}
	switch {
	case pending == 0 && run == 0 && len(failed) > 0:
		sort.Strings(failed)
		r.finish(wf, PhaseFailed, fmt.Sprintf("Workflow failed: step(s) %s failed", strings.Join(failed, ", ")), now)
		return ctrl.Result{}, nil
	case pending == 0 && run == 0:
		r.finish(wf, PhaseSucceeded, "All steps completed successfully", now)
		return ctrl.Result{}, nil
	}
	wf.Status.Phase = PhaseRunning
	setCondition(&wf.Status.Conditions, wf.Generation, ConditionStepsRunning, metav1.ConditionTrue, "StepsActive",
		fmt.Sprintf("%d running, %d waiting", run, pending))
	return ctrl.Result{RequeueAfter: next}, nil
}

// evalCondition evaluates a validated condition against the current step phases.
func evalCondition(expr string, byName map[string]*gryviav1.StepStatus) bool {
	clauses, err := parseCondition(expr)
	if err != nil {
		return false
	}
	for _, c := range clauses {
		ss := byName[c.step]
		match := ss != nil && string(ss.Phase) == c.phase
		if match == c.negate {
			return false
		}
	}
	return true
}

// failStep records a failed attempt: it goes back to Pending while retries remain, else it is Failed.
func (r *GryviaWorkflowReconciler) failStep(ss *gryviav1.StepStatus, spec *gryviav1.WorkflowStep, msg string, now time.Time) {
	t := metav1.NewTime(now)
	ss.CompletionTime = &t // of the attempt; also the start of the retry backoff
	if ss.RetriesAttempted < spec.Retries {
		ss.RetriesAttempted++
		ss.Phase = gryviav1.StepPhasePending
		ss.Message = fmt.Sprintf("Retrying (%d/%d) after failure: %s", ss.RetriesAttempted, spec.Retries, msg)
		return
	}
	ss.Phase = gryviav1.StepPhaseFailed
	ss.Message = msg
}

// attemptName is the child object name for the step's current attempt (and run, for a scheduled workflow).
func attemptName(wf *gryviav1.GryviaWorkflow, step string, attempt int32) string {
	parts := []string{wf.Name, step}
	if wf.Status.Run > 0 {
		parts = append(parts, fmt.Sprintf("n%d", wf.Status.Run))
	}
	if attempt > 0 {
		parts = append(parts, fmt.Sprintf("r%d", attempt))
	}
	return childName(parts...)
}

func stepLabels(wf *gryviav1.GryviaWorkflow, step, component string) map[string]string {
	l := map[string]string{
		"gryvia.io/workflow":  childName(wf.Name),
		"gryvia.io/step":      childName(step),
		"gryvia.io/component": component,
	}
	if wf.Status.Run > 0 {
		l["gryvia.io/run"] = strconv.Itoa(int(wf.Status.Run))
	}
	return l
}

// workflowLookup resolves {{workflow.name}}, {{workflow.run}}, {{parameters.<key>}} and
// {{steps.<name>.outputs.<key>}}.
func workflowLookup(wf *gryviav1.GryviaWorkflow, byName map[string]*gryviav1.StepStatus) func(string) (string, bool) {
	return func(k string) (string, bool) {
		switch {
		case k == "workflow.name":
			return wf.Name, true
		case k == "workflow.run":
			return strconv.Itoa(int(wf.Status.Run)), true
		case strings.HasPrefix(k, "parameters."):
			v, ok := wf.Spec.Parameters[strings.TrimPrefix(k, "parameters.")]
			return v, ok
		case strings.HasPrefix(k, "steps."):
			parts := strings.SplitN(k, ".", 4)
			if len(parts) == 4 && parts[2] == "outputs" {
				if ss := byName[parts[1]]; ss != nil {
					v, ok := ss.Outputs[parts[3]]
					return v, ok
				}
			}
		}
		return "", false
	}
}

// renderStep fills the placeholders of a step payload. A workflow placeholder that stays unresolved (an output
// the step it names never reported) is an error, so a job never starts with a literal "{{steps...}}" argument.
func renderStep(in, out interface{}, lookup func(string) (string, bool)) error {
	if err := renderInto(in, out, lookup); err != nil {
		return fmt.Errorf("rendering placeholders: %w", err)
	}
	b, _ := json.Marshal(out)
	if m := unresolvedRE.Find(b); m != nil {
		return fmt.Errorf("unresolved placeholder %s", m)
	}
	return nil
}

// launchStep starts one step. For a webhook or register step the outcome is set directly.
func (r *GryviaWorkflowReconciler) launchStep(ctx context.Context, wf *gryviav1.GryviaWorkflow, step *gryviav1.WorkflowStep,
	ss *gryviav1.StepStatus, now time.Time, lookup func(string) (string, bool)) error {
	name := attemptName(wf, step.Name, ss.RetriesAttempted)
	started := func() {
		ss.Phase = gryviav1.StepPhaseRunning
		ss.JobName = name
		ss.Message = ""
		t := metav1.NewTime(now)
		ss.StartTime = &t
		ss.CompletionTime = nil
	}

	switch stepKind(step) {
	case gryviav1.StepTypeJob:
		var spec gryviav1.GryviaAIJobSpec
		if err := renderStep(step.JobTemplate, &spec, lookup); err != nil {
			r.failStep(ss, step, err.Error(), now)
			return nil
		}
		job := &gryviav1.GryviaAIJob{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: wf.Namespace, Labels: stepLabels(wf, step.Name, "workflow-step")},
			Spec:       spec,
		}
		job.Spec.Env = r.stepEnv(wf, step, job.Spec.Env)
		if err := controllerutil.SetControllerReference(wf, job, r.Scheme); err != nil {
			return err
		}
		if err := r.Create(ctx, job); err != nil {
			if !errors.IsAlreadyExists(err) {
				if errors.IsInvalid(err) || errors.IsForbidden(err) || errors.IsBadRequest(err) {
					r.failStep(ss, step, fmt.Sprintf("cannot create the job: %v", err), now)
					return nil
				}
				return err
			}
			existing := &gryviav1.GryviaAIJob{}
			if gerr := r.Get(ctx, types.NamespacedName{Namespace: wf.Namespace, Name: name}, existing); gerr != nil {
				return gerr
			}
			if !metav1.IsControlledBy(existing, wf) {
				r.failStep(ss, step, fmt.Sprintf("job %q already exists and is not owned by this workflow", name), now)
				return nil
			}
		}
		started()
	case gryviav1.StepTypeScript:
		rendered := step.DeepCopy()
		if err := renderStep(step.Script, &rendered.Script, lookup); err != nil {
			r.failStep(ss, step, err.Error(), now)
			return nil
		}
		pod, err := r.buildScriptPod(wf, rendered, name)
		if err != nil {
			r.failStep(ss, step, err.Error(), now)
			return nil
		}
		if err := controllerutil.SetControllerReference(wf, pod, r.Scheme); err != nil {
			return err
		}
		if err := r.Create(ctx, pod); err != nil {
			if !errors.IsAlreadyExists(err) {
				if errors.IsInvalid(err) || errors.IsForbidden(err) || errors.IsBadRequest(err) {
					r.failStep(ss, step, fmt.Sprintf("cannot create the script pod: %v", err), now)
					return nil
				}
				return err
			}
			existing := &corev1.Pod{}
			if gerr := r.Get(ctx, types.NamespacedName{Namespace: wf.Namespace, Name: name}, existing); gerr != nil {
				return gerr
			}
			if !metav1.IsControlledBy(existing, wf) {
				r.failStep(ss, step, fmt.Sprintf("pod %q already exists and is not owned by this workflow", name), now)
				return nil
			}
		}
		started()
	case gryviav1.StepTypeWebhook:
		if !r.AllowWebhooks {
			// A configuration problem: retrying cannot help.
			ss.Phase = gryviav1.StepPhaseFailed
			ss.Message = "webhook steps are disabled (start the ai-operator with --workflow-allow-webhooks)"
			t := metav1.NewTime(now)
			ss.CompletionTime = &t
			return nil
		}
		started()
		ss.JobName = ""
		if msg, ok := r.callWebhook(ctx, step.Webhook); ok {
			ss.Phase = gryviav1.StepPhaseSucceeded
			ss.Message = msg
			t := metav1.NewTime(now)
			ss.CompletionTime = &t
		} else {
			r.failStep(ss, step, msg, now)
		}
	case gryviav1.StepTypeRegister:
		started()
		ss.JobName = ""
		entry, msg, err := r.registerModel(ctx, wf, step, lookup)
		if err != nil {
			return err
		}
		if entry == "" {
			r.failStep(ss, step, msg, now)
			return nil
		}
		ss.Phase = gryviav1.StepPhaseSucceeded
		ss.JobName = entry
		ss.Message = msg
		ss.Outputs = map[string]string{"name": entry}
		t := metav1.NewTime(now)
		ss.CompletionTime = &t
	case gryviav1.StepTypeRegistry:
		started()
		ss.JobName = ""
		entry, msg, err := r.updateRegistry(ctx, wf, step, lookup, now)
		if err != nil {
			return err
		}
		if entry == "" {
			r.failStep(ss, step, msg, now)
			return nil
		}
		ss.Phase = gryviav1.StepPhaseSucceeded
		ss.JobName = entry
		ss.Message = msg
		ss.Outputs = map[string]string{"name": entry}
		t := metav1.NewTime(now)
		ss.CompletionTime = &t
	default:
		r.failStep(ss, step, "step has no recognised type", now)
	}
	return nil
}

// registerModel creates the GryviaModelRegistry entry of a register step. It returns the entry name, or "" and
// a failure message; err is for transient API errors. The entry is not owned by the workflow: deleting a run must
// not remove a model that may be serving.
func (r *GryviaWorkflowReconciler) registerModel(ctx context.Context, wf *gryviav1.GryviaWorkflow, step *gryviav1.WorkflowStep,
	lookup func(string) (string, bool)) (string, string, error) {
	var reg gryviav1.RegisterStep
	if err := renderStep(step.Register, &reg, lookup); err != nil {
		return "", err.Error(), nil
	}
	name := reg.Name
	if name == "" {
		name = attemptName(wf, step.Name, 0)
	}
	if !stepNameRE.MatchString(name) || len(name) > maxDNSLabel {
		return "", fmt.Sprintf("register name %q is not a lowercase DNS label", name), nil
	}
	stage := reg.Stage
	if stage == "" {
		stage = gryviav1.ModelStageStaging
	}
	meta := map[string]string{}
	for k, v := range reg.Metadata {
		meta[k] = v
	}
	if src := wf.Annotations[annotationSourceModel]; src != "" {
		if _, set := meta["base_model"]; !set {
			meta["base_model"] = src
		}
		if rev := wf.Annotations[annotationSourceRevision]; rev != "" {
			if _, set := meta["base_revision"]; !set {
				meta["base_revision"] = rev
			}
		}
	}
	labels := stepLabels(wf, step.Name, "workflow-register")
	entry := &gryviav1.GryviaModelRegistry{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: wf.Namespace, Labels: labels},
		Spec: gryviav1.GryviaModelRegistrySpec{
			ModelName:       reg.ModelName,
			Version:         reg.Version,
			Source:          gryviav1.ModelSource{WorkflowRef: wf.Name},
			Artifacts:       reg.Artifacts,
			Stage:           stage,
			Description:     reg.Description,
			Metadata:        meta,
			AutoServe:       reg.AutoServe,
			ServingConfig:   reg.ServingConfig,
			PromotionPolicy: reg.PromotionPolicy,
			RollbackPolicy:  reg.RollbackPolicy,
		},
	}
	if err := r.Create(ctx, entry); err != nil {
		if !errors.IsAlreadyExists(err) {
			if errors.IsInvalid(err) || errors.IsForbidden(err) || errors.IsBadRequest(err) {
				return "", fmt.Sprintf("cannot create the model registry entry: %v", err), nil
			}
			return "", "", err
		}
		existing := &gryviav1.GryviaModelRegistry{}
		if gerr := r.Get(ctx, types.NamespacedName{Namespace: wf.Namespace, Name: name}, existing); gerr != nil {
			return "", "", gerr
		}
		if existing.Spec.Source.WorkflowRef != wf.Name || existing.Labels["gryvia.io/run"] != labels["gryvia.io/run"] {
			return "", fmt.Sprintf("model registry entry %q already exists and was not registered by this run", name), nil
		}
	}
	return name, fmt.Sprintf("Registered %s version %s as %s", reg.ModelName, reg.Version, name), nil
}

// updateRegistry applies a registry step to an existing entry: merges metadata, or sets the rollback annotation
// the model registry controller acts on. It returns the entry name, or "" and a failure message; err is for
// transient API errors.
func (r *GryviaWorkflowReconciler) updateRegistry(ctx context.Context, wf *gryviav1.GryviaWorkflow, step *gryviav1.WorkflowStep,
	lookup func(string) (string, bool), now time.Time) (string, string, error) {
	var reg gryviav1.RegistryStep
	if err := renderStep(step.Registry, &reg, lookup); err != nil {
		return "", err.Error(), nil
	}
	if reg.ServiceName != "" {
		svc := &gryviav1.GryviaInferenceService{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: wf.Namespace, Name: reg.ServiceName}, svc); err != nil {
			if errors.IsNotFound(err) {
				return "", fmt.Sprintf("inference service %q not found", reg.ServiceName), nil
			}
			return "", "", err
		}
		if svc.Spec.ModelRef == "" {
			return "", fmt.Sprintf("inference service %q serves no model registry entry (no modelRef)", reg.ServiceName), nil
		}
		reg.Entry = svc.Spec.ModelRef
	}
	if !stepNameRE.MatchString(reg.Entry) || len(reg.Entry) > maxDNSLabel {
		return "", fmt.Sprintf("registry entry %q is not a lowercase DNS label", reg.Entry), nil
	}
	entry := &gryviav1.GryviaModelRegistry{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: wf.Namespace, Name: reg.Entry}, entry); err != nil {
		if errors.IsNotFound(err) {
			return "", fmt.Sprintf("model registry entry %q not found", reg.Entry), nil
		}
		return "", "", err
	}
	base := entry.DeepCopy()
	var msg string
	switch reg.Action {
	case registryActionUpdate:
		if entry.Spec.Metadata == nil {
			entry.Spec.Metadata = map[string]string{}
		}
		keys := make([]string, 0, len(reg.Metadata))
		for k, v := range reg.Metadata {
			entry.Spec.Metadata[k] = v
			keys = append(keys, k)
		}
		sort.Strings(keys)
		msg = fmt.Sprintf("Updated %s metadata: %s", reg.Entry, strings.Join(keys, ", "))
	case registryActionRollback:
		if entry.Annotations == nil {
			entry.Annotations = map[string]string{}
		}
		entry.Annotations[annotationRollbackRequested] = fmt.Sprintf("workflow %s/%s at %s", wf.Name, step.Name, now.UTC().Format(time.RFC3339))
		msg = fmt.Sprintf("Asked for a rollback of %s", reg.Entry)
	}
	if err := r.Patch(ctx, entry, client.MergeFrom(base)); err != nil {
		if errors.IsInvalid(err) || errors.IsForbidden(err) || errors.IsBadRequest(err) {
			return "", fmt.Sprintf("cannot update the model registry entry: %v", err), nil
		}
		return "", "", err
	}
	return reg.Entry, msg, nil
}

// collectOutputs reads what a finished job or script step reported: "gryvia.io/output-<key>" annotations on the
// GryviaAIJob win over the JSON object in the termination message of the step's (rank 0) pod.
func (r *GryviaWorkflowReconciler) collectOutputs(ctx context.Context, wf *gryviav1.GryviaWorkflow, step *gryviav1.WorkflowStep,
	ss *gryviav1.StepStatus) (map[string]string, error) {
	out := map[string]string{}
	var pods []corev1.Pod
	switch stepKind(step) {
	case gryviav1.StepTypeJob:
		list := &corev1.PodList{}
		if err := r.List(ctx, list, client.InNamespace(wf.Namespace), client.MatchingLabels{"gryvia.io/job": ss.JobName}); err != nil {
			return nil, err
		}
		pods = list.Items
	case gryviav1.StepTypeScript:
		pod := &corev1.Pod{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: wf.Namespace, Name: ss.JobName}, pod); err == nil {
			pods = []corev1.Pod{*pod}
		} else if !errors.IsNotFound(err) {
			return nil, err
		}
	default:
		return nil, nil
	}
	if pod := outputPod(pods); pod != nil {
		for k, v := range parseTerminationOutputs(pod) {
			out[k] = v
		}
	}
	if stepKind(step) == gryviav1.StepTypeJob {
		job := &gryviav1.GryviaAIJob{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: wf.Namespace, Name: ss.JobName}, job); err == nil {
			for k, v := range job.Annotations {
				if key := strings.TrimPrefix(k, annotationOutputPrefix); key != k {
					addOutput(out, key, v)
				}
			}
		} else if !errors.IsNotFound(err) {
			return nil, err
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// outputPod picks the succeeded pod whose termination message holds the outputs: completion index 0 first.
func outputPod(pods []corev1.Pod) *corev1.Pod {
	sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
	var first *corev1.Pod
	for i := range pods {
		p := &pods[i]
		if p.Status.Phase != corev1.PodSucceeded {
			continue
		}
		if p.Labels["batch.kubernetes.io/job-completion-index"] == "0" || p.Annotations["batch.kubernetes.io/job-completion-index"] == "0" {
			return p
		}
		if first == nil {
			first = p
		}
	}
	return first
}

// parseTerminationOutputs reads a JSON object from the first terminated container's message. Strings are kept,
// numbers and booleans are formatted; anything else is ignored.
func parseTerminationOutputs(pod *corev1.Pod) map[string]string {
	for _, cs := range pod.Status.ContainerStatuses {
		t := cs.State.Terminated
		if t == nil || strings.TrimSpace(t.Message) == "" {
			continue
		}
		var raw map[string]interface{}
		if err := json.Unmarshal([]byte(t.Message), &raw); err != nil {
			return nil
		}
		out := map[string]string{}
		keys := make([]string, 0, len(raw))
		for k := range raw {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			switch v := raw[k].(type) {
			case string:
				addOutput(out, k, v)
			case float64:
				addOutput(out, k, strconv.FormatFloat(v, 'f', -1, 64))
			case bool:
				addOutput(out, k, strconv.FormatBool(v))
			}
		}
		return out
	}
	return nil
}

func addOutput(out map[string]string, k, v string) {
	if !outputKeyRE.MatchString(k) || len(v) > maxStepOutputBytes {
		return
	}
	if _, exists := out[k]; !exists && len(out) >= maxStepOutputs {
		return
	}
	out[k] = v
}

// reconcileScheduled runs a scheduled workflow: it waits for the next fire time after the last run started,
// resets the status for a new run, and drives that run like an unscheduled workflow.
func (r *GryviaWorkflowReconciler) reconcileScheduled(ctx context.Context, wf *gryviav1.GryviaWorkflow) (ctrl.Result, error) {
	now := clock(r.Clock)
	if err := validateWorkflow(wf); err != nil {
		if wf.Status.Phase != PhaseFailed || wf.Status.NextScheduleTime != nil {
			setCondition(&wf.Status.Conditions, wf.Generation, ConditionDAGValid, metav1.ConditionFalse, "Invalid", err.Error())
			r.finish(wf, PhaseFailed, fmt.Sprintf("Invalid workflow: %v", err), now)
			wf.Status.NextScheduleTime = nil
		}
		return ctrl.Result{}, nil
	}
	switch wf.Status.Phase {
	case PhasePending, PhaseRunning:
		return r.runScheduled(ctx, wf, now)
	}

	sched, _ := cron.Parse(wf.Spec.Schedule) // validated
	from := wf.CreationTimestamp.Time
	if wf.Status.LastScheduleTime != nil {
		from = wf.Status.LastScheduleTime.Time
	}
	next, ok := sched.Next(from)
	if !ok {
		wf.Status.NextScheduleTime = nil
		wf.Status.Message = "The schedule has no fire time in the next five years"
		return ctrl.Result{}, nil
	}
	if now.Before(next) {
		if wf.Status.Phase == "" {
			wf.Status.Phase = PhaseScheduled
			wf.Status.Message = "Waiting for the first scheduled run"
			setCondition(&wf.Status.Conditions, wf.Generation, ConditionDAGValid, metav1.ConditionTrue, "Valid", "The step graph is a valid DAG")
		}
		t := metav1.NewTime(next)
		wf.Status.NextScheduleTime = &t
		return ctrl.Result{RequeueAfter: next.Sub(now) + time.Second}, nil
	}

	// Fire: a new run starts from a clean status; the previous run's children stay (owned by the workflow).
	t := metav1.NewTime(now)
	wf.Status = gryviav1.GryviaWorkflowStatus{
		Run:              wf.Status.Run + 1,
		LastScheduleTime: &t,
		Conditions:       wf.Status.Conditions,
	}
	removeCondition(&wf.Status.Conditions, ConditionWorkflowDone)
	if n, ok := sched.Next(now); ok {
		nt := metav1.NewTime(n)
		wf.Status.NextScheduleTime = &nt
	}
	return r.runScheduled(ctx, wf, now)
}

// runScheduled drives the current run and, once it is finished, wakes up for the next fire time.
func (r *GryviaWorkflowReconciler) runScheduled(ctx context.Context, wf *gryviav1.GryviaWorkflow, now time.Time) (ctrl.Result, error) {
	res, err := r.reconcileWorkflow(ctx, wf)
	if err == nil && (wf.Status.Phase == PhaseSucceeded || wf.Status.Phase == PhaseFailed) && wf.Status.NextScheduleTime != nil {
		res.RequeueAfter = wf.Status.NextScheduleTime.Sub(now) + time.Second
		if res.RequeueAfter < time.Second {
			res.RequeueAfter = time.Second
		}
	}
	return res, err
}

// stepEnv adds the workflow parameters (sorted, without overriding variables the step sets itself) and the
// WORKFLOW_NAME/WORKFLOW_STEP identifiers to a step's environment.
func (r *GryviaWorkflowReconciler) stepEnv(wf *gryviav1.GryviaWorkflow, step *gryviav1.WorkflowStep, env []corev1.EnvVar) []corev1.EnvVar {
	have := map[string]bool{}
	for _, e := range env {
		have[e.Name] = true
	}
	add := func(k, v string) {
		if !have[k] {
			have[k] = true
			env = append(env, corev1.EnvVar{Name: k, Value: v})
		}
	}
	keys := make([]string, 0, len(wf.Spec.Parameters))
	for k := range wf.Spec.Parameters {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		add(k, wf.Spec.Parameters[k])
	}
	add("WORKFLOW_NAME", wf.Name)
	add("WORKFLOW_STEP", step.Name)
	return env
}

// buildScriptPod is the hardened Pod a script step runs in.
func (r *GryviaWorkflowReconciler) buildScriptPod(wf *gryviav1.GryviaWorkflow, step *gryviav1.WorkflowStep, name string) (*corev1.Pod, error) {
	nonRoot := int64(65534)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: wf.Namespace, Labels: stepLabels(wf, step.Name, "workflow-script")},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: boolPtr(false),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   boolPtr(true),
				RunAsUser:      &nonRoot,
				RunAsGroup:     &nonRoot,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:    "script",
				Image:   step.Script.Image,
				Command: step.Script.Command,
				Args:    step.Script.Args,
				Env:     r.stepEnv(wf, step, nil),
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: boolPtr(false),
					Privileged:               boolPtr(false),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("128Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("1"),
						corev1.ResourceMemory: resource.MustParse("1Gi"),
					},
				},
			}},
		},
	}
	if step.TimeoutSeconds > 0 {
		d := step.TimeoutSeconds
		pod.Spec.ActiveDeadlineSeconds = &d
	}
	return pod, nil
}

// pollStep reports whether a running step finished, whether it succeeded, and a failure message.
func (r *GryviaWorkflowReconciler) pollStep(ctx context.Context, wf *gryviav1.GryviaWorkflow, step *gryviav1.WorkflowStep,
	ss *gryviav1.StepStatus) (done, ok bool, msg string, err error) {
	if ss.JobName == "" {
		return true, false, "step has no child object", nil
	}
	key := types.NamespacedName{Namespace: wf.Namespace, Name: ss.JobName}
	switch stepKind(step) {
	case gryviav1.StepTypeJob:
		job := &gryviav1.GryviaAIJob{}
		if err := r.Get(ctx, key, job); err != nil {
			if errors.IsNotFound(err) {
				return true, false, "step job not found", nil
			}
			return false, false, "", err
		}
		switch job.Status.Phase {
		case PhaseSucceeded:
			return true, true, "", nil
		case PhaseFailed:
			m := job.Status.Message
			if m == "" {
				m = "job failed"
			}
			return true, false, m, nil
		}
	case gryviav1.StepTypeScript:
		pod := &corev1.Pod{}
		if err := r.Get(ctx, key, pod); err != nil {
			if errors.IsNotFound(err) {
				return true, false, "script pod not found", nil
			}
			return false, false, "", err
		}
		switch pod.Status.Phase {
		case corev1.PodSucceeded:
			return true, true, "", nil
		case corev1.PodFailed:
			return true, false, podFailureMessage(pod), nil
		}
	}
	return false, false, "", nil
}

func podFailureMessage(pod *corev1.Pod) string {
	if pod.Status.Reason != "" {
		return fmt.Sprintf("pod failed: %s %s", pod.Status.Reason, pod.Status.Message)
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if t := cs.State.Terminated; t != nil {
			return fmt.Sprintf("script exited with code %d %s", t.ExitCode, t.Reason)
		}
	}
	return "script pod failed"
}

// deleteStepChild best-effort deletes the running child of a timed-out step.
func (r *GryviaWorkflowReconciler) deleteStepChild(ctx context.Context, wf *gryviav1.GryviaWorkflow, step *gryviav1.WorkflowStep, ss *gryviav1.StepStatus) {
	if ss.JobName == "" {
		return
	}
	meta := metav1.ObjectMeta{Name: ss.JobName, Namespace: wf.Namespace}
	var obj client.Object
	switch stepKind(step) {
	case gryviav1.StepTypeJob:
		obj = &gryviav1.GryviaAIJob{ObjectMeta: meta}
	case gryviav1.StepTypeScript:
		obj = &corev1.Pod{ObjectMeta: meta}
	default:
		return
	}
	if err := r.Delete(ctx, obj); err != nil && !errors.IsNotFound(err) {
		r.Log.Error(err, "Failed to delete the child of a timed-out step", "step", step.Name)
	}
}

// callWebhook performs the HTTP call of a webhook step. Success is a 2xx answer, or the status code named by
// successCondition ("status == 200").
func (r *GryviaWorkflowReconciler) callWebhook(ctx context.Context, w *gryviav1.WebhookStep) (string, bool) {
	method := strings.ToUpper(w.Method)
	if method == "" {
		method = http.MethodPost
	}
	cctx, cancel := context.WithTimeout(ctx, webhookTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, method, w.URL, strings.NewReader(w.Body))
	if err != nil {
		return fmt.Sprintf("invalid webhook request: %v", err), false
	}
	for k, v := range w.Headers {
		req.Header.Set(k, v)
	}
	hc := r.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: webhookTimeout}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Sprintf("webhook call failed: %v", err), false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))

	want := 0
	if m := webhookStatusR.FindStringSubmatch(strings.TrimSpace(w.SuccessCondition)); m != nil {
		fmt.Sscanf(m[1], "%d", &want)
	}
	if (want != 0 && resp.StatusCode == want) || (want == 0 && resp.StatusCode >= 200 && resp.StatusCode < 300) {
		return fmt.Sprintf("Webhook answered %d", resp.StatusCode), true
	}
	return fmt.Sprintf("webhook answered %d", resp.StatusCode), false
}

// SetupWithManager sets up the controller with the Manager.
func (r *GryviaWorkflowReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gryviav1.GryviaWorkflow{}).
		Owns(&gryviav1.GryviaAIJob{}).
		Owns(&corev1.Pod{}).
		Complete(r)
}
