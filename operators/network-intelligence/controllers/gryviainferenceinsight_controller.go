// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gryviav1 "github.com/zyvorai/gryvia/operators/network-intelligence/api/v1"
)

const (
	// inferenceInsightInterval is the default requeue interval for inference analysis
	inferenceInsightInterval = 60 * time.Second
)

// GryviaInferenceInsightReconciler reconciles a GryviaInferenceInsight object.
//
// The source is the status of a GryviaFabricSignal with spec.jobRef == spec.targetService (same namespace;
// the serving engine's pods carry the gryvia.io/job label of that job), filled by the collector's opt-in
// engine-metrics scraper (helm ebpf.inferMetrics + ebpf.publishFabricStatus): time to first token,
// inter-token latency, engine queue time, end-to-end latency (p99), queued requests and KV-cache usage
// as the ENGINE reports them, plus the eBPF network wait. A field the engine does not export stays unset
// (never zero). The old per-phase breakdown (dns, tcp connect, tls, gpu exec, postprocess) and the
// p50/p95 totals have no source and stay unset; gpuQueueNs is the engine queue p99 and totalNs/p99TotalNs
// the engine end-to-end p99. spec.analysisWindow is not used.
type GryviaInferenceInsightReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	now    func() time.Time
}

//+kubebuilder:rbac:groups=gryvia.io,resources=gryviainferenceinsights,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviainferenceinsights/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviainferenceinsights/finalizers,verbs=update
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviafabricsignals,verbs=get;list;watch

func (r *GryviaInferenceInsightReconciler) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

func (r *GryviaInferenceInsightReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	insight := &gryviav1.GryviaInferenceInsight{}
	if err := r.Get(ctx, req.NamespacedName, insight); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	sig, serr := lookupFabricSignal(ctx, r.Client, insight.Namespace, insight.Spec.TargetService)
	now := r.clock()
	haveData := serr == nil && sig != nil && sig.Status.UpdatedAt != nil && !sig.Status.UpdatedAt.IsZero()

	uerr := updateStatus(ctx, r.Client, req.NamespacedName, func() *gryviav1.GryviaInferenceInsight { return &gryviav1.GryviaInferenceInsight{} },
		func(t *gryviav1.GryviaInferenceInsight) {
			signalCondition(&t.Status.Conditions, t.Generation, sig, serr, now, insight.Spec.TargetService)
			if !haveData {
				t.Status.Phase = "AwaitingData"
				return
			}
			st := sig.Status
			t.Status.Phase = "Active"
			t.Status.SignalRef = sig.Name
			t.Status.Engine = st.Engine
			t.Status.TTFTP99Ms, t.Status.ITLP99Ms = st.TTFTP99Ms, st.ITLP99Ms
			t.Status.QueueTimeP99Ms, t.Status.E2EP99Ms = st.QueueTimeP99Ms, st.E2EP99Ms
			t.Status.InferWaitP99Ms = st.InferWaitP99Ms
			t.Status.RequestsWaiting, t.Status.KVCacheUsage = st.RequestsWaiting, st.KVCacheUsage

			t.Status.LatencyBreakdown = gryviav1.LatencyBreakdown{}
			t.Status.P50TotalNs, t.Status.P95TotalNs, t.Status.P99TotalNs = 0, 0, 0
			if st.QueueTimeP99Ms != nil {
				t.Status.LatencyBreakdown.GPUQueueNs = msToNs(*st.QueueTimeP99Ms)
			}
			if st.E2EP99Ms != nil {
				t.Status.LatencyBreakdown.TotalNs = msToNs(*st.E2EP99Ms)
				t.Status.P99TotalNs = msToNs(*st.E2EP99Ms)
			}
			t.Status.Bottleneck = inferenceBottleneck(st)
			t.Status.LastAnalysis = metav1.NewTime(st.UpdatedAt.Time)
		})
	if uerr != nil && !errors.IsNotFound(uerr) {
		return ctrl.Result{}, uerr
	}
	logger.Info("GryviaInferenceInsight updated", "service", insight.Spec.TargetService, "haveData", haveData)
	return ctrl.Result{RequeueAfter: inferenceInsightInterval}, nil
}

// inferenceBottleneck names the largest share of the engine end-to-end p99 among the measured parts:
// "queue" (engine queue p99 >= half of e2e), "network_wait" (eBPF accept-to-read p99 >= half), else
// "engine" when an end-to-end figure exists, "unknown" otherwise. p99s are not additive, so this is a
// heuristic pointing at where to look, not an attribution.
func inferenceBottleneck(st fabricStatus) string {
	if st.E2EP99Ms == nil || *st.E2EP99Ms <= 0 {
		return "unknown"
	}
	e2e := *st.E2EP99Ms
	switch {
	case st.QueueTimeP99Ms != nil && *st.QueueTimeP99Ms >= e2e/2:
		return "queue"
	case st.InferWaitP99Ms >= e2e/2:
		return "network_wait"
	default:
		return "engine"
	}
}

// SetupWithManager sets up the controller with the Manager
func (r *GryviaInferenceInsightReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gryviav1.GryviaInferenceInsight{}).
		Complete(r)
}
