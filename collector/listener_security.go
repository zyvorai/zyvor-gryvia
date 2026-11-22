// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Listener security: TLS / mTLS for the collector HTTP listener plus one authentication
// middleware in front of every endpoint except the probes and the Flight Recorder (which keeps
// its own, unchanged, token check).
//
// Generic request signing (all non-probe, non-flight, non-metrics-bearer paths):
//
//	X-Gryvia-Time:      canonical decimal UNIX seconds
//	X-Gryvia-Signature: hex(HMAC-SHA256(token, "GRYVIA-API-V1\n" METHOD "\n" REQUEST-URI "\n" UNIX-SECONDS))
//
// The fixed "GRYVIA-API-V1" prefix domain-separates it from the Flight signature, so the two
// schemes cannot be replayed against each other even if an operator reuses one token.

const (
	apiTimeHeader      = "X-Gryvia-Time"
	apiSignatureHeader = "X-Gryvia-Signature"
	apiSignaturePrefix = "GRYVIA-API-V1\n"
	apiSkew            = 30 * time.Second
	flightPath         = "/api/v1/flight/diagnose"
)

// securityConfig is filled from flags.
type securityConfig struct {
	TLSCertFile      string
	TLSKeyFile       string
	TLSClientCAFile  string
	APITokenFile     string // shared HMAC token for /api/*
	MetricsTokenFile string // bearer token accepted for /metrics only
	Insecure         bool   // explicit opt-out: nothing is authenticated
	RequireAuth      bool   // refuse to start without any authentication
}

func (c securityConfig) tlsEnabled() bool { return c.TLSCertFile != "" || c.TLSKeyFile != "" }

// authConfigured reports whether at least one credential mechanism is on.
func (c securityConfig) authConfigured() bool {
	return c.APITokenFile != "" || c.MetricsTokenFile != "" || c.TLSClientCAFile != ""
}

// validate rejects contradictory or incomplete combinations at start-up.
func (c securityConfig) validate() error {
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		return errors.New("-tls-cert-file and -tls-key-file must be set together")
	}
	if c.TLSClientCAFile != "" && !c.tlsEnabled() {
		return errors.New("-tls-client-ca-file (mTLS) needs -tls-cert-file and -tls-key-file")
	}
	if c.Insecure && (c.authConfigured() || c.RequireAuth) {
		return errors.New("-insecure-listener contradicts -api-token-file, -metrics-token-file, -tls-client-ca-file and -require-auth")
	}
	if c.RequireAuth && !c.authConfigured() {
		return errors.New("-require-auth needs -api-token-file, -metrics-token-file or -tls-client-ca-file")
	}
	return nil
}

// warnings lists start-up warnings for the chosen mode.
func (c securityConfig) warnings() []string {
	var w []string
	switch {
	case c.Insecure:
		w = append(w, "SECURITY: -insecure-listener set: every collector endpoint is served WITHOUT authentication")
	case !c.authConfigured():
		w = append(w, "SECURITY: no collector authentication configured: /metrics and every /api/v1 endpoint (except the token-protected flight endpoint) are unauthenticated; set -api-token-file and/or -tls-client-ca-file, or -insecure-listener to acknowledge")
	}
	if !c.tlsEnabled() {
		w = append(w, "SECURITY: collector listener is plain HTTP (no -tls-cert-file/-tls-key-file): credentials and data cross the network in clear text")
	}
	if c.APITokenFile != "" && !c.tlsEnabled() {
		w = append(w, "SECURITY: request signatures without TLS authenticate callers but do not protect confidentiality or prevent replay inside the 30 s window")
	}
	return w
}

// enforcing reports whether the middleware rejects unauthenticated requests.
func (c securityConfig) enforcing() bool { return !c.Insecure && c.authConfigured() }

// authMiddleware wraps next. now is injectable for tests.
func (c securityConfig) authMiddleware(next http.Handler, now func() time.Time) http.Handler {
	if !c.enforcing() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz", "/readyz":
			next.ServeHTTP(w, r)
			return
		case flightPath:
			next.ServeHTTP(w, r) // has its own token check (flightAuth)
			return
		}
		if c.authorized(r, now) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="gryvia-collector"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

func (c securityConfig) authorized(r *http.Request, now func() time.Time) bool {
	// mTLS: the handshake already required and verified a client certificate.
	if c.TLSClientCAFile != "" && r.TLS != nil && len(r.TLS.VerifiedChains) > 0 {
		return true
	}
	if r.URL.Path == "/metrics" && c.MetricsTokenFile != "" && c.bearerOK(r) {
		return true
	}
	if c.APITokenFile != "" && c.signatureOK(r, now) {
		return true
	}
	return false
}

func (c securityConfig) bearerOK(r *http.Request) bool {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || len(h) > flightMaxTokenFile || !strings.EqualFold(h[:len(prefix)], prefix) {
		return false
	}
	want, ok := readFlightToken(c.MetricsTokenFile)
	if !ok {
		return false
	}
	got := []byte(strings.TrimSpace(h[len(prefix):]))
	return subtle.ConstantTimeCompare(got, want) == 1
}

func (c securityConfig) signatureOK(r *http.Request, now func() time.Time) bool {
	token, ok := readFlightToken(c.APITokenFile)
	if !ok {
		return false
	}
	stamp, sigHex := r.Header.Get(apiTimeHeader), r.Header.Get(apiSignatureHeader)
	if len(stamp) == 0 || len(stamp) > 20 || len(sigHex) != 2*sha256.Size {
		return false
	}
	when, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil || strconv.FormatInt(when, 10) != stamp {
		return false
	}
	delta := now().Unix() - when
	if delta < 0 {
		delta = -delta
	}
	if delta > int64(apiSkew/time.Second) {
		return false
	}
	sig, err := hex.DecodeString(sigHex)
	if err != nil {
		return false
	}
	return hmac.Equal(sig, signAPI(token, r.Method, r.URL.RequestURI(), stamp))
}

// signAPI is the reference implementation of the generic request signature.
func signAPI(token []byte, method, requestURI, stamp string) []byte {
	mac := hmac.New(sha256.New, token)
	_, _ = mac.Write([]byte(apiSignaturePrefix + method + "\n" + requestURI + "\n" + stamp))
	return mac.Sum(nil)
}

// ---- TLS with reloading ----

// cipherSuites: ECDHE + AEAD only (TLS 1.3 suites are not configurable and are all AEAD).
var cipherSuites = []uint16{
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
	tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
}

// fileStamp identifies a file version (mounted Secrets rotate by symlink swap, which changes
// the resolved mtime/size/inode-ish stat of the target).
type fileStamp struct {
	mod  time.Time
	size int64
}

func stampOf(path string) (fileStamp, error) {
	fi, err := os.Stat(path) // follows symlinks: ..data swap changes the target
	if err != nil {
		return fileStamp{}, err
	}
	return fileStamp{fi.ModTime(), fi.Size()}, nil
}

// reloader serves the newest valid cert/key and client CA pool; on a failed reload it keeps
// serving the last good material.
type reloader struct {
	certFile, keyFile, caFile string
	onError                   func(error)

	mu       sync.Mutex
	cert     *tls.Certificate
	certSeen [2]fileStamp
	pool     *x509.CertPool
	caSeen   fileStamp
}

func newReloader(certFile, keyFile, caFile string, onError func(error)) (*reloader, error) {
	r := &reloader{certFile: certFile, keyFile: keyFile, caFile: caFile, onError: onError}
	if err := r.refresh(true); err != nil {
		return nil, err
	}
	return r, nil
}

// refresh reloads changed files. With strict, errors are returned (start-up); otherwise they are
// reported and the previous material is kept.
func (r *reloader) refresh(strict bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	fail := func(err error) error {
		if strict {
			return err
		}
		if r.onError != nil {
			r.onError(err)
		}
		return nil
	}
	cs, e1 := stampOf(r.certFile)
	ks, e2 := stampOf(r.keyFile)
	if e1 != nil || e2 != nil {
		if r.cert == nil || strict {
			return fmt.Errorf("tls files: %v %v", e1, e2)
		}
		return fail(fmt.Errorf("tls files unreadable, keeping previous: %v %v", e1, e2))
	}
	if r.cert == nil || [2]fileStamp{cs, ks} != r.certSeen {
		c, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
		if err != nil {
			if r.cert == nil {
				return fmt.Errorf("loading TLS key pair: %w", err)
			}
			_ = fail(fmt.Errorf("loading TLS key pair, keeping previous: %w", err))
		} else {
			r.cert, r.certSeen = &c, [2]fileStamp{cs, ks}
		}
	}
	if r.caFile != "" {
		as, err := stampOf(r.caFile)
		if err != nil {
			if r.pool == nil {
				return fmt.Errorf("client CA: %w", err)
			}
			return fail(fmt.Errorf("client CA unreadable, keeping previous: %w", err))
		}
		if r.pool == nil || as != r.caSeen {
			pem, err := os.ReadFile(r.caFile)
			pool := x509.NewCertPool()
			if err != nil || !pool.AppendCertsFromPEM(pem) {
				if r.pool == nil {
					return errors.New("client CA file has no usable certificates")
				}
				return fail(errors.New("client CA file has no usable certificates, keeping previous"))
			}
			r.pool, r.caSeen = pool, as
		}
	}
	return nil
}

func (r *reloader) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	_ = r.refresh(false)
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cert, nil
}

// tlsConfig builds the server config. When a client CA is configured every client must present a
// certificate that verifies against the (reloaded) pool.
func (r *reloader) tlsConfig() *tls.Config {
	base := &tls.Config{
		MinVersion:     tls.VersionTLS12,
		CipherSuites:   cipherSuites,
		GetCertificate: r.getCertificate,
	}
	if r.caFile == "" {
		return base
	}
	base.ClientAuth = tls.RequireAndVerifyClientCert
	base.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
		_ = r.refresh(false)
		r.mu.Lock()
		defer r.mu.Unlock()
		cfg := base.Clone()
		cfg.GetConfigForClient = nil
		cfg.ClientCAs = r.pool
		return cfg, nil
	}
	return base
}
