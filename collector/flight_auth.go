// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	flightTimeHeader      = "X-Gryvia-Flight-Time"
	flightSignatureHeader = "X-Gryvia-Flight-Signature"
	flightMinTokenLen     = 32
	flightMaxTokenFile    = 4096
	// flightSkew is the accepted difference between the signer's and this node's clock.
	// A signed request can be replayed unchanged inside this window (the endpoint is read-only).
	flightSkew = 30 * time.Second
)

// flightAuth serves the Flight Recorder report only for requests signed with the shared token:
// hex(HMAC-SHA256(token, METHOD "\n" REQUEST-URI "\n" UNIX-SECONDS)). The token file is read on
// every request so a rotated Secret takes effect without a restart. Nothing secret is logged or
// returned: every rejection is a bare status.
func flightAuth(recorder http.Handler, tokenFile string) http.Handler {
	return flightAuthAt(recorder, tokenFile, time.Now)
}

func flightAuthAt(recorder http.Handler, tokenFile string, now func() time.Time) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tokenFile == "" {
			http.Error(w, "flight recorder API disabled", http.StatusServiceUnavailable)
			return
		}
		token, ok := readFlightToken(tokenFile)
		if !ok {
			http.Error(w, "flight recorder token unavailable", http.StatusServiceUnavailable)
			return
		}
		// Header values are attacker-controlled: bound them before parsing.
		stamp := r.Header.Get(flightTimeHeader)
		sigHex := r.Header.Get(flightSignatureHeader)
		if len(stamp) == 0 || len(stamp) > 20 || len(sigHex) != 2*sha256.Size {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		when, err := strconv.ParseInt(stamp, 10, 64)
		if err != nil || strconv.FormatInt(when, 10) != stamp { // canonical decimal only
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		delta := now().Unix() - when
		if delta < 0 {
			delta = -delta
		}
		if delta > int64(flightSkew/time.Second) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		signature, err := hex.DecodeString(sigHex)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mac := hmac.New(sha256.New, token)
		_, _ = mac.Write([]byte(r.Method + "\n" + r.URL.RequestURI() + "\n" + stamp))
		if !hmac.Equal(signature, mac.Sum(nil)) { // constant-time
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		recorder.ServeHTTP(w, r)
	})
}

// readFlightToken returns the trimmed token, or false when the file is unreadable or too short.
func readFlightToken(path string) ([]byte, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, flightMaxTokenFile+1))
	if err != nil || len(raw) > flightMaxTokenFile {
		return nil, false
	}
	token := strings.TrimSpace(string(raw))
	if len(token) < flightMinTokenLen {
		return nil, false
	}
	return []byte(token), true
}
