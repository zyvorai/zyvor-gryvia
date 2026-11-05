package v1

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Workload kinds for GryviaAIJobSpec.WorkloadKind.
const (
	WorkloadKindJob         = "job"
	WorkloadKindStatefulSet = "statefulset"
)

// TimeoutSeconds parses spec.timeout ("90m", "24h", "7d", "1d12h") into seconds.
// An empty timeout returns nil (no deadline). time.ParseDuration has no day unit,
// so a leading "<n>d" is accepted in addition to the Go duration syntax.
func (s GryviaAIJobSpec) TimeoutSeconds() (*int64, error) {
	return parseDurationSeconds(s.Timeout, "timeout")
}

// IdleTimeoutSeconds parses spec.idleTimeout the same way as TimeoutSeconds.
func (s GryviaAIJobSpec) IdleTimeoutSeconds() (*int64, error) {
	return parseDurationSeconds(s.IdleTimeout, "idleTimeout")
}

func parseDurationSeconds(raw, field string) (*int64, error) {
	t := strings.TrimSpace(raw)
	if t == "" {
		return nil, nil
	}
	var total time.Duration
	if i := strings.Index(t, "d"); i > 0 {
		days, err := strconv.Atoi(t[:i])
		if err != nil || days < 0 {
			return nil, fmt.Errorf("invalid %s %q: day count must be a non-negative integer", field, raw)
		}
		total += time.Duration(days) * 24 * time.Hour
		t = t[i+1:]
	}
	if t != "" {
		d, err := time.ParseDuration(t)
		if err != nil {
			return nil, fmt.Errorf("invalid %s %q: %w", field, raw, err)
		}
		total += d
	}
	secs := int64(total / time.Second)
	if secs <= 0 {
		return nil, fmt.Errorf("invalid %s %q: must be positive", field, raw)
	}
	return &secs, nil
}
