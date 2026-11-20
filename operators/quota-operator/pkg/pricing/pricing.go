// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Package pricing holds the single default GPU price table used when no
// GryviaGpuSku objects exist in the cluster. Once a provider publishes SKUs the
// catalog is the source of truth and this table is ignored.
package pricing

import "strings"

// DefaultCurrency is the currency of the default table.
const DefaultCurrency = "USD"

// DefaultRates are the fallback hourly rates per single GPU, in DefaultCurrency.
// The "default" key applies to unknown GPU types.
var DefaultRates = map[string]float64{
	"H100":     8.00,
	"A100-80G": 4.00,
	"A100-40G": 3.50,
	"L40":      2.50,
	"V100":     2.00,
	"T4":       1.00,
	"default":  1.00,
}

// DefaultRate returns the fallback hourly rate per GPU for a GPU type. Matching
// is case-insensitive; unknown or empty types get the "default" rate.
func DefaultRate(gpuType string) float64 {
	for k, v := range DefaultRates {
		if k != "default" && strings.EqualFold(k, gpuType) {
			return v
		}
	}
	return DefaultRates["default"]
}
