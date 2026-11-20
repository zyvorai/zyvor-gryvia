// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package incident

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zyvorai/gryvia/collector/pkg/diagnosis"
)

var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func openStore(t *testing.T, dir string, c *clock, mutate func(*Options)) *Store {
	t.Helper()
	o := Options{Dir: dir, Retention: 24 * time.Hour, MaxBytes: 1 << 20, Fsync: FsyncAlways, Now: c.now}
	if mutate != nil {
		mutate(&o)
	}
	s, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func diag(kind, sev string, metric string, v float64) diagnosis.Diagnosis {
	return diagnosis.Diagnosis{
		Namespace: "ml", Job: "train", Node: "n1",
		Findings: []diagnosis.Finding{{Kind: kind, Severity: sev, Confidence: diagnosis.ConfMedium, Summary: kind + " summary",
			Evidence: []diagnosis.Evidence{{Source: "cgroup", Metric: metric, Value: v, Window: "5m0s"}}}},
		Metrics:     map[string]float64{metric: v},
		Unavailable: []diagnosis.Unavailable{{Signal: "fabric.pfc", Reason: "x"}},
	}
}

func TestIncidentOpenPeakCloseAndReload(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t0}
	s := openStore(t, dir, c, nil)
	tr := NewTracker(s, TrackerOptions{MinDuration: 60 * time.Second, CloseAfter: 60 * time.Second, SampleEvery: time.Hour, UpdateEvery: time.Second})

	// Below MinDuration: no incident (a flap).
	tr.Observe(c.t, diag("cpu", "warning", "cpu_throttle_ratio", 0.3))
	c.add(30 * time.Second)
	tr.Observe(c.t, diag("cpu", "warning", "cpu_throttle_ratio", 0.3))
	if got := s.Incidents(Filter{}); len(got) != 0 {
		t.Fatalf("opened too early: %+v", got)
	}
	c.add(31 * time.Second)
	tr.Observe(c.t, diag("cpu", "warning", "cpu_throttle_ratio", 0.35))
	incs := s.Incidents(Filter{Namespace: "ml", Job: "train"})
	if len(incs) != 1 || !incs[0].Open || !incs[0].Start.Equal(t0) || incs[0].Kind != "cpu" || incs[0].Severity != "warning" {
		t.Fatalf("open: %+v", incs)
	}
	// Peak and severity grow.
	c.add(10 * time.Second)
	tr.Observe(c.t, diag("cpu", "critical", "cpu_throttle_ratio", 0.8))
	c.add(10 * time.Second)
	tr.Observe(c.t, diag("cpu", "warning", "cpu_throttle_ratio", 0.4)) // lower again: peak stays
	in := s.Incidents(Filter{})[0]
	if in.Severity != "critical" || in.Peak["cpu_throttle_ratio"] != 0.8 || len(in.Unavailable) != 1 {
		t.Fatalf("peak: %+v", in)
	}
	// Absent for CloseAfter: closes with End = last observation.
	last := c.t
	c.add(30 * time.Second)
	tr.Observe(c.t, diagnosis.Diagnosis{Namespace: "ml", Job: "train"})
	tr.Expire(c.t)
	if !s.Incidents(Filter{})[0].Open {
		t.Fatal("closed before CloseAfter")
	}
	c.add(31 * time.Second)
	tr.Expire(c.t)
	in = s.Incidents(Filter{})[0]
	if in.Open || !in.End.Equal(last) || in.CloseReason != "resolved" {
		t.Fatalf("close: %+v", in)
	}
	id := in.ID

	// Reload from disk: the closed incident survives; a new run starts a new segment.
	_ = s.Close()
	s2 := openStore(t, dir, c, nil)
	got, ok := s2.Get(id)
	if !ok || got.Open || got.Peak["cpu_throttle_ratio"] != 0.8 {
		t.Fatalf("reload: %+v %v", got, ok)
	}
	names, _ := s2.segments()
	if len(names) != 2 {
		t.Fatalf("expected the old and one new segment: %v", names)
	}
}

func TestRestartClosesOpenIncident(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t0}
	s := openStore(t, dir, c, nil)
	tr := NewTracker(s, TrackerOptions{MinDuration: time.Second, CloseAfter: time.Minute, UpdateEvery: time.Second})
	tr.Observe(c.t, diag("memory", "critical", "mem_oom_kills", 1))
	c.add(5 * time.Second)
	tr.Observe(c.t, diag("memory", "critical", "mem_oom_kills", 2))
	if !s.Incidents(Filter{})[0].Open {
		t.Fatal("should be open")
	}
	lastUpdate := c.t
	_ = s.Close() // crash: nothing closed it
	c.add(10 * time.Minute)
	s2 := openStore(t, dir, c, nil)
	if len(s2.OpenIncidents()) != 1 {
		t.Fatal("the open incident must be recovered")
	}
	NewTracker(s2, TrackerOptions{})
	in := s2.Incidents(Filter{})[0]
	if in.Open || in.CloseReason != "collector-restarted" || !in.End.Equal(lastUpdate) {
		t.Fatalf("%+v", in)
	}
}

func TestCrashRecoveryTornAndGarbageLines(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t0}
	s := openStore(t, dir, c, nil)
	in := Incident{ID: "inc-1", Namespace: "ml", Job: "train", Kind: "cpu", Severity: "warning", Start: t0, End: t0.Add(time.Minute), UpdatedAt: t0.Add(time.Minute), Peak: map[string]float64{"x": 1}}
	if err := s.PutIncident(in); err != nil {
		t.Fatal(err)
	}
	in2 := in
	in2.ID = "inc-2"
	if err := s.PutIncident(in2); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	names, _ := s.segments()
	first := filepath.Join(dir, names[0])
	full, _ := os.ReadFile(first)
	// Truncate in the middle of the second record (no trailing newline) and add junk before it.
	cut := bytes.LastIndexByte(full[:len(full)-1], '\n') + 1
	torn := append(append([]byte("not json at all\n"), full[:cut]...), full[cut:cut+20]...)
	if err := os.WriteFile(first, torn, 0o600); err != nil {
		t.Fatal(err)
	}
	// An over-long line is skipped too.
	long := append(bytes.Repeat([]byte("a"), maxLineBytes+10), '\n')
	if err := os.WriteFile(filepath.Join(dir, "flight-00000000000000000001.jsonl"), append(long, full[:cut]...), 0o600); err != nil {
		t.Fatal(err)
	}
	// Foreign files are never read or touched.
	foreign := filepath.Join(dir, "notes.txt")
	_ = os.WriteFile(foreign, []byte("keep"), 0o600)

	s2 := openStore(t, dir, c, nil)
	if _, ok := s2.Get("inc-1"); !ok {
		t.Fatal("intact record before the torn tail must survive")
	}
	if _, ok := s2.Get("inc-2"); ok {
		t.Fatal("torn record must not be loaded")
	}
	if st := s2.Stats(); st.CorruptLines != 3 { // junk, torn tail, over-long
		t.Fatalf("corrupt lines: %+v", st)
	}
	if b, _ := os.ReadFile(foreign); string(b) != "keep" {
		t.Fatal("foreign file modified")
	}
	// The torn segment is left as is (never appended to): new writes go to a new segment.
	if b, _ := os.ReadFile(first); !bytes.Equal(b, torn) {
		t.Fatal("old segment was modified")
	}
	if err := s2.PutIncident(in2); err != nil {
		t.Fatal(err)
	}
	if _, ok := s2.Get("inc-2"); !ok {
		t.Fatal("write after recovery failed")
	}
}

func TestRetentionByAgeAndSize(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t0}
	s := openStore(t, dir, c, func(o *Options) { o.Retention = time.Hour; o.MaxBytes = 4 * minSegment })
	// Age: an old segment is removed on the next prune.
	old := filepath.Join(dir, "flight-00000000000000000005.jsonl")
	if err := os.WriteFile(old, []byte(`{"v":1,"type":"sample","at":"2026-03-01T00:00:00Z","sample":{"namespace":"ml","job":"train","at":"2026-03-01T00:00:00Z","metrics":{"a":1}}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	past := t0.Add(-2 * time.Hour)
	_ = os.Chtimes(old, past, past)
	s.Tick()
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("aged segment not pruned")
	}
	// Size: many samples force rotation; total stays within MaxBytes + one segment.
	big := map[string]float64{}
	for i := 0; i < 200; i++ {
		big["metric_number_"+strings.Repeat("x", 10)+string(rune('a'+i%26))+string(rune('a'+i/26))] = float64(i)
	}
	for i := 0; i < 400; i++ {
		c.add(time.Second)
		if err := s.PutSample(Sample{Namespace: "ml", Job: "train", At: c.t, Metrics: big}); err != nil {
			t.Fatal(err)
		}
	}
	s.Tick()
	st := s.Stats()
	if st.Bytes > 4*minSegment+s.seg+4096 || st.Segments < 2 {
		t.Fatalf("size bound: %+v (seg %d)", st, s.seg)
	}
	// The oldest samples are gone, the newest are kept.
	got := s.Samples("ml", "train", t0, c.t)
	if len(got) == 0 || len(got) >= 400 || !got[len(got)-1].At.Equal(c.t) {
		t.Fatalf("samples after pruning: %d", len(got))
	}
}

func TestOpenIncidentSurvivesPruneOfItsSegment(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t0}
	s := openStore(t, dir, c, func(o *Options) { o.MaxBytes = 2 * minSegment })
	in := Incident{ID: "inc-open", Namespace: "ml", Job: "train", Kind: "cpu", Severity: "critical", Start: t0, Open: true, UpdatedAt: t0, Peak: map[string]float64{"a": 1}}
	if err := s.PutIncident(in); err != nil {
		t.Fatal(err)
	}
	big := map[string]float64{}
	for i := 0; i < 300; i++ {
		big["m"+strings.Repeat("y", 12)+string(rune('a'+i%26))+string(rune('a'+i/26))] = 1
	}
	for i := 0; i < 500; i++ {
		c.add(time.Second)
		_ = s.PutSample(Sample{Namespace: "ml", Job: "other", At: c.t, Metrics: big})
	}
	_ = s.Close()
	s2 := openStore(t, dir, c, func(o *Options) { o.MaxBytes = 2 * minSegment })
	if _, ok := s2.Get("inc-open"); !ok {
		t.Fatal("open incident lost when its first segment was pruned")
	}
}

func TestBoundedIncidentIndex(t *testing.T) {
	c := &clock{t0}
	s := openStore(t, t.TempDir(), c, func(o *Options) { o.Fsync = FsyncNone; o.MaxBytes = 64 << 20 })
	for i := 0; i < maxIncidents+50; i++ {
		c.add(time.Second)
		in := Incident{ID: NewID("ml", "j", "cpu", c.t), Namespace: "ml", Job: "j", Kind: "cpu", Start: c.t, End: c.t, UpdatedAt: c.t}
		if err := s.PutIncident(in); err != nil {
			t.Fatal(err)
		}
	}
	if n := s.Stats().Incidents; n != maxIncidents {
		t.Fatalf("index not bounded: %d", n)
	}
}

func TestFsyncPolicyValidation(t *testing.T) {
	if _, err := Open(Options{Dir: t.TempDir(), Fsync: "sometimes"}); err == nil {
		t.Fatal("bad fsync policy accepted")
	}
	if _, err := Open(Options{}); err == nil {
		t.Fatal("empty dir accepted")
	}
}

func TestCompareMath(t *testing.T) {
	c := &clock{t0}
	s := openStore(t, t.TempDir(), c, nil)
	put := func(at time.Time, m map[string]float64) {
		if err := s.PutSample(Sample{Namespace: "ml", Job: "train", At: at, Metrics: m}); err != nil {
			t.Fatal(err)
		}
	}
	// window A around t0+10m: two samples; window B around t0+60m: two samples.
	put(t0.Add(9*time.Minute), map[string]float64{"cpu_throttle_ratio": 0.1, "only_a": 5, "zero": 0})
	put(t0.Add(10*time.Minute), map[string]float64{"cpu_throttle_ratio": 0.3, "only_a": 7, "zero": 0})
	put(t0.Add(59*time.Minute), map[string]float64{"cpu_throttle_ratio": 0.5, "only_b": 2, "zero": 4})
	put(t0.Add(60*time.Minute), map[string]float64{"cpu_throttle_ratio": 0.7, "only_b": 4, "zero": 4})
	// A different job must not leak in.
	_ = s.PutSample(Sample{Namespace: "ml", Job: "other", At: t0.Add(10 * time.Minute), Metrics: map[string]float64{"cpu_throttle_ratio": 99}})

	c.t = t0.Add(2 * time.Hour)
	wa, _, err := s.ResolveWindow("ml", "train", t0.Add(10*time.Minute).Format(time.RFC3339), 5*time.Minute, c.t)
	if err != nil {
		t.Fatal(err)
	}
	wb, _, err := s.ResolveWindow("ml", "train", t0.Add(time.Hour).Format(time.RFC3339), 5*time.Minute, c.t)
	if err != nil {
		t.Fatal(err)
	}
	cmp := s.Compare("ml", "train", wa, wb, nil, nil)
	if cmp.A.Samples != 2 || cmp.B.Samples != 2 || cmp.A.Source != "samples" {
		t.Fatalf("%+v", cmp)
	}
	by := map[string]MetricDelta{}
	for _, m := range cmp.Metrics {
		by[m.Metric] = m
	}
	cpu := by["cpu_throttle_ratio"]
	if *cpu.A < 0.1999 || *cpu.A > 0.2001 || *cpu.B < 0.5999 || *cpu.B > 0.6001 || *cpu.Delta < 0.3999 || *cpu.Delta > 0.4001 || *cpu.PercentChange < 199.9 || *cpu.PercentChange > 200.1 {
		t.Fatalf("cpu: %+v", cpu)
	}
	if z := by["zero"]; *z.Delta != 4 || z.PercentChange != nil {
		t.Fatalf("zero baseline must have no percent: %+v", z)
	}
	if o := by["only_a"]; o.B != nil || o.Delta != nil || !strings.Contains(o.Note, "not measured in window b") {
		t.Fatalf("only_a: %+v", o)
	}
	if o := by["only_b"]; o.A != nil || !strings.Contains(o.Note, "window a") {
		t.Fatalf("only_b: %+v", o)
	}
}

func TestCompareIncidentWindowsAndErrors(t *testing.T) {
	c := &clock{t0.Add(3 * time.Hour)}
	s := openStore(t, t.TempDir(), c, nil)
	in := Incident{ID: "inc-a", Namespace: "ml", Job: "train", Kind: "cpu", Start: t0, End: t0.Add(10 * time.Minute), UpdatedAt: t0, Peak: map[string]float64{"cpu_throttle_ratio": 0.9}}
	_ = s.PutIncident(in)
	other := Incident{ID: "inc-x", Namespace: "other", Job: "train", Kind: "cpu", Start: t0, End: t0, UpdatedAt: t0}
	_ = s.PutIncident(other)
	wa, ia, err := s.ResolveWindow("ml", "train", "inc-a", 5*time.Minute, c.t)
	if err != nil || !wa.From.Equal(t0) || !wa.To.Equal(t0.Add(10*time.Minute)) {
		t.Fatalf("%+v %v", wa, err)
	}
	// No samples in the incident window: falls back to its peak values, and says so.
	wb, _, _ := s.ResolveWindow("ml", "train", "now", time.Minute, c.t)
	cmp := s.Compare("ml", "train", wa, wb, ia, nil)
	if cmp.A.Source != "incident-peak" || cmp.B.Source != "none" || len(cmp.Metrics) != 1 || cmp.Metrics[0].B != nil {
		t.Fatalf("%+v", cmp)
	}
	// Another tenant's incident id is indistinguishable from a missing one.
	if _, _, err := s.ResolveWindow("ml", "train", "inc-x", time.Minute, c.t); err == nil {
		t.Fatal("cross-namespace incident resolved")
	}
	for _, bad := range []string{"", "yesterday", strings.Repeat("a", 100)} {
		if _, _, err := s.ResolveWindow("ml", "train", bad, time.Minute, c.t); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestExportCSVEscaping(t *testing.T) {
	evil := Incident{
		ID: "inc-1", Namespace: "ml", Job: "train", Node: "=cmd|' /C calc'!A0", Kind: "cpu", Severity: "warning",
		Start: t0, End: t0.Add(90 * time.Second), UpdatedAt: t0,
		Summary:     "line1\nline2, with \"quotes\"\x1b[31m and =1+1",
		CloseReason: "@SUM(A1)", Peak: map[string]float64{"neg": -5},
		Unavailable: []string{"+x"},
	}
	var buf bytes.Buffer
	if err := ExportCSV(&buf, []Incident{evil, {ID: "-lead", Namespace: "ml", Job: "j", Start: t0, UpdatedAt: t0, Open: true}}); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(&buf).ReadAll()
	if err != nil || len(rows) != 3 {
		t.Fatalf("csv did not round-trip: %v %d", err, len(rows))
	}
	row := rows[1]
	if !strings.HasPrefix(row[3], "'=") {
		t.Fatalf("formula not neutralised: %q", row[3])
	}
	if !strings.HasPrefix(row[11], "'@") {
		t.Fatalf("@ not neutralised: %q", row[11])
	}
	if strings.Contains(row[12], "\x1b") || !strings.Contains(row[12], "\n") || !strings.Contains(row[12], `"quotes"`) {
		t.Fatalf("summary: %q", row[12])
	}
	if row[10] != "90" || row[9] != "false" {
		t.Fatalf("numeric columns: %v", row)
	}
	if !strings.HasPrefix(rows[2][0], "'-") {
		t.Fatalf("leading minus in text column: %q", rows[2][0])
	}
	// No cell of any row starts with a formula trigger.
	for _, r := range rows[1:] {
		for i, cell := range r {
			if cell != "" && strings.ContainsRune("=+-@\t\r", rune(cell[0])) && i != 12 {
				t.Errorf("unsafe cell %d: %q", i, cell)
			}
		}
	}
}

func handler(t *testing.T, s *Store, c *clock) *Handler {
	return &Handler{Store: s, Node: "n1", Now: c.now}
}

func get(h http.HandlerFunc, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func TestHTTP(t *testing.T) {
	c := &clock{t0.Add(time.Hour)}
	s := openStore(t, t.TempDir(), c, nil)
	_ = s.PutIncident(Incident{ID: "inc-1", Namespace: "ml", Job: "train", Kind: "cpu", Severity: "warning", Start: t0, End: t0.Add(time.Minute), UpdatedAt: t0, Summary: "=evil"})
	_ = s.PutIncident(Incident{ID: "inc-2", Namespace: "ml", Job: "other", Kind: "memory", Start: t0, End: t0, UpdatedAt: t0})
	h := handler(t, s, c)

	rec := get(h.ServeIncidents, "/api/v1/flight/incidents?namespace=ml&job=train")
	var body struct {
		Enabled   bool       `json:"enabled"`
		Incidents []Incident `json:"incidents"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &body) != nil || !body.Enabled || len(body.Incidents) != 1 || body.Incidents[0].ID != "inc-1" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec = get(h.ServeIncidents, "/x?namespace=ml&since=30m"); rec.Code != 200 || strings.Contains(rec.Body.String(), "inc-1") {
		t.Fatalf("since filter: %s", rec.Body)
	}
	if rec = get(h.ServeIncidents, "/x?namespace=ml&since=2h"); !strings.Contains(rec.Body.String(), "inc-1") {
		t.Fatalf("since filter: %s", rec.Body)
	}
	for _, bad := range []string{"/x?namespace=ML", "/x?job=a/b", "/x?since=yesterday", "/x?limit=0", "/x?limit=abc"} {
		if rec = get(h.ServeIncidents, bad); rec.Code != 400 {
			t.Errorf("%s: %d", bad, rec.Code)
		}
	}
	rec = get(h.ServeExport, "/x?format=csv&namespace=ml&job=train")
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/csv") || !strings.Contains(rec.Body.String(), "'=evil") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec = get(h.ServeExport, "/x?format=xml"); rec.Code != 400 {
		t.Fatal("bad format accepted")
	}
	if rec = get(h.ServeExport, "/x?namespace=ml"); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"incidents"`) {
		t.Fatalf("json export: %s", rec.Body)
	}
	// compare
	rec = get(h.ServeCompare, "/x?namespace=ml&job=train&a=inc-1&b=now")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"comparison"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec = get(h.ServeCompare, "/x?namespace=ml&job=train&a=inc-2&b=now"); rec.Code != 400 {
		t.Fatalf("other job's incident: %d", rec.Code)
	}
	if rec = get(h.ServeCompare, "/x?namespace=ml"); rec.Code != 400 {
		t.Fatal("job required")
	}
	// disabled store
	off := &Handler{Node: "n1"}
	for _, f := range []http.HandlerFunc{off.ServeIncidents, off.ServeExport} {
		if rec = get(f, "/x?namespace=ml&job=train"); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"enabled":false`) {
			t.Fatalf("disabled: %d %s", rec.Code, rec.Body)
		}
	}
	if rec = get(off.ServeCompare, "/x?namespace=ml&job=train&a=now&b=now"); !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Fatal(rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeIncidents(rec, httptest.NewRequest(http.MethodPost, "/x", nil))
	if rec.Code != 405 {
		t.Fatal("POST accepted")
	}
}

func TestTrackerSamplesRateLimited(t *testing.T) {
	c := &clock{t0}
	s := openStore(t, t.TempDir(), c, nil)
	tr := NewTracker(s, TrackerOptions{SampleEvery: time.Minute})
	d := diagnosis.Diagnosis{Namespace: "ml", Job: "train", Metrics: map[string]float64{"a": 1}}
	for i := 0; i < 5; i++ {
		tr.Observe(c.t, d)
		c.add(20 * time.Second)
	}
	if n := len(s.Samples("ml", "train", t0, c.t)); n != 2 {
		t.Fatalf("expected 2 samples in 100 s, got %d", n)
	}
}
