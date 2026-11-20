// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zyvorai/gryvia/collector/pkg/flight"
	"github.com/zyvorai/gryvia/collector/pkg/trace"
)

// The trace lookup names pods and remote addresses: it must sit behind the same HMAC as the
// Flight Recorder report.
func TestTraceEndpointIsHMACProtected(t *testing.T) {
	const uri = "/api/v1/flight/trace?traceId=0af7651916cd43dd8448eb211c80319c"
	h := flightAuth(&trace.Handler{Index: trace.NewIndex(), Recorder: flight.New("n"), Node: "n"}, tokenFile(t, testToken+"\n"))
	call := func(signed bool, token string) int {
		r := httptest.NewRequest(http.MethodGet, uri, nil)
		if signed {
			stamp, sig := sign(token, "GET", uri, time.Now().Unix())
			r.Header.Set(flightTimeHeader, stamp)
			r.Header.Set(flightSignatureHeader, sig)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if got := call(false, ""); got != http.StatusUnauthorized {
		t.Errorf("unsigned = %d", got)
	}
	if got := call(true, "wrong-token-wrong-token-wrong-token"); got != http.StatusUnauthorized {
		t.Errorf("wrong token = %d", got)
	}
	// Signed correctly: reaches the handler, which has not observed the trace.
	if got := call(true, testToken); got != http.StatusNotFound {
		t.Errorf("signed = %d, want 404 from the handler", got)
	}
}
