// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package incident

import (
	"sort"
	"strings"
	"time"

	"github.com/zyvorai/gryvia/collector/pkg/diagnosis"
)

// TrackerOptions tunes when findings become incidents.
type TrackerOptions struct {
	// MinDuration is how long a warning-or-worse finding of one kind must persist
	// before an incident is opened (default 60 s). Its Start is the first
	// observation, not the moment the threshold was crossed.
	MinDuration time.Duration
	// CloseAfter is how long a finding must be absent before its incident closes
	// (default 60 s); End is the last observation of the finding.
	CloseAfter time.Duration
	// SampleEvery is how often a job's metrics are stored for comparison (default 60 s).
	SampleEvery time.Duration
	// UpdateEvery bounds how often a growing peak is rewritten (default 30 s).
	UpdateEvery time.Duration
}

type state struct {
	firstSeen time.Time
	lastSeen  time.Time
	lastWrite time.Time
	inc       *Incident // nil until the finding has persisted MinDuration
}

// Tracker turns the stream of per-job diagnoses into incident records.
type Tracker struct {
	store *Store
	opt   TrackerOptions

	states     map[string]*state
	lastSample map[string]time.Time
}

// NewTracker closes incidents a previous run left open (their End is the last
// persisted update: the time between it and the restart was not observed).
func NewTracker(store *Store, opt TrackerOptions) *Tracker {
	if opt.MinDuration <= 0 {
		opt.MinDuration = 60 * time.Second
	}
	if opt.CloseAfter <= 0 {
		opt.CloseAfter = 60 * time.Second
	}
	if opt.SampleEvery <= 0 {
		opt.SampleEvery = 60 * time.Second
	}
	if opt.UpdateEvery <= 0 {
		opt.UpdateEvery = 30 * time.Second
	}
	t := &Tracker{store: store, opt: opt, states: map[string]*state{}, lastSample: map[string]time.Time{}}
	for _, in := range store.OpenIncidents() {
		in.Open = false
		in.End = in.UpdatedAt
		in.CloseReason = "collector-restarted"
		_ = store.PutIncident(in)
	}
	return t
}

func stateKey(ns, job, kind string) string { return ns + "\x00" + job + "\x00" + kind }

// Observe folds one diagnosis. It is called once per job per evaluation cycle.
func (t *Tracker) Observe(now time.Time, d diagnosis.Diagnosis) {
	jk := d.Namespace + "/" + d.Job
	if len(d.Metrics) > 0 && now.Sub(t.lastSample[jk]) >= t.opt.SampleEvery {
		if t.store.PutSample(Sample{Namespace: d.Namespace, Job: d.Job, At: now, Metrics: d.Metrics}) == nil {
			t.lastSample[jk] = now
		}
	}
	present := map[string]bool{}
	var unavailable []string
	for _, u := range d.Unavailable {
		unavailable = append(unavailable, u.Signal)
	}
	for _, f := range d.Findings {
		if diagnosis.SevRank(f.Severity) < diagnosis.SevRank(diagnosis.SevWarning) {
			continue
		}
		k := stateKey(d.Namespace, d.Job, f.Kind)
		present[k] = true
		st := t.states[k]
		if st == nil {
			st = &state{firstSeen: now}
			t.states[k] = st
		}
		st.lastSeen = now
		if st.inc == nil {
			if now.Sub(st.firstSeen) < t.opt.MinDuration {
				continue
			}
			in := &Incident{
				ID: NewID(d.Namespace, d.Job, f.Kind, st.firstSeen), Namespace: d.Namespace, Job: d.Job, Node: d.Node,
				Kind: f.Kind, Start: st.firstSeen, Open: true, Peak: map[string]float64{},
			}
			t.fold(in, f, unavailable, now)
			if t.store.PutIncident(*in) == nil {
				st.inc, st.lastWrite = in, now
			}
			continue
		}
		if t.fold(st.inc, f, unavailable, now) && now.Sub(st.lastWrite) >= t.opt.UpdateEvery {
			if t.store.PutIncident(*st.inc) == nil {
				st.lastWrite = now
			}
		}
	}
	// A pending finding that stopped before MinDuration was a flap: forget it.
	for k, st := range t.states {
		if st.inc == nil && !present[k] && strings.HasPrefix(k, d.Namespace+"\x00"+d.Job+"\x00") {
			delete(t.states, k)
		}
	}
}

// fold merges a finding into the incident and reports whether the persisted
// content changed materially (a higher severity, or a peak that grew by >10%).
func (t *Tracker) fold(in *Incident, f diagnosis.Finding, unavailable []string, now time.Time) bool {
	changed := false
	in.UpdatedAt = now
	if diagnosis.SevRank(f.Severity) > diagnosis.SevRank(in.Severity) {
		in.Severity, in.Confidence, in.Summary, in.Evidence = f.Severity, f.Confidence, f.Summary, f.Evidence
		changed = true
	} else if in.Severity == f.Severity && diagnosis.ConfRank(f.Confidence) > diagnosis.ConfRank(in.Confidence) {
		in.Confidence, changed = f.Confidence, true
	}
	for _, e := range f.Evidence {
		if old, ok := in.Peak[e.Metric]; !ok || e.Value > old {
			if !ok || e.Value > old*1.1 {
				changed = true
			}
			in.Peak[e.Metric] = e.Value
		}
	}
	sort.Strings(unavailable)
	in.Unavailable = unavailable
	return changed
}

// Expire closes incidents whose finding has not been seen for CloseAfter (also
// covers jobs that disappeared) and forgets pending findings that went stale.
func (t *Tracker) Expire(now time.Time) {
	for k, st := range t.states {
		if now.Sub(st.lastSeen) < t.opt.CloseAfter {
			continue
		}
		if st.inc != nil {
			in := *st.inc
			in.Open, in.End, in.CloseReason = false, st.lastSeen, "resolved"
			in.UpdatedAt = now
			if t.store.PutIncident(in) != nil {
				continue // retry next cycle
			}
		}
		delete(t.states, k)
	}
	for k, at := range t.lastSample {
		if now.Sub(at) > 2*t.store.opt.Retention {
			delete(t.lastSample, k)
		}
	}
}
