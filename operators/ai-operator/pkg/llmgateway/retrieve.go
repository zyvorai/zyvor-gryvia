// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package llmgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

const (
	defaultTopK   = 4
	maxTopK       = 50
	maxQueryChars = 8192
	maxStoreReply = 8 << 20
)

// Index is what /v1/retrieve needs of a GryviaVectorIndex.
type Index struct {
	Namespace, Name string
	EmbedModel      string
	StoreURL        string
	Collection      string
	StoreKey        string
	// Queryable is false until the first ingestion has finished.
	Queryable bool
}

// IndexSource looks up an index of a namespace.
type IndexSource interface {
	Index(ctx context.Context, namespace, name string) (Index, bool, error)
}

// Index reads a GryviaVectorIndex and its mirrored store key from the cache.
func (s *CacheSource) Index(ctx context.Context, namespace, name string) (Index, bool, error) {
	idx := &gryviav1.GryviaVectorIndex{}
	if err := s.Reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, idx); err != nil {
		if apierrors.IsNotFound(err) {
			return Index{}, false, nil
		}
		return Index{}, false, err
	}
	out := indexFrom(idx)
	if idx.Spec.Store.Type == gryviav1.VectorStoreExternal {
		sec := &corev1.Secret{}
		err := s.Reader.Get(ctx, types.NamespacedName{Namespace: s.KeyNamespace, Name: StoreSecretName(namespace, name)}, sec)
		if err != nil && !apierrors.IsNotFound(err) {
			return Index{}, false, err
		}
		out.StoreKey = string(sec.Data["apiKey"])
	}
	return out, true, nil
}

func indexFrom(idx *gryviav1.GryviaVectorIndex) Index {
	coll := idx.Status.Collection
	if coll == "" {
		coll = idx.Name
	}
	return Index{
		Namespace: idx.Namespace, Name: idx.Name, EmbedModel: idx.Spec.Embedding.Model,
		StoreURL: strings.TrimRight(idx.Status.StoreURL, "/"), Collection: coll,
		Queryable: idx.Status.LastIngested != nil && idx.Status.StoreURL != "",
	}
}

// RetrieveRequest is the body of POST /v1/retrieve.
type RetrieveRequest struct {
	Index string `json:"index"`
	Query string `json:"query"`
	TopK  int    `json:"topK"`
}

// RetrieveHit is one chunk of a /v1/retrieve reply.
type RetrieveHit struct {
	Score  float64 `json:"score"`
	Text   string  `json:"text"`
	Source string  `json:"source"`
	Chunk  int64   `json:"chunk"`
}

// retrieve embeds the query through the index's embedding model (metered under the caller's key) and returns the
// topK nearest chunks. Keys only see the indexes of their own namespace.
func (g *Gateway) retrieve(w http.ResponseWriter, r *http.Request, k Key) {
	var req RetrieveRequest
	if !g.readBody(w, r, &req) {
		return
	}
	if req.Index == "" || strings.TrimSpace(req.Query) == "" {
		WriteError(w, http.StatusBadRequest, openAIErrorInvalid, "index and query are required")
		return
	}
	if len(req.Query) > maxQueryChars {
		WriteError(w, http.StatusBadRequest, openAIErrorInvalid, fmt.Sprintf("query over %d characters", maxQueryChars))
		return
	}
	if req.TopK <= 0 {
		req.TopK = defaultTopK
	}
	if req.TopK > maxTopK {
		req.TopK = maxTopK
	}
	idx, found, err := g.Indexes.Index(r.Context(), k.Namespace, req.Index)
	if err != nil {
		WriteError(w, http.StatusServiceUnavailable, "api_error", "indexes are not available yet")
		return
	}
	if !found {
		WriteError(w, http.StatusNotFound, openAIErrorInvalid, fmt.Sprintf("index %q does not exist in namespace %s", req.Index, k.Namespace))
		return
	}
	if !idx.Queryable {
		WriteError(w, http.StatusConflict, openAIErrorInvalid, fmt.Sprintf("index %q has not finished its first ingestion", req.Index))
		return
	}

	routes, err := g.Source.Routes(r.Context())
	if err != nil {
		WriteError(w, http.StatusServiceUnavailable, "api_error", "models are not available yet")
		return
	}
	route, ok := pick(routes, k, idx.EmbedModel)
	if !ok {
		g.Meter.Request(k.Tenant, idx.EmbedModel, http.StatusNotFound)
		WriteError(w, http.StatusNotFound, openAIErrorInvalid,
			fmt.Sprintf("embedding model %q of index %q is not available to this key", idx.EmbedModel, req.Index))
		return
	}
	if !g.allowed(w, r, k, idx.EmbedModel) {
		return
	}
	vector, usage, code, err := g.embed(r.Context(), route, req.Query)
	now := g.now()
	g.Meter.Request(k.Tenant, idx.EmbedModel, code)
	g.Meter.Record(k, route, usage.PromptTokens, 0, now)
	if g.Quotas != nil {
		g.Quotas.Add(k.Namespace, usage.PromptTokens, now)
	}
	if err != nil {
		WriteError(w, http.StatusBadGateway, "api_error", "embedding the query: "+err.Error())
		return
	}

	hits, err := g.search(r.Context(), idx, vector, req.TopK)
	if err != nil {
		WriteError(w, http.StatusBadGateway, "api_error", "searching the vector store: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"object": "list", "index": idx.Name, "model": idx.EmbedModel, "data": hits,
		"usage": map[string]int64{"prompt_tokens": usage.PromptTokens, "total_tokens": usage.PromptTokens},
	})
}

func (g *Gateway) embed(ctx context.Context, route Route, query string) ([]float64, Usage, int, error) {
	body, _ := json.Marshal(map[string]interface{}{"model": route.ServedModel, "input": []string{query}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, route.Upstream+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, Usage{}, http.StatusBadGateway, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.client().Do(req)
	if err != nil {
		return nil, Usage{}, http.StatusBadGateway, fmt.Errorf("upstream unavailable")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxBufferedReply))
	usage := usageOf(raw)
	if resp.StatusCode != http.StatusOK {
		return nil, usage, resp.StatusCode, fmt.Errorf("upstream returned %d", resp.StatusCode)
	}
	var out struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Data) == 0 || len(out.Data[0].Embedding) == 0 {
		return nil, usage, http.StatusBadGateway, fmt.Errorf("upstream returned no embedding")
	}
	return out.Data[0].Embedding, usage, resp.StatusCode, nil
}

func (g *Gateway) search(ctx context.Context, idx Index, vector []float64, topK int) ([]RetrieveHit, error) {
	body, _ := json.Marshal(map[string]interface{}{"vector": vector, "limit": topK, "with_payload": true})
	u := fmt.Sprintf("%s/collections/%s/points/search", idx.StoreURL, url.PathEscape(idx.Collection))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if idx.StoreKey != "" {
		req.Header.Set("api-key", idx.StoreKey)
	}
	resp, err := g.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("store unavailable")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxStoreReply))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("store returned %d", resp.StatusCode)
	}
	var out struct {
		Result []struct {
			Score   float64                `json:"score"`
			Payload map[string]interface{} `json:"payload"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("store reply is not JSON")
	}
	hits := make([]RetrieveHit, 0, len(out.Result))
	for _, p := range out.Result {
		h := RetrieveHit{Score: p.Score}
		h.Text, _ = p.Payload["text"].(string)
		h.Source, _ = p.Payload["source"].(string)
		if c, ok := p.Payload["chunk"].(float64); ok {
			h.Chunk = int64(c)
		}
		hits = append(hits, h)
	}
	return hits, nil
}
