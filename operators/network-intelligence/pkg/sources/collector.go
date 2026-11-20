// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package sources

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CollectorSelector is the label the chart puts on collector pods (same as the gateway's discovery).
const CollectorSelectorKey, CollectorSelectorValue = "app.kubernetes.io/component", "collector"

// Request signing (must stay identical to signAPI in collector/listener_security.go and
// api_signature_headers in services/api-gateway/routers/collector_transport.py).
const (
	APITimeHeader      = "X-Gryvia-Time"
	APISignatureHeader = "X-Gryvia-Signature"
	APISignaturePrefix = "GRYVIA-API-V1\n"
	minTokenLen        = 32   // the collector's floor (flightMinTokenLen)
	maxTokenFile       = 4096 // the collector's flightMaxTokenFile
)

// Sign returns hex(HMAC-SHA256(token, "GRYVIA-API-V1\n"+METHOD+"\n"+REQUEST-URI+"\n"+UNIX)).
func Sign(token []byte, method, requestURI, stamp string) string {
	mac := hmac.New(sha256.New, token)
	_, _ = mac.Write([]byte(APISignaturePrefix + method + "\n" + requestURI + "\n" + stamp))
	return hex.EncodeToString(mac.Sum(nil))
}

// CollectorConfig configures the collector client. The zero value of every optional field is safe.
type CollectorConfig struct {
	// Namespace holds the collector pods (label app.kubernetes.io/component=collector).
	Namespace string
	// Port of the collector listener (default 9090).
	Port int
	// TokenFile is a mounted Secret file with the collector API token. It is read on EVERY request so
	// a rotated Secret is picked up. Empty means unsigned requests (collector without -api-token-file).
	TokenFile string
	// TLS switches to https with the system trust store; CAFile (a CA bundle) implies https and pins
	// the trust. CertFile/KeyFile are the client certificate for a collector with mTLS. ServerName is
	// the name verified in the collector certificate (collectors are reached by pod IP).
	TLS                       bool
	CAFile, CertFile, KeyFile string
	ServerName                string
	Timeout                   time.Duration // per request, default 5s
	MaxConcurrency            int           // parallel node requests, default 8
	MaxResponseBytes          int64         // per node response, default 8 MiB
	MaxTargets                int           // collectors contacted per call, default 500
}

func (c CollectorConfig) withDefaults() CollectorConfig {
	if c.Port <= 0 {
		c.Port = 9090
	}
	if c.Timeout <= 0 {
		c.Timeout = 5 * time.Second
	}
	if c.MaxConcurrency <= 0 {
		c.MaxConcurrency = 8
	}
	if c.MaxResponseBytes <= 0 {
		c.MaxResponseBytes = 8 << 20
	}
	if c.MaxTargets <= 0 {
		c.MaxTargets = 500
	}
	return c
}

func (c CollectorConfig) https() bool { return c.TLS || c.CAFile != "" }

// Target is one collector pod.
type Target struct {
	Node string
	Pod  string
	Host string // pod IP
}

// Discoverer lists the running collectors.
type Discoverer func(ctx context.Context) ([]Target, error)

// KubeDiscoverer lists Running collector pods with an IP in namespace. Pass an uncached reader
// (mgr.GetAPIReader()) so the operator does not start a cluster-wide pod informer for this.
func KubeDiscoverer(reader client.Reader, namespace string) Discoverer {
	return func(ctx context.Context) ([]Target, error) {
		var pods corev1.PodList
		err := reader.List(ctx, &pods, client.InNamespace(namespace),
			client.MatchingLabels{CollectorSelectorKey: CollectorSelectorValue})
		if err != nil {
			return nil, &SourceError{Source: "collector", Reason: ReasonUnreachable,
				Message: "cannot list collector pods in namespace " + namespace + ": " + err.Error()}
		}
		var out []Target
		for _, p := range pods.Items {
			if p.Status.Phase != corev1.PodRunning || p.Status.PodIP == "" || p.DeletionTimestamp != nil {
				continue
			}
			out = append(out, Target{Node: p.Spec.NodeName, Pod: p.Name, Host: p.Status.PodIP})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Pod < out[j].Pod })
		return out, nil
	}
}

// CollectorClient fans a GET out to every collector and merges the answers.
type CollectorClient struct {
	cfg      CollectorConfig
	discover Discoverer
	now      func() time.Time
}

var _ Collector = (*CollectorClient)(nil)

// NewCollectorClient builds a client. discover must not be nil.
func NewCollectorClient(cfg CollectorConfig, discover Discoverer) *CollectorClient {
	return &CollectorClient{cfg: cfg.withDefaults(), discover: discover, now: time.Now}
}

func readToken(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, &SourceError{Source: "collector", Reason: ReasonMisconfigured,
			Message: "collector token file is unreadable: " + err.Error()}
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxTokenFile+1))
	if err != nil {
		return nil, &SourceError{Source: "collector", Reason: ReasonMisconfigured, Message: "collector token file: " + err.Error()}
	}
	tok := strings.TrimSpace(string(raw))
	if len(raw) > maxTokenFile || len(tok) < minTokenLen {
		return nil, &SourceError{Source: "collector", Reason: ReasonMisconfigured,
			Message: fmt.Sprintf("collector token file must hold a token of %d..%d characters", minTokenLen, maxTokenFile)}
	}
	return []byte(tok), nil
}

func (c *CollectorClient) httpClient() (*http.Client, error) {
	tr := &http.Transport{DisableKeepAlives: true, Proxy: nil, ResponseHeaderTimeout: c.cfg.Timeout}
	if c.cfg.https() {
		tc := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: c.cfg.ServerName}
		if c.cfg.CAFile != "" {
			pem, err := os.ReadFile(c.cfg.CAFile)
			if err != nil {
				return nil, &SourceError{Source: "collector", Reason: ReasonMisconfigured, Message: "collector CA file: " + err.Error()}
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, &SourceError{Source: "collector", Reason: ReasonMisconfigured, Message: "collector CA file holds no certificate"}
			}
			tc.RootCAs = pool
		}
		if c.cfg.CertFile != "" || c.cfg.KeyFile != "" {
			cert, err := tls.LoadX509KeyPair(c.cfg.CertFile, c.cfg.KeyFile)
			if err != nil {
				return nil, &SourceError{Source: "collector", Reason: ReasonMisconfigured, Message: "collector client certificate: " + err.Error()}
			}
			tc.Certificates = []tls.Certificate{cert}
		}
		tr.TLSClientConfig = tc
	}
	return &http.Client{
		Transport: tr,
		Timeout:   c.cfg.Timeout,
		// Never follow a redirect with a signed request.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

type nodeBody struct {
	target Target
	body   []byte
	err    error
}

// fetchAll GETs path (request URI, including any query) from every collector. It returns the bodies
// of the collectors that answered. A *SourceError means none did (or none exists).
func (c *CollectorClient) fetchAll(ctx context.Context, requestURI string) ([]nodeBody, Stats, error) {
	targets, err := c.discover(ctx)
	if err != nil {
		return nil, Stats{}, err
	}
	if len(targets) == 0 {
		return nil, Stats{}, &SourceError{Source: "collector", Reason: ReasonNoCollectors,
			Message: "no running collector pod (label app.kubernetes.io/component=collector) in namespace " + c.cfg.Namespace +
				"; enable the collector (helm ebpf.enabled) or point --collector-namespace at it"}
	}
	if len(targets) > c.cfg.MaxTargets {
		targets = targets[:c.cfg.MaxTargets]
	}
	hc, err := c.httpClient()
	if err != nil {
		return nil, Stats{Total: len(targets)}, err
	}
	defer hc.CloseIdleConnections()

	results := make([]nodeBody, len(targets))
	sem := make(chan struct{}, c.cfg.MaxConcurrency)
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, t Target) {
			defer wg.Done()
			defer func() { <-sem }()
			body, err := c.get(ctx, hc, t, requestURI)
			results[i] = nodeBody{target: t, body: body, err: err}
		}(i, t)
	}
	wg.Wait()

	stats := Stats{Total: len(targets)}
	var ok []nodeBody
	var firstErr error
	for _, r := range results {
		if r.err != nil {
			var se *SourceError
			if firstErr == nil || (asSource(r.err, &se) && se.Reason == ReasonMisconfigured) {
				firstErr = r.err
			}
			continue
		}
		ok = append(ok, r)
	}
	stats.Reachable = len(ok)
	if len(ok) == 0 {
		var se *SourceError
		if asSource(firstErr, &se) {
			return nil, stats, se
		}
		return nil, stats, &SourceError{Source: "collector", Reason: ReasonUnreachable,
			Message: fmt.Sprintf("0 of %d collectors answered: %v", stats.Total, firstErr)}
	}
	return ok, stats, nil
}

func asSource(err error, target **SourceError) bool { return errors.As(err, target) }

func (c *CollectorClient) get(ctx context.Context, hc *http.Client, t Target, requestURI string) ([]byte, error) {
	scheme := "http"
	if c.cfg.https() {
		scheme = "https"
	}
	u := scheme + "://" + net.JoinHostPort(t.Host, strconv.Itoa(c.cfg.Port)) + requestURI
	rctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if c.cfg.TokenFile != "" {
		tok, err := readToken(c.cfg.TokenFile) // per request: a rotated Secret takes effect at once
		if err != nil {
			return nil, err
		}
		stamp := strconv.FormatInt(c.now().Unix(), 10)
		req.Header.Set(APITimeHeader, stamp)
		req.Header.Set(APISignatureHeader, Sign(tok, http.MethodGet, req.URL.RequestURI(), stamp))
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("collector on node %q: %w", t.Node, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("collector on node %q answered HTTP %d", t.Node, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.cfg.MaxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("collector on node %q: %w", t.Node, err)
	}
	if int64(len(body)) > c.cfg.MaxResponseBytes {
		return nil, fmt.Errorf("collector on node %q: response larger than %d bytes", t.Node, c.cfg.MaxResponseBytes)
	}
	return body, nil
}

// decodeAll decodes each node body; a body that does not parse counts the node as unreachable.
func decodeAll[T any](nodes []nodeBody, stats Stats) ([]T, Stats, error) {
	var out []T
	var lastErr error
	for _, n := range nodes {
		var v T
		if err := json.Unmarshal(n.body, &v); err != nil {
			lastErr = fmt.Errorf("collector on node %q returned invalid JSON: %w", n.target.Node, err)
			stats.Reachable--
			continue
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, stats, &SourceError{Source: "collector", Reason: ReasonUnreachable, Message: lastErr.Error()}
	}
	return out, stats, nil
}

// Graph implements Collector: GET /api/v1/graph merged over all nodes.
func (c *CollectorClient) Graph(ctx context.Context) (Graph, Stats, error) {
	nodes, stats, err := c.fetchAll(ctx, "/api/v1/graph")
	if err != nil {
		return Graph{}, stats, err
	}
	gs, stats, err := decodeAll[Graph](nodes, stats)
	if err != nil {
		return Graph{}, stats, err
	}
	return MergeGraphs(gs), stats, nil
}

// Anomalies implements Collector: GET /api/v1/anomalies merged over all nodes.
func (c *CollectorClient) Anomalies(ctx context.Context) ([]Anomaly, Stats, error) {
	nodes, stats, err := c.fetchAll(ctx, "/api/v1/anomalies")
	if err != nil {
		return nil, stats, err
	}
	as, stats, err := decodeAll[[]Anomaly](nodes, stats)
	if err != nil {
		return nil, stats, err
	}
	return MergeAnomalies(as), stats, nil
}

// SecurityAlerts implements Collector: GET /api/v1/security/alerts merged over all nodes.
func (c *CollectorClient) SecurityAlerts(ctx context.Context) ([]SecurityAlert, Stats, error) {
	nodes, stats, err := c.fetchAll(ctx, "/api/v1/security/alerts")
	if err != nil {
		return nil, stats, err
	}
	type body struct {
		Alerts []SecurityAlert `json:"alerts"`
	}
	bs, stats, err := decodeAll[body](nodes, stats)
	if err != nil {
		return nil, stats, err
	}
	lists := make([][]SecurityAlert, 0, len(bs))
	for _, b := range bs {
		lists = append(lists, b.Alerts)
	}
	return MergeAlerts(lists), stats, nil
}
