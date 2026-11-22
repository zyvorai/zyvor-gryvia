// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package llmgateway

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

// phasedSource serves one scale-to-zero route whose phase the test (or the fake activator) changes.
type phasedSource struct {
	staticSource
	mu    sync.Mutex
	phase string
	reads int
	// readyAfter switches a Deploying route to Ready after that many reads.
	readyAfter int
}

func (s *phasedSource) Routes(context.Context) ([]Route, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	if s.phase == PhaseDeploying && s.readyAfter > 0 && s.reads >= s.readyAfter {
		s.phase = "Ready"
	}
	out := make([]Route, len(s.routes))
	copy(out, s.routes)
	for i := range out {
		out[i].Phase = s.phase
	}
	return out, nil
}

func (s *phasedSource) setPhase(p string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.phase, s.reads = p, 0
}

type fakeActivator struct {
	mu      sync.Mutex
	src     *phasedSource
	wakes   []string
	touches []time.Time
}

func (a *fakeActivator) Wake(_ context.Context, ns, svc string, _ time.Time) error {
	a.mu.Lock()
	a.wakes = append(a.wakes, ns+"/"+svc)
	a.mu.Unlock()
	if a.src != nil {
		a.src.setPhase(PhaseDeploying)
	}
	return nil
}

func (a *fakeActivator) Touch(_ context.Context, _, _ string, at time.Time) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.touches = append(a.touches, at)
	return nil
}

func (a *fakeActivator) counts() (int, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.wakes), len(a.touches)
}

// clock advances by step on every read so cold-start deadlines pass without real waiting.
type clock struct {
	mu   sync.Mutex
	now  time.Time
	step time.Duration
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(c.step)
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func scaleToZeroGateway(t *testing.T, phase string, idle, coldStart time.Duration) (*Gateway, *upstream, *phasedSource, *fakeActivator, *clock) {
	t.Helper()
	gw, up, _ := newGateway(t, func(url string) []Route {
		return []Route{{Model: "chat", Namespace: "team-a", Service: "chat", Upstream: url, ServedModel: "chat",
			ScaleToZero: true, Idle: idle, ColdStart: coldStart}}
	})
	src := &phasedSource{staticSource: gw.Source.(staticSource), phase: phase}
	act := &fakeActivator{src: src}
	clk := &clock{now: time.Date(2026, 10, 1, 10, 30, 0, 0, time.UTC)}
	gw.Source, gw.Activator, gw.Now, gw.WakePoll = src, act, clk.Now, time.Millisecond
	return gw, up, src, act, clk
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

const chatBody = `{"model":"chat","messages":[{"role":"user","content":"hi"}]}`

func TestScaledToZeroRequestWakesAndWaitsUntilReady(t *testing.T) {
	gw, up, src, act, _ := scaleToZeroGateway(t, PhaseScaledToZero, 15*time.Minute, 5*time.Minute)
	src.readyAfter = 3
	rec := call(t, gw, "POST", "/v1/chat/completions", keyA, chatBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("cold start = %d %s", rec.Code, rec.Body)
	}
	if wakes, _ := act.counts(); wakes != 1 || act.wakes[0] != "team-a/chat" {
		t.Fatalf("wakes = %v", act.wakes)
	}
	if len(up.bodies) != 1 {
		t.Fatalf("upstream calls = %d", len(up.bodies))
	}
	if v := testutil.ToFloat64(gw.Meter.tokens.WithLabelValues("team-a", "chat", "input")); v != 11 {
		t.Fatalf("a cold-started request is metered: input tokens = %v", v)
	}
	waitFor(t, "last-request touch", func() bool { _, n := act.counts(); return n == 1 })
}

func TestDeployingRouteWaitsWithoutWaking(t *testing.T) {
	gw, _, src, act, _ := scaleToZeroGateway(t, PhaseDeploying, 15*time.Minute, 5*time.Minute)
	src.readyAfter = 2
	if rec := call(t, gw, "POST", "/v1/chat/completions", keyA, chatBody); rec.Code != http.StatusOK {
		t.Fatalf("deploying = %d %s", rec.Code, rec.Body)
	}
	if wakes, _ := act.counts(); wakes != 0 {
		t.Fatalf("a service already starting was woken again: %v", act.wakes)
	}
}

func TestColdStartTimeoutReturns503WithRetryAfter(t *testing.T) {
	gw, up, src, act, clk := scaleToZeroGateway(t, PhaseScaledToZero, 15*time.Minute, 3*time.Second)
	act.src = nil // the controller never acts
	clk.step = time.Second
	rec := call(t, gw, "POST", "/v1/chat/completions", keyA, chatBody)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "30" {
		t.Fatalf("timeout = %d Retry-After=%q %s", rec.Code, rec.Header().Get("Retry-After"), rec.Body)
	}
	if len(up.bodies) != 0 {
		t.Fatalf("a request reached a service with no replicas")
	}
	if v := testutil.ToFloat64(gw.Meter.requests.WithLabelValues("team-a", "chat", "503")); v != 1 {
		t.Fatalf("503 requests = %v", v)
	}
	if wakes, _ := act.counts(); wakes != 1 {
		t.Fatalf("wakes within 3s = %d, want 1 (repeated at most every %s)", wakes, wakeEvery)
	}
	_ = src
}

func TestClientGoneWhileWaiting(t *testing.T) {
	gw, up, _, act, _ := scaleToZeroGateway(t, PhaseScaledToZero, 15*time.Minute, 5*time.Minute)
	act.src = nil
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		req, _ := http.NewRequestWithContext(ctx, "POST", "/v1/chat/completions", strings.NewReader(chatBody))
		req.Header.Set("Authorization", "Bearer "+keyA)
		gw.ServeHTTP(httptest.NewRecorder(), req)
	}()
	waitFor(t, "wake", func() bool { n, _ := act.counts(); return n == 1 })
	cancel()
	<-done
	if len(up.bodies) != 0 {
		t.Fatalf("upstream called after the client left")
	}
}

func TestLastRequestIsThrottledPerService(t *testing.T) {
	gw, _, _, act, clk := scaleToZeroGateway(t, "Ready", 15*time.Minute, 5*time.Minute)
	for i := 0; i < 3; i++ {
		if rec := call(t, gw, "POST", "/v1/chat/completions", keyA, chatBody); rec.Code != 200 {
			t.Fatalf("chat = %d", rec.Code)
		}
	}
	waitFor(t, "first touch", func() bool { _, n := act.counts(); return n == 1 })
	clk.advance(61 * time.Second)
	call(t, gw, "POST", "/v1/chat/completions", keyA, chatBody)
	waitFor(t, "touch after a minute", func() bool { _, n := act.counts(); return n == 2 })
	if wakes, _ := act.counts(); wakes != 0 {
		t.Fatalf("a ready service was woken")
	}
}

func TestTouchEveryFollowsShortIdle(t *testing.T) {
	for idle, want := range map[time.Duration]time.Duration{
		60 * time.Second: 20 * time.Second, 15 * time.Minute: time.Minute, 0: time.Minute,
	} {
		if got := touchEvery(idle); got != want {
			t.Errorf("touchEvery(%s) = %s, want %s", idle, got, want)
		}
	}
}

func TestRoutesWithoutScaleToZeroAreNotTouched(t *testing.T) {
	gw, _, _ := newGateway(t, defaultRoutes)
	act := &fakeActivator{}
	gw.Activator = act
	if rec := call(t, gw, "POST", "/v1/chat/completions", keyA, chatBody); rec.Code != 200 {
		t.Fatalf("chat = %d", rec.Code)
	}
	time.Sleep(10 * time.Millisecond)
	if w, n := act.counts(); w != 0 || n != 0 {
		t.Fatalf("wakes=%d touches=%d for a service without scaleToZero", w, n)
	}
}

func TestRoutesCarryScaleToZero(t *testing.T) {
	svc := gryviav1.GryviaInferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "chat", Namespace: "team-a", Annotations: map[string]string{AnnotationModel: "chat"}},
		Spec: gryviav1.GryviaInferenceServiceSpec{ScaleToZero: &gryviav1.ScaleToZeroConfig{
			Enabled: true, IdleSeconds: 120, ColdStartTimeoutSeconds: 60}},
		Status: gryviav1.GryviaInferenceServiceStatus{Endpoint: "http://chat.team-a:8000", Phase: PhaseScaledToZero},
	}
	plain := svc.DeepCopy()
	plain.Name, plain.Annotations = "plain", map[string]string{AnnotationModel: "plain"}
	plain.Spec.ScaleToZero = nil
	routes := routesFrom([]gryviav1.GryviaInferenceService{svc, *plain}, 0, 0)
	if len(routes) != 2 {
		t.Fatalf("routes = %+v", routes)
	}
	r := routes[0]
	if !r.ScaleToZero || r.Idle != 2*time.Minute || r.ColdStart != time.Minute || !r.Waking() {
		t.Fatalf("scale-to-zero route = %+v", r)
	}
	if p := routes[1]; p.ScaleToZero || p.Waking() {
		t.Fatalf("plain route = %+v", p)
	}
}

func TestRefusedConnectionAfterWakeIsRetried(t *testing.T) {
	gw, _, src, _, _ := scaleToZeroGateway(t, PhaseScaledToZero, 15*time.Minute, 5*time.Minute)
	src.readyAfter = 1
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close() // refused until the server below takes the port
	src.routes[0].Upstream = "http://" + addr
	go func() {
		time.Sleep(50 * time.Millisecond)
		l, err := net.Listen("tcp", addr)
		if err != nil {
			return
		}
		srv := &httptest.Server{Listener: l, Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
		})}}
		srv.Start()
		t.Cleanup(srv.Close)
	}()
	if rec := call(t, gw, "POST", "/v1/chat/completions", keyA, chatBody); rec.Code != http.StatusOK {
		t.Fatalf("refused then serving = %d %s", rec.Code, rec.Body)
	}
}

func TestRefusedConnectionWithoutWakeIsNotRetried(t *testing.T) {
	gw, _, src, _, _ := scaleToZeroGateway(t, "Ready", 15*time.Minute, 5*time.Minute)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	src.routes[0].Upstream = "http://" + l.Addr().String()
	_ = l.Close()
	start := time.Now()
	if rec := call(t, gw, "POST", "/v1/chat/completions", keyA, chatBody); rec.Code != http.StatusBadGateway {
		t.Fatalf("refused = %d", rec.Code)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("a warm service's refused connection was retried")
	}
}
