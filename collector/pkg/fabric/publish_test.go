// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package fabric

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zyvorai/gryvia/collector/pkg/kube"
)

type fakeAPI struct {
	mu      sync.Mutex
	lists   map[string]string // path -> body
	getErr  error
	patchEr map[string]error
	gets    []string
	patches map[string][]byte
	other   []string
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{lists: map[string]string{}, patchEr: map[string]error{}, patches: map[string][]byte{}}
}

func (f *fakeAPI) Get(_ context.Context, path string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets = append(f.gets, path)
	if f.getErr != nil {
		return nil, f.getErr
	}
	b, ok := f.lists[path]
	if !ok {
		return nil, &kube.StatusError{Code: 404}
	}
	return []byte(b), nil
}

func (f *fakeAPI) MergePatch(_ context.Context, path string, body []byte) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.patchEr[path]; err != nil {
		return nil, err
	}
	f.patches[path] = body
	return []byte("{}"), nil
}

const gfsBase = "/apis/gryvia.io/v1alpha1/namespaces/"

func folderWithJobs(t *testing.T) *Folder {
	t.Helper()
	f := NewFolder(time.Minute)
	f.Bind(10, "ml", "train")
	f.Bind(11, "ml", "orphan") // has no GryviaFabricSignal
	f.Add(Signal{PID: 10, Type: SigStraggler, Rank: 3, LatencyNS: 80_000_000, Comm: "python"})
	f.Add(Signal{PID: 11, Type: SigStraggler, Rank: 1, LatencyNS: 1_000_000, Comm: "python"})
	f.Add(Signal{PID: 99, Type: SigStraggler, Comm: "stranger"}) // _unattributed
	f.AddCNP(1000)
	f.AddPFC(1000)
	return f
}

func TestPublishPatchesOnlyExistingMatchingObjects(t *testing.T) {
	api := newFakeAPI()
	api.lists[gfsBase+"ml/gryviafabricsignals"] = `{"items":[
	 {"metadata":{"name":"train-fabric"},"spec":{"jobRef":"train"}},
	 {"metadata":{"name":"other"},"spec":{"jobRef":"something-else"}}]}`
	lg := &recLog{}
	p := &Publisher{API: api, Folder: folderWithJobs(t), Log: lg}
	if n := p.PublishOnce(context.Background()); n != 1 {
		t.Fatalf("patched %d, want 1 (lines %v)", n, lg.lines)
	}
	if len(api.patches) != 1 {
		t.Fatalf("patches: %v", api.patches)
	}
	body, ok := api.patches[gfsBase+"ml/gryviafabricsignals/train-fabric/status"]
	if !ok {
		t.Fatalf("no patch on the status subresource: %v", api.patches)
	}
	var got struct {
		Status map[string]interface{} `json:"status"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"stragglerRank", "ncclP99ms", "scoreDelta", "updatedAt"} {
		if _, ok := got.Status[f]; !ok {
			t.Errorf("status field %s missing from the patch: %s", f, body)
		}
	}
	for _, f := range []string{"rdmaRetryRate", "gdsHitRatio", "overlapIdleRatio", "cnpRate", "inferWaitP99ms", "pfcRate", "exfilEvents"} {
		if _, ok := got.Status[f]; ok {
			t.Errorf("unmeasured field %s must be omitted from the patch: %s", f, body)
		}
	}
	if got.Status["stragglerRank"].(float64) != 3 {
		t.Errorf("rank: %v", got.Status["stragglerRank"])
	}
	if strings.Contains(string(body), "spec") || strings.Contains(string(body), "ucx") {
		t.Errorf("patch must carry status only: %s", body)
	}
	// Only the namespace of an attributed job is listed: no _unattributed / _node lookups.
	for _, g := range api.gets {
		if strings.Contains(g, "_unattributed") || strings.Contains(g, "_node") {
			t.Errorf("listed a synthetic namespace: %s", g)
		}
	}
}

func TestPublishableKeys(t *testing.T) {
	for k, want := range map[JobKey]bool{
		{"ml", "train"}:                  true,
		{"_unattributed", "python"}:      false,
		{"_node", "cnp"}:                 false,
		{"_node", "pfc"}:                 false,
		{"ml", ""}:                       false,
		{"ml", "_x"}:                     false,
		{"ML", "train"}:                  false,
		{"ml", "a/../b"}:                 false,
		{"ml", strings.Repeat("a", 300)}: false,
	} {
		if Publishable(k) != want {
			t.Errorf("Publishable(%v) = %v, want %v", k, !want, want)
		}
	}
}

func TestPublishNeverCreatesAndSurvivesErrors(t *testing.T) {
	api := newFakeAPI()
	lg := &recLog{}
	p := &Publisher{API: api, Folder: folderWithJobs(t), Log: lg}

	// CRD absent / RBAC missing: list fails, nothing is patched or created.
	api.getErr = &kube.StatusError{Code: 403}
	if n := p.PublishOnce(context.Background()); n != 0 || !lg.has("list GryviaFabricSignal failed") {
		t.Fatalf("n=%d lines=%v", n, lg.lines)
	}
	// Namespace without any object: silent, no patch.
	api.getErr = nil
	api.lists[gfsBase+"ml/gryviafabricsignals"] = `{"items":[]}`
	if n := p.PublishOnce(context.Background()); n != 0 || len(api.patches) != 0 {
		t.Fatalf("n=%d patches=%v", n, api.patches)
	}
	// Patch error is logged, not fatal; 404 (deleted meanwhile) is silent.
	api.lists[gfsBase+"ml/gryviafabricsignals"] = `{"items":[{"metadata":{"name":"a"},"spec":{"jobRef":"train"}},{"metadata":{"name":"b"},"spec":{"jobRef":"train"}}]}`
	api.patchEr[gfsBase+"ml/gryviafabricsignals/a/status"] = errors.New("boom")
	api.patchEr[gfsBase+"ml/gryviafabricsignals/b/status"] = &kube.StatusError{Code: 404}
	lg.lines = nil
	if n := p.PublishOnce(context.Background()); n != 0 || !lg.has("patch failed") {
		t.Fatalf("n=%d lines=%v", n, lg.lines)
	}
	// Garbage list body is tolerated.
	api.lists[gfsBase+"ml/gryviafabricsignals"] = `not json`
	if n := p.PublishOnce(context.Background()); n != 0 {
		t.Fatal("garbage list patched something")
	}
}

func TestPublishBoundedJobs(t *testing.T) {
	api := newFakeAPI()
	f := NewFolder(time.Minute)
	items := []string{}
	for i := 0; i < MaxPublishedJobs+50; i++ {
		job := "j" + strings.Repeat("a", 1) + itoa(i)
		f.Bind(uint32(1000+i), "ml", job)
		f.Add(Signal{PID: uint32(1000 + i), Type: SigStraggler})
		items = append(items, `{"metadata":{"name":"`+job+`"},"spec":{"jobRef":"`+job+`"}}`)
	}
	api.lists[gfsBase+"ml/gryviafabricsignals"] = `{"items":[` + strings.Join(items, ",") + `]}`
	p := &Publisher{API: api, Folder: f}
	if n := p.PublishOnce(context.Background()); n != MaxPublishedJobs {
		t.Fatalf("patched %d, want cap %d", n, MaxPublishedJobs)
	}
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

func TestStatusPatchBodyClampsAndFormats(t *testing.T) {
	st := Status{StragglerHits: 1, StragglerRank: 1 << 31, ExfilEvents: 1 << 63, NCCLP99MS: nan(), ScoreDelta: 5, GDSMeasured: true, GDSHitRatio: 0.25,
		UpdatedAt: time.Date(2026, 1, 2, 3, 4, 5, 999, time.FixedZone("x", 3600))}
	b, err := StatusPatchBody(st)
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Status map[string]interface{} `json:"status"`
	}
	_ = json.Unmarshal(b, &m)
	if m.Status["stragglerRank"].(float64) != 2147483647 || m.Status["ncclP99ms"] != nil ||
		m.Status["scoreDelta"].(float64) != 1 || m.Status["gdsHitRatio"].(float64) != 0.25 ||
		m.Status["updatedAt"] != "2026-01-02T02:04:05Z" {
		t.Fatalf("%s", b)
	}
}

func nan() float64 { var z float64; return z / z }

func TestPublishNilSafe(t *testing.T) {
	var p *Publisher
	if p.PublishOnce(context.Background()) != 0 {
		t.Fatal()
	}
}

func TestStatusPatchBodyGPUFieldsOmittedUnlessMeasured(t *testing.T) {
	get := func(st Status) map[string]interface{} {
		b, err := StatusPatchBody(st)
		if err != nil {
			t.Fatal(err)
		}
		var m struct {
			Status map[string]interface{} `json:"status"`
		}
		_ = json.Unmarshal(b, &m)
		return m.Status
	}
	s := get(Status{CollectiveMaxSkewMS: 12.5})
	for _, k := range []string{"gpuIdleDuringCommRatio", "smActiveDuringCompute", "gpuCorrelationCoverage"} {
		if v, ok := s[k]; ok {
			t.Errorf("%s = %v, want omitted when not measured", k, v)
		}
	}
	if s["collectiveMaxSkewMs"].(float64) != 12.5 {
		t.Errorf("collectiveMaxSkewMs = %v", s["collectiveMaxSkewMs"])
	}
	s = get(Status{GPUCorrelationMeasured: true, GPUIdleDuringCommRatio: 1.7, GPUCorrelationCoverage: 0.4})
	if s["gpuIdleDuringCommRatio"].(float64) != 1 || s["gpuCorrelationCoverage"].(float64) != 0.4 || s["smActiveDuringCompute"] != nil {
		t.Errorf("measured, compute unmeasured: %v", s)
	}
	s = get(Status{GPUCorrelationMeasured: true, GPUComputeMeasured: true, SMActiveDuringCompute: 0.8})
	if s["smActiveDuringCompute"].(float64) != 0.8 {
		t.Errorf("%v", s)
	}
}
