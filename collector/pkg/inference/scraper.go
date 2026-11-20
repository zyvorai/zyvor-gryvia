// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package inference

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"
)

const (
	// DefaultInterval is the scrape period.
	DefaultInterval = 15 * time.Second
	// MinInterval is the shortest accepted period.
	MinInterval = 5 * time.Second
	// MaxBody bounds one exposition.
	MaxBody = 8 << 20
	// ScrapeTimeout bounds one request.
	ScrapeTimeout = 5 * time.Second
	// DiscoverEvery is how often discovery re-lists the node's pods.
	DiscoverEvery = 30 * time.Second
)

// Logger is the subset of zap.SugaredLogger the scraper uses.
type Logger interface {
	Infow(msg string, kv ...interface{})
	Warnw(msg string, kv ...interface{})
}

type nopLogger struct{}

func (nopLogger) Infow(string, ...interface{}) {}
func (nopLogger) Warnw(string, ...interface{}) {}

// Sink receives every successful reading.
type Sink func(t Target, r Reading)

// TargetStatus is the last outcome for one target (served on /api/v1/inference).
type TargetStatus struct {
	Name       string    `json:"name"`
	URL        string    `json:"url"`
	Namespace  string    `json:"namespace,omitempty"`
	Job        string    `json:"job,omitempty"`
	Engine     string    `json:"engine,omitempty"`
	LastScrape time.Time `json:"lastScrape,omitempty"`
	Error      string    `json:"error,omitempty"`
	Available  []string  `json:"available,omitempty"`
	Missing    []string  `json:"missing,omitempty"`
	Notes      []string  `json:"notes,omitempty"`
}

type targetState struct {
	t      Target
	engine Engine
	status TargetStatus
}

// Scraper polls engine metrics endpoints. It only performs HTTP GETs, never follows redirects and
// never writes anything but its own state and the Sink.
type Scraper struct {
	Static   []Target
	Discover func(ctx context.Context) ([]Target, error) // optional
	Client   *http.Client
	Sink     Sink
	Log      Logger
	Interval time.Duration
	Window   time.Duration
	Now      func() time.Time

	mu         sync.Mutex
	states     map[string]*targetState
	discovered []Target
	lastDisc   time.Time
}

// NewScraper returns a Scraper with production defaults for the unset fields.
func NewScraper(static []Target, disc func(context.Context) ([]Target, error), sink Sink, log Logger) *Scraper {
	if log == nil {
		log = nopLogger{}
	}
	return &Scraper{
		Static: static, Discover: disc, Sink: sink, Log: log,
		Client: &http.Client{
			Timeout:       ScrapeTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport:     &http.Transport{Proxy: nil, MaxIdleConns: 8, IdleConnTimeout: 30 * time.Second},
		},
		Interval: DefaultInterval, Window: DefaultWindow, Now: time.Now,
		states: map[string]*targetState{},
	}
}

// Run scrapes every Interval until ctx is done.
func (s *Scraper) Run(ctx context.Context) {
	iv := s.Interval
	if iv < MinInterval {
		iv = MinInterval
	}
	s.ScrapeOnce(ctx)
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.ScrapeOnce(ctx)
		}
	}
}

func (s *Scraper) targets(ctx context.Context) []Target {
	all := append([]Target(nil), s.Static...)
	if s.Discover != nil {
		now := s.Now()
		if s.lastDisc.IsZero() || now.Sub(s.lastDisc) >= DiscoverEvery {
			s.lastDisc = now
			if d, err := s.Discover(ctx); err != nil {
				s.Log.Warnw("inference metrics: pod discovery failed", "error", err)
			} else {
				s.discovered = d
			}
		}
		names := map[string]bool{}
		for _, t := range all {
			names[t.Name] = true
		}
		for _, t := range s.discovered {
			if !names[t.Name] && len(all) < MaxTargets {
				all = append(all, t)
			}
		}
	}
	return all
}

// ScrapeOnce scrapes every target once (sequentially: the set is small and bounded).
func (s *Scraper) ScrapeOnce(ctx context.Context) {
	targets := s.targets(ctx)
	live := map[string]bool{}
	for _, t := range targets {
		if ctx.Err() != nil {
			return
		}
		live[t.Name] = true
		s.scrape(ctx, t)
	}
	s.mu.Lock()
	for name := range s.states {
		if !live[name] {
			delete(s.states, name) // pod gone: forget its baseline
		}
	}
	s.mu.Unlock()
}

func (s *Scraper) state(t Target) *targetState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.states[t.Name]
	if st == nil || st.t.URL != t.URL {
		st = &targetState{t: t}
		s.states[t.Name] = st
	}
	st.t = t
	return st
}

func (s *Scraper) setStatus(st *targetState, f func(*TargetStatus)) {
	s.mu.Lock()
	st.status.Name, st.status.URL, st.status.Namespace, st.status.Job = st.t.Name, st.t.URL, st.t.Namespace, st.t.Job
	f(&st.status)
	s.mu.Unlock()
}

func (s *Scraper) scrape(ctx context.Context, t Target) {
	st := s.state(t)
	now := s.Now()
	fams, err := s.fetch(ctx, t.URL)
	if err != nil {
		// The URL is operator or cluster supplied and carries no credentials; the error text is
		// the transport's, never the response body.
		s.Log.Warnw("inference metrics: scrape failed", "target", t.Name, "error", err.Error())
		s.setStatus(st, func(ts *TargetStatus) { ts.LastScrape, ts.Error = now, err.Error() })
		return
	}
	if st.engine == nil {
		name := t.Engine
		if name == "" {
			name = DetectEngine(fams)
		}
		if name == "" {
			s.setStatus(st, func(ts *TargetStatus) {
				ts.LastScrape, ts.Error = now, "no vllm:, nv_inference_ or tgi_ metrics found (set engine=... if the names are prefixed differently)"
			})
			return
		}
		e, err := NewEngine(name, s.Window)
		if err != nil {
			s.setStatus(st, func(ts *TargetStatus) { ts.LastScrape, ts.Error = now, err.Error() })
			return
		}
		st.engine = e
	}
	r := st.engine.Observe(now, fams)
	if len(r.Available) == 0 {
		s.setStatus(st, func(ts *TargetStatus) {
			ts.LastScrape, ts.Engine, ts.Error = now, r.Engine, "endpoint is reachable but exports none of the "+r.Engine+" latency metrics this collector reads"
			ts.Missing = r.Missing
		})
		return
	}
	s.setStatus(st, func(ts *TargetStatus) {
		ts.LastScrape, ts.Engine, ts.Error = now, r.Engine, ""
		ts.Available, ts.Missing, ts.Notes = r.Available, r.Missing, r.Notes
	})
	if s.Sink != nil {
		s.Sink(t, r)
	}
}

func (s *Scraper) fetch(ctx context.Context, u string) (Families, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/plain;version=0.0.4")
	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	lr := &io.LimitedReader{R: resp.Body, N: MaxBody + 1}
	fams, err := ParseText(lr)
	if err != nil {
		return nil, err
	}
	if lr.N <= 0 {
		return nil, errors.New("response larger than 8 MiB")
	}
	return fams, nil
}

// Status lists the last outcome of every current target, sorted by name.
func (s *Scraper) Status() []TargetStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]TargetStatus, 0, len(s.states))
	for _, st := range s.states {
		out = append(out, st.status)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
