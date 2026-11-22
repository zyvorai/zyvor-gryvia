// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package llmgateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var usageRecordGVK = schema.GroupVersionKind{Group: "gryvia.io", Version: "v1alpha1", Kind: "GryviaUsageRecord"}

type bucketKey struct {
	tenant, namespace, model string
	hour                     int64
}

type bucket struct {
	in, out int64
	cost    float64
}

// LabelInstance names the gateway process that wrote a token record.
const LabelInstance = "gryvia.io/llm-gateway-instance"

// Meter counts tokens per tenant and model: Prometheus counters, and hourly buckets that Flush writes as
// GryviaUsageRecords of kind tokens. Each gateway process writes its own records (its instance id is part of the
// record name), so replicas and restarted containers never overwrite each other's counts.
type Meter struct {
	mu       sync.Mutex
	buckets  map[bucketKey]*bucket
	instance string
	currency string

	tokens   *prometheus.CounterVec
	requests *prometheus.CounterVec
	rejected *prometheus.CounterVec
}

// Instance is a process id for LabelInstance: the pod name and start time, hashed to fit a label value.
func Instance(pod string, start time.Time) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d", pod, start.UnixNano())))
	return hex.EncodeToString(sum[:])[:16]
}

func NewMeter(reg prometheus.Registerer, instance string) *Meter {
	m := &Meter{
		buckets:  map[bucketKey]*bucket{},
		instance: instance,
		currency: "USD",
		tokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gryvia_llm_tokens_total", Help: "Tokens served by the LLM gateway (type input or output)",
		}, []string{"tenant", "model", "type"}),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gryvia_llm_requests_total", Help: "LLM gateway requests by HTTP status code",
		}, []string{"tenant", "model", "code"}),
		rejected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gryvia_llm_quota_rejections_total", Help: "Requests refused with 429 because tokensPerDay was used up",
		}, []string{"tenant"}),
	}
	reg.MustRegister(m.tokens, m.requests, m.rejected)
	return m
}

// Request counts a finished request.
func (m *Meter) Request(tenant, model string, code int) {
	m.requests.WithLabelValues(tenant, model, fmt.Sprint(code)).Inc()
}

// Rejected counts a 429.
func (m *Meter) Rejected(tenant string) { m.rejected.WithLabelValues(tenant).Inc() }

// Record adds the tokens of one response.
func (m *Meter) Record(k Key, r Route, in, out int64, now time.Time) {
	if in <= 0 && out <= 0 {
		return
	}
	m.tokens.WithLabelValues(k.Tenant, r.Model, "input").Add(float64(max(in, 0)))
	m.tokens.WithLabelValues(k.Tenant, r.Model, "output").Add(float64(max(out, 0)))
	key := bucketKey{tenant: k.Tenant, namespace: k.Namespace, model: r.Model, hour: now.UTC().Truncate(time.Hour).Unix()}
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.buckets[key]
	if b == nil {
		b = &bucket{}
		m.buckets[key] = b
	}
	b.in += max(in, 0)
	b.out += max(out, 0)
	b.cost += float64(max(in, 0))*r.PriceIn/1e6 + float64(max(out, 0))*r.PriceOut/1e6
}

func (m *Meter) recordName(k bucketKey) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%s|%d", m.instance, k.tenant, k.namespace, k.model, k.hour)))
	return "tokens-" + hex.EncodeToString(sum[:])[:20]
}

// Flush writes every bucket as a usage record (created, or updated while it is open) and drops the buckets of
// finished hours once their record is final. It returns the first error and keeps the failed buckets.
func (m *Meter) Flush(ctx context.Context, c client.Client, now time.Time) error {
	m.mu.Lock()
	snapshot := make(map[bucketKey]bucket, len(m.buckets))
	for k, b := range m.buckets {
		snapshot[k] = *b
	}
	m.mu.Unlock()

	var first error
	for k, b := range snapshot {
		final := !now.UTC().Before(time.Unix(k.hour, 0).Add(time.Hour))
		if err := m.write(ctx, c, k, b, final); err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		if final {
			m.mu.Lock()
			if cur := m.buckets[k]; cur != nil && *cur == b {
				delete(m.buckets, k)
			}
			m.mu.Unlock()
		}
	}
	return first
}

func (m *Meter) write(ctx context.Context, c client.Client, k bucketKey, b bucket, final bool) error {
	name := m.recordName(k)
	start := time.Unix(k.hour, 0).UTC()
	spec := map[string]interface{}{
		"kind": "tokens", "tenant": k.tenant, "model": k.model, "job": "llm:" + k.model, "jobUID": name,
		"gpus": int64(0), "gpuHours": float64(0), "rate": float64(0), "cost": b.cost, "currency": m.currency,
		"inputTokens": b.in, "outputTokens": b.out, "final": final,
		"start": start.Format(time.RFC3339), "end": start.Add(time.Hour).Format(time.RFC3339),
	}
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(usageRecordGVK)
	err := c.Get(ctx, types.NamespacedName{Namespace: k.namespace, Name: name}, existing)
	if errors.IsNotFound(err) {
		obj := &unstructured.Unstructured{Object: map[string]interface{}{"spec": spec}}
		obj.SetGroupVersionKind(usageRecordGVK)
		obj.SetNamespace(k.namespace)
		obj.SetName(name)
		obj.SetLabels(map[string]string{LabelTenant: labelValue(k.tenant), LabelModel: labelValue(k.model),
			"gryvia.io/usage-kind": "tokens", LabelInstance: m.instance})
		return c.Create(ctx, obj)
	}
	if err != nil {
		return err
	}
	if done, _, _ := unstructured.NestedBool(existing.Object, "spec", "final"); done {
		return nil
	}
	existing.Object["spec"] = spec
	return c.Update(ctx, existing)
}

// labelValue keeps a value a valid label (names that pass modelNameRE and tenant names already are).
func labelValue(v string) string {
	if len(v) > 63 {
		v = v[:63]
	}
	return v
}
