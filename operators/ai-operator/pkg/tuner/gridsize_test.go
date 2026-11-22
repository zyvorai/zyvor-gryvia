// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package tuner

import (
	"testing"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

func TestGridSize(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	cat := gryviav1.ParameterSpec{Name: "o", Type: gryviav1.ParameterTypeCategorical, Values: []string{"a", "b", "c"}}
	ints := gryviav1.ParameterSpec{Name: "n", Type: gryviav1.ParameterTypeInt, Min: f(1), Max: f(4)}
	flt := gryviav1.ParameterSpec{Name: "lr", Type: gryviav1.ParameterTypeFloat, Min: f(0.1), Max: f(1)}
	huge := gryviav1.ParameterSpec{Name: "h", Type: gryviav1.ParameterTypeInt, Min: f(0), Max: f(2e9)}

	for name, tc := range map[string]struct {
		space []gryviav1.ParameterSpec
		want  int
	}{
		"categorical":    {[]gryviav1.ParameterSpec{cat}, 3},
		"product":        {[]gryviav1.ParameterSpec{cat, ints}, 12},
		"float is 11":    {[]gryviav1.ParameterSpec{flt}, 11},
		"saturates":      {[]gryviav1.ParameterSpec{huge, huge}, 101},
		"huge int alone": {[]gryviav1.ParameterSpec{huge}, 101},
		"empty param":    {[]gryviav1.ParameterSpec{{Name: "x", Type: gryviav1.ParameterTypeCategorical}}, 0},
	} {
		if got := GridSize(tc.space, 100); got != tc.want {
			t.Errorf("%s: GridSize = %d, want %d", name, got, tc.want)
		}
	}
}
