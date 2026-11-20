// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controllers

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/zyvorai/gryvia/operators/network-intelligence/pkg/sources"
)

// ConditionSourceAvailable is True when the data behind a status could be read and False (with a
// reason and message) when it could not, so an empty status is never mistaken for "nothing happened".
const ConditionSourceAvailable = "SourceAvailable"

// errNoCollector is what a reconciler without a collector client reports.
var errNoCollector = &sources.SourceError{Source: "collector", Reason: sources.ReasonNotConfigured,
	Message: "the operator has no collector client (internal configuration error)"}

// setSource sets the SourceAvailable condition from a source error (nil = available).
// note is appended to the message of an available source (limits the reader should know).
func setSource(conds *[]metav1.Condition, generation int64, err error, stats sources.Stats, note string) {
	c := metav1.Condition{Type: ConditionSourceAvailable, ObservedGeneration: generation}
	switch {
	case err != nil:
		se := sources.AsSourceError("source", err)
		c.Status, c.Reason, c.Message = metav1.ConditionFalse, se.Reason, truncate(se.Message, 1000)
	case stats.Total > 0 && stats.Reachable < stats.Total:
		c.Status, c.Reason = metav1.ConditionTrue, sources.ReasonPartial
		c.Message = fmt.Sprintf("%d of %d collectors answered; figures cover those nodes only", stats.Reachable, stats.Total)
		if note != "" {
			c.Message += ". " + note
		}
	default:
		c.Status, c.Reason, c.Message = metav1.ConditionTrue, sources.ReasonOK, note
	}
	meta.SetStatusCondition(conds, c)
}

// setCondition sets an arbitrary condition.
func setCondition(conds *[]metav1.Condition, generation int64, typ string, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(conds, metav1.Condition{Type: typ, Status: status, Reason: reason,
		Message: truncate(msg, 1000), ObservedGeneration: generation})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

// updateStatus re-reads obj, applies mutate and writes the status subresource, retrying on conflict.
func updateStatus[T client.Object](ctx context.Context, c client.Client, key types.NamespacedName, fresh func() T, mutate func(T)) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		obj := fresh()
		if err := c.Get(ctx, key, obj); err != nil {
			return err
		}
		mutate(obj)
		return c.Status().Update(ctx, obj)
	})
}

// isNoKind reports that a CRD is not installed (the API server has no such kind).
func isNoKind(err error) bool {
	return meta.IsNoMatchError(err) || (err != nil && (strings.Contains(err.Error(), "no matches for kind") ||
		strings.Contains(err.Error(), "the server could not find the requested resource")))
}

// ---- GryviaFabricSignal (read through unstructured: it belongs to the ai-operator module) ----

var fabricSignalGVK = schema.GroupVersionKind{Group: "gryvia.io", Version: "v1alpha1", Kind: "GryviaFabricSignalList"}

// fabricStatus holds the GryviaFabricSignal.status fields this operator reads (json tags as in
// operators/ai-operator/api/v1/gryviafabricsignal_types.go).
type fabricStatus struct {
	StragglerRank          int32        `json:"stragglerRank"`
	NCCLP99Ms              float64      `json:"ncclP99ms"`
	RDMARetryRate          float64      `json:"rdmaRetryRate"`
	OverlapIdleRatio       float64      `json:"overlapIdleRatio"`
	CNPRate                float64      `json:"cnpRate"`
	PFCRate                float64      `json:"pfcRate"`
	CollectiveMaxSkewMs    float64      `json:"collectiveMaxSkewMs"`
	GPUIdleDuringCommRatio float64      `json:"gpuIdleDuringCommRatio"`
	ScoreDelta             float64      `json:"scoreDelta"`
	InferWaitP99Ms         float64      `json:"inferWaitP99ms"`
	Engine                 string       `json:"engine"`
	TTFTP99Ms              *float64     `json:"ttftP99ms"`
	ITLP99Ms               *float64     `json:"itlP99ms"`
	QueueTimeP99Ms         *float64     `json:"queueTimeP99ms"`
	E2EP99Ms               *float64     `json:"e2eP99ms"`
	RequestsWaiting        *int64       `json:"requestsWaiting"`
	KVCacheUsage           *float64     `json:"kvCacheUsage"`
	UpdatedAt              *metav1.Time `json:"updatedAt"`
}

type fabricSignal struct {
	Name   string
	Status fabricStatus
}

// fabricSignalStale is the age after which a signal is reported as stale (the collector publishes every 30 s).
const fabricSignalStale = 5 * time.Minute

// lookupFabricSignal finds the GryviaFabricSignal of a job in namespace (spec.jobRef == job).
func lookupFabricSignal(ctx context.Context, c client.Client, namespace, job string) (*fabricSignal, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(fabricSignalGVK)
	if err := c.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	var best *fabricSignal
	for _, u := range list.Items {
		ref, _, _ := unstructured.NestedString(u.Object, "spec", "jobRef")
		if ref != job {
			continue
		}
		var st fabricStatus
		if raw, ok, _ := unstructured.NestedMap(u.Object, "status"); ok {
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &st); err != nil {
				continue
			}
		}
		if best == nil || u.GetName() < best.Name {
			best = &fabricSignal{Name: u.GetName(), Status: st}
		}
	}
	return best, nil
}

// signalCondition sets SourceAvailable for a fabric-signal based status.
func signalCondition(conds *[]metav1.Condition, gen int64, sig *fabricSignal, err error, now time.Time, job string) {
	switch {
	case err != nil && isNoKind(err):
		setCondition(conds, gen, ConditionSourceAvailable, metav1.ConditionFalse, "CRDNotInstalled",
			"the GryviaFabricSignal CRD is not installed")
	case err != nil:
		setCondition(conds, gen, ConditionSourceAvailable, metav1.ConditionFalse, sources.ReasonUnreachable,
			"cannot list GryviaFabricSignal: "+err.Error())
	case sig == nil:
		setCondition(conds, gen, ConditionSourceAvailable, metav1.ConditionFalse, "NoSignal",
			fmt.Sprintf("no GryviaFabricSignal with spec.jobRef=%q in this namespace; create one and enable the collector with ebpf.publishFabricStatus", job))
	case sig.Status.UpdatedAt == nil || sig.Status.UpdatedAt.IsZero():
		setCondition(conds, gen, ConditionSourceAvailable, metav1.ConditionFalse, "NoData",
			fmt.Sprintf("GryviaFabricSignal %s has no status yet (the collector has not published for this job)", sig.Name))
	case now.Sub(sig.Status.UpdatedAt.Time) > fabricSignalStale:
		setCondition(conds, gen, ConditionSourceAvailable, metav1.ConditionFalse, "Stale",
			fmt.Sprintf("GryviaFabricSignal %s was last updated %s ago; the figures below are the last published ones",
				sig.Name, now.Sub(sig.Status.UpdatedAt.Time).Round(time.Second)))
	default:
		setCondition(conds, gen, ConditionSourceAvailable, metav1.ConditionTrue, sources.ReasonOK,
			"read from GryviaFabricSignal "+sig.Name)
	}
}

// ---- small helpers ----

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func formatMs(ms float64) string {
	if ms <= 0 {
		return ""
	}
	return fmt.Sprintf("%.1fms", ms)
}

func satInt64(u uint64) int64 {
	if u > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(u)
}

func msToNs(ms float64) int64 { return int64(ms * 1e6) }

// anomalySeverity classifies a collector anomaly by how far the value is beyond its threshold.
func anomalySeverity(a sources.Anomaly) string {
	if a.Threshold <= 0 {
		return "medium"
	}
	switch ratio := a.Value / a.Threshold; {
	case ratio > 5:
		return "critical"
	case ratio > 3:
		return "high"
	case ratio > 1.5:
		return "medium"
	default:
		return "low"
	}
}

// endpointMatches reports whether a collector node id names service (plain name or namespace/name).
func endpointMatches(id, namespace, service string) bool {
	return id == service || (namespace != "" && id == namespace+"/"+service)
}

func isSourceErr(err error, reason string) bool {
	var se *sources.SourceError
	return errors.As(err, &se) && se.Reason == reason
}
