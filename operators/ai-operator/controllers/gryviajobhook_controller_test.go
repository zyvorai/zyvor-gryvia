// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/ai-operator/pkg/jobhook"
)

var hookT0 = time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)

func testHook(name string, events ...gryviav1.JobHookEvent) *gryviav1.GryviaJobHook {
	return &gryviav1.GryviaJobHook{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team", Generation: 1,
			CreationTimestamp: metav1.NewTime(hookT0.Add(-time.Hour))},
		Spec: gryviav1.GryviaJobHookSpec{Events: events,
			Webhook: gryviav1.JobHookWebhook{URL: "https://hooks.example.com/x"}},
	}
}

func failedJob(name string, labels map[string]string) *gryviav1.GryviaAIJob {
	start, end := metav1.NewTime(hookT0.Add(-10*time.Minute)), metav1.NewTime(hookT0.Add(-time.Minute))
	return &gryviav1.GryviaAIJob{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team", UID: types.UID("uid-" + name), Labels: labels,
			CreationTimestamp: metav1.NewTime(hookT0.Add(-20 * time.Minute))},
		Status: gryviav1.GryviaAIJobStatus{Phase: "Failed", Message: "OOMKilled", StartTime: &start, CompletionTime: &end},
	}
}

type postRecorder struct {
	reqs []jobhook.Request
	fail int // number of calls to fail before succeeding; -1 = always
}

func (p *postRecorder) post(_ context.Context, r jobhook.Request) (int, error) {
	p.reqs = append(p.reqs, r)
	if p.fail != 0 {
		if p.fail > 0 {
			p.fail--
		}
		return 503, errors.New("webhook returned HTTP 503")
	}
	return 204, nil
}

func hookEnv(t *testing.T, objs ...client.Object) (*GryviaJobHookReconciler, *postRecorder, *time.Time) {
	t.Helper()
	b := fake.NewClientBuilder().WithScheme(mlScheme()).WithObjects(objs...)
	for _, o := range objs {
		if _, ok := o.(*corev1.Secret); !ok {
			b = b.WithStatusSubresource(o)
		}
	}
	now := hookT0
	p := &postRecorder{}
	r := &GryviaJobHookReconciler{Client: b.Build(), Scheme: mlScheme(), Log: ctrl.Log, Post: p.post,
		Now: func() time.Time { return now }}
	return r, p, &now
}

func reconcileAIJob(t *testing.T, r *GryviaJobHookReconciler, name string) ctrl.Result {
	t.Helper()
	res, err := aiJobHookReconciler{r}.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "team", Name: name}})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func getHook(t *testing.T, r *GryviaJobHookReconciler, name string) *gryviav1.GryviaJobHook {
	t.Helper()
	var h gryviav1.GryviaJobHook
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "team", Name: name}, &h); err != nil {
		t.Fatal(err)
	}
	return &h
}

func TestJobHook_DeliversOncePerTransition(t *testing.T) {
	h := testHook("notify", "Failed")
	h.Spec.Webhook.Headers = map[string]string{"X-Team": "nlp"}
	r, p, _ := hookEnv(t, h, failedJob("train", map[string]string{"team": "nlp"}))

	reconcileAIJob(t, r, "train")
	reconcileAIJob(t, r, "train")
	if len(p.reqs) != 1 {
		t.Fatalf("deliveries = %d, want 1", len(p.reqs))
	}
	req := p.reqs[0]
	var body map[string]interface{}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body["event"] != "Failed" || body["kind"] != "GryviaAIJob" || body["name"] != "train" || body["namespace"] != "team" ||
		body["message"] != "OOMKilled" || body["uid"] != "uid-train" || body["hook"] != "notify" ||
		body["time"] != "2026-10-02T09:59:00Z" || body["attempt"] != float64(1) || body["delivery"] != req.Delivery {
		t.Fatalf("body = %v", body)
	}
	if _, ok := body["run"]; ok {
		t.Fatal("run set for an AI job")
	}
	if req.Event != "Failed" || req.Headers["X-Team"] != "nlp" || req.Secret != "" || req.Timeout != 10*time.Second {
		t.Fatalf("request = %+v", req)
	}

	got := getHook(t, r, "notify")
	if got.Status.Deliveries != 1 || got.Status.LastDelivery == nil || !got.Status.LastDelivery.Succeeded {
		t.Fatalf("status = %+v", got.Status)
	}
	var j gryviav1.GryviaAIJob
	_ = r.Get(context.Background(), types.NamespacedName{Namespace: "team", Name: "train"}, &j)
	if j.Annotations[AnnotationJobHooks] == "" {
		t.Fatal("delivery not recorded on the job")
	}

	// A new transition (the job is retried and fails again) is delivered again.
	start := metav1.NewTime(hookT0)
	j.Status.StartTime = &start
	end := metav1.NewTime(hookT0.Add(time.Minute))
	j.Status.CompletionTime = &end
	if err := r.Status().Update(context.Background(), &j); err != nil {
		t.Fatal(err)
	}
	reconcileAIJob(t, r, "train")
	if len(p.reqs) != 2 || p.reqs[1].Delivery == req.Delivery {
		t.Fatalf("second transition: %d deliveries", len(p.reqs))
	}
	reconcileAIJob(t, r, "train")
	if len(p.reqs) != 2 {
		t.Fatalf("second transition delivered again: %d deliveries", len(p.reqs))
	}
}

// blockingPost holds the POSTs to slowURL until release is closed and records every request.
type blockingPost struct {
	mu      sync.Mutex
	urls    []string
	slowURL string
	started chan struct{}
	release chan struct{}
}

func (b *blockingPost) post(ctx context.Context, r jobhook.Request) (int, error) {
	if r.URL == b.slowURL {
		b.started <- struct{}{}
		select {
		case <-b.release:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	b.mu.Lock()
	b.urls = append(b.urls, r.URL)
	b.mu.Unlock()
	return 204, nil
}

func (b *blockingPost) delivered() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.urls...)
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestJobHook_WorkersDoNotBlockOnASlowReceiver(t *testing.T) {
	slow, fast := testHook("a-slow", "Failed"), testHook("b-fast", "Failed")
	slow.Spec.Webhook.URL = "https://slow.example.com/"
	r, _, _ := hookEnv(t, slow, fast, failedJob("train", nil), failedJob("eval", nil))
	b := &blockingPost{slowURL: slow.Spec.Webhook.URL, started: make(chan struct{}, 4), release: make(chan struct{})}
	r.Post, r.Workers = b.post, 3
	r.initQueue()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := 0; i < r.Workers; i++ {
		go r.work(ctx)
	}

	start := time.Now()
	reconcileAIJob(t, r, "train")
	reconcileAIJob(t, r, "eval")
	if d := time.Since(start); d > time.Second {
		t.Fatalf("reconciles waited for the receivers: %v", d)
	}
	<-b.started
	<-b.started
	waitFor(t, "the fast hook for both jobs", func() bool { return len(b.delivered()) == 2 })

	// Queued or in-flight deliveries are not queued twice.
	reconcileAIJob(t, r, "train")
	if n := len(r.tasks); n != 0 {
		t.Fatalf("a delivery in flight was queued again (%d queued)", n)
	}
	close(b.release)
	waitFor(t, "the slow hook", func() bool { return len(b.delivered()) == 4 })

	// Each finished delivery enqueues its job again, and the records stop further POSTs.
	waitFor(t, "four job events", func() bool { return len(r.events["GryviaAIJob"]) == 4 })
	reconcileAIJob(t, r, "train")
	reconcileAIJob(t, r, "eval")
	time.Sleep(50 * time.Millisecond)
	if n := len(b.delivered()); n != 4 {
		t.Fatalf("deliveries = %d, want 4", n)
	}
	if got := getHook(t, r, "a-slow"); got.Status.Deliveries != 2 {
		t.Fatalf("slow hook status = %+v", got.Status)
	}
}

func TestJobHook_FullQueueRetriesSoon(t *testing.T) {
	r, _, _ := hookEnv(t, testHook("a", "Failed"), testHook("b", "Failed"), failedJob("train", nil))
	r.Workers, r.QueueSize = 1, 1
	r.initQueue()
	res := reconcileAIJob(t, r, "train")
	if len(r.tasks) != 1 || res.RequeueAfter != queueRetry {
		t.Fatalf("queued %d, requeue %v", len(r.tasks), res.RequeueAfter)
	}
}

func TestJobHook_OutcomeOfAnOldTransitionIsNotRecorded(t *testing.T) {
	h := testHook("notify", "Failed")
	r, p, _ := hookEnv(t, h, failedJob("train", nil))
	var j gryviav1.GryviaAIJob
	_ = r.Get(context.Background(), types.NamespacedName{Namespace: "team", Name: "train"}, &j)
	stale := aiJobView(j.DeepCopy())
	// The job is retried before the delivery for its first failure finishes.
	j.Status.Phase = "Running"
	if err := r.Status().Update(context.Background(), &j); err != nil {
		t.Fatal(err)
	}
	if _, err := r.run(context.Background(), hookTask{hook: h, view: stale, attempt: 1}); err != nil {
		t.Fatal(err)
	}
	_ = r.Get(context.Background(), types.NamespacedName{Namespace: "team", Name: "train"}, &j)
	if len(p.reqs) != 1 || j.Annotations[AnnotationJobHooks] != "" {
		t.Fatalf("posts %d, records %q", len(p.reqs), j.Annotations[AnnotationJobHooks])
	}
}

func TestJobHook_RetriesThenGivesUp(t *testing.T) {
	h := testHook("notify", "Failed")
	h.Spec.Retry = &gryviav1.JobHookRetry{Attempts: 3, BackoffSeconds: 5}
	r, p, now := hookEnv(t, h, failedJob("train", nil))
	p.fail = -1

	res := reconcileAIJob(t, r, "train")
	if len(p.reqs) != 1 || res.RequeueAfter != 5*time.Second {
		t.Fatalf("first attempt: %d, requeue %v", len(p.reqs), res.RequeueAfter)
	}
	reconcileAIJob(t, r, "train") // still backing off
	if len(p.reqs) != 1 {
		t.Fatal("retried before the backoff")
	}
	*now = now.Add(5 * time.Second)
	res = reconcileAIJob(t, r, "train")
	if len(p.reqs) != 2 || res.RequeueAfter != 10*time.Second {
		t.Fatalf("second attempt: %d, requeue %v", len(p.reqs), res.RequeueAfter)
	}
	*now = now.Add(10 * time.Second)
	reconcileAIJob(t, r, "train")
	*now = now.Add(time.Hour)
	reconcileAIJob(t, r, "train")
	if len(p.reqs) != 3 {
		t.Fatalf("attempts = %d, want 3", len(p.reqs))
	}
	got := getHook(t, r, "notify")
	if got.Status.Failures != 1 || got.Status.Deliveries != 0 || got.Status.LastDelivery.Succeeded ||
		got.Status.LastDelivery.Attempt != 3 || got.Status.LastDelivery.Error == "" {
		t.Fatalf("status = %+v", got.Status)
	}
}

func TestJobHook_FiltersAndNoBackfill(t *testing.T) {
	old := failedJob("old", nil)
	end := metav1.NewTime(hookT0.Add(-2 * time.Hour)) // failed before the hook was created
	old.Status.CompletionTime = &end

	selected := testHook("selected", "Failed")
	selected.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"team": "cv"}}
	workflowsOnly := testHook("workflows", "Failed")
	workflowsOnly.Spec.Kinds = []gryviav1.JobHookKind{"GryviaWorkflow"}
	succeeded := testHook("succeeded", "Succeeded")
	suspended := testHook("suspended", "Failed")
	suspended.Spec.Suspend = true
	invalid := testHook("invalid", "Failed")
	invalid.Spec.Webhook.URL = "https://user:pw@hooks.example.com"
	other := testHook("other-ns", "Failed")
	other.Namespace = "elsewhere"

	r, p, _ := hookEnv(t, old, failedJob("train", map[string]string{"team": "nlp"}),
		selected, workflowsOnly, succeeded, suspended, invalid, other)
	reconcileAIJob(t, r, "old")
	reconcileAIJob(t, r, "train")
	if len(p.reqs) != 0 {
		t.Fatalf("unexpected deliveries: %d", len(p.reqs))
	}
}

func TestJobHook_SignsAndSlackFormat(t *testing.T) {
	h := testHook("slack", "Failed")
	h.Spec.Webhook.Format = "slack"
	h.Spec.Webhook.SecretRef = &gryviav1.JobHookSecretRef{Name: "hook-secret"}
	h.Spec.Webhook.TimeoutSeconds = 3
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "hook-secret", Namespace: "team"},
		Data: map[string][]byte{"secret": []byte("s3cret")}}
	r, p, _ := hookEnv(t, h, sec, failedJob("train", nil))
	reconcileAIJob(t, r, "train")
	if len(p.reqs) != 1 {
		t.Fatal("not delivered")
	}
	if string(p.reqs[0].Body) != `{"text":"GryviaAIJob team/train Failed: OOMKilled"}` || p.reqs[0].Secret != "s3cret" ||
		p.reqs[0].Timeout != 3*time.Second {
		t.Fatalf("request = %+v body %s", p.reqs[0], p.reqs[0].Body)
	}
}

func TestJobHook_MissingSecretCountsAsFailedAttempt(t *testing.T) {
	h := testHook("signed", "Failed")
	h.Spec.Webhook.SecretRef = &gryviav1.JobHookSecretRef{Name: "absent"}
	h.Spec.Retry = &gryviav1.JobHookRetry{Attempts: 1}
	r, p, _ := hookEnv(t, h, failedJob("train", nil))
	reconcileAIJob(t, r, "train")
	if len(p.reqs) != 0 {
		t.Fatal("posted without the signing secret")
	}
	if got := getHook(t, r, "signed"); got.Status.Failures != 1 {
		t.Fatalf("status = %+v", got.Status)
	}
}

func TestJobHook_WorkflowRuns(t *testing.T) {
	start, end := metav1.NewTime(hookT0.Add(-5*time.Minute)), metav1.NewTime(hookT0.Add(-time.Minute))
	w := &gryviav1.GryviaWorkflow{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "team", UID: "uid-wf",
			CreationTimestamp: metav1.NewTime(hookT0.Add(-30 * time.Minute))},
		Status: gryviav1.GryviaWorkflowStatus{Phase: "Succeeded", Run: 4, StartTime: &start, CompletionTime: &end},
	}
	r, p, _ := hookEnv(t, testHook("done", "Succeeded"), w)
	rec := func() {
		if _, err := (workflowHookReconciler{r}).Reconcile(context.Background(),
			ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "team", Name: "nightly"}}); err != nil {
			t.Fatal(err)
		}
	}
	rec()
	rec()
	if len(p.reqs) != 1 {
		t.Fatalf("deliveries = %d", len(p.reqs))
	}
	var body map[string]interface{}
	_ = json.Unmarshal(p.reqs[0].Body, &body)
	if body["kind"] != "GryviaWorkflow" || body["run"] != float64(4) {
		t.Fatalf("body = %v", body)
	}
	// The next scheduled run succeeds too.
	var cur gryviav1.GryviaWorkflow
	_ = r.Get(context.Background(), client.ObjectKeyFromObject(w), &cur)
	cur.Status.Run = 5
	s2, e2 := metav1.NewTime(hookT0), metav1.NewTime(hookT0.Add(time.Minute))
	cur.Status.StartTime, cur.Status.CompletionTime = &s2, &e2
	if err := r.Status().Update(context.Background(), &cur); err != nil {
		t.Fatal(err)
	}
	rec()
	if len(p.reqs) != 2 {
		t.Fatalf("next run not delivered: %d", len(p.reqs))
	}
}

func TestJobHook_RecordsPrunedWhenHookDeleted(t *testing.T) {
	h := testHook("notify", "Failed")
	r, _, _ := hookEnv(t, h, failedJob("train", nil))
	reconcileAIJob(t, r, "train")
	if err := r.Delete(context.Background(), getHook(t, r, "notify")); err != nil {
		t.Fatal(err)
	}
	reconcileAIJob(t, r, "train")
	var j gryviav1.GryviaAIJob
	_ = r.Get(context.Background(), types.NamespacedName{Namespace: "team", Name: "train"}, &j)
	if _, ok := j.Annotations[AnnotationJobHooks]; ok {
		t.Fatalf("annotation kept: %v", j.Annotations)
	}
}

func TestJobHook_ReadyCondition(t *testing.T) {
	good := testHook("good", "Failed")
	bad := testHook("bad", "Failed")
	bad.Spec.Webhook.Headers = map[string]string{"X-Gryvia-Signature": "forged"}
	unsigned := testHook("unsigned", "Failed")
	unsigned.Spec.Webhook.SecretRef = &gryviav1.JobHookSecretRef{Name: "absent"}
	paused := testHook("paused", "Failed")
	paused.Spec.Suspend = true
	r, _, _ := hookEnv(t, good, bad, unsigned, paused)
	for name, want := range map[string]string{"good": "Valid", "bad": "InvalidSpec", "unsigned": "SecretMissing",
		"paused": "Suspended"} {
		res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "team", Name: name}})
		if err != nil {
			t.Fatal(err)
		}
		c := findCond(getHook(t, r, name).Status.Conditions, "Ready")
		if c == nil || c.Reason != want {
			t.Fatalf("%s: condition %+v, want %s", name, c, want)
		}
		if name == "unsigned" && res.RequeueAfter != time.Minute {
			t.Fatalf("missing secret not requeued: %v", res)
		}
	}
	if b := hookBackoff(&gryviav1.GryviaJobHook{}, 20); b != time.Hour {
		t.Fatalf("backoff not capped: %v", b)
	}
}
