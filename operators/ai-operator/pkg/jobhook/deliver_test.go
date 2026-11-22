// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package jobhook

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func mustCIDRs(t *testing.T, s string) []*net.IPNet {
	t.Helper()
	n, err := ParseCIDRs(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestBlocked(t *testing.T) {
	g := Guard{Allowed: mustCIDRs(t, "10.43.0.0/16, 192.168.1.5")}
	for ip, want := range map[string]bool{
		"127.0.0.1": true, "::1": true, "10.0.0.1": true, "172.16.3.4": true, "192.168.1.6": true,
		"169.254.169.254": true, "100.64.1.1": true, "0.0.0.0": true, "255.255.255.255": true, "fd00::1": true,
		"fe80::1": true, "224.0.0.1": true, "::ffff:127.0.0.1": true,
		"10.43.2.9": false, "192.168.1.5": false, "8.8.8.8": false, "2001:4860:4860::8888": false,
	} {
		if got := g.Blocked(net.ParseIP(ip)); got != want {
			t.Errorf("Blocked(%s) = %v, want %v", ip, got, want)
		}
	}
	if _, err := ParseCIDRs("10.0.0.0/33"); err == nil {
		t.Error("invalid CIDR accepted")
	}
	if _, err := ParseCIDRs("nope"); err == nil {
		t.Error("invalid address accepted")
	}
}

func TestCheckURLAndHeaders(t *testing.T) {
	for _, u := range []string{"ftp://x", "https://", "https://u:p@x.example", "/rel", "https://x.example/#f"} {
		if CheckURL(u) == nil {
			t.Errorf("%q accepted", u)
		}
	}
	if err := CheckURL("https://hooks.example.com/a?b=c"); err != nil {
		t.Error(err)
	}
	for _, h := range []map[string]string{{"Host": "x"}, {"X-Gryvia-Signature": "x"}, {"Bad Name": "x"},
		{"X-Ok": "a\r\nInjected: 1"}, {"Content-Type": "text/plain"}} {
		if CheckHeaders(h) == nil {
			t.Errorf("%v accepted", h)
		}
	}
	if err := CheckHeaders(map[string]string{"X-Team": "nlp", "Authorization": "Bearer t"}); err != nil {
		t.Error(err)
	}
}

func TestPostSignsAndGuards(t *testing.T) {
	var got *http.Request
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got, body = r, string(b)
		if r.URL.Path == "/fail" {
			w.WriteHeader(503)
			return
		}
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
			return
		}
		w.WriteHeader(204)
	}))
	defer srv.Close()
	now := time.Unix(1700000000, 0)
	req := Request{URL: srv.URL + "/hook", Body: []byte(`{"a":1}`), Headers: map[string]string{"X-Team": "nlp"},
		Secret: "s3cret", Event: "Failed", Delivery: "d1"}

	// Loopback and plain http are refused without an allowed CIDR.
	if _, err := (Guard{}).Post(context.Background(), req); err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("loopback not blocked: %v", err)
	}

	g := Guard{Allowed: mustCIDRs(t, "127.0.0.0/8"), Now: func() time.Time { return now }}
	code, err := g.Post(context.Background(), req)
	if err != nil || code != 204 {
		t.Fatalf("post: %d %v", code, err)
	}
	if body != `{"a":1}` || got.Header.Get("X-Gryvia-Event") != "Failed" || got.Header.Get("X-Gryvia-Delivery") != "d1" ||
		got.Header.Get("X-Team") != "nlp" || got.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("request: %v %q", got.Header, body)
	}
	if got.Header.Get("X-Gryvia-Timestamp") != "1700000000" ||
		got.Header.Get("X-Gryvia-Signature") != Sign("s3cret", "1700000000", []byte(`{"a":1}`)) {
		t.Fatalf("signature: %v", got.Header)
	}

	req.URL = srv.URL + "/fail"
	if code, err := g.Post(context.Background(), req); err == nil || code != 503 {
		t.Fatalf("503 not an error: %d %v", code, err)
	}
	req.URL = srv.URL + "/redirect"
	if code, err := g.Post(context.Background(), req); err == nil || code != 302 {
		t.Fatalf("redirect followed: %d %v", code, err)
	}

	// A public-looking name that resolves to a private address is refused at dial time.
	g2 := Guard{Lookup: func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("10.1.2.3")}}, nil
	}}
	req.URL = "https://hooks.example.com/x"
	if _, err := g2.Post(context.Background(), req); err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("rebinding not blocked: %v", err)
	}
	// Plain http to a public address is refused.
	g3 := Guard{Lookup: func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
	}}
	req.URL = "http://hooks.example.com/x"
	if _, err := g3.Post(context.Background(), req); err == nil || !strings.Contains(err.Error(), "plain http") {
		t.Fatalf("plain http allowed: %v", err)
	}
}
