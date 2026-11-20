// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package kube

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoRereadsTokenAndSendsMergePatch(t *testing.T) {
	var gotAuth, gotCT, gotMethod, gotBody string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotCT, gotMethod = r.Header.Get("Authorization"), r.Header.Get("Content-Type"), r.Method
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	tok := filepath.Join(t.TempDir(), "token")
	c := New(srv.URL+"/", srv.Client(), tok)

	if _, err := c.Get(context.Background(), "/x"); err == nil {
		t.Fatal("missing token file must fail the request")
	}
	if err := os.WriteFile(tok, []byte("t1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.MergePatch(context.Background(), "/apis/x/status", []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer t1" || gotCT != "application/merge-patch+json" || gotMethod != "PATCH" || gotBody != `{"a":1}` {
		t.Fatalf("request: %q %q %q %q", gotAuth, gotCT, gotMethod, gotBody)
	}
	// Token rotation: the next request must use the new token.
	if err := os.WriteFile(tok, []byte("t2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(context.Background(), "/x"); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer t2" {
		t.Fatalf("token not re-read: %q", gotAuth)
	}
}

func TestDoStatusErrorAndBounds(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/big":
			_, _ = w.Write([]byte(strings.Repeat("a", MaxBody+10)))
		default:
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(strings.Repeat("b", 1000)))
		}
	}))
	defer srv.Close()
	tok := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(tok, []byte("t"), 0o600)
	c := New(srv.URL, srv.Client(), tok)

	_, err := c.Get(context.Background(), "/denied")
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 403 || len(se.Body) > 200 {
		t.Fatalf("want bounded 403 StatusError, got %v", err)
	}
	if _, err := c.Get(context.Background(), "/big"); err == nil {
		t.Fatal("oversized body accepted")
	}
}

func TestNewInClusterRefusesOutsideCluster(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	if _, err := NewInCluster(); !errors.Is(err, ErrNotInCluster) {
		t.Fatalf("got %v", err)
	}
}
