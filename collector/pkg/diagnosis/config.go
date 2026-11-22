// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package diagnosis

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// Threshold is a warning/critical pair. A value strictly greater than Warn is a
// warning, strictly greater than Crit is critical.
type Threshold struct {
	Warn float64 `json:"warn"`
	Crit float64 `json:"crit"`
}

// Config holds every number the rules use. Defaults are DefaultConfig; an
// operator can override any field with a JSON file (-flight-diagnosis-thresholds).
// Unknown fields are rejected so a typo cannot silently keep a default.
type Config struct {
	// WindowSeconds is how far back events and cgroup samples are considered.
	WindowSeconds int `json:"windowSeconds"`
	// MinSampleSpanSeconds is the least time between the first and last cgroup
	// sample before counter deltas (throttling, memory events) are trusted.
	MinSampleSpanSeconds int `json:"minSampleSpanSeconds"`
	// MinCPUPeriods is the least number of CFS periods in the window before the
	// throttle ratio is evaluated (a few periods say nothing).
	MinCPUPeriods int `json:"minCpuPeriods"`

	TCPRetransmits Threshold `json:"tcpRetransmits"` // retransmitted segments on closed connections in the window
	CNPRate        Threshold `json:"cnpRate"`        // RoCEv2 CNPs per second (node-level)
	PFCRate        Threshold `json:"pfcRate"`        // PFC pause frames per second (node-level)

	PipelineStallRatio Threshold `json:"pipelineStallRatio"` // sum of GPU-idle-before-launch gaps / window
	PipelineStallMin   int       `json:"pipelineStallMin"`   // least stall events before the ratio counts
	IOPressure         Threshold `json:"ioPressure"`         // io.pressure some avg10 (percent)
	GDSHitRatioLow     float64   `json:"gdsHitRatioLow"`     // direct-path ratio below this is a finding

	CPUThrottleRatio Threshold `json:"cpuThrottleRatio"` // nr_throttled / nr_periods (max over the job's cgroups)
	CPUPressure      Threshold `json:"cpuPressure"`      // cpu.pressure some avg10 (percent)

	MemUsageRatio Threshold `json:"memUsageRatio"`  // memory.current / memory.max (max over cgroups with a limit)
	MemPressure   Threshold `json:"memPressure"`    // memory.pressure some avg10 (percent)
	MemEvents     Threshold `json:"memHighMaxEvts"` // memory.events high+max increments in the window

	NCCLP99MS   Threshold `json:"ncclP99ms"` // p99 of straggler-span latency (ms)
	OverlapIdle Threshold `json:"overlapIdleRatio"`
	Straggler   Threshold `json:"stragglerHits"`
	RDMARetry   Threshold `json:"rdmaRetryRate"`
	Exfil       Threshold `json:"exfilEvents"`
}

// DefaultConfig returns the documented defaults.
func DefaultConfig() Config {
	return Config{
		WindowSeconds:        300,
		MinSampleSpanSeconds: 20,
		MinCPUPeriods:        20,

		TCPRetransmits: Threshold{10, 100},
		CNPRate:        Threshold{100, 1000},
		PFCRate:        Threshold{1000, 10000},

		PipelineStallRatio: Threshold{0.10, 0.30},
		PipelineStallMin:   5,
		IOPressure:         Threshold{10, 30},
		GDSHitRatioLow:     0.5,

		CPUThrottleRatio: Threshold{0.25, 0.50},
		CPUPressure:      Threshold{20, 50},

		MemUsageRatio: Threshold{0.90, 0.97},
		MemPressure:   Threshold{10, 30},
		MemEvents:     Threshold{0, 100},

		NCCLP99MS:   Threshold{50, 200},
		OverlapIdle: Threshold{0.30, 0.60},
		Straggler:   Threshold{10, 50},
		RDMARetry:   Threshold{0.02, 0.10},
		Exfil:       Threshold{0, 2},
	}
}

// Window returns the evaluation window.
func (c Config) Window() time.Duration { return time.Duration(c.WindowSeconds) * time.Second }

// MinSpan returns MinSampleSpanSeconds as a duration.
func (c Config) MinSpan() time.Duration { return time.Duration(c.MinSampleSpanSeconds) * time.Second }

// Validate rejects values that would make a rule meaningless.
func (c Config) Validate() error {
	if c.WindowSeconds < 30 || c.WindowSeconds > 24*3600 {
		return errors.New("windowSeconds must be between 30 and 86400")
	}
	if c.MinSampleSpanSeconds < 1 || c.MinSampleSpanSeconds > c.WindowSeconds {
		return errors.New("minSampleSpanSeconds must be between 1 and windowSeconds")
	}
	if c.MinCPUPeriods < 1 || c.PipelineStallMin < 1 {
		return errors.New("minCpuPeriods and pipelineStallMin must be at least 1")
	}
	if c.GDSHitRatioLow < 0 || c.GDSHitRatioLow > 1 {
		return errors.New("gdsHitRatioLow must be within [0,1]")
	}
	for name, t := range map[string]Threshold{
		"tcpRetransmits": c.TCPRetransmits, "cnpRate": c.CNPRate, "pfcRate": c.PFCRate,
		"pipelineStallRatio": c.PipelineStallRatio, "ioPressure": c.IOPressure,
		"cpuThrottleRatio": c.CPUThrottleRatio, "cpuPressure": c.CPUPressure,
		"memUsageRatio": c.MemUsageRatio, "memPressure": c.MemPressure, "memHighMaxEvts": c.MemEvents,
		"ncclP99ms": c.NCCLP99MS, "overlapIdleRatio": c.OverlapIdle, "stragglerHits": c.Straggler,
		"rdmaRetryRate": c.RDMARetry, "exfilEvents": c.Exfil,
	} {
		if t.Warn < 0 || t.Crit < t.Warn {
			return fmt.Errorf("%s: need 0 <= warn <= crit", name)
		}
	}
	return nil
}

// LoadConfig reads a JSON override file over the defaults ("" returns the defaults).
func LoadConfig(path string) (Config, error) {
	c := DefaultConfig()
	if path == "" {
		return c, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err != nil {
		return c, err
	}
	if len(raw) > 1<<20 {
		return c, errors.New("thresholds file exceeds 1 MiB")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return DefaultConfig(), fmt.Errorf("thresholds file: %w", err)
	}
	if err := c.Validate(); err != nil {
		return DefaultConfig(), err
	}
	return c, nil
}
