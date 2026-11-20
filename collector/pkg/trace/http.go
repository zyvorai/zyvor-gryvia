// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package trace

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/zyvorai/gryvia/collector/pkg/flight"
)

// maxTraceEvents bounds the events returned for one trace.
const maxTraceEvents = 200

// Handler serves GET /api/v1/flight/trace?traceId=<32 hex>. It must be wrapped by the Flight
// Recorder's HMAC authentication (the collector's flightAuth): the response names pods,
// namespaces and remote addresses.
//
// Each observation carries the identity of the job pod that received the request, which is what
// the gateway uses to keep tenants inside their own namespaces. The trace id is neither logged
// nor echoed into an error message.
type Handler struct {
	Index    *Index
	Recorder *flight.Recorder
	Node     string
}

// ObservationJSON is one request as seen at ingress.
type ObservationJSON struct {
	Time     time.Time       `json:"time"`
	SpanID   string          `json:"spanId"`
	Client   string          `json:"client"`
	Server   string          `json:"server"`
	Identity flight.Identity `json:"identity"`
}

// Response is the body of a successful lookup.
type Response struct {
	Node         string            `json:"node"`
	TraceID      string            `json:"traceId"`
	Scope        string            `json:"scope"`
	Observations []ObservationJSON `json:"observations"`
	Events       []flight.Event    `json:"events"`
}

// Scope is stated in every response so a consumer cannot mistake it for a complete trace.
const Scope = "node-local; observed events only; plaintext HTTP/1.x requests carrying a traceparent header, IPv4; " +
	"events joined by TCP 4-tuple (traceMatch=tuple) or by pod and local port within seconds (traceMatch=port_time)"

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.ToLower(r.URL.Query().Get("traceId"))
	if !ValidID(id) {
		http.Error(w, "traceId must be 32 hex digits", http.StatusBadRequest)
		return
	}
	entries := h.Index.Lookup(id)
	if len(entries) == 0 {
		http.Error(w, "trace not observed on this node", http.StatusNotFound)
		return
	}
	resp := Response{Node: h.Node, TraceID: id, Scope: Scope, Observations: []ObservationJSON{}, Events: []flight.Event{}}
	var jobs []flight.Identity
	for _, e := range entries {
		resp.Observations = append(resp.Observations, ObservationJSON{
			Time: e.Seen.UTC(), SpanID: e.SpanID, Client: e.Client.String(), Server: e.Server.String(), Identity: e.Identity,
		})
		jobs = append(jobs, e.Identity)
	}
	if h.Recorder != nil {
		if evs := h.Recorder.TraceEvents(id, jobs, maxTraceEvents); evs != nil {
			resp.Events = evs
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(resp)
}
