// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package inference

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseTargets(t *testing.T) {
	got, err := ParseTargets("name=vllm,url=http://127.0.0.1:8000/metrics; name=trt,url=https://10.0.0.5:8002/metrics,engine=Triton,namespace=ml,job=serve\n")
	if err != nil || len(got) != 2 {
		t.Fatalf("got %+v err=%v", got, err)
	}
	if got[0].Name != "vllm" || got[0].Engine != "" || got[0].Namespace != "" {
		t.Errorf("first = %+v", got[0])
	}
	if got[1].Engine != "triton" || got[1].Namespace != "ml" || got[1].Job != "serve" {
		t.Errorf("second = %+v", got[1])
	}
	if out, err := ParseTargets(""); err != nil || len(out) != 0 {
		t.Errorf("empty spec = %v %v", out, err)
	}
	bad := []string{
		"url=http://h/metrics",                        // no name
		"name=a",                                      // no url
		"name=a,url=ftp://h/x",                        // scheme
		"name=a,url=http://user:pw@h/x",               // credentials
		"name=a,url=http:///x",                        // no host
		"name=a,url=http://h/x,engine=sglang",         // engine
		"name=a,url=http://h/x,namespace=ml",          // half attribution
		"name=a,url=http://h/x,namespace=ML,job=j",    // not DNS-1123
		"name=a,url=http://h/x,bogus=1",               // unknown key
		"name=a,url=http://h/x;name=a,url=http://h/y", // duplicate
		"name=a b,url=http://h/x",                     // bad name
		"nonsense",
	}
	for _, b := range bad {
		if _, err := ParseTargets(b); err == nil {
			t.Errorf("%q should be rejected", b)
		}
	}
	var many []string
	for i := 0; i <= MaxTargets; i++ {
		many = append(many, "name=t"+strings.Repeat("x", i%3)+string(rune('a'+i%26))+string(rune('a'+i/26))+",url=http://h/m")
	}
	if _, err := ParseTargets(strings.Join(many, ";")); err == nil {
		t.Error("too many targets")
	}
}

func TestParsePorts(t *testing.T) {
	if p, err := ParsePorts("8000, 8002,8080"); err != nil || len(p) != 3 || p[1] != 8002 {
		t.Errorf("%v %v", p, err)
	}
	for _, b := range []string{"0", "70000", "x", "8000,,abc"} {
		if _, err := ParsePorts(b); err == nil {
			t.Errorf("%q should be rejected", b)
		}
	}
}

const podsJSON = `{"items":[
 {"metadata":{"name":"vllm-0","namespace":"ml","labels":{"gryvia.io/job":"serve"}},
  "spec":{"nodeName":"node-a","containers":[{"ports":[{"containerPort":8000,"protocol":"TCP"},{"containerPort":9999}]}]},
  "status":{"phase":"Running","podIP":"10.1.2.3"}},
 {"metadata":{"name":"triton-0","namespace":"ml","labels":{"gryvia.io/job":"serve"}},
  "spec":{"nodeName":"node-a","containers":[{"ports":[{"containerPort":8002},{"containerPort":8000}]}]},
  "status":{"phase":"Running","podIP":"10.1.2.4"}},
 {"metadata":{"name":"other-node","namespace":"ml","labels":{"gryvia.io/job":"serve"}},
  "spec":{"nodeName":"node-b","containers":[{"ports":[{"containerPort":8000}]}]},"status":{"phase":"Running","podIP":"10.1.2.5"}},
 {"metadata":{"name":"no-label","namespace":"ml"},
  "spec":{"nodeName":"node-a","containers":[{"ports":[{"containerPort":8000}]}]},"status":{"phase":"Running","podIP":"10.1.2.6"}},
 {"metadata":{"name":"hostnet","namespace":"ml","labels":{"gryvia.io/job":"serve"}},
  "spec":{"nodeName":"node-a","hostNetwork":true,"containers":[{"ports":[{"containerPort":8000}]}]},"status":{"phase":"Running","podIP":"192.168.0.1"}},
 {"metadata":{"name":"pending","namespace":"ml","labels":{"gryvia.io/job":"serve"}},
  "spec":{"nodeName":"node-a","containers":[{"ports":[{"containerPort":8000}]}]},"status":{"phase":"Pending","podIP":"10.1.2.7"}},
 {"metadata":{"name":"udp","namespace":"ml","labels":{"gryvia.io/job":"serve"}},
  "spec":{"nodeName":"node-a","containers":[{"ports":[{"containerPort":8000,"protocol":"UDP"}]}]},"status":{"phase":"Running","podIP":"10.1.2.8"}},
 {"metadata":{"name":"badip","namespace":"ml","labels":{"gryvia.io/job":"serve"}},
  "spec":{"nodeName":"node-a","containers":[{"ports":[{"containerPort":8000}]}]},"status":{"phase":"Running","podIP":"not-an-ip/../x"}}
]}`

type fakeAPI struct {
	body string
	err  error
	path string
}

func (f *fakeAPI) Get(_ context.Context, p string) ([]byte, error) {
	f.path = p
	return []byte(f.body), f.err
}

func TestDiscover(t *testing.T) {
	api := &fakeAPI{body: podsJSON}
	got, err := Discover(context.Background(), api, "node-a", []int{8000, 8002})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(api.path, "fieldSelector=spec.nodeName%3Dnode-a") {
		t.Errorf("path = %q", api.path)
	}
	var names []string
	for _, tg := range got {
		names = append(names, tg.Name+"="+tg.URL)
	}
	want := []string{
		"triton-0:8000=http://10.1.2.4:8000/metrics",
		"triton-0:8002=http://10.1.2.4:8002/metrics",
		"vllm-0:8000=http://10.1.2.3:8000/metrics",
	}
	if strings.Join(names, " ") != strings.Join(want, " ") {
		t.Errorf("targets = %v", names)
	}
	if got[0].Namespace != "ml" || got[0].Job != "serve" || got[0].Pod != "triton-0" {
		t.Errorf("attribution = %+v", got[0])
	}
	if _, err := Discover(context.Background(), api, "", []int{8000}); err == nil {
		t.Error("missing node name")
	}
	api.err = errors.New("boom")
	if _, err := Discover(context.Background(), api, "node-a", nil); err == nil {
		t.Error("api error")
	}
}

// engineServer serves the fixture files in order, one per request.
func engineServer(t *testing.T, files ...string) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	i := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		f := files[min(i, len(files)-1)]
		i++
		mu.Unlock()
		b, err := os.ReadFile("testdata/" + f)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		_, _ = w.Write(b)
	}))
}

func TestScraperEndToEnd(t *testing.T) {
	srv := engineServer(t, "vllm_v0_t0.txt", "vllm_v0_t1.txt")
	defer srv.Close()
	var mu sync.Mutex
	var got []Reading
	var gotT []Target
	s := NewScraper([]Target{{Name: "v", URL: srv.URL + "/metrics", Namespace: "ml", Job: "serve"}}, nil,
		func(tg Target, r Reading) { mu.Lock(); got = append(got, r); gotT = append(gotT, tg); mu.Unlock() }, nil)
	now := t0
	s.Now = func() time.Time { return now }
	s.ScrapeOnce(context.Background())
	now = now.Add(15 * time.Second)
	s.ScrapeOnce(context.Background())
	if len(got) != 2 || gotT[0].Job != "serve" {
		t.Fatalf("sink calls = %d", len(got))
	}
	unmeasured(t, "first scrape ttft", got[0].TTFTP99)
	near(t, "second scrape ttft", got[1].TTFTP99, 475)
	st := s.Status()
	if len(st) != 1 || st[0].Engine != "vllm" || st[0].Error != "" || len(st[0].Available) == 0 || len(st[0].Missing) != 2 {
		t.Errorf("status = %+v", st)
	}
}

func TestScraperErrors(t *testing.T) {
	notFound := httptest.NewServer(http.NotFoundHandler())
	defer notFound.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data", http.StatusFound)
	}))
	defer redirect.Close()
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("# TYPE process_cpu_seconds_total counter\nprocess_cpu_seconds_total 1\n"))
	}))
	defer other.Close()
	huge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		line := "# " + strings.Repeat("x", 1000) + "\n"
		for i := 0; i < (MaxBody/len(line))+10; i++ {
			_, _ = w.Write([]byte(line))
		}
	}))
	defer huge.Close()
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("# TYPE vllm:something_else gauge\nvllm:something_else 1\n"))
	}))
	defer empty.Close()

	calls := 0
	s := NewScraper([]Target{
		{Name: "nf", URL: notFound.URL}, {Name: "rd", URL: redirect.URL}, {Name: "ot", URL: other.URL},
		{Name: "hg", URL: huge.URL}, {Name: "em", URL: empty.URL}, {Name: "dead", URL: "http://127.0.0.1:1/metrics"},
	}, nil, func(Target, Reading) { calls++ }, nil)
	s.Client.Timeout = 2 * time.Second
	s.ScrapeOnce(context.Background())
	if calls != 0 {
		t.Errorf("sink called %d times for failing targets", calls)
	}
	byName := map[string]TargetStatus{}
	for _, st := range s.Status() {
		byName[st.Name] = st
	}
	for _, name := range []string{"nf", "rd", "ot", "hg", "em", "dead"} {
		if byName[name].Error == "" {
			t.Errorf("%s should report an error: %+v", name, byName[name])
		}
	}
	if !strings.Contains(byName["nf"].Error, "404") || !strings.Contains(byName["rd"].Error, "302") {
		t.Errorf("errors: nf=%q rd=%q", byName["nf"].Error, byName["rd"].Error)
	}
	if !strings.Contains(byName["hg"].Error, "8 MiB") {
		t.Errorf("hg = %q", byName["hg"].Error)
	}
	if byName["em"].Engine != "vllm" {
		t.Errorf("em should be recognised as vllm: %+v", byName["em"])
	}
}

func TestScraperDiscoveryAndForgetting(t *testing.T) {
	srv := engineServer(t, "tgi_t0.txt")
	defer srv.Close()
	pods := []Target{{Name: "p:80", URL: srv.URL, Namespace: "ml", Job: "serve", Pod: "p"}}
	fails := false
	s := NewScraper(nil, func(context.Context) ([]Target, error) {
		if fails {
			return nil, errors.New("api down")
		}
		return pods, nil
	}, func(Target, Reading) {}, nil)
	now := t0
	s.Now = func() time.Time { return now }
	s.ScrapeOnce(context.Background())
	if len(s.Status()) != 1 {
		t.Fatal("discovered target scraped")
	}
	// Discovery fails: the last known set is kept.
	fails = true
	now = now.Add(DiscoverEvery + time.Second)
	s.ScrapeOnce(context.Background())
	if len(s.Status()) != 1 {
		t.Error("a failing pod list must not drop targets")
	}
	// Pod is gone: its state is forgotten.
	fails, pods = false, nil
	now = now.Add(DiscoverEvery + time.Second)
	s.ScrapeOnce(context.Background())
	if len(s.Status()) != 0 {
		t.Errorf("status = %+v", s.Status())
	}
}
