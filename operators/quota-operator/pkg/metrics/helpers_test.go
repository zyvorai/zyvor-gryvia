// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func gather(c prometheus.Collector) ([]*dto.MetricFamily, error) {
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(c); err != nil {
		return nil, err
	}
	return reg.Gather()
}

// errGauge returns a gauge holding the list-errors value from one scrape of c.
func errGauge(c *Collector) prometheus.Gauge {
	g := prometheus.NewGauge(prometheus.GaugeOpts{Name: "e"})
	mfs, _ := gather(c)
	for _, mf := range mfs {
		if mf.GetName() == "gryvia_quota_metrics_list_errors" {
			g.Set(mf.GetMetric()[0].GetGauge().GetValue())
		}
	}
	return g
}
