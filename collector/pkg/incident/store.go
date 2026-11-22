// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

// Package incident persists Flight Recorder diagnosis history on the node.
//
// The store is an append-only set of JSON-lines segment files. Two record types
// are written: "incident" (the full state of a per-job incident; the last record
// per id wins) and "sample" (the measured metrics of a job at one instant, used
// for before/after comparison). A crash can tear only the last line of the
// newest segment; readers skip any line that does not parse, and a restart never
// appends to an old segment. Retention is by age and by total size; only files
// this package created (flight-<n>.jsonl) are ever read or deleted.
package incident

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/zyvorai/gryvia/collector/pkg/diagnosis"
)

// Fsync policies.
const (
	FsyncAlways   = "always"   // every record is fsynced before Append returns
	FsyncInterval = "interval" // incident records are fsynced at once; samples on Tick
	FsyncNone     = "none"     // never (the OS decides); a crash can lose recent records
)

const (
	recordVersion  = 1
	maxRecordBytes = 64 << 10
	maxLineBytes   = 1 << 20
	maxIncidents   = 2000 // bounded in-memory index; older closed incidents stay on disk until retention
	maxEvidence    = 32
	minSegment     = 64 << 10
	maxSegment     = 8 << 20
)

var segmentName = regexp.MustCompile(`^flight-(\d{20})\.jsonl$`)

// Incident is one persisted bottleneck episode of a job.
type Incident struct {
	ID         string    `json:"id"`
	Namespace  string    `json:"namespace"`
	Job        string    `json:"job"`
	Node       string    `json:"node,omitempty"`
	Kind       string    `json:"kind"`
	Severity   string    `json:"severity"`   // peak severity
	Confidence string    `json:"confidence"` // at peak
	Start      time.Time `json:"start"`
	End        time.Time `json:"end,omitempty"` // zero while open
	Open       bool      `json:"open"`
	// CloseReason is "resolved" (the finding stopped for the close-after period)
	// or "collector-restarted" (End is the last persisted update, not observed).
	CloseReason string               `json:"closeReason,omitempty"`
	Summary     string               `json:"summary"`
	Peak        map[string]float64   `json:"peak"`     // largest value seen per evidence metric
	Evidence    []diagnosis.Evidence `json:"evidence"` // snapshot at the peak severity
	// Unavailable lists signals that could not be measured at the last update:
	// an incident is a lower bound on what happened.
	Unavailable []string  `json:"unavailable,omitempty"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// Sample is the measured metrics of one job at one instant.
type Sample struct {
	Namespace string             `json:"namespace"`
	Job       string             `json:"job"`
	At        time.Time          `json:"at"`
	Metrics   map[string]float64 `json:"metrics"`
}

type record struct {
	V        int       `json:"v"`
	Type     string    `json:"type"`
	At       time.Time `json:"at"`
	Incident *Incident `json:"incident,omitempty"`
	Sample   *Sample   `json:"sample,omitempty"`
}

// Options configures a Store.
type Options struct {
	Dir       string
	Retention time.Duration // records/segments older than this are removed (default 24h)
	MaxBytes  int64         // total size bound of all segments (default 64 MiB)
	Fsync     string        // default FsyncInterval
	Now       func() time.Time
}

// Stats describe the store for /flight/incidents responses and tests.
type Stats struct {
	Segments     int   `json:"segments"`
	Bytes        int64 `json:"bytes"`
	CorruptLines int   `json:"corruptLines"` // lines skipped while loading (a torn tail counts here)
	Incidents    int   `json:"incidents"`
}

// Store is the on-disk history plus a bounded in-memory incident index.
type Store struct {
	opt Options
	seg int64 // segment size bound

	mu        sync.Mutex
	f         *os.File
	fSize     int64
	fName     string
	dirty     bool
	incidents map[string]*Incident
	corrupt   int
	closed    bool
}

// Open creates the directory if needed, loads the retained history and starts a
// fresh segment (an existing segment is never appended to).
func Open(opt Options) (*Store, error) {
	if opt.Dir == "" {
		return nil, errors.New("incident store: empty directory")
	}
	if opt.Retention <= 0 {
		opt.Retention = 24 * time.Hour
	}
	if opt.MaxBytes <= 0 {
		opt.MaxBytes = 64 << 20
	}
	switch opt.Fsync {
	case "":
		opt.Fsync = FsyncInterval
	case FsyncAlways, FsyncInterval, FsyncNone:
	default:
		return nil, fmt.Errorf("incident store: unknown fsync policy %q", opt.Fsync)
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if err := os.MkdirAll(opt.Dir, 0o700); err != nil {
		return nil, err
	}
	seg := opt.MaxBytes / 8
	if seg < minSegment {
		seg = minSegment
	}
	if seg > maxSegment {
		seg = maxSegment
	}
	s := &Store{opt: opt, seg: seg, incidents: map[string]*Incident{}}
	if err := s.load(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.rotateLocked(); err != nil {
		return nil, err
	}
	s.pruneLocked()
	return s, nil
}

// segments lists this package's regular files, oldest first.
func (s *Store) segments() ([]string, error) {
	ents, err := os.ReadDir(s.opt.Dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range ents {
		if !segmentName.MatchString(e.Name()) {
			continue
		}
		fi, err := e.Info()
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// scan streams the parseable records of one segment. Lines that do not parse
// (a torn tail, garbage, an over-long line) are counted and skipped.
func scan(path string, fn func(record)) (corrupt int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64<<10)
	for {
		line, tooLong, rerr := readLine(r)
		if len(line) > 0 || tooLong {
			var rec record
			if tooLong || json.Unmarshal(line, &rec) != nil || rec.V != recordVersion || (rec.Incident == nil && rec.Sample == nil) {
				corrupt++
			} else {
				fn(rec)
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return corrupt, nil
			}
			return corrupt, rerr
		}
	}
}

// readLine returns one line without its terminator; lines over maxLineBytes are
// consumed and reported as tooLong.
func readLine(r *bufio.Reader) (line []byte, tooLong bool, err error) {
	for {
		chunk, isPrefix, e := r.ReadLine()
		if e != nil {
			return line, tooLong, e
		}
		if !tooLong {
			if len(line)+len(chunk) > maxLineBytes {
				tooLong, line = true, nil
			} else {
				line = append(line, chunk...)
			}
		}
		if !isPrefix {
			return line, tooLong, nil
		}
	}
}

func (s *Store) load() error {
	names, err := s.segments()
	if err != nil {
		return err
	}
	cutoff := s.opt.Now().Add(-s.opt.Retention)
	for _, n := range names {
		c, err := scan(filepath.Join(s.opt.Dir, n), func(rec record) {
			if rec.Incident != nil && rec.At.After(cutoff) {
				s.indexLocked(rec.Incident)
			}
		})
		s.corrupt += c
		if err != nil {
			return fmt.Errorf("incident store: reading %s: %w", n, err)
		}
	}
	return nil
}

func validID(id string) bool { return len(id) > 0 && len(id) <= 64 }

func (s *Store) indexLocked(in *Incident) {
	if !validID(in.ID) || in.Namespace == "" || in.Job == "" {
		return
	}
	cp := *in
	s.incidents[in.ID] = &cp
	if len(s.incidents) <= maxIncidents {
		return
	}
	// Evict the oldest closed incident (open ones are never evicted).
	var victim *Incident
	for _, x := range s.incidents {
		if x.Open {
			continue
		}
		if victim == nil || x.Start.Before(victim.Start) {
			victim = x
		}
	}
	if victim != nil {
		delete(s.incidents, victim.ID)
	}
}

func (s *Store) rotateLocked() error {
	if s.f != nil {
		_ = s.f.Sync()
		_ = s.f.Close()
		s.f = nil
	}
	name := fmt.Sprintf("flight-%020d.jsonl", s.opt.Now().UnixNano())
	// O_EXCL: never reuse or follow an existing entry.
	f, err := os.OpenFile(filepath.Join(s.opt.Dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0o600)
	if err != nil {
		// Same-nanosecond collision (or a test clock): fall back to a bumped name.
		name = fmt.Sprintf("flight-%020d.jsonl", s.opt.Now().UnixNano()+1)
		if f, err = os.OpenFile(filepath.Join(s.opt.Dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0o600); err != nil {
			return err
		}
	}
	s.f, s.fName, s.fSize = f, name, 0
	// Carry open incidents forward so pruning old segments cannot lose them.
	for _, in := range s.incidents {
		if in.Open {
			if err := s.writeLocked(record{V: recordVersion, Type: "incident", At: in.UpdatedAt, Incident: in}, false); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) writeLocked(rec record, sync bool) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if len(b) > maxRecordBytes {
		return fmt.Errorf("incident store: record of %d bytes exceeds %d", len(b), maxRecordBytes)
	}
	b = append(b, '\n')
	n, err := s.f.Write(b)
	s.fSize += int64(n)
	if err != nil {
		return err
	}
	s.dirty = true
	if sync && s.opt.Fsync != FsyncNone {
		if err := s.f.Sync(); err != nil {
			return err
		}
		s.dirty = false
	}
	return nil
}

func (s *Store) appendLocked(rec record, syncNow bool) error {
	if s.closed || s.f == nil {
		return errors.New("incident store closed")
	}
	if s.fSize >= s.seg {
		if err := s.rotateLocked(); err != nil {
			return err
		}
		s.pruneLocked()
	}
	return s.writeLocked(rec, syncNow || s.opt.Fsync == FsyncAlways)
}

// PutIncident records the full current state of an incident and indexes it.
func (s *Store) PutIncident(in Incident) error {
	if len(in.Evidence) > maxEvidence {
		in.Evidence = in.Evidence[:maxEvidence]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.appendLocked(record{V: recordVersion, Type: "incident", At: in.UpdatedAt, Incident: &in}, true); err != nil {
		return err
	}
	s.indexLocked(&in)
	return nil
}

// PutSample records the measured metrics of a job.
func (s *Store) PutSample(sm Sample) error {
	if len(sm.Metrics) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendLocked(record{V: recordVersion, Type: "sample", At: sm.At, Sample: &sm}, false)
}

// Tick fsyncs pending records (the interval policy) and applies retention.
func (s *Store) Tick() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.f == nil {
		return
	}
	if s.dirty && s.opt.Fsync != FsyncNone {
		if s.f.Sync() == nil {
			s.dirty = false
		}
	}
	s.pruneLocked()
}

// pruneLocked removes segments older than the retention and then the oldest
// segments until the total fits MaxBytes. The active segment is never removed,
// so the total can exceed MaxBytes by at most one segment. Incidents whose last
// update is older than the retention leave the in-memory index.
func (s *Store) pruneLocked() {
	now := s.opt.Now()
	cutoff := now.Add(-s.opt.Retention)
	names, err := s.segments()
	if err != nil {
		return
	}
	type seg struct {
		name string
		size int64
		mod  time.Time
	}
	var segs []seg
	var total int64
	for _, n := range names {
		fi, err := os.Lstat(filepath.Join(s.opt.Dir, n))
		if err != nil {
			continue
		}
		segs = append(segs, seg{n, fi.Size(), fi.ModTime()})
		total += fi.Size()
	}
	for _, sg := range segs {
		if sg.name == s.fName {
			continue
		}
		if sg.mod.Before(cutoff) || total > s.opt.MaxBytes {
			if os.Remove(filepath.Join(s.opt.Dir, sg.name)) == nil {
				total -= sg.size
			}
		}
	}
	for id, in := range s.incidents {
		if !in.Open && in.UpdatedAt.Before(cutoff) {
			delete(s.incidents, id)
		}
	}
}

// Close syncs and closes the active segment.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.f == nil {
		return nil
	}
	_ = s.f.Sync()
	return s.f.Close()
}

// Stats returns sizes and counters.
func (s *Store) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Stats{CorruptLines: s.corrupt, Incidents: len(s.incidents)}
	names, _ := s.segments()
	for _, n := range names {
		if fi, err := os.Lstat(filepath.Join(s.opt.Dir, n)); err == nil {
			st.Segments++
			st.Bytes += fi.Size()
		}
	}
	return st
}

// Filter selects incidents.
type Filter struct {
	Namespace, Job string
	Since          time.Time // incidents that ended (or are still open) at or after this
	Limit          int
}

// Incidents returns matching incidents, newest first.
func (s *Store) Incidents(f Filter) []Incident {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Incident
	for _, in := range s.incidents {
		if f.Namespace != "" && in.Namespace != f.Namespace {
			continue
		}
		if f.Job != "" && in.Job != f.Job {
			continue
		}
		if !f.Since.IsZero() && !in.Open && in.End.Before(f.Since) {
			continue
		}
		out = append(out, *in)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Start.Equal(out[j].Start) {
			return out[i].Start.After(out[j].Start)
		}
		return out[i].ID < out[j].ID
	})
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out
}

// Get returns one incident.
func (s *Store) Get(id string) (Incident, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in, ok := s.incidents[id]
	if !ok {
		return Incident{}, false
	}
	return *in, true
}

// OpenIncidents lists incidents still marked open (used after a restart).
func (s *Store) OpenIncidents() []Incident {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Incident
	for _, in := range s.incidents {
		if in.Open {
			out = append(out, *in)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Samples returns the job's samples with from <= At <= to, oldest first, by
// scanning the segments (samples are not held in memory).
func (s *Store) Samples(ns, job string, from, to time.Time) []Sample {
	s.mu.Lock()
	if s.f != nil {
		_ = s.f.Sync() // make our own recent writes visible to the reader
	}
	names, _ := s.segments()
	s.mu.Unlock()
	var out []Sample
	for _, n := range names {
		_, _ = scan(filepath.Join(s.opt.Dir, n), func(rec record) {
			sm := rec.Sample
			if sm == nil || sm.Namespace != ns || sm.Job != job || sm.At.Before(from) || sm.At.After(to) {
				return
			}
			out = append(out, *sm)
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

// NewID derives a stable incident id.
func NewID(ns, job, kind string, start time.Time) string {
	h := uint64(1469598103934665603)
	for _, b := range []byte(ns + "\x00" + job + "\x00" + kind + "\x00" + fmt.Sprint(start.UnixNano())) {
		h ^= uint64(b)
		h *= 1099511628211
	}
	return fmt.Sprintf("inc-%016x", h)
}
