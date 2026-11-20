// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Package cron is a small parser for standard five-field cron expressions
// ("minute hour day-of-month month day-of-week"), evaluated in UTC. It exists so the
// reservation and workflow controllers can honour cron schedules without a dependency. The quota-operator
// keeps an identical copy (pkg/cron); the operators are separate Go modules.
//
// Supported per field: "*", a number, "a-b", "*/n", "a-b/n", "a/n" and comma lists of those.
// Day of week is 0-7 (0 and 7 are Sunday). As in classic cron, when both day-of-month and
// day-of-week are restricted a day matches if either does. Names (JAN, MON) and macros
// (@daily) are not supported and are rejected.
package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed expression.
type Schedule struct {
	minute, hour, dom, month, dow uint64
	domStar, dowStar              bool
}

type bounds struct{ min, max int }

var fieldBounds = [5]bounds{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 7}}
var fieldNames = [5]string{"minute", "hour", "day-of-month", "month", "day-of-week"}

// Parse parses a five-field expression.
func Parse(expr string) (*Schedule, error) {
	f := strings.Fields(expr)
	if len(f) != 5 {
		return nil, fmt.Errorf("cron %q: want 5 fields (minute hour day-of-month month day-of-week), got %d", expr, len(f))
	}
	var bits [5]uint64
	for i := range f {
		b, err := parseField(f[i], fieldBounds[i])
		if err != nil {
			return nil, fmt.Errorf("cron %q: %s: %w", expr, fieldNames[i], err)
		}
		bits[i] = b
	}
	// Sunday may be written 7.
	if bits[4]&(1<<7) != 0 {
		bits[4] |= 1
		bits[4] &^= 1 << 7
	}
	return &Schedule{
		minute: bits[0], hour: bits[1], dom: bits[2], month: bits[3], dow: bits[4],
		domStar: strings.HasPrefix(f[2], "*"), dowStar: strings.HasPrefix(f[4], "*"),
	}, nil
}

func parseField(s string, b bounds) (uint64, error) {
	var out uint64
	for _, part := range strings.Split(s, ",") {
		if part == "" {
			return 0, fmt.Errorf("empty list element")
		}
		rng, step := part, 1
		if i := strings.Index(part, "/"); i >= 0 {
			n, err := strconv.Atoi(part[i+1:])
			if err != nil || n < 1 {
				return 0, fmt.Errorf("bad step in %q", part)
			}
			rng, step = part[:i], n
		}
		lo, hi := b.min, b.max
		switch {
		case rng == "*":
		case strings.Contains(rng, "-"):
			ab := strings.SplitN(rng, "-", 2)
			var err1, err2 error
			lo, err1 = strconv.Atoi(ab[0])
			hi, err2 = strconv.Atoi(ab[1])
			if err1 != nil || err2 != nil {
				return 0, fmt.Errorf("bad range %q", rng)
			}
		default:
			n, err := strconv.Atoi(rng)
			if err != nil {
				return 0, fmt.Errorf("bad value %q (names and macros are not supported)", rng)
			}
			lo, hi = n, n
			if strings.Contains(part, "/") {
				hi = b.max // "a/n" means a-max/n
			}
		}
		if lo < b.min || hi > b.max || lo > hi {
			return 0, fmt.Errorf("%q out of range %d-%d", part, b.min, b.max)
		}
		for v := lo; v <= hi; v += step {
			out |= 1 << uint(v)
		}
	}
	return out, nil
}

// Matches reports whether the schedule fires in the minute containing t (UTC).
func (s *Schedule) Matches(t time.Time) bool {
	t = t.UTC()
	if s.minute&(1<<uint(t.Minute())) == 0 || s.hour&(1<<uint(t.Hour())) == 0 || s.month&(1<<uint(t.Month())) == 0 {
		return false
	}
	return s.dayMatches(t)
}

func (s *Schedule) dayMatches(t time.Time) bool {
	domOK := s.dom&(1<<uint(t.Day())) != 0
	dowOK := s.dow&(1<<uint(t.Weekday())) != 0
	switch {
	case s.domStar && s.dowStar:
		return true
	case s.domStar:
		return dowOK
	case s.dowStar:
		return domOK
	default:
		return domOK || dowOK
	}
}

// Next returns the first fire time strictly after t, or false when there is none within
// five years (an expression such as "0 0 31 2 *").
func (s *Schedule) Next(t time.Time) (time.Time, bool) {
	t = t.UTC().Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(5, 0, 0)
	for t.Before(limit) {
		if s.month&(1<<uint(t.Month())) == 0 {
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, time.UTC)
			continue
		}
		if !s.dayMatches(t) {
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, time.UTC)
			continue
		}
		if s.hour&(1<<uint(t.Hour())) == 0 {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, time.UTC)
			continue
		}
		if s.minute&(1<<uint(t.Minute())) == 0 {
			t = t.Add(time.Minute)
			continue
		}
		return t, true
	}
	return time.Time{}, false
}

// Prev returns the last fire time at or before t looking back at most within, or false.
func (s *Schedule) Prev(t time.Time, within time.Duration) (time.Time, bool) {
	t = t.UTC().Truncate(time.Minute)
	stop := t.Add(-within)
	for ; !t.Before(stop); t = t.Add(-time.Minute) {
		if s.Matches(t) {
			return t, true
		}
	}
	return time.Time{}, false
}
