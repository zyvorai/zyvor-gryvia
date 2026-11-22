// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gryviav1 "github.com/zyvorai/gryvia/operators/network-intelligence/api/v1"
	"github.com/zyvorai/gryvia/operators/network-intelligence/pkg/sources"
)

const maxTopTalkers = 10

// GryviaTrafficInsightReconciler reconciles a GryviaTrafficInsight object.
//
// Everything is derived from the merged collector graph and anomaly list:
//   - topTalkers: sources of edges INTO the service, by cumulative collector bytes;
//   - p50Latency: byte-flow-weighted mean of the inbound edges' smoothed latency (a moving average, not a percentile);
//   - throughputBps: growth of the inbound edges' byte counters between two reconciles (unset on the first one);
//   - anomalies: collector anomalies of that service in the last hour.
//
// p99Latency and dropCount have no source in the collector and stay unset.
type GryviaTrafficInsightReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Collector sources.Collector

	mu   sync.Mutex
	prev map[string]counterSample
	now  func() time.Time
}

type counterSample struct {
	at    time.Time
	bytes map[string]uint64 // by edge key
}

//+kubebuilder:rbac:groups=gryvia.io,resources=gryviatrafficinsights,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviatrafficinsights/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviatrafficinsights/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *GryviaTrafficInsightReconciler) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

func (r *GryviaTrafficInsightReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	insight := &gryviav1.GryviaTrafficInsight{}
	if err := r.Get(ctx, req.NamespacedName, insight); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	requeue := 30 * time.Second
	if insight.Spec.Window != "" {
		if parsed, err := time.ParseDuration(insight.Spec.Window); err == nil && parsed > 0 {
			requeue = parsed
		}
	}
	targetNs := insight.Spec.Namespace
	if targetNs == "" {
		targetNs = insight.Namespace
	}

	var g sources.Graph
	var stats sources.Stats
	var err error
	if r.Collector == nil {
		err = errNoCollector
	} else {
		g, stats, err = r.Collector.Graph(ctx)
	}

	var talkers []gryviav1.TopTalker
	var p50 string
	var bps int64
	var anomalies []gryviav1.TrafficAnomaly
	var anomalyErr error
	if err == nil {
		talkers, p50, bps = r.analyse(req.NamespacedName.String(), g, targetNs, insight.Spec.Service)
		var as []sources.Anomaly
		as, _, anomalyErr = r.Collector.Anomalies(ctx)
		if anomalyErr == nil {
			anomalies = trafficAnomalies(as, insight.Spec.Service, r.clock())
		}
	}

	uerr := updateStatus(ctx, r.Client, req.NamespacedName, func() *gryviav1.GryviaTrafficInsight { return &gryviav1.GryviaTrafficInsight{} },
		func(t *gryviav1.GryviaTrafficInsight) {
			if err == nil {
				t.Status.TopTalkers = talkers
				t.Status.P50Latency = p50
				if bps > 0 {
					t.Status.ThroughputBps = bps
				}
				if anomalyErr == nil {
					t.Status.Anomalies = anomalies
				}
				t.Status.LastUpdated = metav1.Now()
			}
			note := "bytes are collector counters of live edges; p99Latency and dropCount have no source and stay unset"
			if anomalyErr != nil {
				note += "; anomalies unavailable: " + anomalyErr.Error()
			}
			setSource(&t.Status.Conditions, t.Generation, err, stats, note)
		})
	if uerr != nil && !errors.IsNotFound(uerr) {
		return ctrl.Result{}, uerr
	}
	logger.Info("GryviaTrafficInsight updated", "topTalkers", len(talkers), "anomalies", len(anomalies), "collectorError", err != nil)
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// analyse aggregates the inbound edges of service. key identifies the insight for the byte-rate memory.
func (r *GryviaTrafficInsightReconciler) analyse(key string, g sources.Graph, ns, service string) ([]gryviav1.TopTalker, string, int64) {
	type acc struct {
		bytes       uint64
		latW, latWt float64
	}
	talkers := map[string]*acc{}
	cur := map[string]uint64{}
	var latW, latWt float64
	for _, e := range g.Edges {
		if !endpointMatches(e.Target, ns, service) {
			continue
		}
		a := talkers[e.Source]
		if a == nil {
			a = &acc{}
			talkers[e.Source] = a
		}
		a.bytes += e.BytesTotal
		if e.LatencyMs > 0 {
			w := float64(e.FlowCount)
			if w == 0 {
				w = 1
			}
			a.latW += e.LatencyMs * w
			a.latWt += w
			latW += e.LatencyMs * w
			latWt += w
		}
		cur[fmt.Sprintf("%s|%s|%d", e.Source, e.Protocol, e.Port)] += e.BytesTotal
	}
	out := make([]gryviav1.TopTalker, 0, len(talkers))
	for name, a := range talkers {
		t := gryviav1.TopTalker{Service: name, Bytes: satInt64(a.bytes)}
		if a.latWt > 0 {
			t.Latency = formatMs(a.latW / a.latWt)
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Bytes != out[j].Bytes {
			return out[i].Bytes > out[j].Bytes
		}
		return out[i].Service < out[j].Service
	})
	if len(out) > maxTopTalkers {
		out = out[:maxTopTalkers]
	}
	p50 := ""
	if latWt > 0 {
		p50 = formatMs(latW / latWt)
	}

	now := r.clock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.prev == nil {
		r.prev = map[string]counterSample{}
	}
	var bps int64
	if p, ok := r.prev[key]; ok && now.After(p.at) {
		var delta uint64
		for k, v := range cur {
			if old, seen := p.bytes[k]; seen && v >= old { // a smaller counter means the edge restarted: skip it
				delta += v - old
			}
		}
		bps = int64(float64(delta) / now.Sub(p.at).Seconds())
	}
	r.prev[key] = counterSample{at: now, bytes: cur}
	return out, p50, bps
}

// trafficAnomalies keeps the collector anomalies of service from the last hour.
func trafficAnomalies(as []sources.Anomaly, service string, now time.Time) []gryviav1.TrafficAnomaly {
	var out []gryviav1.TrafficAnomaly
	for _, a := range as {
		if a.Service != service || now.Sub(a.DetectedAt) > time.Hour {
			continue
		}
		out = append(out, gryviav1.TrafficAnomaly{Type: a.Type, Severity: anomalySeverity(a), Description: a.Message,
			Detected: metav1.NewTime(a.DetectedAt)})
	}
	return out
}

// SetupWithManager sets up the controller with the Manager
func (r *GryviaTrafficInsightReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gryviav1.GryviaTrafficInsight{}).
		Complete(r)
}
