// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zyvorai/gryvia/collector/pkg/flight"
)

const testToken = "0123456789abcdef0123456789abcdef"

func testRecorder() *flight.Recorder {
	job := flight.New("gpu-1")
	job.Record(flight.Event{Identity: flight.Identity{Namespace: "ml", Job: "train"}, Kind: "flow"})
	return job
}

func tokenFile(t *testing.T, content string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return file
}

func sign(token, method, uri string, timestamp int64) (string, string) {
	stamp := strconv.FormatInt(timestamp, 10)
	mac := hmac.New(sha256.New, []byte(token))
	_, _ = mac.Write([]byte(method + "\n" + uri + "\n" + stamp))
	return stamp, hex.EncodeToString(mac.Sum(nil))
}

func TestFlightEndpointFailsClosed(t *testing.T) {
	const uri = "/api/v1/flight/diagnose?namespace=ml&job=train"
	request := func(handler http.Handler, token string, timestamp int64) int {
		r := httptest.NewRequest(http.MethodGet, uri, nil)
		stamp, sig := sign(token, "GET", uri, timestamp)
		r.Header.Set(flightTimeHeader, stamp)
		r.Header.Set(flightSignatureHeader, sig)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}
	now := time.Now().Unix()
	job := testRecorder()
	if got := request(flightAuth(job, ""), "", now); got != http.StatusServiceUnavailable {
		t.Fatal(got)
	}
	if got := request(flightAuth(job, filepath.Join(t.TempDir(), "missing")), testToken, now); got != http.StatusServiceUnavailable {
		t.Fatal("missing file", got)
	}
	if got := request(flightAuth(job, tokenFile(t, "short\n")), "short", now); got != http.StatusServiceUnavailable {
		t.Fatal("short token", got)
	}
	handler := flightAuth(job, tokenFile(t, testToken+"\n"))
	if got := request(handler, "wrong", now); got != http.StatusUnauthorized {
		t.Fatal(got)
	}
	if got := request(handler, testToken, now-60); got != http.StatusUnauthorized {
		t.Fatal("stale", got)
	}
	if got := request(handler, testToken, now+60); got != http.StatusUnauthorized {
		t.Fatal("future", got)
	}
	if got := request(handler, testToken, now); got != http.StatusOK {
		t.Fatal(got)
	}
}

func TestFlightAuthRejectsMalformedAndTamperedRequests(t *testing.T) {
	const uri = "/api/v1/flight/diagnose?namespace=ml&job=train"
	handler := flightAuth(testRecorder(), tokenFile(t, testToken))
	now := time.Now().Unix()
	stamp, sig := sign(testToken, "GET", uri, now)
	cases := map[string]struct{ uri, stamp, sig string }{
		"no headers":        {uri, "", ""},
		"odd hex":           {uri, stamp, sig[:63]},
		"non hex":           {uri, stamp, strings.Repeat("zz", 32)},
		"truncated":         {uri, stamp, sig[:32]},
		"padded timestamp":  {uri, "+" + stamp, sig},
		"leading zero":      {uri, "0" + stamp, sig},
		"long timestamp":    {uri, strings.Repeat("9", 40), sig},
		"other job":         {"/api/v1/flight/diagnose?namespace=ml&job=other", stamp, sig},
		"other namespace":   {"/api/v1/flight/diagnose?namespace=kube-system&job=train", stamp, sig},
		"reordered query":   {"/api/v1/flight/diagnose?job=train&namespace=ml", stamp, sig},
		"other timestamp":   {uri, strconv.FormatInt(now+1, 10), sig},
		"uppercase changes": {uri, stamp, strings.ToUpper(sig) + "00"},
	}
	for name, c := range cases {
		r := httptest.NewRequest(http.MethodGet, c.uri, nil)
		r.Header.Set(flightTimeHeader, c.stamp)
		r.Header.Set(flightSignatureHeader, c.sig)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s: got %d", name, w.Code)
		}
		if strings.Contains(w.Body.String(), testToken) || strings.Contains(w.Body.String(), sig) {
			t.Errorf("%s: response leaks secret material", name)
		}
	}
	// The method is part of the signature.
	r := httptest.NewRequest(http.MethodPost, uri, nil)
	r.Header.Set(flightTimeHeader, stamp)
	r.Header.Set(flightSignatureHeader, sig)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("POST with GET signature: got %d", w.Code)
	}
}

func TestFlightAuthReadsTokenPerRequest(t *testing.T) {
	const uri = "/api/v1/flight/diagnose?namespace=ml&job=train"
	file := tokenFile(t, testToken)
	handler := flightAuth(testRecorder(), file)
	code := func(token string) int {
		r := httptest.NewRequest(http.MethodGet, uri, nil)
		stamp, sig := sign(token, "GET", uri, time.Now().Unix())
		r.Header.Set(flightTimeHeader, stamp)
		r.Header.Set(flightSignatureHeader, sig)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}
	rotated := "fedcba9876543210fedcba9876543210"
	if code(testToken) != http.StatusOK || code(rotated) != http.StatusUnauthorized {
		t.Fatal("initial token")
	}
	if err := os.WriteFile(file, []byte(rotated), 0600); err != nil {
		t.Fatal(err)
	}
	if code(rotated) != http.StatusOK || code(testToken) != http.StatusUnauthorized {
		t.Fatal("rotated token not honoured")
	}
}

// TestFlightSignatureCrossLanguageVector pins the wire format shared with the gateway signer
// (services/api-gateway/routers/flight.py signature_headers). The same constants appear in
// services/api-gateway/tests/test_flight.py; if either side changes, both tests must be
// updated together, which is what keeps signer and verifier from drifting.
func TestFlightSignatureCrossLanguageVector(t *testing.T) {
	const (
		uri       = "/api/v1/flight/diagnose?namespace=ml&job=train"
		timestamp = int64(1700000000)
		// Produced by the Python signer: signature_headers(uri, testToken, 1700000000).
		pythonSignature = "1f2b78d5323fef0d77f9f1426ab3f23ef50b33dd60b244c298bdaefae1b5d46f"
	)
	fixed := func() time.Time { return time.Unix(timestamp, 0) }
	handler := flightAuthAt(testRecorder(), tokenFile(t, testToken), fixed)
	r := httptest.NewRequest(http.MethodGet, uri, nil)
	r.Header.Set(flightTimeHeader, strconv.FormatInt(timestamp, 10))
	r.Header.Set(flightSignatureHeader, pythonSignature)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("Python-signed request rejected: %d", w.Code)
	}
	if _, sig := sign(testToken, "GET", uri, timestamp); sig != pythonSignature {
		t.Fatalf("Go signer disagrees with the Python vector: %s", sig)
	}
}
