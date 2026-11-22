// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package fabric

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zyvorai/gryvia/collector/pkg/kube"
)

func keysOf(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m struct {
		Status map[string]any `json:"status"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	return m.Status
}

func f64p(v float64) *float64 { return &v }

// Each field group is omitted when the node did not measure it and present when it did.
func TestMeasuredFieldGroups(t *testing.T) {
	inf := &Inference{Engine: "vllm", TTFTP99MS: f64p(10), ITLP99MS: f64p(2), QueueP99MS: f64p(3), E2EP99MS: f64p(4),
		QueueMeanMS: f64p(1), E2EMeanMS: f64p(5), RequestsWaiting: f64p(7), KVCacheUsage: f64p(0.5)}
	cases := []struct {
		name string
		st   Status
		keys []string
	}{
		{"nccl", Status{StragglerHits: 2, StragglerRank: 0, NCCLP99MS: 80}, []string{"stragglerRank", "ncclP99ms"}},
		{"rdma", Status{RDMARetryRate: 0.1}, []string{"rdmaRetryRate"}},
		{"gds", Status{GDSMeasured: true, GDSHitRatio: 0}, []string{"gdsHitRatio"}},
		{"overlap", Status{OverlapIdleRatio: 0.4}, []string{"overlapIdleRatio"}},
		{"cnp/pfc", Status{CNPRate: 5, PFCRate: 6}, []string{"cnpRate", "pfcRate"}},
		{"infer wait", Status{InferWaitP99MS: 9}, []string{"inferWaitP99ms"}},
		{"exfil", Status{ExfilEvents: 3}, []string{"exfilEvents"}},
		{"collectives", Status{CollectiveMaxSkewMS: 2}, []string{"collectiveMaxSkewMs"}},
		{"gpu", Status{GPUCorrelationMeasured: true, GPUComputeMeasured: true, GPUIdleDuringCommRatio: .2, SMActiveDuringCompute: .9, GPUCorrelationCoverage: .5},
			[]string{"gpuIdleDuringCommRatio", "smActiveDuringCompute", "gpuCorrelationCoverage"}},
		{"score", Status{ScoreDelta: 0.35}, []string{"scoreDelta"}},
		{"engine", Status{Inference: inf}, []string{"engine", "ttftP99ms", "itlP99ms", "queueTimeP99ms", "e2eP99ms", "queueTimeMeanMs", "e2eMeanMs", "requestsWaiting", "kvCacheUsage"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, _, err := legacyBody(Measured(c.st, true), time.Unix(1, 0), nil)
			if err != nil {
				t.Fatal(err)
			}
			got := keysOf(t, b)
			want := map[string]bool{"updatedAt": true}
			for _, k := range c.keys {
				want[k] = true
			}
			for k, v := range got {
				if !want[k] {
					t.Errorf("unmeasured %s sent as %v: %s", k, v, b)
				}
				if v == nil {
					t.Errorf("%s is null without a previous send", k)
				}
			}
			for k := range want {
				if _, ok := got[k]; !ok {
					t.Errorf("measured %s missing: %s", k, b)
				}
			}
		})
	}
	// A completely empty status sends only updatedAt.
	b, _, _ := legacyBody(Measured(Status{}, true), time.Unix(1, 0), nil)
	if got := keysOf(t, b); len(got) != 1 {
		t.Errorf("empty status: %s", b)
	}
	// Rank 0 is a real rank when a straggler was seen.
	b, _, _ = legacyBody(Measured(Status{StragglerHits: 1}, false), time.Unix(1, 0), nil)
	if v, ok := keysOf(t, b)["stragglerRank"]; !ok || v.(float64) != 0 {
		t.Errorf("rank 0: %s", b)
	}
}

// A value this node sent earlier and no longer measures is cleared with an explicit null, once;
// a field it never sent is never nulled.
func TestLegacyClearsOnlyWhatThisNodeSent(t *testing.T) {
	api := newFakeAPI()
	api.lists[gfsBase+"ml/gryviafabricsignals"] = `{"items":[{"metadata":{"name":"g"},"spec":{"jobRef":"train"}}]}`
	now := time.Now()
	f := NewFolder(time.Minute)
	f.now = func() time.Time { return now }
	f.Bind(10, "ml", "train")
	f.Add(Signal{PID: 10, Type: SigStraggler, Rank: 2, LatencyNS: 90_000_000})
	f.AddCNP(6000) // job _node/cnp is not the job's; nothing else measured
	p := &Publisher{API: api, Folder: f}
	path := gfsBase + "ml/gryviafabricsignals/g/status"
	p.PublishOnce(context.Background())
	first := keysOf(t, api.patches[path])
	if first["ncclP99ms"].(float64) != 90 {
		t.Fatalf("%v", first)
	}
	now = now.Add(2 * time.Minute) // window passes: the job's signals expire; new signal without nccl
	f.Add(Signal{PID: 10, Type: SigInferWait, LatencyNS: 5_000_000})
	p.PublishOnce(context.Background())
	second := keysOf(t, api.patches[path])
	for _, k := range []string{"ncclP99ms", "stragglerRank", "scoreDelta"} {
		if v, ok := second[k]; k != "scoreDelta" && (!ok || v != nil) {
			t.Errorf("%s = %v, want explicit null (this node sent it before)", k, v)
		}
	}
	if _, ok := second["rdmaRetryRate"]; ok {
		t.Errorf("never sent, must not be nulled: %v", second)
	}
	if second["inferWaitP99ms"].(float64) != 5 {
		t.Errorf("%v", second)
	}
	f.Add(Signal{PID: 10, Type: SigInferWait, LatencyNS: 5_000_000})
	p.PublishOnce(context.Background())
	third := keysOf(t, api.patches[path])
	if _, ok := third["ncclP99ms"]; ok {
		t.Errorf("null must be sent once only: %v", third)
	}
}

type applyCall struct{ method, path, query, ctype, body string }

func newAPIServer(t *testing.T, lists map[string]string) (*kube.Client, *[]applyCall, *sync.Mutex) {
	t.Helper()
	var mu sync.Mutex
	var calls []applyCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.Method == http.MethodGet {
			if l, ok := lists[r.URL.Path]; ok {
				_, _ = w.Write([]byte(l))
				return
			}
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		calls = append(calls, applyCall{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Content-Type"), string(b)})
		mu.Unlock()
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(srv.Close)
	tok := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tok, []byte("t"), 0o600); err != nil {
		t.Fatal(err)
	}
	return kube.New(srv.URL, srv.Client(), tok), &calls, &mu
}

func TestPerNodeServerSideApply(t *testing.T) {
	c, calls, mu := newAPIServer(t, map[string]string{
		"/apis/gryvia.io/v1alpha1/namespaces/ml/gryviafabricsignals": `{"items":[{"metadata":{"name":"train-fabric"},"spec":{"jobRef":"train"}}]}`,
	})
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	f := NewFolder(time.Minute)
	f.now = func() time.Time { return now }
	f.Bind(10, "ml", "train")
	f.Add(Signal{PID: 10, Type: SigStraggler, Rank: 4, LatencyNS: 70_000_000})
	f.Add(Signal{PID: 10, Type: SigGDS, Bytes: 100, Retries: 1})
	f.SetGDSDirectVisible(true)
	p := &Publisher{API: c, Folder: f, PerNode: true, Node: "gpu-1", Now: func() time.Time { return now }}
	if n := p.PublishOnce(context.Background()); n != 1 {
		t.Fatalf("published %d", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*calls) != 1 {
		t.Fatalf("calls: %+v", *calls)
	}
	cl := (*calls)[0]
	if cl.method != http.MethodPatch || cl.ctype != "application/apply-patch+yaml" ||
		cl.path != "/apis/gryvia.io/v1alpha1/namespaces/ml/gryviafabricsignals/train-fabric/status" {
		t.Fatalf("call: %+v", cl)
	}
	if !strings.Contains(cl.query, "fieldManager=gryvia-collector-gpu-1") || !strings.Contains(cl.query, "force=true") {
		t.Errorf("query %q", cl.query)
	}
	var obj struct {
		APIVersion string         `json:"apiVersion"`
		Kind       string         `json:"kind"`
		Metadata   map[string]any `json:"metadata"`
		Spec       any            `json:"spec"`
		Status     struct {
			Nodes []map[string]any `json:"nodes"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(cl.body), &obj); err != nil {
		t.Fatal(err)
	}
	if obj.APIVersion != "gryvia.io/v1alpha1" || obj.Kind != "GryviaFabricSignal" || obj.Metadata["name"] != "train-fabric" ||
		obj.Metadata["namespace"] != "ml" || obj.Spec != nil || len(obj.Status.Nodes) != 1 {
		t.Fatalf("body %s", cl.body)
	}
	e := obj.Status.Nodes[0]
	if e["node"] != "gpu-1" || e["measuredAt"] != "2026-05-01T12:00:00Z" || e["ttlSeconds"].(float64) != 300 ||
		e["sampleCount"].(float64) != 2 || e["ncclP99ms"].(float64) != 70 || e["stragglerRank"].(float64) != 4 || e["gdsHitRatio"].(float64) != 1 {
		t.Errorf("entry %v", e)
	}
	for _, k := range []string{"rdmaRetryRate", "cnpRate", "pfcRate", "gpuIdleDuringCommRatio", "ttftP99ms"} {
		if _, ok := e[k]; ok {
			t.Errorf("unmeasured %s in the entry", k)
		}
	}

	// The job leaves this node: the entry is released with an apply that carries no status.
	now = now.Add(10 * time.Minute)
	*calls = nil
	mu.Unlock()
	p.PublishOnce(context.Background())
	mu.Lock()
	if len(*calls) != 1 || strings.Contains((*calls)[0].body, "status") || !strings.Contains((*calls)[0].query, "fieldManager=gryvia-collector-gpu-1") {
		t.Fatalf("release: %+v", *calls)
	}
	// ... and only once.
	*calls = nil
	mu.Unlock()
	p.PublishOnce(context.Background())
	mu.Lock()
	if len(*calls) != 0 {
		t.Errorf("released twice: %+v", *calls)
	}
}

// Without a usable node name or an API that can apply, the publisher stays in legacy mode.
func TestPerNodeFallsBackToLegacy(t *testing.T) {
	api := newFakeAPI() // has no Do
	api.lists[gfsBase+"ml/gryviafabricsignals"] = `{"items":[{"metadata":{"name":"g"},"spec":{"jobRef":"train"}}]}`
	f := NewFolder(time.Minute)
	f.Bind(10, "ml", "train")
	f.Add(Signal{PID: 10, Type: SigStraggler, LatencyNS: 1})
	p := &Publisher{API: api, Folder: f, PerNode: true, Node: "n1"}
	if n := p.PublishOnce(context.Background()); n != 1 || len(api.patches) != 1 {
		t.Fatalf("n=%d %v", n, api.patches)
	}
	if FieldManager(strings.Repeat("a", 300)) == "" || len(FieldManager(strings.Repeat("a", 300))) != 128 {
		t.Error("field manager is not bounded")
	}
}
