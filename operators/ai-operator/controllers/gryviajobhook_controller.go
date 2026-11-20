// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/source"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/ai-operator/pkg/jobhook"
)

// AnnotationJobHooks records, on each job, the deliveries of every hook for the job's current transition.
const AnnotationJobHooks = "gryvia.io/job-hooks"

const (
	jobHookSecretKey     = "secret"
	defaultHookAttempts  = 3
	defaultHookBackoff   = 10
	defaultHookTimeout   = 10
	maxHookMessageLength = 1000
)

// hookRecord is one hook's state for one job.
type hookRecord struct {
	Key      string `json:"k"`
	Attempts int32  `json:"n,omitempty"`
	Next     int64  `json:"next,omitempty"`
	Done     bool   `json:"done,omitempty"`
}

// jobView is the part of a GryviaAIJob or GryviaWorkflow a hook needs.
type jobView struct {
	obj       client.Object
	kind      string
	phase     string
	message   string
	run       int32
	key       string     // identifies the current transition
	changedAt *time.Time // when the job entered the phase, when known
}

func transitionTime(phase string, start, completion *metav1.Time) *time.Time {
	var t *metav1.Time
	switch phase {
	case "Running":
		t = start
	case "Succeeded", "Failed", "Cancelled", "Preempted", "Rejected":
		t = completion
	}
	if t == nil {
		return nil
	}
	v := t.Time
	return &v
}

func unixOf(t *metav1.Time) int64 {
	if t == nil {
		return 0
	}
	return t.Unix()
}

func aiJobView(j *gryviav1.GryviaAIJob) jobView {
	return jobView{obj: j, kind: "GryviaAIJob", phase: j.Status.Phase, message: j.Status.Message,
		key:       fmt.Sprintf("%s@%d", j.Status.Phase, unixOf(j.Status.StartTime)),
		changedAt: transitionTime(j.Status.Phase, j.Status.StartTime, j.Status.CompletionTime)}
}

func workflowView(w *gryviav1.GryviaWorkflow) jobView {
	return jobView{obj: w, kind: "GryviaWorkflow", phase: w.Status.Phase, message: w.Status.Message, run: w.Status.Run,
		key:       fmt.Sprintf("%s#%d@%d", w.Status.Phase, w.Status.Run, unixOf(w.Status.StartTime)),
		changedAt: transitionTime(w.Status.Phase, w.Status.StartTime, w.Status.CompletionTime)}
}

// GryviaJobHookReconciler validates hooks and delivers their webhooks when watched jobs change phase.
//
// With Workers > 0 the job reconcilers only decide which deliveries are due and queue them; Workers goroutines POST
// them, record the outcome on the job and enqueue the job again. A slow receiver then holds one worker, not a
// reconcile, and a full queue makes the job retry shortly. With Workers = 0 deliveries run inside the reconcile.
type GryviaJobHookReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Log    logr.Logger
	Guard  jobhook.Guard
	// Workers is the number of delivery goroutines; QueueSize bounds the deliveries waiting for one.
	Workers   int
	QueueSize int
	// Post overrides delivery (tests); nil = Guard.Post.
	Post func(ctx context.Context, r jobhook.Request) (int, error)
	// Now is the clock (tests); nil = time.Now.
	Now func() time.Time

	tasks    chan hookTask
	events   map[string]chan event.GenericEvent // per job kind: jobs to reconcile after a delivery
	mu       sync.Mutex
	inflight map[string]bool // delivery IDs queued or being posted
}

// hookTask is one POST of one hook for one job transition.
type hookTask struct {
	hook    *gryviav1.GryviaJobHook
	view    jobView
	attempt int32
}

// queueRetry is how soon a job is looked at again when the delivery queue is full.
const queueRetry = time.Second

func (r *GryviaJobHookReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// validateHook returns why a hook cannot deliver, or "".
func validateHook(h *gryviav1.GryviaJobHook) string {
	if len(h.Spec.Events) == 0 {
		return "spec.events is empty"
	}
	if err := jobhook.CheckURL(h.Spec.Webhook.URL); err != nil {
		return err.Error()
	}
	if err := jobhook.CheckHeaders(h.Spec.Webhook.Headers); err != nil {
		return err.Error()
	}
	if h.Spec.Selector != nil {
		if _, err := metav1.LabelSelectorAsSelector(h.Spec.Selector); err != nil {
			return "invalid selector: " + err.Error()
		}
	}
	return ""
}

// Reconcile sets the hook's Ready condition.
func (r *GryviaJobHookReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var h gryviav1.GryviaJobHook
	if err := r.Get(ctx, req.NamespacedName, &h); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	cond := metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Valid", Message: "Delivering",
		ObservedGeneration: h.Generation}
	var requeue time.Duration
	if msg := validateHook(&h); msg != "" {
		cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, "InvalidSpec", msg
	} else if ref := h.Spec.Webhook.SecretRef; ref != nil {
		if _, err := r.signingSecret(ctx, h.Namespace, ref.Name); err != nil {
			cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, "SecretMissing", err.Error()
			requeue = time.Minute
		}
	}
	if cond.Status == metav1.ConditionTrue && h.Spec.Suspend {
		cond.Reason, cond.Message = "Suspended", "Suspended; transitions are not delivered"
	}
	prior := meta.FindStatusCondition(h.Status.Conditions, "Ready")
	if prior == nil || prior.Status != cond.Status || prior.Reason != cond.Reason || prior.Message != cond.Message ||
		h.Status.ObservedGeneration != h.Generation {
		meta.SetStatusCondition(&h.Status.Conditions, cond)
		h.Status.ObservedGeneration = h.Generation
		if err := r.Status().Update(ctx, &h); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

func (r *GryviaJobHookReconciler) signingSecret(ctx context.Context, ns, name string) (string, error) {
	var s corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &s); err != nil {
		return "", fmt.Errorf("signing Secret %s: %v", name, err)
	}
	v := s.Data[jobHookSecretKey]
	if len(v) == 0 {
		return "", fmt.Errorf("signing Secret %s has no %q key", name, jobHookSecretKey)
	}
	return string(v), nil
}

func hookWatches(h *gryviav1.GryviaJobHook, v jobView) bool {
	if h.Spec.Suspend || validateHook(h) != "" {
		return false
	}
	if len(h.Spec.Kinds) > 0 {
		found := false
		for _, k := range h.Spec.Kinds {
			found = found || string(k) == v.kind
		}
		if !found {
			return false
		}
	}
	found := false
	for _, e := range h.Spec.Events {
		found = found || string(e) == v.phase
	}
	if !found {
		return false
	}
	if h.Spec.Selector != nil {
		sel, _ := metav1.LabelSelectorAsSelector(h.Spec.Selector)
		if !sel.Matches(labels.Set(v.obj.GetLabels())) {
			return false
		}
	}
	return true
}

// predates reports whether the transition happened before the hook existed (hooks are not backfilled).
func predates(v jobView, h *gryviav1.GryviaJobHook) bool {
	created := h.CreationTimestamp.Time.Truncate(time.Second)
	if v.changedAt != nil {
		return v.changedAt.Before(created)
	}
	return v.obj.GetCreationTimestamp().Time.Before(created)
}

func hookAttempts(h *gryviav1.GryviaJobHook) int32 {
	if h.Spec.Retry != nil && h.Spec.Retry.Attempts > 0 {
		return h.Spec.Retry.Attempts
	}
	return defaultHookAttempts
}

func hookBackoff(h *gryviav1.GryviaJobHook, attempt int32) time.Duration {
	base := int32(defaultHookBackoff)
	if h.Spec.Retry != nil && h.Spec.Retry.BackoffSeconds > 0 {
		base = h.Spec.Retry.BackoffSeconds
	}
	d := time.Duration(base) * time.Second
	for i := int32(1); i < attempt && d < time.Hour; i++ {
		d *= 2
	}
	if d > time.Hour {
		d = time.Hour
	}
	return d
}

// hookPayload is the gryvia delivery body.
type hookPayload struct {
	Hook      string            `json:"hook"`
	Event     string            `json:"event"`
	Kind      string            `json:"kind"`
	Namespace string            `json:"namespace"`
	Name      string            `json:"name"`
	UID       string            `json:"uid"`
	Message   string            `json:"message,omitempty"`
	Run       *int32            `json:"run,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
	Time      string            `json:"time"`
	Delivery  string            `json:"delivery"`
	Attempt   int32             `json:"attempt"`
}

func hookBody(h *gryviav1.GryviaJobHook, v jobView, delivery string, attempt int32, now time.Time) ([]byte, error) {
	msg := truncate(v.message, maxHookMessageLength)
	if h.Spec.Webhook.Format == "slack" {
		text := fmt.Sprintf("%s %s/%s %s", v.kind, v.obj.GetNamespace(), v.obj.GetName(), v.phase)
		if msg != "" {
			text += ": " + msg
		}
		return json.Marshal(map[string]string{"text": text})
	}
	at := now
	if v.changedAt != nil {
		at = *v.changedAt
	}
	p := hookPayload{Hook: h.Name, Event: v.phase, Kind: v.kind, Namespace: v.obj.GetNamespace(), Name: v.obj.GetName(),
		UID: string(v.obj.GetUID()), Message: msg, Labels: v.obj.GetLabels(), Time: at.UTC().Format(time.RFC3339),
		Delivery: delivery, Attempt: attempt}
	if v.kind == "GryviaWorkflow" {
		run := v.run
		p.Run = &run
	}
	return json.Marshal(p)
}

func deliveryID(v jobView, h *gryviav1.GryviaJobHook) string {
	sum := sha256.Sum256([]byte(string(v.obj.GetUID()) + "/" + v.key + "/" + h.Namespace + "/" + h.Name))
	return hex.EncodeToString(sum[:16])
}

// deliver starts the due deliveries of the hooks that watch the job's current transition, prunes the records of
// deleted hooks, and returns when to look again.
func (r *GryviaJobHookReconciler) deliver(ctx context.Context, v jobView) (time.Duration, error) {
	if v.phase == "" {
		return 0, nil
	}
	var hooks gryviav1.GryviaJobHookList
	if err := r.List(ctx, &hooks, client.InNamespace(v.obj.GetNamespace())); err != nil {
		return 0, err
	}
	records := parseRecords(v.obj)
	existing := map[string]bool{}
	sort.Slice(hooks.Items, func(i, j int) bool { return hooks.Items[i].Name < hooks.Items[j].Name })
	now := r.now()
	var requeue time.Duration
	soon := func(d time.Duration) {
		if requeue == 0 || d < requeue {
			requeue = d
		}
	}
	stale := false
	for name := range records {
		stale = stale || !hookExists(hooks.Items, name)
	}
	if stale {
		if err := r.updateRecords(ctx, v.obj, func(_ client.Object, recs map[string]hookRecord) {
			for name := range recs {
				if !hookExists(hooks.Items, name) {
					delete(recs, name)
				}
			}
		}); err != nil {
			return 0, err
		}
	}
	for i := range hooks.Items {
		h := &hooks.Items[i]
		existing[h.Name] = true
		rec := records[h.Name]
		if rec.Key != v.key {
			rec = hookRecord{Key: v.key}
		}
		if rec.Done || !hookWatches(h, v) || predates(v, h) {
			continue
		}
		if rec.Next > now.Unix() {
			soon(time.Unix(rec.Next, 0).Sub(now))
			continue
		}
		task := hookTask{hook: h.DeepCopy(), view: v, attempt: rec.Attempts + 1}
		if r.tasks == nil {
			done, err := r.run(ctx, task)
			if err != nil {
				return 0, err
			}
			if !done.Done && done.Next > 0 {
				soon(time.Unix(done.Next, 0).Sub(r.now()))
			}
			continue
		}
		id := deliveryID(v, h)
		r.mu.Lock()
		busy := r.inflight[id]
		if !busy {
			select {
			case r.tasks <- task:
				r.inflight[id] = true
			default:
				soon(queueRetry)
			}
		}
		r.mu.Unlock()
	}
	return requeue, nil
}

// viewOf is the hook view of a job object.
func viewOf(obj client.Object) (jobView, bool) {
	switch j := obj.(type) {
	case *gryviav1.GryviaAIJob:
		return aiJobView(j), true
	case *gryviav1.GryviaWorkflow:
		return workflowView(j), true
	}
	return jobView{}, false
}

func hookExists(hooks []gryviav1.GryviaJobHook, name string) bool {
	for i := range hooks {
		if hooks[i].Name == name {
			return true
		}
	}
	return false
}

func parseRecords(obj client.Object) map[string]hookRecord {
	records := map[string]hookRecord{}
	if raw := obj.GetAnnotations()[AnnotationJobHooks]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &records)
	}
	return records
}

// run POSTs one delivery, records the outcome on the job (unless the job has moved to another transition) and on the
// hook, and returns the job's new record for the hook.
func (r *GryviaJobHookReconciler) run(ctx context.Context, t hookTask) (hookRecord, error) {
	h, v := t.hook, t.view
	now := r.now()
	err := r.post(ctx, h, v, t.attempt, now)
	rec := hookRecord{Key: v.key, Attempts: t.attempt}
	d := gryviav1.JobHookDelivery{Kind: v.kind, Name: v.obj.GetName(), Event: v.phase, Time: metav1.NewTime(now),
		Attempt: t.attempt, Succeeded: err == nil}
	gaveUp := false
	switch {
	case err == nil:
		rec.Done = true
	case t.attempt >= hookAttempts(h):
		rec.Done, gaveUp = true, true
		d.Error = truncate(err.Error(), 300)
	default:
		rec.Next = now.Add(hookBackoff(h, t.attempt)).Unix()
		d.Error = truncate(err.Error(), 300)
	}
	if err != nil {
		r.Log.Info("job hook delivery failed", "hook", h.Name, "kind", v.kind, "job", v.obj.GetName(),
			"event", v.phase, "attempt", t.attempt, "error", err.Error())
	}
	werr := r.updateRecords(ctx, v.obj, func(cur client.Object, recs map[string]hookRecord) {
		if cv, ok := viewOf(cur); ok && cv.key == v.key {
			recs[h.Name] = rec
		}
	})
	if serr := r.recordDelivery(ctx, h, d, gaveUp); serr != nil {
		r.Log.Error(serr, "recording a job hook delivery", "hook", h.Name)
	}
	return rec, werr
}

// updateRecords applies change to the job's hook records, retrying on conflicts with the job's other writers.
func (r *GryviaJobHookReconciler) updateRecords(ctx context.Context, job client.Object, change func(client.Object, map[string]hookRecord)) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := job.DeepCopyObject().(client.Object)
		if err := r.Get(ctx, client.ObjectKeyFromObject(job), cur); err != nil {
			return err
		}
		records := parseRecords(cur)
		before, _ := json.Marshal(records)
		change(cur, records)
		after, _ := json.Marshal(records)
		if string(before) == string(after) {
			return nil
		}
		ann := cur.GetAnnotations()
		if ann == nil {
			ann = map[string]string{}
		}
		if len(records) == 0 {
			delete(ann, AnnotationJobHooks)
		} else {
			ann[AnnotationJobHooks] = string(after)
		}
		cur.SetAnnotations(ann)
		return r.Update(ctx, cur)
	})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func (r *GryviaJobHookReconciler) initQueue() {
	size := r.QueueSize
	if size <= 0 {
		size = 256
	}
	r.tasks = make(chan hookTask, size)
	r.inflight = map[string]bool{}
	r.events = map[string]chan event.GenericEvent{
		"GryviaAIJob": make(chan event.GenericEvent, size), "GryviaWorkflow": make(chan event.GenericEvent, size)}
}

// work POSTs queued deliveries until ctx ends.
func (r *GryviaJobHookReconciler) work(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-r.tasks:
			if _, err := r.run(ctx, t); err != nil && ctx.Err() == nil {
				r.Log.Error(err, "recording a job hook delivery on the job", "hook", t.hook.Name, "job", t.view.obj.GetName())
			}
			r.mu.Lock()
			delete(r.inflight, deliveryID(t.view, t.hook))
			r.mu.Unlock()
			select {
			case r.events[t.view.kind] <- event.GenericEvent{Object: t.view.obj}:
			case <-ctx.Done():
				return
			}
		}
	}
}

func (r *GryviaJobHookReconciler) post(ctx context.Context, h *gryviav1.GryviaJobHook, v jobView, attempt int32, now time.Time) error {
	secret := ""
	if ref := h.Spec.Webhook.SecretRef; ref != nil {
		s, err := r.signingSecret(ctx, h.Namespace, ref.Name)
		if err != nil {
			return err
		}
		secret = s
	}
	id := deliveryID(v, h)
	body, err := hookBody(h, v, id, attempt, now)
	if err != nil {
		return err
	}
	timeout := int32(defaultHookTimeout)
	if h.Spec.Webhook.TimeoutSeconds > 0 {
		timeout = h.Spec.Webhook.TimeoutSeconds
	}
	req := jobhook.Request{URL: h.Spec.Webhook.URL, Body: body, Headers: h.Spec.Webhook.Headers, Secret: secret,
		Event: v.phase, Delivery: id, Timeout: time.Duration(timeout) * time.Second}
	post := r.Post
	if post == nil {
		post = r.Guard.Post
	}
	_, err = post(ctx, req)
	return err
}

func (r *GryviaJobHookReconciler) recordDelivery(ctx context.Context, h *gryviav1.GryviaJobHook, d gryviav1.JobHookDelivery, gaveUp bool) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur gryviav1.GryviaJobHook
		if err := r.Get(ctx, client.ObjectKeyFromObject(h), &cur); err != nil {
			return client.IgnoreNotFound(err)
		}
		if d.Succeeded {
			cur.Status.Deliveries++
		}
		if gaveUp {
			cur.Status.Failures++
		}
		cur.Status.LastDelivery = &d
		return r.Status().Update(ctx, &cur)
	})
}

type aiJobHookReconciler struct{ *GryviaJobHookReconciler }

func (r aiJobHookReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var j gryviav1.GryviaAIJob
	if err := r.Get(ctx, req.NamespacedName, &j); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	wait, err := r.deliver(ctx, aiJobView(&j))
	return ctrl.Result{RequeueAfter: wait}, err
}

type workflowHookReconciler struct{ *GryviaJobHookReconciler }

func (r workflowHookReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var w gryviav1.GryviaWorkflow
	if err := r.Get(ctx, req.NamespacedName, &w); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	wait, err := r.deliver(ctx, workflowView(&w))
	return ctrl.Result{RequeueAfter: wait}, err
}

// SetupWithManager registers the hook validation controller, one delivery controller per watched kind and, with
// Workers > 0, the delivery workers.
func (r *GryviaJobHookReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := ctrl.NewControllerManagedBy(mgr).For(&gryviav1.GryviaJobHook{}).Complete(r); err != nil {
		return err
	}
	opts := controller.Options{MaxConcurrentReconciles: 4}
	aiJobs := ctrl.NewControllerManagedBy(mgr).Named("jobhook-aijob").For(&gryviav1.GryviaAIJob{}).WithOptions(opts)
	workflows := ctrl.NewControllerManagedBy(mgr).Named("jobhook-workflow").For(&gryviav1.GryviaWorkflow{}).WithOptions(opts)
	if r.Workers > 0 {
		r.initQueue()
		aiJobs = aiJobs.WatchesRawSource(source.Channel(r.events["GryviaAIJob"], &handler.EnqueueRequestForObject{}))
		workflows = workflows.WatchesRawSource(source.Channel(r.events["GryviaWorkflow"], &handler.EnqueueRequestForObject{}))
		if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
			var wg sync.WaitGroup
			for i := 0; i < r.Workers; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					r.work(ctx)
				}()
			}
			wg.Wait()
			return nil
		})); err != nil {
			return err
		}
	}
	if err := aiJobs.Complete(aiJobHookReconciler{r}); err != nil {
		return err
	}
	return workflows.Complete(workflowHookReconciler{r})
}
