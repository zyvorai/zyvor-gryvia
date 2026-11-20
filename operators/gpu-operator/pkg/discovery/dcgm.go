// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package discovery

import (
	"math"
	"sort"
	"strconv"
	"strings"
)

// GPUMetric is one GPU's readings from the DCGM exporter.
type GPUMetric struct {
	Index       int
	UUID        string
	Temperature int // Celsius
	Utilization int // percent
	PowerUsage  int // Watts
	MemoryUsed  int // MiB
	MemoryFree  int // MiB
	// HasTemperature is false when the exporter did not report a temperature.
	HasTemperature bool
}

// MemoryTotal returns used+free in MiB.
func (m GPUMetric) MemoryTotal() int { return m.MemoryUsed + m.MemoryFree }

// ParseDCGM parses the DCGM exporter Prometheus text format, grouping the
// samples per GPU index. Comments, blank lines, unknown metrics and
// malformed lines are ignored. The result is sorted by index.
func ParseDCGM(text string) []GPUMetric {
	byIdx := map[int]*GPUMetric{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, labels, value, ok := parseSample(line)
		if !ok {
			continue
		}
		switch name {
		case "DCGM_FI_DEV_GPU_TEMP", "DCGM_FI_DEV_GPU_UTIL", "DCGM_FI_DEV_FB_USED",
			"DCGM_FI_DEV_FB_FREE", "DCGM_FI_DEV_POWER_USAGE":
		default:
			continue
		}
		idx, err := strconv.Atoi(labels["gpu"])
		if err != nil || idx < 0 {
			continue
		}
		m := byIdx[idx]
		if m == nil {
			m = &GPUMetric{Index: idx}
			byIdx[idx] = m
		}
		if u := labels["UUID"]; u != "" {
			m.UUID = u
		}
		v := int(math.Round(value))
		switch name {
		case "DCGM_FI_DEV_GPU_TEMP":
			m.Temperature, m.HasTemperature = v, true
		case "DCGM_FI_DEV_GPU_UTIL":
			m.Utilization = v
		case "DCGM_FI_DEV_FB_USED":
			m.MemoryUsed = v
		case "DCGM_FI_DEV_FB_FREE":
			m.MemoryFree = v
		case "DCGM_FI_DEV_POWER_USAGE":
			m.PowerUsage = v
		}
	}
	out := make([]GPUMetric, 0, len(byIdx))
	for _, m := range byIdx {
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out
}

// parseSample parses `name{a="b",c="d"} value [timestamp]`.
func parseSample(line string) (name string, labels map[string]string, value float64, ok bool) {
	labels = map[string]string{}
	rest := line
	if i := strings.IndexByte(line, '{'); i >= 0 {
		j := strings.LastIndexByte(line, '}')
		if j < i {
			return "", nil, 0, false
		}
		name = strings.TrimSpace(line[:i])
		if !parseLabels(line[i+1:j], labels) {
			return "", nil, 0, false
		}
		rest = line[j+1:]
	} else {
		f := strings.Fields(line)
		if len(f) < 2 {
			return "", nil, 0, false
		}
		name = f[0]
		rest = strings.Join(f[1:], " ")
	}
	f := strings.Fields(rest)
	if name == "" || len(f) == 0 {
		return "", nil, 0, false
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return "", nil, 0, false
	}
	return name, labels, v, true
}

// parseLabels parses `a="b",c="d"` tolerating commas inside quoted values.
func parseLabels(s string, out map[string]string) bool {
	for len(s) > 0 {
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			return false
		}
		key := strings.TrimSpace(strings.TrimLeft(s[:eq], ", "))
		s = s[eq+1:]
		if len(s) == 0 || s[0] != '"' {
			return false
		}
		s = s[1:]
		var val strings.Builder
		closed := false
		for i := 0; i < len(s); i++ {
			if s[i] == '\\' && i+1 < len(s) {
				i++
				val.WriteByte(s[i])
				continue
			}
			if s[i] == '"' {
				s = s[i+1:]
				closed = true
				break
			}
			val.WriteByte(s[i])
		}
		if !closed {
			return false
		}
		if key != "" {
			out[key] = val.String()
		}
	}
	return true
}
