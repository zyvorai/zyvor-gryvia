// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package llmgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

type staticIndexes map[string]Index

func (s staticIndexes) Index(_ context.Context, ns, name string) (Index, bool, error) {
	idx, ok := s[ns+"/"+name]
	return idx, ok, nil
}

// ragBackend is a stand-in embeddings server and Qdrant on one listener.
type ragBackend struct {
	mu         sync.Mutex
	embedBody  map[string]interface{}
	searchBody map[string]interface{}
	searchPath string
	apiKey     string
	storeCode  int
}

func (b *ragBackend) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		b.mu.Lock()
		defer b.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/embeddings" {
			b.embedBody = body
			fmt.Fprint(w, `{"data":[{"index":0,"embedding":[0.1,0.2,0.3]}],"usage":{"prompt_tokens":4,"total_tokens":4}}`)
			return
		}
		b.searchBody, b.searchPath, b.apiKey = body, r.URL.Path, r.Header.Get("api-key")
		if b.storeCode != 0 {
			w.WriteHeader(b.storeCode)
			fmt.Fprint(w, `{"status":{"error":"boom"}}`)
			return
		}
		fmt.Fprint(w, `{"result":[{"id":"a","score":0.91,"payload":{"text":"Quotas cap GPU hours.","source":"faq.md","chunk":2}},`+
			`{"id":"b","score":0.5,"payload":{"text":"Budgets.","source":"b.md","chunk":0}}]}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newRetrieveGateway(t *testing.T) (*Gateway, *ragBackend) {
	t.Helper()
	be := &ragBackend{}
	srv := be.server(t)
	gw, _, _ := newGateway(t, func(string) []Route {
		return []Route{{Model: "embed", Namespace: "platform", Service: "embed", Upstream: srv.URL, ServedModel: "bge-small", Shared: true, PriceIn: 1}}
	})
	gw.Indexes = staticIndexes{
		"team-a/kb":      {Namespace: "team-a", Name: "kb", EmbedModel: "embed", StoreURL: srv.URL, Collection: "kb", StoreKey: "sk", Queryable: true},
		"team-a/new":     {Namespace: "team-a", Name: "new", EmbedModel: "embed", StoreURL: srv.URL, Collection: "new"},
		"team-a/private": {Namespace: "team-a", Name: "private", EmbedModel: "nope", StoreURL: srv.URL, Collection: "p", Queryable: true},
	}
	return gw, be
}

func TestRetrieveEmbedsSearchesAndMeters(t *testing.T) {
	gw, be := newRetrieveGateway(t)
	rec := call(t, gw, "POST", "/v1/retrieve", keyA, `{"index":"kb","query":"what caps GPU hours?","topK":3}`)
	if rec.Code != 200 {
		t.Fatalf("code = %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Data  []RetrieveHit    `json:"data"`
		Usage map[string]int64 `json:"usage"`
		Model string           `json:"model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data) != 2 || out.Data[0].Text != "Quotas cap GPU hours." || out.Data[0].Source != "faq.md" || out.Data[0].Chunk != 2 || out.Data[0].Score != 0.91 {
		t.Fatalf("hits = %+v", out.Data)
	}
	if out.Usage["prompt_tokens"] != 4 || out.Model != "embed" {
		t.Fatalf("usage = %v model %q", out.Usage, out.Model)
	}
	if be.embedBody["model"] != "bge-small" || fmt.Sprint(be.embedBody["input"]) != "[what caps GPU hours?]" {
		t.Fatalf("embedding request = %v", be.embedBody)
	}
	if be.searchPath != "/collections/kb/points/search" || be.apiKey != "sk" || be.searchBody["limit"] != float64(3) ||
		be.searchBody["with_payload"] != true || fmt.Sprint(be.searchBody["vector"]) != "[0.1 0.2 0.3]" {
		t.Fatalf("search = %s %q %v", be.searchPath, be.apiKey, be.searchBody)
	}
	if v := testutil.ToFloat64(gw.Meter.tokens.WithLabelValues("team-a", "embed", "input")); v != 4 {
		t.Fatalf("input tokens metered = %v", v)
	}
	if v := testutil.ToFloat64(gw.Meter.requests.WithLabelValues("team-a", "embed", "200")); v != 1 {
		t.Fatalf("requests metered = %v", v)
	}
}

func TestRetrieveTopKDefaultsAndCap(t *testing.T) {
	gw, be := newRetrieveGateway(t)
	call(t, gw, "POST", "/v1/retrieve", keyA, `{"index":"kb","query":"q"}`)
	if be.searchBody["limit"] != float64(defaultTopK) {
		t.Fatalf("default limit = %v", be.searchBody["limit"])
	}
	call(t, gw, "POST", "/v1/retrieve", keyA, `{"index":"kb","query":"q","topK":1000}`)
	if be.searchBody["limit"] != float64(maxTopK) {
		t.Fatalf("capped limit = %v", be.searchBody["limit"])
	}
}

func TestRetrieveErrors(t *testing.T) {
	gw, be := newRetrieveGateway(t)
	cases := []struct {
		name, key, body string
		code            int
		msg             string
	}{
		{"no key", "", `{"index":"kb","query":"q"}`, 401, "bearer"},
		{"missing query", keyA, `{"index":"kb"}`, 400, "required"},
		{"not json", keyA, `[1]`, 400, "JSON object"},
		{"long query", keyA, `{"index":"kb","query":"` + strings.Repeat("x", maxQueryChars+1) + `"}`, 400, "characters"},
		{"other namespace", keyB, `{"index":"kb","query":"q"}`, 404, "namespace team-b"},
		{"unknown", keyA, `{"index":"zzz","query":"q"}`, 404, "does not exist"},
		{"not ingested", keyA, `{"index":"new","query":"q"}`, 409, "first ingestion"},
		{"model not available", keyA, `{"index":"private","query":"q"}`, 404, "embedding model"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := call(t, gw, "POST", "/v1/retrieve", c.key, c.body)
			if rec.Code != c.code || !strings.Contains(rec.Body.String(), c.msg) {
				t.Fatalf("code = %d body %s, want %d with %q", rec.Code, rec.Body, c.code, c.msg)
			}
		})
	}
	be.storeCode = 500
	rec := call(t, gw, "POST", "/v1/retrieve", keyA, `{"index":"kb","query":"q"}`)
	if rec.Code != 502 || !strings.Contains(rec.Body.String(), "store returned 500") {
		t.Fatalf("store failure: %d %s", rec.Code, rec.Body)
	}
	// The query embedding was still metered.
	if v := testutil.ToFloat64(gw.Meter.tokens.WithLabelValues("team-a", "embed", "input")); v != 4 {
		t.Fatalf("input tokens metered = %v", v)
	}
	if rec := call(t, gw, "GET", "/v1/retrieve", keyA, ""); rec.Code != 404 {
		t.Fatalf("GET /v1/retrieve = %d", rec.Code)
	}
	gw.Indexes = nil
	if rec := call(t, gw, "POST", "/v1/retrieve", keyA, `{"index":"kb","query":"q"}`); rec.Code != 404 {
		t.Fatalf("without indexes = %d", rec.Code)
	}
}

type denyQuota struct{ added int64 }

func (d *denyQuota) Check(context.Context, string, time.Time) (bool, string, int64, int64, error) {
	return false, "q", 10, 12, nil
}
func (d *denyQuota) Add(_ string, n int64, _ time.Time) { d.added += n }

func TestRetrieveHonoursTokenQuota(t *testing.T) {
	gw, be := newRetrieveGateway(t)
	gw.Quotas = &denyQuota{}
	rec := call(t, gw, "POST", "/v1/retrieve", keyA, `{"index":"kb","query":"q"}`)
	if rec.Code != 429 || be.embedBody != nil {
		t.Fatalf("code = %d, embedded %v", rec.Code, be.embedBody)
	}
}

func TestCacheSourceIndex(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = gryviav1.AddToScheme(scheme)
	done := metav1.Now()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&gryviav1.GryviaVectorIndex{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "ext"},
			Spec: gryviav1.GryviaVectorIndexSpec{Embedding: gryviav1.VectorEmbedding{Model: "embed"},
				Store: gryviav1.VectorStore{Type: gryviav1.VectorStoreExternal, External: &gryviav1.ExternalVectorStore{URL: "https://q"}}},
			Status: gryviav1.GryviaVectorIndexStatus{StoreURL: "https://q/", Collection: "handbook", LastIngested: &done},
		},
		&gryviav1.GryviaVectorIndex{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "fresh"},
			Spec:       gryviav1.GryviaVectorIndexSpec{Store: gryviav1.VectorStore{Type: gryviav1.VectorStoreManaged}},
		},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "keys", Name: StoreSecretName("team-a", "ext")},
			Data: map[string][]byte{"apiKey": []byte("sk")}},
	).Build()
	s := &CacheSource{Reader: c, KeyNamespace: "keys"}
	idx, ok, err := s.Index(context.Background(), "team-a", "ext")
	if err != nil || !ok {
		t.Fatalf("ext: %v %v", ok, err)
	}
	if idx.StoreURL != "https://q" || idx.Collection != "handbook" || idx.StoreKey != "sk" || !idx.Queryable || idx.EmbedModel != "embed" {
		t.Fatalf("ext = %+v", idx)
	}
	idx, ok, _ = s.Index(context.Background(), "team-a", "fresh")
	if !ok || idx.Queryable || idx.Collection != "fresh" || idx.StoreKey != "" {
		t.Fatalf("fresh = %+v", idx)
	}
	if _, ok, err := s.Index(context.Background(), "team-b", "ext"); ok || err != nil {
		t.Fatalf("other namespace: %v %v", ok, err)
	}
}
