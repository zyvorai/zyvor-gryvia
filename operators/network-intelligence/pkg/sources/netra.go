// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package sources

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// NetraHistoryPath is Netra's documented flow-history endpoint (also read by the gateway).
const NetraHistoryPath = "/api/v1/flows/history"

// NetraConfig configures the Netra client. URL empty means Netra is not configured.
type NetraConfig struct {
	URL              string        // GRYVIA_NETRA_URL
	Token            string        // GRYVIA_NETRA_TOKEN (bearer); may be empty
	CAFile           string        // GRYVIA_NETRA_CA_FILE: CA bundle for an https netrad
	InsecureSkipTLS  bool          // GRYVIA_NETRA_INSECURE=1 (same switch as the gateway); unverified TLS
	Timeout          time.Duration // default 10s
	MaxResponseBytes int64         // default 16 MiB
}

// NetraFromEnv reads GRYVIA_NETRA_URL, GRYVIA_NETRA_TOKEN, GRYVIA_NETRA_CA_FILE and GRYVIA_NETRA_INSECURE.
func NetraFromEnv(getenv func(string) string) NetraConfig {
	return NetraConfig{
		URL:             strings.TrimRight(strings.TrimSpace(getenv("GRYVIA_NETRA_URL")), "/"),
		Token:           strings.TrimSpace(getenv("GRYVIA_NETRA_TOKEN")),
		CAFile:          strings.TrimSpace(getenv("GRYVIA_NETRA_CA_FILE")),
		InsecureSkipTLS: getenv("GRYVIA_NETRA_INSECURE") == "1",
	}
}

// NetraClient reads Netra's flow history. Unverified against a live netrad: the request/response
// shape follows services/api-gateway/routers/netra.py and Netra's documented history API.
type NetraClient struct{ cfg NetraConfig }

var _ FlowHistory = (*NetraClient)(nil)

// NewNetraClient builds a client (Configured() is false when URL is empty).
func NewNetraClient(cfg NetraConfig) *NetraClient {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.MaxResponseBytes <= 0 {
		cfg.MaxResponseBytes = 16 << 20
	}
	return &NetraClient{cfg: cfg}
}

// Configured reports whether a Netra URL is set.
func (n *NetraClient) Configured() bool { return n != nil && n.cfg.URL != "" }

// History returns the flow records of the last `since` (at most `limit`).
func (n *NetraClient) History(ctx context.Context, since time.Duration, limit int) ([]FlowRecord, error) {
	if !n.Configured() {
		return nil, &SourceError{Source: "netra", Reason: ReasonNotConfigured,
			Message: "GRYVIA_NETRA_URL is not set on the operator; flow history needs Netra"}
	}
	if since < time.Second {
		since = time.Second
	}
	if limit <= 0 || limit > 20000 {
		limit = 5000
	}
	q := url.Values{"since": {formatSince(since)}, "limit": {strconv.Itoa(limit)}}
	tr := &http.Transport{DisableKeepAlives: true}
	if strings.HasPrefix(n.cfg.URL, "https://") {
		tc := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: n.cfg.InsecureSkipTLS} //nolint:gosec // explicit opt-in, same switch as the gateway
		if n.cfg.CAFile != "" {
			pem, err := os.ReadFile(n.cfg.CAFile)
			if err != nil {
				return nil, &SourceError{Source: "netra", Reason: ReasonMisconfigured, Message: "GRYVIA_NETRA_CA_FILE: " + err.Error()}
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, &SourceError{Source: "netra", Reason: ReasonMisconfigured, Message: "GRYVIA_NETRA_CA_FILE holds no certificate"}
			}
			tc.RootCAs = pool
		}
		tr.TLSClientConfig = tc
	}
	hc := &http.Client{Transport: tr, Timeout: n.cfg.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer hc.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.cfg.URL+NetraHistoryPath+"?"+q.Encode(), nil)
	if err != nil {
		return nil, &SourceError{Source: "netra", Reason: ReasonMisconfigured, Message: err.Error()}
	}
	req.Header.Set("Accept", "application/json")
	if n.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+n.cfg.Token)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, &SourceError{Source: "netra", Reason: ReasonUnreachable, Message: err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &SourceError{Source: "netra", Reason: ReasonUnreachable, Message: fmt.Sprintf("Netra answered HTTP %d", resp.StatusCode)}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, n.cfg.MaxResponseBytes+1))
	if err != nil || int64(len(body)) > n.cfg.MaxResponseBytes {
		return nil, &SourceError{Source: "netra", Reason: ReasonUnreachable, Message: "Netra response unreadable or too large"}
	}
	var out struct {
		Records []FlowRecord `json:"records"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, &SourceError{Source: "netra", Reason: ReasonUnreachable, Message: "Netra returned invalid JSON: " + err.Error()}
	}
	return out.Records, nil
}

// formatSince renders a duration in the h/m/s form the history API takes (whole units, at least 1s).
func formatSince(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	case d%time.Minute == 0:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	default:
		return strconv.Itoa(int((d+time.Second-1)/time.Second)) + "s"
	}
}
