// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"fmt"
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
	// trainingInsightInterval is the default requeue interval while a job is running
	trainingInsightInterval = 60 * time.Second

	// overlapIdleCommunicationBound is the OverlapIdleRatio from which the job is reported as communication bound.
	overlapIdleCommunicationBound = 0.3
)

// GryviaTrainingInsightReconciler reconciles a GryviaTrainingInsight object.
//
// The source is the status of the job's GryviaFabricSignal (spec.jobRef == spec.targetJob, same namespace),
// which the collector fills with -publish-fabric-status. That status has per-job aggregates only: there
// are NO per-rank statistics, communication pattern or comm/compute ratio, so status.rankStats,
// commPattern and commComputeRatio stay unset ("unknown"). A straggler is reported only when the
// collector flagged one (ncclP99ms > 0), with its rank and no invented slowdown factor. The bottleneck is
// "communication" only with such evidence (a flagged straggler or overlapIdleRatio >= 0.3), otherwise
// "unknown". spec.analysisWindow and spec.metrics are not used (the collector's window is fixed).
type GryviaTrainingInsightReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	now    func() time.Time
}

//+kubebuilder:rbac:groups=gryvia.io,resources=gryviatraininginsights,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviatraininginsights/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviatraininginsights/finalizers,verbs=update
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviafabricsignals,verbs=get;list;watch

func (r *GryviaTrainingInsightReconciler) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

func (r *GryviaTrainingInsightReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	insight := &gryviav1.GryviaTrainingInsight{}
	if err := r.Get(ctx, req.NamespacedName, insight); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	sig, serr := lookupFabricSignal(ctx, r.Client, insight.Namespace, insight.Spec.TargetJob)
	now := r.clock()
	haveData := serr == nil && sig != nil && sig.Status.UpdatedAt != nil && !sig.Status.UpdatedAt.IsZero()

	uerr := updateStatus(ctx, r.Client, req.NamespacedName, func() *gryviav1.GryviaTrainingInsight { return &gryviav1.GryviaTrainingInsight{} },
		func(t *gryviav1.GryviaTrainingInsight) {
			signalCondition(&t.Status.Conditions, t.Generation, sig, serr, now, insight.Spec.TargetJob)
			if !haveData {
				t.Status.Phase = "AwaitingData"
				return
			}
			st := sig.Status
			t.Status.Phase = "Active"
			t.Status.SignalRef = sig.Name
			t.Status.NCCLP99Ms = st.NCCLP99Ms
			t.Status.CollectiveMaxSkewMs = st.CollectiveMaxSkewMs
			t.Status.OverlapIdleRatio = st.OverlapIdleRatio
			t.Status.RDMARetryRate = st.RDMARetryRate
			t.Status.ScoreDelta = st.ScoreDelta
			t.Status.CommPattern = "unknown"
			t.Status.RankStats = nil
			t.Status.Stragglers = nil
			if st.NCCLP99Ms > 0 {
				t.Status.Stragglers = []gryviav1.StragglerInfo{{
					Rank:   int(st.StragglerRank),
					Reason: fmt.Sprintf("nccl_straggler_flagged (p99 %.1f ms, max collective skew %.1f ms)", st.NCCLP99Ms, st.CollectiveMaxSkewMs),
				}}
			}
			t.Status.Bottleneck = "unknown"
			if st.NCCLP99Ms > 0 || st.OverlapIdleRatio >= overlapIdleCommunicationBound {
				t.Status.Bottleneck = "communication"
			}
			t.Status.LastAnalysis = metav1.NewTime(st.UpdatedAt.Time)
		})
	if uerr != nil && !errors.IsNotFound(uerr) {
		return ctrl.Result{}, uerr
	}
	logger.Info("GryviaTrainingInsight updated", "job", insight.Spec.TargetJob, "haveData", haveData)
	return ctrl.Result{RequeueAfter: trainingInsightInterval}, nil
}

// SetupWithManager sets up the controller with the Manager
func (r *GryviaTrainingInsightReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gryviav1.GryviaTrainingInsight{}).
		Complete(r)
}
