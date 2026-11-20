// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package sources

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Cross-language vector: identical constants in collector/listener_security_test.go
// (TestAPISignatureCrossLanguageVector) and services/api-gateway/tests/test_collector_transport.py.
const (
	vectorToken = "0123456789abcdef0123456789abcdef"
	vectorURI   = "/api/v1/graph?x=1"
	vectorStamp = "1700000000"
	vectorSig   = "31bcceb2e106086a5b3623200f4b7f54c95ba27df0dbd81918ba88c898b1e501"
)

func TestSignMatchesCollectorVector(t *testing.T) {
	if got := Sign([]byte(vectorToken), "GET", vectorURI, vectorStamp); got != vectorSig {
		t.Fatalf("signature %s, want the collector's vector %s", got, vectorSig)
	}
}

func tokenFile(t *testing.T, tok string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte(tok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// collectorServer starts a fake collector (optionally verifying the HMAC like the real one) and returns
// the host and port to use.
func collectorServer(t *testing.T, token string, now time.Time, routes map[string]string) (Target, int, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token != "" {
			stamp := r.Header.Get(APITimeHeader)
			want := Sign([]byte(token), r.Method, r.URL.RequestURI(), stamp)
			when, err := strconv.ParseInt(stamp, 10, 64)
			if err != nil || r.Header.Get(APISignatureHeader) != want || now.Unix()-when > 30 || when-now.Unix() > 30 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		body, ok := routes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	p, _ := strconv.Atoi(port)
	return Target{Node: "n-" + port, Pod: "c-" + port, Host: host}, p, srv
}

const graphA = `{"nodes":[{"id":"web","service":"web","namespace":"shop","pod_count":1},{"id":"api","service":"api","namespace":"shop","pod_count":2}],
"edges":[{"source":"web","target":"api","protocol":"TCP","port":8080,"bytes_total":100,"latency_p50_ms":10,"flow_count":10,"last_seen":"2026-01-01T00:00:10Z"}]}`
const graphB = `{"nodes":[{"id":"web","service":"web","namespace":"shop","pod_count":1}],
"edges":[{"source":"web","target":"api","protocol":"TCP","port":8080,"bytes_total":300,"latency_p50_ms":20,"flow_count":30,"last_seen":"2026-01-01T00:00:20Z"},
{"source":"api","target":"db","protocol":"TCP","port":5432,"bytes_total":50,"latency_p50_ms":0,"flow_count":5,"last_seen":"2026-01-01T00:00:05Z"}]}`

func TestCollectorSignsAndMerges(t *testing.T) {
	now := time.Now()
	t1, port, _ := collectorServer(t, vectorToken, now, map[string]string{"/api/v1/graph": graphA})
	// Both fake nodes listen on one port each; the client uses one configured port, so serve both from
	// one server and give the two targets the same host: distinct nodes are simulated by two calls' worth
	// of bodies in TestMergeGraphs. Here we check signing end to end against one collector.
	c := NewCollectorClient(CollectorConfig{Namespace: "gryvia-network", Port: port, TokenFile: tokenFile(t, vectorToken)},
		func(context.Context) ([]Target, error) { return []Target{t1}, nil })
	c.now = func() time.Time { return now }
	g, stats, err := c.Graph(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Reachable != 1 || stats.Total != 1 || len(g.Edges) != 1 || g.Edges[0].BytesTotal != 100 {
		t.Fatalf("unexpected graph %+v stats %+v", g, stats)
	}
}

func TestCollectorRejectedWithoutOrWrongToken(t *testing.T) {
	now := time.Now()
	t1, port, _ := collectorServer(t, vectorToken, now, map[string]string{"/api/v1/graph": graphA})
	disc := func(context.Context) ([]Target, error) { return []Target{t1}, nil }
	for name, cfg := range map[string]CollectorConfig{
		"unsigned":    {Port: port},
		"wrong token": {Port: port, TokenFile: tokenFile(t, strings.Repeat("x", 40))},
	} {
		c := NewCollectorClient(cfg, disc)
		c.now = func() time.Time { return now }
		_, _, err := c.Graph(context.Background())
		var se *SourceError
		if !errors.As(err, &se) || se.Reason != ReasonUnreachable || !strings.Contains(se.Message, "401") {
			t.Fatalf("%s: want Unreachable with HTTP 401, got %v", name, err)
		}
	}
}

func TestShortOrMissingTokenFileFailsClosed(t *testing.T) {
	disc := func(context.Context) ([]Target, error) { return []Target{{Node: "n", Host: "127.0.0.1"}}, nil }
	for name, path := range map[string]string{"short": tokenFile(t, "short"), "missing": filepath.Join(t.TempDir(), "nope")} {
		c := NewCollectorClient(CollectorConfig{Port: 1, TokenFile: path}, disc)
		_, _, err := c.Graph(context.Background())
		var se *SourceError
		if !errors.As(err, &se) || se.Reason != ReasonMisconfigured {
			t.Fatalf("%s: want Misconfigured, got %v", name, err)
		}
	}
}

func TestTokenIsReadPerRequest(t *testing.T) {
	now := time.Now()
	t1, port, _ := collectorServer(t, vectorToken, now, map[string]string{"/api/v1/graph": graphA})
	path := tokenFile(t, strings.Repeat("y", 40))
	c := NewCollectorClient(CollectorConfig{Port: port, TokenFile: path}, func(context.Context) ([]Target, error) { return []Target{t1}, nil })
	c.now = func() time.Time { return now }
	if _, _, err := c.Graph(context.Background()); err == nil {
		t.Fatal("wrong token must fail")
	}
	if err := os.WriteFile(path, []byte(vectorToken), 0o600); err != nil { // rotated Secret
		t.Fatal(err)
	}
	if _, _, err := c.Graph(context.Background()); err != nil {
		t.Fatalf("rotated token not picked up: %v", err)
	}
}

func TestNoCollectorsAndDiscoveryErrors(t *testing.T) {
	c := NewCollectorClient(CollectorConfig{Namespace: "ns"}, func(context.Context) ([]Target, error) { return nil, nil })
	_, _, err := c.Graph(context.Background())
	var se *SourceError
	if !errors.As(err, &se) || se.Reason != ReasonNoCollectors || !strings.Contains(se.Message, "ns") {
		t.Fatalf("want NoCollectors, got %v", err)
	}
	c = NewCollectorClient(CollectorConfig{}, func(context.Context) ([]Target, error) {
		return nil, &SourceError{Source: "collector", Reason: ReasonUnreachable, Message: "boom"}
	})
	if _, _, err = c.Graph(context.Background()); !errors.As(err, &se) || se.Reason != ReasonUnreachable {
		t.Fatalf("want Unreachable, got %v", err)
	}
}

func TestPartialNodeFailure(t *testing.T) {
	now := time.Now()
	good, port, _ := collectorServer(t, "", now, map[string]string{"/api/v1/graph": graphA})
	// second target: same port, but nothing listens on that host address
	dead := Target{Node: "dead", Host: "127.0.0.2"}
	c := NewCollectorClient(CollectorConfig{Port: port, Timeout: 500 * time.Millisecond},
		func(context.Context) ([]Target, error) { return []Target{good, dead}, nil })
	g, stats, err := c.Graph(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Total != 2 || stats.Reachable != 1 || len(g.Edges) != 1 {
		t.Fatalf("stats %+v edges %d", stats, len(g.Edges))
	}
}

func TestInvalidJSONAndOversizeCountAsFailures(t *testing.T) {
	now := time.Now()
	t1, port, _ := collectorServer(t, "", now, map[string]string{"/api/v1/graph": "not json", "/api/v1/anomalies": strings.Repeat("[", 100)})
	disc := func(context.Context) ([]Target, error) { return []Target{t1}, nil }
	c := NewCollectorClient(CollectorConfig{Port: port}, disc)
	if _, _, err := c.Graph(context.Background()); err == nil {
		t.Fatal("invalid JSON must be an error")
	}
	c = NewCollectorClient(CollectorConfig{Port: port, MaxResponseBytes: 10}, disc)
	if _, _, err := c.Anomalies(context.Background()); err == nil || !strings.Contains(err.Error(), "larger") {
		t.Fatalf("oversize response must fail, got %v", err)
	}
}

func TestSecurityAlertsUseCollectorJSONTags(t *testing.T) {
	now := time.Now()
	body := `{"alerts":[{"timestamp":"2026-01-01T00:00:01Z","event_type":"crypto_mining","severity":"high","pid":7,"uid":0,
"process_name":"xmrig","details":"pool connect","src_ip":"10.0.0.9","dst_ip":"1.2.3.4","dst_port":3333,"path":"/tmp/x"}],"counts":{"crypto_mining":1}}`
	t1, port, _ := collectorServer(t, "", now, map[string]string{"/api/v1/security/alerts": body})
	c := NewCollectorClient(CollectorConfig{Port: port}, func(context.Context) ([]Target, error) { return []Target{t1}, nil })
	alerts, _, err := c.SecurityAlerts(context.Background())
	if err != nil || len(alerts) != 1 {
		t.Fatalf("%v %v", alerts, err)
	}
	a := alerts[0]
	if a.EventType != "crypto_mining" || a.ProcessName != "xmrig" || a.SrcIP != "10.0.0.9" || a.Details != "pool connect" || a.DstPort != 3333 {
		t.Fatalf("collector fields not decoded: %+v", a)
	}
}

func TestMergeGraphs(t *testing.T) {
	var a, b Graph
	if err := json.Unmarshal([]byte(graphA), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(graphB), &b); err != nil {
		t.Fatal(err)
	}
	m := MergeGraphs([]Graph{a, b})
	if len(m.Nodes) != 2 || m.Nodes[0].ID != "api" || m.Nodes[1].PodCount != 2 {
		t.Fatalf("nodes %+v", m.Nodes)
	}
	if len(m.Edges) != 2 {
		t.Fatalf("edges %+v", m.Edges)
	}
	e := m.Edges[1] // web -> api (sorted after api -> db)
	if e.Source != "web" || e.BytesTotal != 400 || e.FlowCount != 40 {
		t.Fatalf("summed edge %+v", e)
	}
	// weighted latency: (10*10 + 20*30)/40 = 17.5
	if e.LatencyMs < 17.49 || e.LatencyMs > 17.51 || !e.LastSeen.Equal(time.Date(2026, 1, 1, 0, 0, 20, 0, time.UTC)) {
		t.Fatalf("latency/last_seen %+v", e)
	}
	// deterministic regardless of input order
	m2 := MergeGraphs([]Graph{b, a})
	j1, _ := json.Marshal(m)
	j2, _ := json.Marshal(m2)
	if string(j1) != string(j2) {
		t.Fatal("merge must not depend on node order")
	}
}

func TestMergeAnomaliesAndAlertsDedupe(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	an := Anomaly{Type: "latency_spike", Service: "web", DetectedAt: at}
	got := MergeAnomalies([][]Anomaly{{an}, {an, {Type: "traffic_burst", Service: "web", DetectedAt: at.Add(time.Second)}}})
	if len(got) != 2 || got[0].Type != "latency_spike" {
		t.Fatalf("%+v", got)
	}
	al := SecurityAlert{Timestamp: at, EventType: "crypto_mining", PID: 1, ProcessName: "x"}
	if got := MergeAlerts([][]SecurityAlert{{al}, {al}}); len(got) != 1 {
		t.Fatalf("%+v", got)
	}
}

func TestNetraClient(t *testing.T) {
	var gotAuth, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotQuery = r.Header.Get("Authorization"), r.URL.RawQuery
		if r.URL.Path != NetraHistoryPath {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"records":[{"observedAt":"2026-01-01T00:00:00Z","namespace":"shop","pod":"web-1","peer":"api","protocol":"tcp","port":8080,"bytes":42,"blocked":false}]}`))
	}))
	defer srv.Close()
	n := NewNetraClient(NetraFromEnv(func(k string) string {
		return map[string]string{"GRYVIA_NETRA_URL": srv.URL + "/", "GRYVIA_NETRA_TOKEN": "tok"}[k]
	}))
	if !n.Configured() {
		t.Fatal("configured")
	}
	recs, err := n.History(context.Background(), 90*time.Second, 100)
	if err != nil || len(recs) != 1 || recs[0].Pod != "web-1" || recs[0].Bytes != 42 {
		t.Fatalf("%v %v", recs, err)
	}
	q, _ := url.ParseQuery(gotQuery)
	if gotAuth != "Bearer tok" || q.Get("since") != "90s" || q.Get("limit") != "100" {
		t.Fatalf("auth %q query %q", gotAuth, gotQuery)
	}
	off := NewNetraClient(NetraConfig{})
	var se *SourceError
	if _, err := off.History(context.Background(), time.Minute, 1); !errors.As(err, &se) || se.Reason != ReasonNotConfigured {
		t.Fatalf("want NotConfigured, got %v", err)
	}
	down := NewNetraClient(NetraConfig{URL: "http://127.0.0.1:1"})
	if _, err := down.History(context.Background(), time.Minute, 1); !errors.As(err, &se) || se.Reason != ReasonUnreachable {
		t.Fatalf("want Unreachable, got %v", err)
	}
}

func TestFormatSince(t *testing.T) {
	for d, want := range map[time.Duration]string{time.Hour: "1h", 15 * time.Minute: "15m", 90 * time.Second: "90s", 1500 * time.Millisecond: "2s"} {
		if got := formatSince(d); got != want {
			t.Errorf("%v -> %s, want %s", d, got, want)
		}
	}
}
