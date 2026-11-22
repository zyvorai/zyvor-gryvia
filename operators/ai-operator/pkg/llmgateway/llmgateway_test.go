// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package llmgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

const (
	keyA = "gk-team-a-secret"
	keyB = "gk-team-b-secret"
)

type staticSource struct {
	keys   map[string]Key
	routes []Route
}

func (s staticSource) Keys(context.Context) (map[string]Key, error) { return s.keys, nil }
func (s staticSource) Routes(context.Context) ([]Route, error)      { return s.routes, nil }

type upstream struct {
	mu     sync.Mutex
	bodies []map[string]interface{}
	auth   []string
	paths  []string
}

func (u *upstream) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		u.mu.Lock()
		u.bodies = append(u.bodies, body)
		u.auth = append(u.auth, r.Header.Get("Authorization"))
		u.paths = append(u.paths, r.URL.Path)
		u.mu.Unlock()
		if stream, _ := body["stream"].(bool); stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3}}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"x","choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":11,"completion_tokens":5}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newGateway(t *testing.T, routes func(url string) []Route) (*Gateway, *upstream, *prometheus.Registry) {
	t.Helper()
	up := &upstream{}
	srv := up.server(t)
	reg := prometheus.NewRegistry()
	gw := &Gateway{
		Source: staticSource{
			keys: map[string]Key{
				HashKey(keyA): {Name: "a-key", Tenant: "team-a", Namespace: "team-a"},
				HashKey(keyB): {Name: "b-key", Tenant: "team-b", Namespace: "team-b"},
			},
			routes: routes(srv.URL),
		},
		Meter: NewMeter(reg, "gw-0"),
		Now:   func() time.Time { return time.Date(2026, 10, 1, 10, 30, 0, 0, time.UTC) },
	}
	return gw, up, reg
}

func defaultRoutes(url string) []Route {
	return []Route{
		{Model: "chat", Namespace: "team-a", Service: "chat", Upstream: url, ServedModel: "/models/chat-v3", PriceIn: 1, PriceOut: 2},
		{Model: "shared-embed", Namespace: "platform", Service: "embed", Upstream: url, ServedModel: "shared-embed", Shared: true},
	}
}

func call(t *testing.T, h http.Handler, method, path, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAuthentication(t *testing.T) {
	gw, up, _ := newGateway(t, defaultRoutes)
	if rec := call(t, gw, "GET", "/healthz", "", ""); rec.Code != 200 {
		t.Fatalf("healthz = %d", rec.Code)
	}
	if rec := call(t, gw, "GET", "/v1/models", "", ""); rec.Code != 401 || !strings.Contains(rec.Body.String(), "authentication_error") {
		t.Fatalf("no key = %d %s", rec.Code, rec.Body)
	}
	if rec := call(t, gw, "POST", "/v1/chat/completions", "gk-wrong", `{"model":"chat"}`); rec.Code != 401 {
		t.Fatalf("wrong key = %d", rec.Code)
	}
	if len(up.bodies) != 0 {
		t.Fatalf("unauthenticated requests reached the upstream")
	}
}

func TestModelsAreScopedToTheKey(t *testing.T) {
	gw, _, _ := newGateway(t, defaultRoutes)
	ids := func(key string) []string {
		var out struct {
			Data []struct{ ID string } `json:"data"`
		}
		rec := call(t, gw, "GET", "/v1/models", key, "")
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, d := range out.Data {
			ids = append(ids, d.ID)
		}
		return ids
	}
	if got := strings.Join(ids(keyA), ","); got != "chat,shared-embed" {
		t.Fatalf("team-a models = %s", got)
	}
	if got := strings.Join(ids(keyB), ","); got != "shared-embed" {
		t.Fatalf("team-b models = %s", got)
	}
}

func TestProxyRewritesModelStripsKeyAndMeters(t *testing.T) {
	gw, up, reg := newGateway(t, defaultRoutes)
	rec := call(t, gw, "POST", "/v1/chat/completions", keyA, `{"model":"chat","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"prompt_tokens":11`) {
		t.Fatalf("chat = %d %s", rec.Code, rec.Body)
	}
	if up.bodies[0]["model"] != "/models/chat-v3" || up.auth[0] != "" || up.paths[0] != "/v1/chat/completions" {
		t.Fatalf("upstream saw model=%v auth=%q path=%s", up.bodies[0]["model"], up.auth[0], up.paths[0])
	}
	if v := testutil.ToFloat64(gw.Meter.tokens.WithLabelValues("team-a", "chat", "input")); v != 11 {
		t.Fatalf("input tokens = %v", v)
	}
	if v := testutil.ToFloat64(gw.Meter.tokens.WithLabelValues("team-a", "chat", "output")); v != 5 {
		t.Fatalf("output tokens = %v", v)
	}
	if v := testutil.ToFloat64(gw.Meter.requests.WithLabelValues("team-a", "chat", "200")); v != 1 {
		t.Fatalf("requests = %v", v)
	}
	_ = reg
	if rec := call(t, gw, "POST", "/v1/chat/completions", keyB, `{"model":"chat"}`); rec.Code != 404 {
		t.Fatalf("team-b using team-a's model = %d", rec.Code)
	}
	if rec := call(t, gw, "POST", "/v1/embeddings", keyB, `{"model":"shared-embed","input":"x"}`); rec.Code != 200 {
		t.Fatalf("team-b using the shared model = %d", rec.Code)
	}
}

func TestStreamingAddsUsageOptionAndCountsTheFinalChunk(t *testing.T) {
	gw, up, _ := newGateway(t, defaultRoutes)
	rec := call(t, gw, "POST", "/v1/chat/completions", keyA, `{"model":"chat","stream":true,"stream_options":{"foo":1}}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "data: [DONE]") || !strings.Contains(rec.Body.String(), `"content":"hi"`) {
		t.Fatalf("stream = %d %s", rec.Code, rec.Body)
	}
	opts, _ := up.bodies[0]["stream_options"].(map[string]interface{})
	if opts["include_usage"] != true || opts["foo"] != float64(1) {
		t.Fatalf("stream_options = %v", opts)
	}
	if v := testutil.ToFloat64(gw.Meter.tokens.WithLabelValues("team-a", "chat", "input")); v != 7 {
		t.Fatalf("streamed input tokens = %v", v)
	}
}

func TestBadRequests(t *testing.T) {
	gw, _, _ := newGateway(t, defaultRoutes)
	gw.MaxBody = 64
	for _, tc := range []struct {
		body string
		code int
	}{
		{`not json`, 400},
		{`{"messages":[]}`, 400},
		{`{"model":"missing"}`, 404},
		{`{"model":"chat","pad":"` + strings.Repeat("x", 100) + `"}`, 413},
	} {
		if rec := call(t, gw, "POST", "/v1/chat/completions", keyA, tc.body); rec.Code != tc.code {
			t.Errorf("%.20s = %d, want %d", tc.body, rec.Code, tc.code)
		}
	}
	if rec := call(t, gw, "DELETE", "/v1/chat/completions", keyA, ""); rec.Code != 404 {
		t.Errorf("DELETE = %d", rec.Code)
	}
}

func TestUpstreamDown(t *testing.T) {
	gw, _, _ := newGateway(t, func(string) []Route {
		return []Route{{Model: "chat", Namespace: "team-a", Upstream: "http://127.0.0.1:1", ServedModel: "chat"}}
	})
	if rec := call(t, gw, "POST", "/v1/chat/completions", keyA, `{"model":"chat"}`); rec.Code != 502 {
		t.Fatalf("upstream down = %d", rec.Code)
	}
}

func TestRoutesAndKeys(t *testing.T) {
	svc := func(ns, name string, ann map[string]string, endpoint string) gryviav1.GryviaInferenceService {
		s := gryviav1.GryviaInferenceService{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Annotations: ann}}
		s.Status.Endpoint = endpoint
		return s
	}
	routes := routesFrom([]gryviav1.GryviaInferenceService{
		svc("team-a", "chat", map[string]string{AnnotationModel: "chat", AnnotationPriceIn: "0.5"}, "http://chat-inference.team-a.svc.cluster.local:8000/"),
		svc("team-a", "pending", map[string]string{AnnotationModel: "pending"}, ""),
		svc("team-a", "plain", nil, "http://plain"),
		svc("team-b", "bad", map[string]string{AnnotationModel: "has space"}, "http://bad"),
		svc("platform", "chat", map[string]string{AnnotationModel: "chat", AnnotationShared: "true", AnnotationPriceOut: "-1"}, "http://shared"),
	}, 0.1, 0.2)
	if len(routes) != 2 || routes[0].Namespace != "platform" || routes[1].Upstream != "http://chat-inference.team-a.svc.cluster.local:8000" {
		t.Fatalf("routes = %+v", routes)
	}
	if routes[1].PriceIn != 0.5 || routes[1].PriceOut != 0.2 || routes[0].PriceOut != 0.2 || routes[1].ServedModel != "chat" {
		t.Fatalf("prices = %+v", routes)
	}
	own, _ := pick(routes, Key{Namespace: "team-a"}, "chat")
	other, _ := pick(routes, Key{Namespace: "team-c"}, "chat")
	if own.Namespace != "team-a" || other.Namespace != "platform" {
		t.Fatalf("pick own=%s other=%s", own.Namespace, other.Namespace)
	}

	secret := func(name string, labels map[string]string, data map[string]string) corev1.Secret {
		s := corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}, Data: map[string][]byte{}}
		for k, v := range data {
			s.Data[k] = []byte(v)
		}
		return s
	}
	keys := keysFrom([]corev1.Secret{
		secret("ok", map[string]string{LabelKey: "true", LabelTenant: "acme"}, map[string]string{"hash": strings.ToUpper(HashKey(keyA)), "namespace": "team-a"}),
		secret("no-tenant", map[string]string{LabelKey: "true"}, map[string]string{"hash": HashKey(keyB), "namespace": "team-b"}),
		secret("unlabelled", nil, map[string]string{"hash": HashKey("x"), "namespace": "team-a"}),
		secret("short", map[string]string{LabelKey: "true"}, map[string]string{"hash": "abc", "namespace": "team-a"}),
	})
	if len(keys) != 2 || keys[HashKey(keyA)].Tenant != "acme" || keys[HashKey(keyB)].Tenant != "team-b" {
		t.Fatalf("keys = %+v", keys)
	}
}

func newFakeClient(objs ...client.Object) client.Client {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	for _, kind := range []string{"GryviaUsageRecord", "GryviaQuota"} {
		gv := schema.GroupVersion{Group: "gryvia.io", Version: "v1alpha1"}
		scheme.AddKnownTypeWithName(gv.WithKind(kind), &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(gv.WithKind(kind+"List"), &unstructured.UnstructuredList{})
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func usageRecords(t *testing.T, c client.Client, ns string) []unstructured.Unstructured {
	t.Helper()
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(usageListGVK)
	if err := c.List(context.Background(), list, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	return list.Items
}

func TestMeterFlushWritesHourlyRecords(t *testing.T) {
	c := newFakeClient()
	m := NewMeter(prometheus.NewRegistry(), "gw-0")
	k := Key{Tenant: "team-a", Namespace: "team-a"}
	r := Route{Model: "chat", PriceIn: 1, PriceOut: 2}
	at := time.Date(2026, 10, 1, 10, 15, 0, 0, time.UTC)
	m.Record(k, r, 1_000_000, 500_000, at)
	m.Record(k, r, 0, 0, at)
	ctx := context.Background()
	if err := m.Flush(ctx, c, at.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	recs := usageRecords(t, c, "team-a")
	if len(recs) != 1 {
		t.Fatalf("records = %d", len(recs))
	}
	spec := recs[0].Object["spec"].(map[string]interface{})
	if spec["kind"] != "tokens" || spec["job"] != "llm:chat" || spec["final"] != false || spec["start"] != "2026-10-01T10:00:00Z" ||
		spec["end"] != "2026-10-01T11:00:00Z" || fmt.Sprint(spec["cost"]) != "2" || spec["inputTokens"] != int64(1_000_000) {
		t.Fatalf("open record = %v", spec)
	}
	if recs[0].GetLabels()["gryvia.io/usage-kind"] != "tokens" || recs[0].GetLabels()[LabelTenant] != "team-a" {
		t.Fatalf("labels = %v", recs[0].GetLabels())
	}

	m.Record(k, r, 10, 0, at.Add(20*time.Minute))
	if err := m.Flush(ctx, c, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	recs = usageRecords(t, c, "team-a")
	spec = recs[0].Object["spec"].(map[string]interface{})
	if len(recs) != 1 || spec["final"] != true || spec["inputTokens"] != int64(1_000_010) {
		t.Fatalf("final record = %v", spec)
	}
	if len(m.buckets) != 0 {
		t.Fatalf("final bucket kept: %v", m.buckets)
	}
}

func quota(name string, perDay int64, nss ...string) *unstructured.Unstructured {
	q := &unstructured.Unstructured{Object: map[string]interface{}{"spec": map[string]interface{}{
		"team": name, "namespaces": toInterfaces(nss), "tokensPerDay": perDay,
	}}}
	q.SetGroupVersionKind(schema.GroupVersionKind{Group: "gryvia.io", Version: "v1alpha1", Kind: "GryviaQuota"})
	q.SetName(name)
	return q
}

func toInterfaces(ss []string) []interface{} {
	out := make([]interface{}, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func tokenRecord(ns, name, start string, in, out int64) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]interface{}{"spec": map[string]interface{}{
		"kind": "tokens", "start": start, "inputTokens": in, "outputTokens": out,
	}}}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "gryvia.io", Version: "v1alpha1", Kind: "GryviaUsageRecord"})
	u.SetNamespace(ns)
	u.SetName(name)
	u.SetLabels(map[string]string{"gryvia.io/usage-kind": "tokens", LabelInstance: "old-replica"})
	return u
}

func counter(t *testing.T, c client.Client, day string) map[string]string {
	t.Helper()
	cm := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "gryvia-system", Name: "gryvia-llm-tokens-" + day}, cm); err != nil {
		t.Fatalf("counter %s: %v", day, err)
	}
	if cm.Labels[LabelTokenCounter] != "true" {
		t.Fatalf("counter labels = %v", cm.Labels)
	}
	return cm.Data
}

func TestQuotaIsSharedAcrossReplicasAndRejectsWith429(t *testing.T) {
	now := time.Date(2026, 10, 1, 10, 30, 0, 0, time.UTC)
	c := newFakeClient(quota("team-a", 100, "team-a", "team-a-dev"),
		tokenRecord("team-a", "yesterday", "2026-09-30T23:00:00Z", 900, 0),
		tokenRecord("team-a", "today", "2026-10-01T09:00:00Z", 60, 20),
		tokenRecord("team-b", "today", "2026-10-01T08:00:00Z", 7, 0))
	ctx := context.Background()
	a, b := NewQuotas(c, "gryvia-system"), NewQuotas(c, "gryvia-system")
	for _, q := range []*Quotas{a, b} {
		if err := q.Refresh(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, _, _, err := a.Check(ctx, "team-a", now); err != ErrQuotaNotSynced {
		t.Fatalf("a check before the first sync must fail: %v", err)
	}
	if ok, _, _, _, err := a.Check(ctx, "team-b", now); !ok || err != nil {
		t.Fatalf("a namespace without a quota needs no sync: ok=%v err=%v", ok, err)
	}

	// The first sync of the day creates the counter, seeded from today's records plus what a counted so far.
	a.Add("team-a", 15, now)
	if err := a.Sync(ctx, now); err != nil {
		t.Fatal(err)
	}
	if got := counter(t, c, "2026-10-01"); got["team-a"] != "95" || got["team-b"] != "7" || len(got) != 2 {
		t.Fatalf("seeded counter = %v", got)
	}
	if err := b.Sync(ctx, now); err != nil {
		t.Fatal(err)
	}
	if ok, _, _, used, _ := b.Check(ctx, "team-a", now); !ok || used != 0 {
		t.Fatalf("95 of 100 should pass on b")
	}

	// b's own traffic counts at once on b, and on a after b and then a have synced.
	b.Add("team-a-dev", 5, now)
	if ok, name, per, used, _ := b.Check(ctx, "team-a", now); ok || name != "team-a" || per != 100 || used != 100 {
		t.Fatalf("b at the limit: ok=%v name=%s per=%d used=%d", ok, name, per, used)
	}
	if ok, _, _, _, _ := a.Check(ctx, "team-a", now); !ok {
		t.Fatalf("a cannot see b's unsynced traffic")
	}
	for _, q := range []*Quotas{b, a, a, b} {
		if err := q.Sync(ctx, now); err != nil {
			t.Fatal(err)
		}
	}
	for name, q := range map[string]*Quotas{"a": a, "b": b} {
		if ok, _, _, used, _ := q.Check(ctx, "team-a", now); ok || used != 100 {
			t.Fatalf("%s after syncs: ok=%v used=%d (synced tokens must not count twice)", name, ok, used)
		}
	}
	if got := counter(t, c, "2026-10-01"); got["team-a"] != "95" || got["team-a-dev"] != "5" {
		t.Fatalf("counter = %v", got)
	}

	up := &upstream{}
	srv := up.server(t)
	gw := &Gateway{
		Source: staticSource{keys: map[string]Key{HashKey(keyA): {Tenant: "team-a", Namespace: "team-a"}},
			routes: []Route{{Model: "chat", Namespace: "team-a", Upstream: srv.URL, ServedModel: "chat"}}},
		Meter:  NewMeter(prometheus.NewRegistry(), "gw-0"),
		Quotas: a,
		Now:    func() time.Time { return now },
	}
	rec := call(t, gw, "POST", "/v1/chat/completions", keyA, `{"model":"chat"}`)
	body, _ := io.ReadAll(rec.Body)
	if rec.Code != 429 || !strings.Contains(string(body), "rate_limit_error") || len(up.bodies) != 0 {
		t.Fatalf("over quota = %d %s", rec.Code, body)
	}
	if v := testutil.ToFloat64(gw.Meter.rejected.WithLabelValues("team-a")); v != 1 {
		t.Fatalf("rejections = %v", v)
	}

	// The next UTC day starts from zero with a new counter; the day before yesterday's counter is deleted.
	tomorrow := now.Add(14 * time.Hour)
	if ok, _, _, _, _ := a.Check(ctx, "team-a", tomorrow); !ok {
		t.Fatalf("the counter did not reset on the next UTC day")
	}
	if err := a.Sync(ctx, tomorrow); err != nil {
		t.Fatal(err)
	}
	counter(t, c, "2026-10-01")
	if got := counter(t, c, "2026-10-02"); len(got) != 0 {
		t.Fatalf("new day's counter = %v", got)
	}
	if err := a.Sync(ctx, now.AddDate(0, 0, 2)); err != nil {
		t.Fatal(err)
	}
	var cms corev1.ConfigMapList
	if err := c.List(ctx, &cms, client.InNamespace("gryvia-system")); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, cm := range cms.Items {
		names = append(names, cm.Name)
	}
	if strings.Join(names, ",") != "gryvia-llm-tokens-2026-10-02,gryvia-llm-tokens-2026-10-03" {
		t.Fatalf("counters kept = %v", names)
	}
}

func TestQuotaSyncsConcurrentReplicasWithoutLosingTokens(t *testing.T) {
	now := time.Date(2026, 10, 1, 10, 30, 0, 0, time.UTC)
	c := newFakeClient()
	ctx := context.Background()
	replicas := []*Quotas{NewQuotas(c, "gryvia-system"), NewQuotas(c, "gryvia-system"), NewQuotas(c, "gryvia-system")}
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(q *Quotas) {
			defer wg.Done()
			q.Add("team-a", 1, now)
			if err := q.Sync(ctx, now); err != nil {
				t.Error(err)
			}
		}(replicas[i%len(replicas)])
	}
	wg.Wait()
	for _, q := range replicas {
		if err := q.Sync(ctx, now); err != nil {
			t.Fatal(err)
		}
	}
	if got := counter(t, c, "2026-10-01")["team-a"]; got != "30" {
		t.Fatalf("counter = %s, want 30", got)
	}
}
