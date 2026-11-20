// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

const metricsTok = "fedcba9876543210fedcba9876543210"

func newSec(t *testing.T) securityConfig {
	return securityConfig{
		APITokenFile:     tokenFile(t, testToken+"\n"),
		MetricsTokenFile: tokenFile(t, metricsTok+"\n"),
	}
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
}

func do(h http.Handler, method, uri string, hdr map[string]string) int {
	r := httptest.NewRequest(method, uri, nil)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code
}

func apiHeaders(token, method, uri string, when int64) map[string]string {
	stamp := strconv.FormatInt(when, 10)
	return map[string]string{apiTimeHeader: stamp, apiSignatureHeader: hex.EncodeToString(signAPI([]byte(token), method, uri, stamp))}
}

func TestAuthMiddlewareSignatures(t *testing.T) {
	c := newSec(t)
	now := time.Unix(1_700_000_000, 0)
	h := c.authMiddleware(okHandler(), func() time.Time { return now })
	uri := "/api/v1/graph?x=1"
	ts := now.Unix()

	if got := do(h, "GET", uri, nil); got != 401 {
		t.Fatal("missing", got)
	}
	if got := do(h, "GET", uri, apiHeaders(testToken, "GET", uri, ts)); got != 200 {
		t.Fatal("valid", got)
	}
	if got := do(h, "GET", uri, apiHeaders("wrong-token-wrong-token-wrong-token", "GET", uri, ts)); got != 401 {
		t.Fatal("wrong token", got)
	}
	// Tampered: signature for another URI / method.
	if got := do(h, "GET", uri, apiHeaders(testToken, "GET", "/api/v1/graph?x=2", ts)); got != 401 {
		t.Fatal("tampered uri", got)
	}
	if got := do(h, "POST", uri, apiHeaders(testToken, "GET", uri, ts)); got != 401 {
		t.Fatal("tampered method", got)
	}
	// Expired / future.
	if got := do(h, "GET", uri, apiHeaders(testToken, "GET", uri, ts-31)); got != 401 {
		t.Fatal("expired", got)
	}
	if got := do(h, "GET", uri, apiHeaders(testToken, "GET", uri, ts+31)); got != 401 {
		t.Fatal("future", got)
	}
	// Non-canonical stamp and short/garbled signature.
	bad := apiHeaders(testToken, "GET", uri, ts)
	bad[apiTimeHeader] = "+" + bad[apiTimeHeader]
	if got := do(h, "GET", uri, bad); got != 401 {
		t.Fatal("noncanonical", got)
	}
	bad = apiHeaders(testToken, "GET", uri, ts)
	bad[apiSignatureHeader] = bad[apiSignatureHeader][:10]
	if got := do(h, "GET", uri, bad); got != 401 {
		t.Fatal("short sig", got)
	}
	// A Flight signature (no domain prefix) must not authenticate the generic scheme.
	stamp, sig := sign(testToken, "GET", uri, ts)
	if got := do(h, "GET", uri, map[string]string{apiTimeHeader: stamp, apiSignatureHeader: sig}); got != 401 {
		t.Fatal("flight signature replayed", got)
	}
	// Unknown path is denied by default, probes are open, flight passes through to its own auth.
	if got := do(h, "GET", "/nope", nil); got != 401 {
		t.Fatal(got)
	}
	for _, p := range []string{"/healthz", "/readyz", flightPath} {
		if got := do(h, "GET", p, nil); got != 200 {
			t.Fatal(p, got)
		}
	}
	// A missing/short API token file fails closed.
	c.APITokenFile = filepath.Join(t.TempDir(), "missing")
	h = c.authMiddleware(okHandler(), func() time.Time { return now })
	if got := do(h, "GET", uri, apiHeaders(testToken, "GET", uri, ts)); got != 401 {
		t.Fatal("missing token file", got)
	}
}

func TestMetricsBearer(t *testing.T) {
	c := newSec(t)
	h := c.authMiddleware(okHandler(), time.Now)
	if got := do(h, "GET", "/metrics", nil); got != 401 {
		t.Fatal(got)
	}
	if got := do(h, "GET", "/metrics", map[string]string{"Authorization": "Bearer " + metricsTok}); got != 200 {
		t.Fatal(got)
	}
	if got := do(h, "GET", "/metrics", map[string]string{"Authorization": "Bearer " + metricsTok + "x"}); got != 401 {
		t.Fatal(got)
	}
	// The metrics token opens /metrics only.
	if got := do(h, "GET", "/api/v1/graph", map[string]string{"Authorization": "Bearer " + metricsTok}); got != 401 {
		t.Fatal("bearer must not open /api", got)
	}
	// The API token as bearer does not open anything.
	if got := do(h, "GET", "/metrics", map[string]string{"Authorization": "Bearer " + testToken}); got != 401 {
		t.Fatal(got)
	}
	// Signed requests also reach /metrics.
	if got := do(h, "GET", "/metrics", apiHeaders(testToken, "GET", "/metrics", time.Now().Unix())); got != 200 {
		t.Fatal(got)
	}
}

func TestModes(t *testing.T) {
	open := securityConfig{}
	if open.enforcing() || len(open.warnings()) < 2 {
		t.Fatal("legacy mode must not enforce and must warn")
	}
	if got := do(open.authMiddleware(okHandler(), time.Now), "GET", "/api/v1/graph", nil); got != 200 {
		t.Fatal("legacy mode changed behaviour", got)
	}
	ins := securityConfig{Insecure: true}
	if ins.enforcing() || len(ins.warnings()) == 0 || ins.validate() != nil {
		t.Fatal("insecure")
	}
	for _, bad := range []securityConfig{
		{Insecure: true, APITokenFile: "x"}, {Insecure: true, RequireAuth: true}, {RequireAuth: true},
		{TLSCertFile: "a"}, {TLSKeyFile: "a"}, {TLSClientCAFile: "ca"},
	} {
		if bad.validate() == nil {
			t.Fatalf("%+v should be invalid", bad)
		}
	}
	good := securityConfig{TLSCertFile: "a", TLSKeyFile: "b", TLSClientCAFile: "c", RequireAuth: true}
	if good.validate() != nil || !good.enforcing() || len(good.warnings()) != 0 {
		t.Fatal("mtls config")
	}
}

// ---- TLS / mTLS ----

type pki struct {
	dir    string
	ca     *x509.Certificate
	caKey  *ecdsa.PrivateKey
	caFile string
}

func newPKI(t *testing.T) *pki {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(der)
	p := &pki{dir: t.TempDir(), ca: ca, caKey: key}
	p.caFile = filepath.Join(p.dir, "ca.crt")
	writePEM(t, p.caFile, "CERTIFICATE", der)
	return p
}

func writePEM(t *testing.T, path, typ string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
}

// issue writes name.crt/name.key signed by the CA and returns the tls.Certificate.
func (p *pki) issue(t *testing.T, name string, serial int64, usage x509.ExtKeyUsage, notAfter time.Time) (string, string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.ca, &key.PublicKey, p.caKey)
	if err != nil {
		t.Fatal(err)
	}
	kd, _ := x509.MarshalECPrivateKey(key)
	crt, k := filepath.Join(p.dir, name+".crt"), filepath.Join(p.dir, name+".key")
	writePEM(t, crt, "CERTIFICATE", der)
	writePEM(t, k, "EC PRIVATE KEY", kd)
	return crt, k
}

func (p *pki) clientTLS(t *testing.T, crt, key string) *tls.Config {
	pool := x509.NewCertPool()
	pool.AddCert(p.ca)
	cfg := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	if crt != "" {
		c, err := tls.LoadX509KeyPair(crt, key)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Certificates = []tls.Certificate{c}
	}
	return cfg
}

func startTLS(t *testing.T, c securityConfig) *httptest.Server {
	t.Helper()
	rl, err := newReloader(c.TLSCertFile, c.TLSKeyFile, c.TLSClientCAFile, func(err error) { t.Log("reload:", err) })
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(c.authMiddleware(okHandler(), time.Now))
	// Not StartTLS: it injects its own certificate and would bypass our GetCertificate.
	srv.Listener = tls.NewListener(srv.Listener, rl.tlsConfig())
	srv.Start()
	srv.URL = "https://" + srv.Listener.Addr().String()
	t.Cleanup(srv.Close)
	return srv
}

func get(cfg *tls.Config, url string, hdr map[string]string) (int, *tls.ConnectionState, error) {
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}, Timeout: 5 * time.Second}
	req, _ := http.NewRequest("GET", url, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, resp.TLS, nil
}

func TestMTLSAcceptReject(t *testing.T) {
	p := newPKI(t)
	sc, sk := p.issue(t, "server", 10, x509.ExtKeyUsageServerAuth, time.Now().Add(time.Hour))
	cc, ck := p.issue(t, "client", 11, x509.ExtKeyUsageClientAuth, time.Now().Add(time.Hour))
	c := securityConfig{TLSCertFile: sc, TLSKeyFile: sk, TLSClientCAFile: p.caFile}
	srv := startTLS(t, c)

	code, st, err := get(p.clientTLS(t, cc, ck), srv.URL+"/api/v1/graph", nil)
	if err != nil || code != 200 {
		t.Fatal("valid client cert", code, err)
	}
	if st.Version < tls.VersionTLS12 {
		t.Fatal("version")
	}
	if _, _, err := get(p.clientTLS(t, "", ""), srv.URL+"/api/v1/graph", nil); err == nil {
		t.Fatal("no client cert must be rejected at handshake")
	}
	// Certificate from another CA.
	other := newPKI(t)
	oc, ok := other.issue(t, "client", 12, x509.ExtKeyUsageClientAuth, time.Now().Add(time.Hour))
	cfg := p.clientTLS(t, oc, ok)
	if _, _, err := get(cfg, srv.URL+"/api/v1/graph", nil); err == nil {
		t.Fatal("foreign CA client cert must be rejected")
	}
	// TLS 1.1 is refused.
	old := p.clientTLS(t, cc, ck)
	old.MinVersion, old.MaxVersion = tls.VersionTLS10, tls.VersionTLS11
	if _, _, err := get(old, srv.URL+"/healthz", nil); err == nil {
		t.Fatal("TLS < 1.2 must be refused")
	}
	// A client that does not trust the server CA fails verification.
	if _, _, err := get(&tls.Config{}, srv.URL+"/healthz", nil); err == nil {
		t.Fatal("untrusted server must fail")
	}
}

func TestTLSWithTokenOnly(t *testing.T) {
	p := newPKI(t)
	sc, sk := p.issue(t, "server", 10, x509.ExtKeyUsageServerAuth, time.Now().Add(time.Hour))
	c := newSec(t)
	c.TLSCertFile, c.TLSKeyFile = sc, sk
	srv := startTLS(t, c)
	cfg := p.clientTLS(t, "", "")
	if code, _, err := get(cfg, srv.URL+"/api/v1/graph", nil); err != nil || code != 401 {
		t.Fatal(code, err)
	}
	uri := "/api/v1/graph"
	if code, _, err := get(cfg, srv.URL+uri, apiHeaders(testToken, "GET", uri, time.Now().Unix())); err != nil || code != 200 {
		t.Fatal(code, err)
	}
	if code, _, err := get(cfg, srv.URL+"/healthz", nil); err != nil || code != 200 {
		t.Fatal(code, err)
	}
}

func TestCertReload(t *testing.T) {
	p := newPKI(t)
	sc, sk := p.issue(t, "server", 20, x509.ExtKeyUsageServerAuth, time.Now().Add(time.Hour))
	cc, ck := p.issue(t, "client", 21, x509.ExtKeyUsageClientAuth, time.Now().Add(time.Hour))
	srv := startTLS(t, securityConfig{TLSCertFile: sc, TLSKeyFile: sk, TLSClientCAFile: p.caFile})
	serial := func() int64 {
		_, st, err := get(p.clientTLS(t, cc, ck), srv.URL+"/healthz", nil)
		if err != nil {
			t.Fatal(err)
		}
		return st.PeerCertificates[0].SerialNumber.Int64()
	}
	if serial() != 20 {
		t.Fatal("initial serial")
	}
	// Rotate in place (same file names) and bump the mtime.
	p.issue(t, "server", 22, x509.ExtKeyUsageServerAuth, time.Now().Add(time.Hour))
	future := time.Now().Add(time.Minute)
	_ = os.Chtimes(sc, future, future)
	_ = os.Chtimes(sk, future, future)
	if serial() != 22 {
		t.Fatal("cert was not reloaded")
	}
	// A corrupt rotation keeps serving the last good certificate.
	_ = os.WriteFile(sc, []byte("garbage"), 0600)
	later := future.Add(time.Minute)
	_ = os.Chtimes(sc, later, later)
	if serial() != 22 {
		t.Fatal("bad rotation must keep the previous certificate")
	}
}

func TestReloaderStartupErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := newReloader(filepath.Join(dir, "a"), filepath.Join(dir, "b"), "", nil); err == nil {
		t.Fatal("missing files")
	}
	p := newPKI(t)
	sc, sk := p.issue(t, "server", 30, x509.ExtKeyUsageServerAuth, time.Now().Add(time.Hour))
	empty := filepath.Join(dir, "ca")
	_ = os.WriteFile(empty, []byte("nope"), 0600)
	if _, err := newReloader(sc, sk, empty, nil); err == nil {
		t.Fatal("bad CA bundle")
	}
}

// TestAPISignatureCrossLanguageVector pins the generic signing format shared with the gateway
// (services/api-gateway/routers/collector_transport.py api_signature_headers). The same constants
// appear in services/api-gateway/tests/test_collector_transport.py; change both together.
func TestAPISignatureCrossLanguageVector(t *testing.T) {
	const (
		uri       = "/api/v1/graph?x=1"
		timestamp = int64(1700000000)
		// Produced by the Python signer: api_signature_headers(uri, testToken, 1700000000).
		pythonSignature = "31bcceb2e106086a5b3623200f4b7f54c95ba27df0dbd81918ba88c898b1e501"
	)
	c := securityConfig{APITokenFile: tokenFile(t, testToken)}
	h := c.authMiddleware(okHandler(), func() time.Time { return time.Unix(timestamp, 0) })
	hdr := map[string]string{apiTimeHeader: strconv.FormatInt(timestamp, 10), apiSignatureHeader: pythonSignature}
	if got := do(h, "GET", uri, hdr); got != 200 {
		t.Fatalf("Python-signed request rejected: %d", got)
	}
	if got := hex.EncodeToString(signAPI([]byte(testToken), "GET", uri, "1700000000")); got != pythonSignature {
		t.Fatalf("Go signer disagrees with the Python vector: %s", got)
	}
}
