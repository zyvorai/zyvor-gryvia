// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package llmgateway

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	quotaListGVK = schema.GroupVersionKind{Group: "gryvia.io", Version: "v1alpha1", Kind: "GryviaQuotaList"}
	usageListGVK = schema.GroupVersionKind{Group: "gryvia.io", Version: "v1alpha1", Kind: "GryviaUsageRecordList"}
)

const (
	// LabelTokenCounter marks the ConfigMaps that hold the shared daily token counts.
	LabelTokenCounter = "gryvia.io/llm-token-counter"
	counterPrefix     = "gryvia-llm-tokens-"
	counterDayLayout  = "2006-01-02"
)

// ErrQuotaNotSynced is returned by Check until the shared token counter has been read once.
var ErrQuotaNotSynced = errors.New("the shared token counter has not been read yet")

type quotaLimit struct {
	name       string
	namespaces []string
	perDay     int64
}

// Quotas enforces GryviaQuota.spec.tokensPerDay over the namespaces a quota lists. The tokens of the current UTC day
// are kept in a ConfigMap shared by every gateway replica (one per day, in the gateway's namespace, data: namespace ->
// tokens). Each replica counts its finished requests in memory and, on every Sync, adds them to the ConfigMap with an
// optimistic-concurrency update and reads back the totals. A check uses those totals plus what this replica counted
// since, so another replica's traffic is seen within about two sync intervals. The first replica to create a day's
// ConfigMap seeds it from that day's token records, which covers traffic from before the counter existed.
type Quotas struct {
	client    client.Client
	namespace string

	syncMu sync.Mutex // one Sync at a time, so pending tokens are flushed once

	mu      sync.Mutex
	limits  []quotaLimit
	day     time.Time
	shared  map[string]int64 // the ConfigMap's counts as of the last sync of day
	pending map[string]int64 // counted here and not yet added to the ConfigMap
	synced  bool             // a sync has succeeded since the process started
}

// NewQuotas reads quotas and records with c and keeps the shared counter in namespace.
func NewQuotas(c client.Client, namespace string) *Quotas {
	return &Quotas{client: c, namespace: namespace, shared: map[string]int64{}, pending: map[string]int64{}}
}

// Refresh reloads the quotas that set tokensPerDay.
func (q *Quotas) Refresh(ctx context.Context) error {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(quotaListGVK)
	if err := q.client.List(ctx, list); err != nil {
		return err
	}
	var limits []quotaLimit
	for _, item := range list.Items {
		per, found, _ := unstructured.NestedInt64(item.Object, "spec", "tokensPerDay")
		if !found || per <= 0 {
			continue
		}
		nss, _, _ := unstructured.NestedStringSlice(item.Object, "spec", "namespaces")
		limits = append(limits, quotaLimit{name: item.GetName(), namespaces: nss, perDay: per})
	}
	q.mu.Lock()
	q.limits = limits
	q.mu.Unlock()
	return nil
}

func (q *Quotas) rollover(now time.Time) {
	day := now.UTC().Truncate(24 * time.Hour)
	if !day.Equal(q.day) {
		q.day = day
		q.shared = map[string]int64{}
		q.pending = map[string]int64{}
	}
}

// Check reports whether a key of namespace ns may make another request now. When a quota is used up it returns
// false with that quota's name, limit and usage.
func (q *Quotas) Check(_ context.Context, ns string, now time.Time) (bool, string, int64, int64, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.rollover(now)
	for _, l := range q.limits {
		applies := false
		for _, n := range l.namespaces {
			applies = applies || n == ns
		}
		if !applies {
			continue
		}
		if !q.synced {
			return false, "", 0, 0, ErrQuotaNotSynced
		}
		var used int64
		for _, n := range l.namespaces {
			used += q.shared[n] + q.pending[n]
		}
		if used >= l.perDay {
			return false, l.name, l.perDay, used, nil
		}
	}
	return true, "", 0, 0, nil
}

// Add counts the tokens of a finished request.
func (q *Quotas) Add(ns string, tokens int64, now time.Time) {
	if tokens <= 0 {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.rollover(now)
	q.pending[ns] += tokens
}

// Sync adds the tokens counted since the last sync to today's shared counter and reads back every replica's totals.
func (q *Quotas) Sync(ctx context.Context, now time.Time) error {
	q.syncMu.Lock()
	defer q.syncMu.Unlock()
	q.mu.Lock()
	q.rollover(now)
	day := q.day
	flush := make(map[string]int64, len(q.pending))
	for ns, n := range q.pending {
		flush[ns] = n
	}
	q.mu.Unlock()

	key := client.ObjectKey{Namespace: q.namespace, Name: counterPrefix + day.Format(counterDayLayout)}
	var totals map[string]int64
	created := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cm := &corev1.ConfigMap{}
		err := q.client.Get(ctx, key, cm)
		if apierrors.IsNotFound(err) {
			seed, err := q.recorded(ctx, day)
			if err != nil {
				return err
			}
			addCounts(seed, flush)
			cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name,
				Labels: map[string]string{LabelTokenCounter: "true"}}, Data: formatCounts(seed)}
			if err := q.client.Create(ctx, cm); err != nil {
				if apierrors.IsAlreadyExists(err) {
					return apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, key.Name, err)
				}
				return err
			}
			totals, created = seed, true
			return nil
		}
		if err != nil {
			return err
		}
		counts := parseCounts(cm.Data)
		if len(flush) > 0 {
			addCounts(counts, flush)
			cm.Data = formatCounts(counts)
			if err := q.client.Update(ctx, cm); err != nil {
				return err
			}
		}
		totals = counts
		return nil
	})
	if err != nil {
		return err
	}

	q.mu.Lock()
	if q.day.Equal(day) {
		q.shared = totals
		for ns, n := range flush {
			if q.pending[ns] -= n; q.pending[ns] <= 0 {
				delete(q.pending, ns)
			}
		}
		q.synced = true
	}
	q.mu.Unlock()
	if created {
		return q.deleteOldCounters(ctx, day)
	}
	return nil
}

// deleteOldCounters removes the counters of days before yesterday.
func (q *Quotas) deleteOldCounters(ctx context.Context, today time.Time) error {
	var list corev1.ConfigMapList
	if err := q.client.List(ctx, &list, client.InNamespace(q.namespace), client.MatchingLabels{LabelTokenCounter: "true"}); err != nil {
		return err
	}
	keep := today.AddDate(0, 0, -1)
	for i := range list.Items {
		cm := &list.Items[i]
		if len(cm.Name) <= len(counterPrefix) {
			continue
		}
		day, err := time.Parse(counterDayLayout, cm.Name[len(counterPrefix):])
		if err != nil || !day.Before(keep) {
			continue
		}
		if err := q.client.Delete(ctx, cm); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// recorded sums the input and output tokens of every token record that starts on day, by namespace.
func (q *Quotas) recorded(ctx context.Context, day time.Time) (map[string]int64, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(usageListGVK)
	if err := q.client.List(ctx, list, client.MatchingLabels{"gryvia.io/usage-kind": "tokens"}); err != nil {
		return nil, err
	}
	totals := map[string]int64{}
	for _, item := range list.Items {
		if kind, _, _ := unstructured.NestedString(item.Object, "spec", "kind"); kind != "tokens" {
			continue
		}
		s, _, _ := unstructured.NestedString(item.Object, "spec", "start")
		start, err := time.Parse(time.RFC3339, s)
		if err != nil || start.UTC().Before(day) || !start.UTC().Before(day.Add(24*time.Hour)) {
			continue
		}
		in, _, _ := unstructured.NestedInt64(item.Object, "spec", "inputTokens")
		out, _, _ := unstructured.NestedInt64(item.Object, "spec", "outputTokens")
		if in+out > 0 {
			totals[item.GetNamespace()] += in + out
		}
	}
	return totals, nil
}

func addCounts(into, from map[string]int64) {
	for ns, n := range from {
		into[ns] += n
	}
}

func parseCounts(data map[string]string) map[string]int64 {
	out := make(map[string]int64, len(data))
	for ns, v := range data {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			out[ns] = n
		}
	}
	return out
}

func formatCounts(counts map[string]int64) map[string]string {
	out := make(map[string]string, len(counts))
	for ns, n := range counts {
		out[ns] = strconv.FormatInt(n, 10)
	}
	return out
}
