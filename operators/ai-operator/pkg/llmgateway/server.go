package llmgateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Source supplies the key index and the published models (backed by an informer cache in the gateway).
type Source interface {
	Keys(ctx context.Context) (map[string]Key, error)
	Routes(ctx context.Context) ([]Route, error)
}

// QuotaChecker is the part of Quotas the handler uses.
type QuotaChecker interface {
	Check(ctx context.Context, ns string, now time.Time) (bool, string, int64, int64, error)
	Add(ns string, tokens int64, now time.Time)
}

// Gateway is the OpenAI-compatible HTTP handler.
type Gateway struct {
	Source  Source
	Meter   *Meter
	Quotas  QuotaChecker
	Client  *http.Client
	Now     func() time.Time
	MaxBody int64
	// Indexes serves POST /v1/retrieve; nil leaves it 404.
	Indexes IndexSource
	// Activator wakes scale-to-zero services and records their traffic; nil disables both.
	Activator Activator
	// WakePoll is how often a held request re-reads the routes (default 1s).
	WakePoll time.Duration
	// Intent rewrites models from prompt shape and optional burst-to-external when local GPUs are saturated.
	Intent IntentPolicy

	wakes, touches throttle
}

const (
	defaultMaxBody     = 16 << 20
	maxBufferedReply   = 64 << 20
	maxSSELine         = 4 << 20
	openAIErrorInvalid = "invalid_request_error"
)

var proxied = map[string]bool{"/v1/chat/completions": true, "/v1/completions": true, "/v1/embeddings": true}

func (g *Gateway) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

// ServeHTTP serves /healthz, GET /v1/models and POST to the proxied routes.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		w.WriteHeader(http.StatusOK)
		return
	}
	key, ok := g.authenticate(w, r)
	if !ok {
		return
	}
	switch {
	case r.URL.Path == "/v1/models" && r.Method == http.MethodGet:
		g.models(w, r, key)
	case proxied[r.URL.Path] && r.Method == http.MethodPost:
		g.proxy(w, r, key)
	case r.URL.Path == "/v1/retrieve" && r.Method == http.MethodPost && g.Indexes != nil:
		g.retrieve(w, r, key)
	default:
		WriteError(w, http.StatusNotFound, openAIErrorInvalid, "unknown route "+r.Method+" "+r.URL.Path)
	}
}

// WriteError writes an OpenAI-style error body.
func WriteError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"error": map[string]interface{}{"message": msg, "type": typ, "code": status}})
}

func (g *Gateway) authenticate(w http.ResponseWriter, r *http.Request) (Key, bool) {
	raw := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if raw == "" || raw == r.Header.Get("Authorization") {
		WriteError(w, http.StatusUnauthorized, "authentication_error", "missing bearer token")
		return Key{}, false
	}
	keys, err := g.Source.Keys(r.Context())
	if err != nil {
		WriteError(w, http.StatusServiceUnavailable, "api_error", "keys are not available yet")
		return Key{}, false
	}
	k, ok := keys[HashKey(raw)]
	if !ok {
		WriteError(w, http.StatusUnauthorized, "authentication_error", "invalid API key")
		return Key{}, false
	}
	return k, true
}

func (g *Gateway) models(w http.ResponseWriter, r *http.Request, k Key) {
	routes, err := g.Source.Routes(r.Context())
	if err != nil {
		WriteError(w, http.StatusServiceUnavailable, "api_error", "models are not available yet")
		return
	}
	data := []map[string]interface{}{}
	for _, rt := range visible(routes, k) {
		data = append(data, map[string]interface{}{"id": rt.Model, "object": "model", "owned_by": rt.Namespace})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"object": "list", "data": data})
}

// Usage is the token count of a response.
type Usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}

// readBody decodes a JSON object request body of at most MaxBody bytes into v, writing the error reply on failure.
func (g *Gateway) readBody(w http.ResponseWriter, r *http.Request, v interface{}) bool {
	limit := g.MaxBody
	if limit <= 0 {
		limit = defaultMaxBody
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil || int64(len(raw)) > limit {
		WriteError(w, http.StatusRequestEntityTooLarge, openAIErrorInvalid, fmt.Sprintf("request body over %d bytes", limit))
		return false
	}
	if err := json.Unmarshal(raw, v); err != nil {
		WriteError(w, http.StatusBadRequest, openAIErrorInvalid, "request body is not a JSON object")
		return false
	}
	return true
}

// allowed checks the key's token quota, writing the 429 or 503 reply when the request may not proceed.
func (g *Gateway) allowed(w http.ResponseWriter, r *http.Request, k Key, model string) bool {
	if g.Quotas == nil {
		return true
	}
	ok, quota, perDay, used, err := g.Quotas.Check(r.Context(), k.Namespace, g.now())
	if err != nil {
		WriteError(w, http.StatusServiceUnavailable, "api_error", "cannot read the token quota")
		return false
	}
	if !ok {
		g.Meter.Rejected(k.Tenant)
		g.Meter.Request(k.Tenant, model, http.StatusTooManyRequests)
		WriteError(w, http.StatusTooManyRequests, "rate_limit_error",
			fmt.Sprintf("quota %s: %d of %d tokens per day used", quota, used, perDay))
		return false
	}
	return true
}

func (g *Gateway) client() *http.Client {
	if g.Client != nil {
		return g.Client
	}
	return http.DefaultClient
}

func (g *Gateway) proxy(w http.ResponseWriter, r *http.Request, k Key) {
	var body map[string]interface{}
	if !g.readBody(w, r, &body) {
		return
	}
	model, _ := body["model"].(string)
	if model == "" {
		WriteError(w, http.StatusBadRequest, openAIErrorInvalid, "model is required")
		return
	}
	rawForIntent, _ := json.Marshal(body)
	resolved := g.Intent.ResolveModel(model, rawForIntent)
	routes, err := g.Source.Routes(r.Context())
	if err != nil {
		WriteError(w, http.StatusServiceUnavailable, "api_error", "models are not available yet")
		return
	}
	route, ok := pick(routes, k, resolved)
	if !ok && resolved != model {
		route, ok = pick(routes, k, model)
		resolved = model
	}
	if !ok {
		g.Meter.Request(k.Tenant, model, http.StatusNotFound)
		WriteError(w, http.StatusNotFound, openAIErrorInvalid, fmt.Sprintf("model %q does not exist or is not available to this key", model))
		return
	}
	if !g.allowed(w, r, k, resolved) {
		return
	}
	woke := route.Waking()
	burst := false
	if woke && g.Intent.AllowBurst(resolved) {
		burst = true
		woke = false
	}
	if woke {
		if route, ok = g.awaitReady(w, r, k, resolved, route); !ok {
			return
		}
	}

	stream, _ := body["stream"].(bool)
	if burst {
		body["model"] = resolved
	} else {
		body["model"] = route.ServedModel
	}
	if stream && r.URL.Path != "/v1/embeddings" {
		opts, _ := body["stream_options"].(map[string]interface{})
		if opts == nil {
			opts = map[string]interface{}{}
		}
		opts["include_usage"] = true
		body["stream_options"] = opts
	}
	out, _ := json.Marshal(body)
	upstreamBase := route.Upstream
	if burst {
		upstreamBase = strings.TrimRight(g.Intent.BurstURL, "/")
	}
	send := func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamBase+r.URL.Path, bytes.NewReader(out))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		if a := r.Header.Get("Accept"); a != "" {
			req.Header.Set("Accept", a)
		}
		return g.client().Do(req)
	}
	resp, err := send()
	if woke {
		resp, err = g.retryRefused(r.Context(), resp, err, send)
	}
	if err != nil {
		g.Meter.Request(k.Tenant, model, http.StatusBadGateway)
		WriteError(w, http.StatusBadGateway, "api_error", "upstream unavailable")
		return
	}
	defer resp.Body.Close()

	for _, h := range []string{"Content-Type", "Cache-Control"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.Header().Set("X-Gryvia-Model-Namespace", route.Namespace)
	w.Header().Set("X-Gryvia-Resolved-Model", resolved)
	if burst {
		w.Header().Set("X-Gryvia-Burst", "1")
	}
	w.WriteHeader(resp.StatusCode)

	var usage Usage
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		usage = relaySSE(w, resp.Body)
	} else {
		reply, _ := io.ReadAll(io.LimitReader(resp.Body, maxBufferedReply))
		_, _ = w.Write(reply)
		usage = usageOf(reply)
	}
	now := g.now()
	if resp.StatusCode < http.StatusInternalServerError {
		g.touch(route)
	}
	g.Meter.Request(k.Tenant, model, resp.StatusCode)
	g.Meter.Record(k, route, usage.PromptTokens, usage.CompletionTokens, now)
	if g.Quotas != nil {
		g.Quotas.Add(k.Namespace, usage.PromptTokens+usage.CompletionTokens, now)
	}
}

func usageOf(b []byte) Usage {
	var v struct {
		Usage *Usage `json:"usage"`
	}
	if json.Unmarshal(b, &v) != nil || v.Usage == nil {
		return Usage{}
	}
	return *v.Usage
}

// relaySSE copies a server-sent event stream line by line, flushing after each line, and returns the usage of
// the last data event that carried one (the final chunk with stream_options.include_usage).
func relaySSE(w http.ResponseWriter, body io.Reader) Usage {
	flusher, _ := w.(http.Flusher)
	rd := bufio.NewReaderSize(body, 64<<10)
	var usage Usage
	for {
		line, err := readLine(rd)
		if len(line) > 0 {
			if _, werr := w.Write(line); werr != nil {
				return usage
			}
			if len(bytes.TrimSpace(line)) == 0 && flusher != nil {
				flusher.Flush()
			}
			if data, ok := bytes.CutPrefix(bytes.TrimRight(line, "\r\n"), []byte("data:")); ok {
				if u := usageOf(bytes.TrimSpace(data)); u.PromptTokens > 0 || u.CompletionTokens > 0 {
					usage = u
				}
			}
		}
		if err != nil {
			if flusher != nil {
				flusher.Flush()
			}
			return usage
		}
	}
}

// readLine returns one line including its newline; a line longer than maxSSELine is returned in pieces.
func readLine(rd *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := rd.ReadSlice('\n')
		buf = append(buf, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) && len(buf) < maxSSELine {
			continue
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			return buf, nil
		}
		return buf, err
	}
}
