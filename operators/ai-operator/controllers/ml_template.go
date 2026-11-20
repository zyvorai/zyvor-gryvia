// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controllers

import (
	"encoding/json"
	"regexp"
)

var placeholderRE = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_.\-]+)\s*\}\}`)

// renderString replaces each {{key}} that lookup knows; unknown placeholders are kept, so a value can be rendered
// in stages (the model watch fills {{model.*}}, the workflow later fills {{steps.*}}).
func renderString(s string, lookup func(string) (string, bool)) string {
	return placeholderRE.ReplaceAllStringFunc(s, func(m string) string {
		if v, ok := lookup(placeholderRE.FindStringSubmatch(m)[1]); ok {
			return v
		}
		return m
	})
}

// renderInto renders every string value (not map keys) of in, which must be JSON-serialisable, and decodes the
// result into out. Values are substituted after decoding, so they cannot break the JSON structure.
func renderInto(in, out interface{}, lookup func(string) (string, bool)) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	var v interface{}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	b, err = json.Marshal(renderValue(v, lookup))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func renderValue(v interface{}, lookup func(string) (string, bool)) interface{} {
	switch t := v.(type) {
	case string:
		return renderString(t, lookup)
	case []interface{}:
		for i := range t {
			t[i] = renderValue(t[i], lookup)
		}
	case map[string]interface{}:
		for k := range t {
			t[k] = renderValue(t[k], lookup)
		}
	}
	return v
}

// mapLookup is a lookup over a fixed map.
func mapLookup(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}
