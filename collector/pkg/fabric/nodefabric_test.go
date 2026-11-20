// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package fabric

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zyvorai/gryvia/collector/pkg/kube"
)

func TestReasonsMirrorScoreDelta(t *testing.T) {
	cases := []Status{
		{},
		{NCCLP99MS: 80},
		{NCCLP99MS: 80, RDMARetryRate: 0.1, CNPRate: 500, PFCRate: 5000, StragglerHits: 20, OverlapIdleRatio: 0.9, GDSMeasured: true, GDSHitRatio: 0.1},
		{GDSMeasured: false, GDSHitRatio: 0},
	}
	for i, st := range cases {
		sum := 0.0
		for _, r := range Reasons(st) {
			sum += r.Penalty
		}
		if want := ScoreDelta(st); want != minf(sum, 1) {
			t.Errorf("case %d: reasons sum %v != ScoreDelta %v", i, sum, want)
		}
	}
	r := Reasons(cases[2])
	if r[0].Penalty < r[len(r)-1].Penalty {
		t.Error("reasons not sorted by penalty")
	}
}

func minf(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func TestNodeHealth(t *testing.T) {
	if d, r := NodeHealth(nil); d != 0 || r != nil {
		t.Errorf("empty = %v %v", d, r)
	}
	snap := map[JobKey]Status{
		{Namespace: "ml", Job: "ok"}:           {},
		{Namespace: "_node", Job: "cnp"}:       {CNPRate: 500, ScoreDelta: ScoreDelta(Status{CNPRate: 500})},
		{Namespace: "ml", Job: "sick"}:         {RDMARetryRate: 0.5, NCCLP99MS: 90, ScoreDelta: ScoreDelta(Status{RDMARetryRate: 0.5, NCCLP99MS: 90})},
		{Namespace: "_unattributed", Job: "p"}: {},
	}
	d, r := NodeHealth(snap)
	if d != 0.6 || len(r) != 2 || !strings.Contains(r[0], "rdma") || !strings.HasSuffix(r[0], "[ml/sick]") {
		t.Errorf("delta=%v reasons=%v", d, r)
	}
}

type fakeNodeAPI struct {
	mu       sync.Mutex
	patchErr error
	posts    [][]byte
	patches  map[string][]byte
}

func (f *fakeNodeAPI) MergePatch(_ context.Context, path string, b []byte) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.patchErr != nil {
		return nil, f.patchErr
	}
	if f.patches == nil {
		f.patches = map[string][]byte{}
	}
	f.patches[path] = b
	return []byte("{}"), nil
}

func (f *fakeNodeAPI) Do(_ context.Context, method, path, _ string, b []byte) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if method != http.MethodPost || path != "/apis/gryvia.io/v1alpha1/gryvianodefabrics" {
		return nil, &kube.StatusError{Code: 400}
	}
	f.posts = append(f.posts, b)
	return []byte("{}"), nil
}

func sickFolder() *Folder {
	f := NewFolder(time.Minute)
	f.Bind(10, "ml", "train")
	f.Add(Signal{PID: 10, Type: SigRDMARetry, Retries: 50, WorldSize: 100})
	return f
}

func TestNodePublisherPatchAndCreate(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	api := &fakeNodeAPI{}
	p := &NodePublisher{API: api, Folder: sickFolder(), Node: "gpu-1", Now: func() time.Time { return now }}
	if !p.PublishOnce(context.Background()) {
		t.Fatal("publish failed")
	}
	var got struct {
		Spec nodeFabricSpec `json:"spec"`
	}
	if err := json.Unmarshal(api.patches["/apis/gryvia.io/v1alpha1/gryvianodefabrics/gpu-1"], &got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.NodeName != "gpu-1" || got.Spec.ScoreDelta != 0.35 || got.Spec.TTLSeconds != 300 ||
		got.Spec.MeasuredAt != "2026-01-01T00:00:00Z" || got.Spec.ExpiresAt != "2026-01-01T00:05:00Z" || len(got.Spec.Reasons) != 1 {
		t.Errorf("spec = %+v", got.Spec)
	}

	// 404 on patch -> create.
	api = &fakeNodeAPI{patchErr: &kube.StatusError{Code: 404}}
	p.API = api
	if !p.PublishOnce(context.Background()) || len(api.posts) != 1 {
		t.Fatalf("create path: posts=%d", len(api.posts))
	}
	var obj map[string]any
	_ = json.Unmarshal(api.posts[0], &obj)
	if obj["kind"] != "GryviaNodeFabric" || obj["metadata"].(map[string]any)["name"] != "gpu-1" {
		t.Errorf("create body = %v", obj)
	}

	// Other errors: nothing created, reported false.
	api = &fakeNodeAPI{patchErr: &kube.StatusError{Code: 403}}
	p.API = api
	if p.PublishOnce(context.Background()) || len(api.posts) != 0 {
		t.Error("403 must not create and must report failure")
	}

	// Invalid node names are never used in a path.
	p.Node = "../x"
	if p.PublishOnce(context.Background()) {
		t.Error("invalid node name published")
	}
}

func TestNodePublisherHealthyClearsReasons(t *testing.T) {
	api := &fakeNodeAPI{}
	p := &NodePublisher{API: api, Folder: NewFolder(time.Minute), Node: "n"}
	p.PublishOnce(context.Background())
	if b := string(api.patches["/apis/gryvia.io/v1alpha1/gryvianodefabrics/n"]); !strings.Contains(b, `"reasons":[]`) || !strings.Contains(b, `"scoreDelta":0`) {
		t.Errorf("body = %s", b)
	}
}

// End to end against a real HTTP server through the in-cluster REST client.
func TestNodePublisherHTTP(t *testing.T) {
	var mu sync.Mutex
	var reqs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		reqs = append(reqs, r.Method+" "+r.URL.Path+" "+r.Header.Get("Content-Type")+" "+r.Header.Get("Authorization"))
		mu.Unlock()
		if r.Method == http.MethodPatch {
			http.Error(w, `{"kind":"Status"}`, http.StatusNotFound)
			return
		}
		if !strings.Contains(string(b), `"kind":"GryviaNodeFabric"`) {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()
	tok := t.TempDir() + "/token"
	if err := os.WriteFile(tok, []byte("tkn\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &NodePublisher{API: kube.New(srv.URL, srv.Client(), tok), Folder: sickFolder(), Node: "gpu-1"}
	if !p.PublishOnce(context.Background()) {
		t.Fatal("publish failed")
	}
	want := []string{
		"PATCH /apis/gryvia.io/v1alpha1/gryvianodefabrics/gpu-1 application/merge-patch+json Bearer tkn",
		"POST /apis/gryvia.io/v1alpha1/gryvianodefabrics application/json Bearer tkn",
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(reqs, "|") != strings.Join(want, "|") {
		t.Errorf("requests = %v", reqs)
	}
}
