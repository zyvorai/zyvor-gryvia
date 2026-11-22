// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"time"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/ai-operator/pkg/fabricmerge"
)

// GryviaFabricSignalReconciler folds status.nodes[] (one entry per collector node, written with
// server-side apply) into the top-level status fields of a GryviaFabricSignal. It is registered
// only with --merge-fabric-signals; without per-node entries (collectors in the default mode) it
// changes nothing, and when every entry is stale the top-level status is left as it is.
type GryviaFabricSignalReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Now    func() time.Time // nil = time.Now
}

//+kubebuilder:rbac:groups=gryvia.io,resources=gryviafabricsignals,verbs=get;list;watch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviafabricsignals/status,verbs=get;update;patch

const (
	fabricMergeMinRequeue = 5 * time.Second
	fabricMergeMaxRequeue = 5 * time.Minute
)

func (r *GryviaFabricSignalReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	sig := &gryviav1.GryviaFabricSignal{}
	if err := r.Get(ctx, req.NamespacedName, sig); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if len(sig.Status.Nodes) == 0 {
		return ctrl.Result{}, nil
	}
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	t := now()
	merged, ok := fabricmerge.Merge(t, sig.Status.Nodes)
	if !ok {
		log.FromContext(ctx).V(1).Info("all fabric node entries are stale; status left untouched", "name", req.Name)
		return ctrl.Result{}, nil
	}

	// Come back when the first fresh entry expires so a dead collector drops out by itself.
	next := fabricMergeMaxRequeue
	for _, n := range sig.Status.Nodes {
		if fabricmerge.Fresh(n, t) {
			if d := fabricmerge.Expiry(n).Sub(t) + time.Second; d < next {
				next = d
			}
		}
	}
	if next < fabricMergeMinRequeue {
		next = fabricMergeMinRequeue
	}

	orig := sig.DeepCopy()
	fabricmerge.Apply(&sig.Status, merged)
	if apiequality.Semantic.DeepEqual(orig.Status, sig.Status) {
		return ctrl.Result{RequeueAfter: next}, nil
	}
	if err := r.Status().Patch(ctx, sig, client.MergeFrom(orig)); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: next}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *GryviaFabricSignalReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gryviav1.GryviaFabricSignal{}).
		Named("gryviafabricsignal-merger").
		Complete(r)
}
