// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package cron

import (
	"testing"
	"time"
)

func ts(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04", s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestParseErrors(t *testing.T) {
	for _, e := range []string{"", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * * 13 *",
		"* * * * 8", "*/0 * * * *", "a * * * *", "5-1 * * * *", "@daily", "* * * JAN *", "1,,2 * * * *", "* * * * MON"} {
		if _, err := Parse(e); err == nil {
			t.Errorf("Parse(%q) should fail", e)
		}
	}
}

func TestNext(t *testing.T) {
	cases := []struct{ expr, from, want string }{
		{"* * * * *", "2026-09-15 12:00", "2026-09-15 12:01"},
		{"0 9 * * *", "2026-09-15 12:00", "2026-09-16 09:00"},
		{"0 9 * * *", "2026-09-15 08:59", "2026-09-15 09:00"},
		{"*/15 * * * *", "2026-09-15 12:14", "2026-09-15 12:15"},
		{"0 22 * * 1-5", "2026-09-18 23:00", "2026-09-21 22:00"}, // Fri night -> Monday
		{"30 2 1 * *", "2026-09-15 00:00", "2026-10-01 02:30"},
		{"0 0 * * 7", "2026-09-15 00:00", "2026-09-20 00:00"}, // 7 = Sunday
		{"0 0 * * 0", "2026-09-15 00:00", "2026-09-20 00:00"},
		{"0 0 1,15 * *", "2026-09-02 00:00", "2026-09-15 00:00"},
		{"0 0 29 2 *", "2026-09-15 00:00", "2028-02-29 00:00"},
		{"10-20/5 8 * * *", "2026-09-15 08:12", "2026-09-15 08:15"},
		{"5/20 * * * *", "2026-09-15 08:06", "2026-09-15 08:25"},
	}
	for _, c := range cases {
		s, err := Parse(c.expr)
		if err != nil {
			t.Fatalf("%s: %v", c.expr, err)
		}
		got, ok := s.Next(ts(c.from))
		if !ok || !got.Equal(ts(c.want)) {
			t.Errorf("%s from %s: %v (%v), want %s", c.expr, c.from, got, ok, c.want)
		}
	}
}

func TestDomDowOr(t *testing.T) {
	// Both restricted: fires on the 13th OR on Fridays (classic cron).
	s, _ := Parse("0 0 13 * 5")
	if !s.Matches(ts("2026-09-13 00:00")) { // Sunday the 13th
		t.Error("13th must match")
	}
	if !s.Matches(ts("2026-09-18 00:00")) { // a Friday
		t.Error("Friday must match")
	}
	if s.Matches(ts("2026-09-16 00:00")) {
		t.Error("Wednesday the 16th must not match")
	}
}

func TestNextImpossible(t *testing.T) {
	s, _ := Parse("0 0 31 2 *")
	if _, ok := s.Next(ts("2026-01-01 00:00")); ok {
		t.Error("Feb 31 never fires")
	}
}

func TestPrev(t *testing.T) {
	s, _ := Parse("0 9 * * *")
	got, ok := s.Prev(ts("2026-09-15 12:34"), 24*time.Hour)
	if !ok || !got.Equal(ts("2026-09-15 09:00")) {
		t.Errorf("prev = %v %v", got, ok)
	}
	if _, ok := s.Prev(ts("2026-09-15 12:34"), time.Hour); ok {
		t.Error("nothing within the last hour")
	}
	got, ok = s.Prev(ts("2026-09-15 09:00"), time.Minute)
	if !ok || !got.Equal(ts("2026-09-15 09:00")) {
		t.Errorf("prev at the fire minute = %v %v", got, ok)
	}
}
