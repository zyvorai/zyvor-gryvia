// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

// Package kube is a minimal in-cluster Kubernetes REST client: TLS against the
// service account CA, a bearer token that is re-read on every request (projected
// tokens rotate) and bounded response bodies. It exists so the collector needs no
// client-go; it supports only what the collector uses (GET and merge-patch).
package kube

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	// SADir is the service account directory mounted in every pod.
	SADir = "/var/run/secrets/kubernetes.io/serviceaccount"
	// DefaultBase is the in-cluster API server address.
	DefaultBase = "https://kubernetes.default.svc"
	// MaxBody bounds every response body.
	MaxBody = 8 << 20
)

// ErrNotInCluster is returned by NewInCluster outside a pod.
var ErrNotInCluster = errors.New("kube: not running in a cluster (KUBERNETES_SERVICE_HOST unset)")

// Client talks to one API server. It is safe for concurrent use.
type Client struct {
	base      string
	http      *http.Client
	tokenFile string
}

// New builds a Client from explicit parts (used by tests and by NewInCluster).
func New(base string, hc *http.Client, tokenFile string) *Client {
	return &Client{base: strings.TrimRight(base, "/"), http: hc, tokenFile: tokenFile}
}

// NewInCluster builds a Client from the service account files. It refuses to
// run when KUBERNETES_SERVICE_HOST is unset.
func NewInCluster() (*Client, error) {
	if os.Getenv("KUBERNETES_SERVICE_HOST") == "" {
		return nil, ErrNotInCluster
	}
	ca, err := os.ReadFile(SADir + "/ca.crt")
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("invalid Kubernetes service account CA")
	}
	hc := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
	}}
	return New(DefaultBase, hc, SADir+"/token"), nil
}

// StatusError is a non-2xx answer.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("kubernetes API: HTTP %d: %s", e.Code, e.Body)
}

// Do sends one request and returns the (bounded) body of a 2xx answer; any other
// status is a *StatusError. path starts with "/" and may carry a query.
func (c *Client) Do(ctx context.Context, method, path, contentType string, body []byte) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return nil, err
	}
	token, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxBody {
		return nil, fmt.Errorf("kubernetes API: response larger than %d bytes", MaxBody)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := string(b)
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return nil, &StatusError{Code: resp.StatusCode, Body: msg}
	}
	return b, nil
}

// Get is Do with GET.
func (c *Client) Get(ctx context.Context, path string) ([]byte, error) {
	return c.Do(ctx, http.MethodGet, path, "", nil)
}

// MergePatch sends an application/merge-patch+json PATCH.
func (c *Client) MergePatch(ctx context.Context, path string, body []byte) ([]byte, error) {
	return c.Do(ctx, http.MethodPatch, path, "application/merge-patch+json", body)
}
