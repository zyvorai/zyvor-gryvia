// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Package inference reads the serving engines' own Prometheus metrics (vLLM, NVIDIA Triton,
// HuggingFace TGI) and folds them into per-job inference latency figures.
//
// Kernel probes cannot see tokens: time to first token, inter-token latency and engine queue time
// exist only inside the engine, so the honest source is the engine's own /metrics endpoint. This
// package scrapes it (opt-in), turns cumulative histograms into per-interval quantiles, and
// tolerates the metric-name differences between engine versions.
package inference

import (
	"bufio"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Limits that bound a hostile or broken exposition.
const (
	maxLineBytes   = 1 << 20
	maxSamples     = 200000
	maxLabelsBytes = 8192
)

// Bucket is one cumulative histogram bucket (count of observations <= Le).
type Bucket struct {
	Le    float64
	Count float64
}

// Series is one label set of a metric family. Which fields are set depends on the family type:
// Value for gauge/counter/untyped, Buckets/Sum/Count for a histogram, Quantiles/Sum/Count for a
// summary.
type Series struct {
	Labels    map[string]string
	Value     float64
	Buckets   []Bucket // sorted by Le
	Quantiles map[float64]float64
	Sum       float64
	Count     float64
	hasCount  bool
}

// Family is a metric family: its declared type and its series.
type Family struct {
	Name   string
	Type   string // "counter", "gauge", "histogram", "summary" or "untyped"
	Series []*Series
}

// Families maps a family name to its parsed series.
type Families map[string]*Family

// ParseText parses the Prometheus text exposition format (also the OpenMetrics subset the
// engines emit: "# EOF", "_created" samples are ignored). Malformed lines are skipped, not fatal:
// an engine upgrade that adds a new line shape must not blind the scraper. It stops after
// maxSamples samples.
func ParseText(r io.Reader) (Families, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxLineBytes)
	types := map[string]string{}
	fams := Families{}
	type key struct {
		fam    string
		labels string
	}
	index := map[key]*Series{}
	n := 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if line[0] == '#' {
			f := strings.Fields(line)
			if len(f) >= 4 && f[1] == "TYPE" {
				types[f[2]] = strings.ToLower(f[3])
			}
			continue
		}
		name, labels, value, ok := parseSample(line)
		if !ok {
			continue
		}
		if n++; n > maxSamples {
			break
		}
		base, part := resolve(name, types, labels)
		if part == "created" {
			continue
		}
		fam := fams[base]
		if fam == nil {
			t := types[base]
			if t == "" {
				t = "untyped"
			}
			fam = &Family{Name: base, Type: t}
			fams[base] = fam
		}
		le, hasLe := labels["le"]
		q, hasQ := labels["quantile"]
		if part == "bucket" || part == "quantile" {
			delete(labels, "le")
			delete(labels, "quantile")
		}
		k := key{base, canonical(labels)}
		s := index[k]
		if s == nil {
			s = &Series{Labels: labels}
			index[k] = s
			fam.Series = append(fam.Series, s)
		}
		switch part {
		case "bucket":
			bound, err := parseFloat(le)
			if err != nil || !hasLe || math.IsNaN(value) {
				continue
			}
			s.Buckets = append(s.Buckets, Bucket{Le: bound, Count: value})
		case "quantile":
			qv, err := parseFloat(q)
			if err != nil || !hasQ {
				continue
			}
			if s.Quantiles == nil {
				s.Quantiles = map[float64]float64{}
			}
			s.Quantiles[qv] = value
		case "sum":
			s.Sum = value
		case "count":
			s.Count, s.hasCount = value, true
		default:
			s.Value = value
		}
	}
	if err := sc.Err(); err != nil {
		return fams, err
	}
	for _, fam := range fams {
		for _, s := range fam.Series {
			sort.Slice(s.Buckets, func(i, j int) bool { return s.Buckets[i].Le < s.Buckets[j].Le })
			if !s.hasCount && len(s.Buckets) > 0 {
				s.Count = s.Buckets[len(s.Buckets)-1].Count
			}
		}
	}
	return fams, nil
}

// resolve maps a sample name to (family, part). Suffixes only count when the base family was
// declared as a histogram or summary: "nv_inference_exec_count" is a plain counter, not the
// _count of a histogram named "nv_inference_exec".
func resolve(name string, types map[string]string, labels map[string]string) (string, string) {
	for _, sfx := range []string{"_bucket", "_sum", "_count", "_created"} {
		if !strings.HasSuffix(name, sfx) {
			continue
		}
		base := strings.TrimSuffix(name, sfx)
		t := types[base]
		switch sfx {
		case "_bucket":
			if t == "histogram" || (t == "" && hasKey(labels, "le")) {
				return base, "bucket"
			}
		case "_sum", "_count":
			if t == "histogram" || t == "summary" {
				return base, sfx[1:]
			}
		case "_created":
			if t == "histogram" || t == "summary" || t == "counter" {
				return base, "created"
			}
		}
	}
	if types[name] == "summary" && hasKey(labels, "quantile") {
		return name, "quantile"
	}
	return name, "value"
}

func hasKey(m map[string]string, k string) bool { _, ok := m[k]; return ok }

func canonical(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte(0)
		b.WriteString(labels[k])
		b.WriteByte(1)
	}
	return b.String()
}

func parseFloat(s string) (float64, error) {
	switch s {
	case "+Inf", "Inf":
		return math.Inf(1), nil
	case "-Inf":
		return math.Inf(-1), nil
	}
	return strconv.ParseFloat(s, 64)
}

// parseSample splits "name{l="v",...} value [timestamp]".
func parseSample(line string) (name string, labels map[string]string, value float64, ok bool) {
	i := 0
	for i < len(line) && line[i] != '{' && line[i] != ' ' && line[i] != '\t' {
		i++
	}
	name = line[:i]
	if name == "" {
		return "", nil, 0, false
	}
	labels = map[string]string{}
	rest := line[i:]
	if strings.HasPrefix(rest, "{") {
		end, ok2 := parseLabels(rest, labels)
		if !ok2 {
			return "", nil, 0, false
		}
		rest = rest[end:]
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 || len(fields) > 2 {
		return "", nil, 0, false
	}
	v, err := parseFloat(fields[0])
	if err != nil {
		return "", nil, 0, false
	}
	return name, labels, v, true
}

// parseLabels parses "{a="b",c="d"}" from the start of s and returns the index after "}".
func parseLabels(s string, out map[string]string) (int, bool) {
	i := 1
	for {
		for i < len(s) && (s[i] == ' ' || s[i] == ',') {
			i++
		}
		if i >= len(s) {
			return 0, false
		}
		if s[i] == '}' {
			return i + 1, true
		}
		if i > maxLabelsBytes {
			return 0, false
		}
		j := i
		for j < len(s) && s[j] != '=' && s[j] != '}' {
			j++
		}
		if j+1 >= len(s) || s[j] != '=' || s[j+1] != '"' {
			return 0, false
		}
		lname := strings.TrimSpace(s[i:j])
		var val strings.Builder
		j += 2
		closed := false
		for j < len(s) {
			c := s[j]
			if c == '\\' && j+1 < len(s) {
				switch s[j+1] {
				case 'n':
					val.WriteByte('\n')
				default:
					val.WriteByte(s[j+1])
				}
				j += 2
				continue
			}
			if c == '"' {
				closed = true
				j++
				break
			}
			val.WriteByte(c)
			j++
		}
		if !closed || lname == "" {
			return 0, false
		}
		out[lname] = val.String()
		i = j
	}
}
