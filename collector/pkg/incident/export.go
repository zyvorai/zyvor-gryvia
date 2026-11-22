// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package incident

import (
	"encoding/csv"
	"encoding/json"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

// CSVHeader is the column order of ExportCSV.
var CSVHeader = []string{"id", "namespace", "job", "node", "kind", "severity", "confidence", "start", "end", "open", "duration_seconds", "close_reason", "summary", "peak_metrics", "unavailable"}

// SafeCell neutralises spreadsheet formula injection and control characters in a
// text cell: a leading = + - @ tab or CR gets a single quote prefix (OWASP CSV
// injection guidance), other control characters become spaces. encoding/csv then
// handles quoting of commas, quotes and newlines. Numeric columns never pass
// through SafeCell, so negative numbers are unaffected.
func SafeCell(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if s != "" {
		switch s[0] {
		case '=', '+', '-', '@', '\t', '\r':
			return "'" + s
		}
	}
	return s
}

func tfmt(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// ExportCSV writes incidents as CSV with SafeCell applied to every text column.
func ExportCSV(w io.Writer, incs []Incident) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(CSVHeader); err != nil {
		return err
	}
	for _, in := range incs {
		end := in.End
		dur := ""
		if !in.Start.IsZero() {
			e := end
			if e.IsZero() {
				e = in.UpdatedAt
			}
			dur = strconv.FormatFloat(e.Sub(in.Start).Seconds(), 'f', 0, 64)
		}
		keys := make([]string, 0, len(in.Peak))
		for k := range in.Peak {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		ordered := make([]map[string]float64, 0, len(keys))
		for _, k := range keys {
			ordered = append(ordered, map[string]float64{k: in.Peak[k]})
		}
		peak, _ := json.Marshal(ordered)
		unav, _ := json.Marshal(in.Unavailable)
		row := []string{
			SafeCell(in.ID), SafeCell(in.Namespace), SafeCell(in.Job), SafeCell(in.Node), SafeCell(in.Kind),
			SafeCell(in.Severity), SafeCell(in.Confidence), tfmt(in.Start), tfmt(end), strconv.FormatBool(in.Open), dur,
			SafeCell(in.CloseReason), SafeCell(in.Summary), SafeCell(string(peak)), SafeCell(string(unav)),
		}
		if err := cw.Write(row); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}
