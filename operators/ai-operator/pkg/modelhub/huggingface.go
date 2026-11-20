// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Package modelhub lists models from a model hub. Only the Hugging Face Hub API is implemented
// (GET /api/models); the base URL is an operator flag, never part of a user's object, so a watch cannot
// make the operator call arbitrary hosts.
package modelhub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultHuggingFaceURL is the public hub.
const DefaultHuggingFaceURL = "https://huggingface.co"

const maxResponseBytes = 8 << 20

var (
	// idRE is what the controller accepts as a model id: it ends up in object names, labels and arguments.
	idRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}/[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)
	shaRE = regexp.MustCompile(`^[0-9a-f]{7,64}$`)
	// sizeRE finds a size such as "7B", "0.5b", "135M" or "8x7B" in a model name.
	sizeRE = regexp.MustCompile(`(?i)(?:^|[^a-z0-9.])(?:(\d+)x)?(\d+(?:\.\d+)?)([bm])(?:$|[^a-z0-9])`)
)

// Model is one hub entry.
type Model struct {
	ID           string
	SHA          string
	LastModified time.Time
	Downloads    int64
	Tags         []string
	PipelineTag  string
	Gated        bool
	// Params is the parameter count from the safetensors metadata; 0 when the hub does not know it.
	Params int64
}

// Query is one listing.
type Query struct {
	Author      string
	PipelineTag string
	Limit       int
}

// Client talks to a Hugging Face compatible hub.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

type hfModel struct {
	ID           string          `json:"id"`
	SHA          string          `json:"sha"`
	LastModified string          `json:"lastModified"`
	Downloads    int64           `json:"downloads"`
	Tags         []string        `json:"tags"`
	PipelineTag  string          `json:"pipeline_tag"`
	Gated        json.RawMessage `json:"gated"`
	Safetensors  *struct {
		Total int64 `json:"total"`
	} `json:"safetensors"`
}

// List returns the most recently modified models of an author, newest first.
func (c *Client) List(ctx context.Context, q Query, token string) ([]Model, error) {
	base := c.BaseURL
	if base == "" {
		base = DefaultHuggingFaceURL
	}
	if q.Limit <= 0 {
		q.Limit = 20
	}
	v := url.Values{}
	v.Set("author", q.Author)
	v.Set("sort", "lastModified")
	v.Set("direction", "-1")
	v.Set("limit", strconv.Itoa(q.Limit))
	if q.PipelineTag != "" {
		v.Set("pipeline_tag", q.PipelineTag)
	}
	for _, f := range []string{"sha", "lastModified", "downloads", "tags", "pipeline_tag", "gated", "safetensors"} {
		v.Add("expand[]", f)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/api/models?"+v.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hub request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("reading hub response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hub answered %d", resp.StatusCode)
	}
	var raw []hfModel
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decoding hub response: %w", err)
	}
	out := make([]Model, 0, len(raw))
	for _, r := range raw {
		m := Model{ID: r.ID, SHA: r.SHA, Downloads: r.Downloads, Tags: r.Tags, PipelineTag: r.PipelineTag}
		if t, err := time.Parse(time.RFC3339, r.LastModified); err == nil {
			m.LastModified = t
		}
		// "gated" is false or a string such as "auto" or "manual".
		if g := strings.TrimSpace(string(r.Gated)); g != "" && g != "false" && g != "null" {
			m.Gated = true
		}
		if r.Safetensors != nil {
			m.Params = r.Safetensors.Total
		}
		out = append(out, m)
	}
	return out, nil
}

// ValidID reports whether id is a well-formed "author/name".
func ValidID(id string) bool { return idRE.MatchString(id) }

// ValidSHA reports whether s looks like a commit hash.
func ValidSHA(s string) bool { return shaRE.MatchString(s) }

// License is the value of the "license:" tag, or "".
func (m Model) License() string {
	for _, t := range m.Tags {
		if strings.HasPrefix(t, "license:") {
			return strings.TrimPrefix(t, "license:")
		}
	}
	return ""
}

// Name is the part of the id after the author.
func (m Model) Name() string {
	if i := strings.IndexByte(m.ID, '/'); i >= 0 {
		return m.ID[i+1:]
	}
	return m.ID
}

// EstimateParams is the parameter count from the hub metadata, else from a size in the name ("Qwen2.5-7B",
// "Mixtral-8x7B", "SmolLM-135M"), else 0.
func (m Model) EstimateParams() int64 {
	if m.Params > 0 {
		return m.Params
	}
	match := sizeRE.FindStringSubmatch(m.Name())
	if match == nil {
		return 0
	}
	n, err := strconv.ParseFloat(match[2], 64)
	if err != nil {
		return 0
	}
	if match[1] != "" {
		experts, _ := strconv.ParseFloat(match[1], 64)
		n *= experts
	}
	unit := 1e9
	if strings.EqualFold(match[3], "m") {
		unit = 1e6
	}
	return int64(n * unit)
}
