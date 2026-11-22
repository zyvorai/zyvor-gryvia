// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package incident

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func validLabel(s string) bool { return len(s) <= 63 && dnsLabel.MatchString(s) }

// Handler serves the incident endpoints. A nil Store answers 200 with
// {"enabled":false}: a node without a store is reachable, just has no history.
// Mount it behind the Flight Recorder's HMAC check.
type Handler struct {
	Store *Store
	Node  string
	Now   func() time.Time
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// parseSince accepts an RFC3339 time or a Go duration meaning "that long ago".
func parseSince(v string, now time.Time) (time.Time, bool) {
	if v == "" {
		return time.Time{}, true
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, true
	}
	if d, err := time.ParseDuration(v); err == nil && d >= 0 {
		return now.Add(-d), true
	}
	return time.Time{}, false
}

func (h *Handler) common(w http.ResponseWriter, r *http.Request, needJob bool) (ns, job string, ok bool) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return "", "", false
	}
	q := r.URL.Query()
	ns, job = q.Get("namespace"), q.Get("job")
	if (ns != "" && !validLabel(ns)) || (job != "" && !validLabel(job)) {
		http.Error(w, "namespace and job must be DNS-1123 labels", http.StatusBadRequest)
		return "", "", false
	}
	if needJob && (ns == "" || job == "") {
		http.Error(w, "namespace and job required", http.StatusBadRequest)
		return "", "", false
	}
	return ns, job, true
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) ([]Incident, bool) {
	ns, job, ok := h.common(w, r, false)
	if !ok {
		return nil, false
	}
	since, ok := parseSince(r.URL.Query().Get("since"), h.now())
	if !ok {
		http.Error(w, "since must be an RFC3339 time or a duration such as 6h", http.StatusBadRequest)
		return nil, false
	}
	limit := 500
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 2000 {
			http.Error(w, "limit must be 1..2000", http.StatusBadRequest)
			return nil, false
		}
		limit = n
	}
	return h.Store.Incidents(Filter{Namespace: ns, Job: job, Since: since, Limit: limit}), true
}

// ServeIncidents serves GET /api/v1/flight/incidents.
func (h *Handler) ServeIncidents(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		if _, _, ok := h.common(w, r, false); ok {
			writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "node": h.Node, "incidents": []Incident{}})
		}
		return
	}
	incs, ok := h.list(w, r)
	if !ok {
		return
	}
	if incs == nil {
		incs = []Incident{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "node": h.Node, "incidents": incs, "store": h.Store.Stats()})
}

// ServeExport serves GET /api/v1/flight/incidents/export?format=json|csv.
func (h *Handler) ServeExport(w http.ResponseWriter, r *http.Request) {
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "json"
	}
	if format != "json" && format != "csv" {
		http.Error(w, "format must be json or csv", http.StatusBadRequest)
		return
	}
	if h.Store == nil {
		if _, _, ok := h.common(w, r, false); ok {
			writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "node": h.Node, "incidents": []Incident{}})
		}
		return
	}
	incs, ok := h.list(w, r)
	if !ok {
		return
	}
	if format == "csv" {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="gryvia-incidents.csv"`)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_ = ExportCSV(w, incs)
		return
	}
	if incs == nil {
		incs = []Incident{}
	}
	w.Header().Set("Content-Disposition", `attachment; filename="gryvia-incidents.json"`)
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "node": h.Node, "incidents": incs})
}

// ServeCompare serves GET /api/v1/flight/compare?namespace=&job=&a=&b=[&window=5m].
func (h *Handler) ServeCompare(w http.ResponseWriter, r *http.Request) {
	ns, job, ok := h.common(w, r, true)
	if !ok {
		return
	}
	if h.Store == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "node": h.Node})
		return
	}
	q := r.URL.Query()
	def := 5 * time.Minute
	if v := q.Get("window"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Minute || d > 24*time.Hour {
			http.Error(w, "window must be a duration between 1m and 24h", http.StatusBadRequest)
			return
		}
		def = d
	}
	now := h.now()
	wa, ia, err := h.Store.ResolveWindow(ns, job, q.Get("a"), def, now)
	if err != nil {
		http.Error(w, "a: "+err.Error(), http.StatusBadRequest)
		return
	}
	wb, ib, err := h.Store.ResolveWindow(ns, job, q.Get("b"), def, now)
	if err != nil {
		http.Error(w, "b: "+err.Error(), http.StatusBadRequest)
		return
	}
	c := h.Store.Compare(ns, job, wa, wb, ia, ib)
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "node": h.Node, "comparison": c})
}
