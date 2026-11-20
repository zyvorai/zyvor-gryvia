// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zyvorai/gryvia/collector/pkg/diagnosis"
	"github.com/zyvorai/gryvia/collector/pkg/fabric"
	"github.com/zyvorai/gryvia/collector/pkg/flight"
	"github.com/zyvorai/gryvia/collector/pkg/loader"
)

type fakeProbes struct {
	status []loader.ProgramStatus
	drops  map[string]uint64
}

func (f fakeProbes) Status() []loader.ProgramStatus { return f.status }
func (f fakeProbes) HasMap(o, n string) bool        { _, ok := f.drops[o]; return ok && n == "drops" }
func (f fakeProbes) ReadCounter(o, n string, k uint32) (uint64, error) {
	return f.drops[o], nil
}

const wireUID = "12345678-1234-1234-1234-123456789abc"

func newService(t *testing.T) (*diagnosisService, string) {
	t.Helper()
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, "cgroup.controllers"), []byte("cpu memory io\n"), 0o644)
	us := strings.ReplaceAll(wireUID, "-", "_")
	pod := filepath.Join(root, "kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod"+us+".slice")
	ctr := filepath.Join(pod, "cri-containerd-a.scope")
	_ = os.MkdirAll(ctr, 0o755)
	_ = os.WriteFile(filepath.Join(ctr, "memory.events"), []byte("oom_kill 0\n"), 0o644)
	_ = os.WriteFile(filepath.Join(pod, "cpu.pressure"), []byte("some avg10=55.00 avg60=1 avg300=1 total=9\n"), 0o644)

	id := flight.NewResolver("n1", t.TempDir())
	if err := id.SetPods([]byte(`{"items":[{"metadata":{"uid":"` + wireUID + `","name":"w-0","namespace":"ml","labels":{"gryvia.io/job":"train"}},"spec":{"nodeName":"n1"},"status":{"phase":"Running","qosClass":"Burstable"}}]}`)); err != nil {
		t.Fatal(err)
	}
	rec := flight.New("n1")
	rec.Record(flight.Event{Identity: flight.Identity{Namespace: "ml", Job: "train", Pod: "w-0", Node: "n1"}, Kind: "tcp_retransmit", Retransmits: 500})
	s := &diagnosisService{
		node: "n1", cfg: diagnosis.DefaultConfig(), recorder: rec, identity: id, folder: fabric.NewFolder(0),
		probes: fakeProbes{
			status: []loader.ProgramStatus{{Object: "tcp_trace.o", Program: "k", Attached: true}, {Object: "straggler.o", Program: "u", Reason: "skipped: no libnccl"}},
			drops:  map[string]uint64{"tcp_trace.o": 4},
		},
		sampler: diagnosis.NewSampler(root, id), now: time.Now,
		userDrops: func() []diagnosis.DropCount {
			return []diagnosis.DropCount{{Source: "userspace/gpu-decoder", Count: 2}}
		},
	}
	s.sampler.Poll()
	return s, root
}

func TestDiagnosisServiceEndToEnd(t *testing.T) {
	s, _ := newService(t)
	d, ok := s.evaluate("ml", "train")
	if !ok {
		t.Fatal("job must be known")
	}
	var net, cpu bool
	for _, f := range d.Findings {
		net = net || f.Kind == "network"
		cpu = cpu || f.Kind == "cpu"
	}
	if !net {
		t.Fatalf("network finding missing: %+v", d.Findings)
	}
	if !cpu {
		t.Fatalf("cpu pressure 55 must produce a cpu finding from one sample: %v", d.Findings)
	}
	c := d.Completeness
	if c.Complete || c.DroppedEvents.Total != 6 || c.ProbesAttached != 1 || len(c.ProbesSkipped) != 1 {
		t.Fatalf("%+v", c)
	}
	un := map[string]bool{}
	for _, u := range d.Unavailable {
		un[u.Signal] = true
	}
	for _, want := range []string{diagnosis.SigStraggler, diagnosis.SigCPUStat, diagnosis.SigMemUsage, diagnosis.SigIOPressure} {
		if !un[want] {
			t.Errorf("%s should be unavailable: %+v", want, d.Unavailable)
		}
	}
	if _, ok := s.evaluate("ml", "nothing"); ok {
		t.Fatal("unknown job must be 404")
	}
}

func signed(target, token string, when time.Time) *http.Request {
	r := httptest.NewRequest(http.MethodGet, target, nil)
	stamp := strconv.FormatInt(when.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte("GET\n" + r.URL.RequestURI() + "\n" + stamp))
	r.Header.Set(flightTimeHeader, stamp)
	r.Header.Set(flightSignatureHeader, hex.EncodeToString(mac.Sum(nil)))
	return r
}

func TestDiagnosisRoutesAreHMACProtected(t *testing.T) {
	s, _ := newService(t)
	tokenFile := filepath.Join(t.TempDir(), "token")
	token := strings.Repeat("k", 40)
	_ = os.WriteFile(tokenFile, []byte(token), 0o600)
	mux := http.NewServeMux()
	s.register(mux, tokenFile)
	for _, path := range []string{"/api/v1/flight/diagnosis?namespace=ml&job=train", "/api/v1/flight/incidents?namespace=ml", "/api/v1/flight/incidents/export?format=csv", "/api/v1/flight/compare?namespace=ml&job=train&a=now&b=now"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s unsigned: %d", path, rec.Code)
		}
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, signed(path, token, time.Now()))
		if rec.Code != http.StatusOK {
			t.Errorf("%s signed: %d %s", path, rec.Code, rec.Body)
		}
	}
	// Disabled endpoint stays disabled without a token file.
	mux2 := http.NewServeMux()
	s.register(mux2, "")
	rec := httptest.NewRecorder()
	mux2.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/flight/diagnosis?namespace=ml&job=train", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("%d", rec.Code)
	}
	// Body shape and validation.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, signed("/api/v1/flight/diagnosis?namespace=ml&job=train", token, time.Now()))
	var d diagnosis.Diagnosis
	if json.Unmarshal(rec.Body.Bytes(), &d) != nil || d.Job != "train" || d.Completeness.Sampling.Ratio != 1 {
		t.Fatalf("%s", rec.Body)
	}
	for path, want := range map[string]int{
		"/api/v1/flight/diagnosis?namespace=ml":            http.StatusBadRequest,
		"/api/v1/flight/diagnosis?namespace=ml&job=UPPER":  http.StatusBadRequest,
		"/api/v1/flight/diagnosis?namespace=ml&job=absent": http.StatusNotFound,
	} {
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, signed(path, token, time.Now()))
		if rec.Code != want {
			t.Errorf("%s: %d want %d", path, rec.Code, want)
		}
	}
}
