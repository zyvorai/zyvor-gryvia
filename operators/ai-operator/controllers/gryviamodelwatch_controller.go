// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controllers

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
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
	"github.com/zyvorai/gryvia/operators/ai-operator/pkg/modelhub"
)

const (
	PhaseWatching  = "Watching"
	PhaseSuspended = "Suspended"

	ConditionWatchValid  = "Valid"
	ConditionHubReadable = "HubReadable"

	DefaultModelWatchPollInterval = time.Hour
	MinModelWatchPollInterval     = 5 * time.Minute

	maxWatchCandidates  = 200
	maxWatchSourceLimit = 100

	annotationSourceModel    = "gryvia.io/source-model"
	annotationSourceRevision = "gryvia.io/source-revision"
	labelModelWatch          = "gryvia.io/model-watch"
)

var slugInvalidRE = regexp.MustCompile(`[^a-z0-9-]+`)

// ModelLister lists models from a hub (modelhub.Client in production, a fake in tests).
type ModelLister interface {
	List(ctx context.Context, q modelhub.Query, token string) ([]modelhub.Model, error)
}

// GryviaModelWatchReconciler reconciles a GryviaModelWatch object.
//
// Every pollInterval it lists the most recently modified models of each source, keeps the ones passing the
// filters (name, task, downloads, license, size, gated without a token) and creates one GryviaWorkflow per new
// model from spec.workflowTemplate, owned by the watch, at most maxConcurrentRuns at a time. The first poll records
// what already exists as the baseline unless includeExisting is set.
type GryviaModelWatchReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Log    logr.Logger

	// Hub lists models; required.
	Hub ModelLister
	// MinPollInterval is the shortest pollInterval honoured (5m when 0).
	MinPollInterval time.Duration
	// Clock returns the current time (time.Now when nil); tests move it.
	Clock func() time.Time
}

//+kubebuilder:rbac:groups=gryvia.io,resources=gryviamodelwatches,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviamodelwatches/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviamodelwatches/finalizers,verbs=update
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaworkflows,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get

// Reconcile drives one GryviaModelWatch.
func (r *GryviaModelWatchReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("gryviamodelwatch", req.NamespacedName)

	w := &gryviav1.GryviaModelWatch{}
	if err := r.Get(ctx, req.NamespacedName, w); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !w.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil // workflows are garbage-collected through their owner reference
	}
	orig := w.DeepCopy()
	res, err := r.reconcileWatch(ctx, w)
	if perr := patchStatus(ctx, r.Client, w, orig); perr != nil {
		log.Error(perr, "Failed to patch model watch status")
		if err == nil {
			err = perr
		}
	}
	return res, err
}

func (r *GryviaModelWatchReconciler) pollInterval(w *gryviav1.GryviaModelWatch) time.Duration {
	d := DefaultModelWatchPollInterval
	if w.Spec.PollInterval != nil && w.Spec.PollInterval.Duration > 0 {
		d = w.Spec.PollInterval.Duration
	}
	floor := r.MinPollInterval
	if floor <= 0 {
		floor = MinModelWatchPollInterval
	}
	if d < floor {
		d = floor
	}
	return d
}

// validateWatch rejects a watch that can never work.
func validateWatch(w *gryviav1.GryviaModelWatch) error {
	if len(w.Spec.Sources) == 0 {
		return fmt.Errorf("spec.sources is empty")
	}
	for i, s := range w.Spec.Sources {
		if s.Provider != gryviav1.ProviderHuggingFace && s.Provider != gryviav1.ProviderNGC {
			return fmt.Errorf("sources[%d]: unknown provider %q", i, s.Provider)
		}
		if s.Author == "" {
			return fmt.Errorf("sources[%d]: author is required", i)
		}
		if s.NameRegex != "" {
			if _, err := regexp.Compile(s.NameRegex); err != nil {
				return fmt.Errorf("sources[%d]: nameRegex: %v", i, err)
			}
		}
	}
	if w.Spec.MaxParamsB < 0 || w.Spec.MaxConcurrentRuns < 0 {
		return fmt.Errorf("maxParamsB and maxConcurrentRuns must not be negative")
	}
	probe := &gryviav1.GryviaWorkflow{Spec: *w.Spec.WorkflowTemplate.DeepCopy()}
	if err := validateWorkflow(probe); err != nil {
		return fmt.Errorf("workflowTemplate: %w", err)
	}
	if sz := w.Spec.Sizing; sz != nil {
		for _, name := range sz.AutoSizeSteps {
			found := false
			for _, s := range w.Spec.WorkflowTemplate.Steps {
				if s.Name == name && s.JobTemplate != nil {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("sizing.autoSizeSteps: %q is not a job step of the template", name)
			}
		}
	}
	return nil
}

func (r *GryviaModelWatchReconciler) reconcileWatch(ctx context.Context, w *gryviav1.GryviaModelWatch) (ctrl.Result, error) {
	now := clock(r.Clock)

	if err := validateWatch(w); err != nil {
		w.Status.Phase = PhaseFailed
		w.Status.Message = err.Error()
		setCondition(&w.Status.Conditions, w.Generation, ConditionWatchValid, metav1.ConditionFalse, "Invalid", err.Error())
		return ctrl.Result{}, nil
	}
	setCondition(&w.Status.Conditions, w.Generation, ConditionWatchValid, metav1.ConditionTrue, "Valid", "The watch is valid")

	if err := r.syncRuns(ctx, w); err != nil {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, err
	}

	if w.Spec.Suspend {
		w.Status.Phase = PhaseSuspended
		w.Status.Message = "Suspended: not polling or launching"
		w.Status.NextPollTime = nil
		return ctrl.Result{}, nil
	}
	w.Status.Phase = PhaseWatching

	interval := r.pollInterval(w)
	if w.Status.NextPollTime == nil || !now.Before(w.Status.NextPollTime.Time) {
		next := now.Add(interval)
		if err := r.poll(ctx, w, now); err != nil {
			w.Status.Message = err.Error()
			setCondition(&w.Status.Conditions, w.Generation, ConditionHubReadable, metav1.ConditionFalse, "PollFailed", err.Error())
			if retry := now.Add(MinModelWatchPollInterval); retry.Before(next) {
				next = retry
			}
		} else {
			w.Status.Message = ""
			setCondition(&w.Status.Conditions, w.Generation, ConditionHubReadable, metav1.ConditionTrue, "Polled", "The hub was read")
		}
		t := metav1.NewTime(next)
		w.Status.NextPollTime = &t
	}

	if err := r.launchQueued(ctx, w, now); err != nil {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, err
	}

	wait := w.Status.NextPollTime.Sub(now)
	if w.Status.ActiveRuns > 0 && wait > time.Minute {
		wait = time.Minute // workflows are watched too; this only bounds a missed event
	}
	if wait < time.Second {
		wait = time.Second
	}
	return ctrl.Result{RequeueAfter: wait}, nil
}

// syncRuns mirrors the phase of each candidate's workflow and counts the active ones.
func (r *GryviaModelWatchReconciler) syncRuns(ctx context.Context, w *gryviav1.GryviaModelWatch) error {
	active := int32(0)
	for i := range w.Status.Candidates {
		c := &w.Status.Candidates[i]
		if c.Phase != gryviav1.CandidateRunning {
			continue
		}
		wf := &gryviav1.GryviaWorkflow{}
		err := r.Get(ctx, types.NamespacedName{Namespace: w.Namespace, Name: c.Workflow}, wf)
		switch {
		case errors.IsNotFound(err):
			c.Phase = gryviav1.CandidateFailed
			c.Message = "workflow was deleted"
		case err != nil:
			return err
		case wf.Status.Phase == PhaseSucceeded:
			c.Phase = gryviav1.CandidateSucceeded
			c.Message = ""
		case wf.Status.Phase == PhaseFailed:
			c.Phase = gryviav1.CandidateFailed
			c.Message = wf.Status.Message
		default:
			active++
		}
	}
	w.Status.ActiveRuns = active
	return nil
}

func (r *GryviaModelWatchReconciler) token(ctx context.Context, w *gryviav1.GryviaModelWatch) (string, error) {
	ref := w.Spec.TokenSecretRef
	if ref == nil {
		return "", nil
	}
	sec := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: w.Namespace, Name: ref.Name}, sec); err != nil {
		return "", fmt.Errorf("token secret %q: %w", ref.Name, err)
	}
	k := ref.Key
	if k == "" {
		k = "token"
	}
	v, ok := sec.Data[k]
	if !ok || len(v) == 0 {
		return "", fmt.Errorf("token secret %q has no key %q", ref.Name, k)
	}
	return strings.TrimSpace(string(v)), nil
}

// poll lists every source and records new candidates (newest first).
func (r *GryviaModelWatchReconciler) poll(ctx context.Context, w *gryviav1.GryviaModelWatch, now time.Time) error {
	if r.Hub == nil {
		return fmt.Errorf("no model hub client configured")
	}
	token, err := r.token(ctx, w)
	if err != nil {
		return err
	}
	baseline := w.Status.LastPollTime == nil && !w.Spec.IncludeExisting
	index := make(map[string]int, len(w.Status.Candidates))
	for i, c := range w.Status.Candidates {
		index[c.ID] = i
	}

	var fresh []gryviav1.ModelCandidate
	var unsupported []string
	for _, src := range w.Spec.Sources {
		if src.Provider != gryviav1.ProviderHuggingFace {
			unsupported = append(unsupported, string(src.Provider))
			continue
		}
		limit := int(src.Limit)
		if limit <= 0 {
			limit = 20
		}
		if limit > maxWatchSourceLimit {
			limit = maxWatchSourceLimit
		}
		models, err := r.Hub.List(ctx, modelhub.Query{Author: src.Author, PipelineTag: src.PipelineTag, Limit: limit}, token)
		if err != nil {
			return fmt.Errorf("listing %s models: %w", src.Author, err)
		}
		var nameRE *regexp.Regexp
		if src.NameRegex != "" {
			nameRE = regexp.MustCompile(src.NameRegex) // validated
		}
		for _, m := range models {
			if !modelhub.ValidID(m.ID) || !strings.EqualFold(strings.SplitN(m.ID, "/", 2)[0], src.Author) {
				continue
			}
			if nameRE != nil && !nameRE.MatchString(m.Name()) {
				continue
			}
			if src.PipelineTag != "" && m.PipelineTag != "" && m.PipelineTag != src.PipelineTag {
				continue
			}
			if m.Downloads < src.MinDownloads {
				continue
			}
			rev := m.SHA
			if !modelhub.ValidSHA(rev) {
				rev = ""
			}
			if i, seen := index[m.ID]; seen {
				c := &w.Status.Candidates[i]
				if w.Spec.RetrainOnNewRevision && rev != "" && rev != c.Revision && !baseline &&
					c.Phase != gryviav1.CandidateQueued && c.Phase != gryviav1.CandidateRunning {
					*c = r.evaluate(w, m, rev, metav1.NewTime(now))
				}
				continue
			}
			index[m.ID] = -1
			c := r.evaluate(w, m, rev, metav1.NewTime(now))
			if baseline {
				c.Phase = gryviav1.CandidateBaseline
				c.Message = "existed at the first poll"
			}
			fresh = append(fresh, c)
		}
	}
	w.Status.Candidates = trimCandidates(append(fresh, w.Status.Candidates...))
	t := metav1.NewTime(now)
	w.Status.LastPollTime = &t
	if len(unsupported) > 0 {
		return fmt.Errorf("provider(s) %s not supported yet; huggingface sources were polled", strings.Join(uniq(unsupported), ", "))
	}
	return nil
}

// evaluate applies the license, size and gating filters and sizes the run.
func (r *GryviaModelWatchReconciler) evaluate(w *gryviav1.GryviaModelWatch, m modelhub.Model, rev string, seen metav1.Time) gryviav1.ModelCandidate {
	params := m.EstimateParams()
	c := gryviav1.ModelCandidate{ID: m.ID, Revision: rev, License: m.License(), FirstSeen: seen, Phase: gryviav1.CandidateQueued}
	if params > 0 {
		c.ParamsB = formatParamsB(params)
	}
	reject := func(msg string) gryviav1.ModelCandidate {
		c.Phase = gryviav1.CandidateRejected
		c.Message = msg
		return c
	}
	if len(w.Spec.LicenseAllowlist) > 0 && !containsFold(w.Spec.LicenseAllowlist, c.License) {
		if c.License == "" {
			return reject("no license tag; licenseAllowlist is set")
		}
		return reject(fmt.Sprintf("license %q is not in licenseAllowlist", c.License))
	}
	if w.Spec.MaxParamsB > 0 {
		if params == 0 {
			return reject("parameter count unknown; maxParamsB is set")
		}
		if params > int64(w.Spec.MaxParamsB)*1e9 {
			return reject(fmt.Sprintf("%sB parameters exceeds maxParamsB %d", c.ParamsB, w.Spec.MaxParamsB))
		}
	}
	if m.Gated && w.Spec.TokenSecretRef == nil {
		return reject("gated model; set tokenSecretRef to a token that has accepted its terms")
	}
	gpus, err := estimateGPUs(params, w.Spec.Sizing)
	if err != nil {
		return reject(err.Error())
	}
	c.GPUs = gpus
	return c
}

// estimateGPUs sizes a LoRA fine-tune: bf16 weights (2 bytes per parameter) times the overhead, over GPUs of
// gpuMemoryGB, rounded up to 1, 2, 4, 8 and then multiples of 8. An unknown size gets one GPU.
func estimateGPUs(params int64, s *gryviav1.ModelSizing) (int32, error) {
	memGB, overhead, maxGPUs := 80.0, 150.0, int32(8)
	if s != nil {
		if s.GPUMemoryGB > 0 {
			memGB = float64(s.GPUMemoryGB)
		}
		if s.OverheadPercent > 0 {
			overhead = float64(s.OverheadPercent)
		}
		if s.MaxGPUs > 0 {
			maxGPUs = s.MaxGPUs
		}
	}
	if params <= 0 {
		return 1, nil
	}
	needGB := float64(params) * 2 / 1e9 * overhead / 100
	n := int32(math.Ceil(needGB / memGB))
	switch {
	case n <= 1:
		n = 1
	case n <= 2:
		n = 2
	case n <= 4:
		n = 4
	default:
		n = (n + 7) / 8 * 8
	}
	if n > maxGPUs {
		return 0, fmt.Errorf("needs about %d GPUs of %.0fGB (%.0fGB), more than sizing.maxGPUs %d", n, memGB, needGB, maxGPUs)
	}
	return n, nil
}

func formatParamsB(params int64) string {
	return strconv.FormatFloat(math.Round(float64(params)/1e7)/100, 'f', -1, 64)
}

func containsFold(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

// trimCandidates keeps at most maxWatchCandidates, dropping the oldest finished ones first.
func trimCandidates(in []gryviav1.ModelCandidate) []gryviav1.ModelCandidate {
	for i := len(in) - 1; len(in) > maxWatchCandidates && i >= 0; i-- {
		if p := in[i].Phase; p != gryviav1.CandidateQueued && p != gryviav1.CandidateRunning {
			in = append(in[:i], in[i+1:]...)
		}
	}
	return in
}

// modelSlug is a DNS-label form of the model name.
func modelSlug(name string) string {
	s := strings.Trim(slugInvalidRE.ReplaceAllString(strings.ToLower(strings.ReplaceAll(name, ".", "-")), "-"), "-")
	if len(s) > 40 {
		s = strings.TrimRight(s[:40], "-")
	}
	if s == "" {
		s = "model"
	}
	return s
}

// launchQueued starts the oldest queued candidates while fewer than maxConcurrentRuns are active.
func (r *GryviaModelWatchReconciler) launchQueued(ctx context.Context, w *gryviav1.GryviaModelWatch, now time.Time) error {
	limit := w.Spec.MaxConcurrentRuns
	if limit <= 0 {
		limit = 1
	}
	for i := len(w.Status.Candidates) - 1; i >= 0 && w.Status.ActiveRuns < limit; i-- {
		c := &w.Status.Candidates[i]
		if c.Phase != gryviav1.CandidateQueued {
			continue
		}
		wf, err := r.buildWorkflow(w, c)
		if err != nil {
			c.Phase = gryviav1.CandidateRejected
			c.Message = err.Error()
			continue
		}
		if err := r.Create(ctx, wf); err != nil {
			if !errors.IsAlreadyExists(err) {
				if errors.IsInvalid(err) || errors.IsForbidden(err) || errors.IsBadRequest(err) {
					c.Phase = gryviav1.CandidateFailed
					c.Message = fmt.Sprintf("cannot create the workflow: %v", err)
					continue
				}
				return err
			}
			existing := &gryviav1.GryviaWorkflow{}
			if gerr := r.Get(ctx, types.NamespacedName{Namespace: w.Namespace, Name: wf.Name}, existing); gerr != nil {
				return gerr
			}
			if !metav1.IsControlledBy(existing, w) {
				c.Phase = gryviav1.CandidateFailed
				c.Message = fmt.Sprintf("workflow %q already exists and is not owned by this watch", wf.Name)
				continue
			}
		}
		c.Phase = gryviav1.CandidateRunning
		c.Workflow = wf.Name
		c.Message = ""
		w.Status.ActiveRuns++
	}
	return nil
}

// buildWorkflow renders the template for one candidate.
func (r *GryviaModelWatchReconciler) buildWorkflow(w *gryviav1.GryviaModelWatch, c *gryviav1.ModelCandidate) (*gryviav1.GryviaWorkflow, error) {
	m := modelhub.Model{ID: c.ID}
	slug := modelSlug(m.Name())
	gpus := c.GPUs
	if gpus <= 0 {
		gpus = 1
	}
	vals := map[string]string{
		"model.id":       c.ID,
		"model.name":     m.Name(),
		"model.slug":     slug,
		"model.revision": c.Revision,
		"model.paramsB":  c.ParamsB,
		"model.license":  c.License,
		"model.gpus":     strconv.Itoa(int(gpus)),
	}
	var spec gryviav1.GryviaWorkflowSpec
	if err := renderInto(w.Spec.WorkflowTemplate, &spec, mapLookup(vals)); err != nil {
		return nil, fmt.Errorf("rendering workflowTemplate: %w", err)
	}
	if spec.Parameters == nil {
		spec.Parameters = map[string]string{}
	}
	for k, v := range map[string]string{
		"MODEL_ID": c.ID, "MODEL_REVISION": c.Revision, "MODEL_PARAMS_B": c.ParamsB, "MODEL_GPUS": vals["model.gpus"],
	} {
		if _, set := spec.Parameters[k]; !set {
			spec.Parameters[k] = v
		}
	}
	if sz := w.Spec.Sizing; sz != nil {
		for i := range spec.Steps {
			s := &spec.Steps[i]
			if s.JobTemplate != nil && s.JobTemplate.GPUs == 0 && contains(sz.AutoSizeSteps, s.Name) {
				s.JobTemplate.GPUs = gpus
			}
		}
	}
	parts := []string{w.Name, slug}
	if len(c.Revision) >= 7 {
		parts = append(parts, c.Revision[:7])
	}
	wf := &gryviav1.GryviaWorkflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:      childName(parts...),
			Namespace: w.Namespace,
			Labels:    map[string]string{labelModelWatch: childName(w.Name), "gryvia.io/component": "model-watch-run"},
			Annotations: map[string]string{
				annotationSourceModel:    c.ID,
				annotationSourceRevision: c.Revision,
			},
		},
		Spec: spec,
	}
	if err := validateWorkflow(wf); err != nil {
		return nil, fmt.Errorf("the rendered workflow is invalid: %w", err)
	}
	if err := controllerutil.SetControllerReference(w, wf, r.Scheme); err != nil {
		return nil, err
	}
	return wf, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *GryviaModelWatchReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gryviav1.GryviaModelWatch{}).
		Owns(&gryviav1.GryviaWorkflow{}).
		Complete(r)
}
