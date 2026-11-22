// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package notify

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

func TestCheckURL(t *testing.T) {
	ok := []string{"https://hooks.example.com/x", "http://localhost:9000/x", "http://127.0.0.1/x"}
	bad := []string{"", "ftp://x/y", "http://hooks.example.com/x", "https://user:pw@hooks.example.com/x", "https:///x"}
	for _, u := range ok {
		if err := CheckURL(u); err != nil {
			t.Errorf("CheckURL(%q) = %v, want ok", u, err)
		}
	}
	for _, u := range bad {
		if err := CheckURL(u); err == nil {
			t.Errorf("CheckURL(%q) accepted", u)
		}
	}
}

func TestBlockedAddr(t *testing.T) {
	for ip, want := range map[string]bool{
		"169.254.169.254": true, "fe80::1": true, "224.0.0.1": true, "0.0.0.0": true, "255.255.255.255": true,
		"10.1.2.3": false, "192.168.0.5": false, "8.8.8.8": false,
	} {
		if got := blockedAddr(net.ParseIP(ip), false); got != want {
			t.Errorf("blockedAddr(%s) = %v, want %v", ip, got, want)
		}
	}
	if !blockedAddr(net.ParseIP("127.0.0.1"), false) || blockedAddr(net.ParseIP("127.0.0.1"), true) {
		t.Error("loopback must be blocked unless the URL itself is loopback")
	}
}

func TestPostSignsAndDelivers(t *testing.T) {
	var gotBody, gotSig, gotTS, gotType string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotSig, gotTS, gotType = string(b), r.Header.Get("X-Gryvia-Signature"), r.Header.Get("X-Gryvia-Timestamp"), r.Header.Get("Content-Type")
	}))
	defer srv.Close()
	w := &Webhook{URL: srv.URL, Secret: "s3cret", Now: func() time.Time { return time.Unix(1700000000, 0) }}
	if err := w.Post(context.Background(), []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if gotBody != `{"a":1}` || gotTS != "1700000000" || gotType != "application/json" {
		t.Errorf("body=%q ts=%q type=%q", gotBody, gotTS, gotType)
	}
	if want := Sign("s3cret", "1700000000", []byte(`{"a":1}`)); gotSig != want || !strings.HasPrefix(gotSig, "sha256=") {
		t.Errorf("signature %q, want %q", gotSig, want)
	}
}

func TestPostUnsignedWithoutSecret(t *testing.T) {
	var sig string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { sig = r.Header.Get("X-Gryvia-Signature") }))
	defer srv.Close()
	if err := (&Webhook{URL: srv.URL}).Post(context.Background(), []byte("{}")); err != nil || sig != "" {
		t.Errorf("err=%v sig=%q", err, sig)
	}
}

func TestPostFailsOnNon2xxAndNeverFollowsRedirects(t *testing.T) {
	hits := 0
	target := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { hits++ }))
	defer target.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		http.Redirect(rw, r, target.URL, http.StatusFound)
	}))
	defer redir.Close()
	if err := (&Webhook{URL: redir.URL}).Post(context.Background(), []byte("{}")); err == nil {
		t.Error("a 302 must be an error")
	}
	if hits != 0 {
		t.Error("the redirect was followed")
	}
	bad := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { rw.WriteHeader(500) }))
	defer bad.Close()
	if err := (&Webhook{URL: bad.URL}).Post(context.Background(), []byte("{}")); err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("err = %v", err)
	}
}

func TestPostRefusesBlockedAddressAtDialTime(t *testing.T) {
	// A hostname that resolves to the metadata address must be refused by the dialer (no resolve-then-connect gap).
	w := &Webhook{URL: "https://169.254.169.254/latest"}
	if err := w.Post(context.Background(), []byte("{}")); err == nil || !strings.Contains(err.Error(), "blocked address") {
		t.Errorf("err = %v, want blocked address", err)
	}
}
