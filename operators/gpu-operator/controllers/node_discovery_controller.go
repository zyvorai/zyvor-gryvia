// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"reflect"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gryviav1 "github.com/zyvorai/gryvia/operators/gpu-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/gpu-operator/pkg/discovery"
)

const (
	// AutoRegisteredLabel marks GryviaGpuNodes created by NodeDiscoveryReconciler.
	// Objects without it are never modified or deleted by the discovery controller.
	AutoRegisteredLabel = "gryvia.io/auto-registered"
)

// NodeDiscoveryReconciler registers a GryviaGpuNode for every Node that the
// NVIDIA GPU Feature Discovery labelled as having GPUs.
type NodeDiscoveryReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Log    logr.Logger
}

//+kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviagpunodes,verbs=get;list;watch;create;update;delete

// Reconcile creates, updates or deletes the auto-registered GryviaGpuNode named after the Node.
func (r *NodeDiscoveryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("node", req.Name)

	var d discovery.Discovered
	hasGPU := false
	node := &corev1.Node{}
	if err := r.Get(ctx, types.NamespacedName{Name: req.Name}, node); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	} else if node.DeletionTimestamp.IsZero() {
		d, hasGPU = discovery.FromNode(node.Labels, nil)
	}

	existing := &gryviav1.GryviaGpuNode{}
	err := r.Get(ctx, types.NamespacedName{Name: req.Name}, existing)
	found := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	auto := found && existing.Labels[AutoRegisteredLabel] == "true"

	if !hasGPU {
		if !found {
			return ctrl.Result{}, nil
		}
		if !auto {
			log.V(1).Info("Node has no GPU but GryviaGpuNode is hand-made; leaving it", "gryviagpunode", existing.Name)
			return ctrl.Result{}, nil
		}
		if err := r.Delete(ctx, existing); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		log.Info("Deleted auto-registered GryviaGpuNode: node no longer has GPUs")
		return ctrl.Result{}, nil
	}

	if d.GPUType == "" {
		log.V(1).Info("Node has no nvidia.com/gpu.product label yet; skipping")
		return ctrl.Result{}, nil
	}
	count := d.GPUCount
	if count == 0 {
		count = d.Allocatable
	}
	if count == 0 {
		if q, ok := node.Status.Allocatable[corev1.ResourceName(discovery.ResourceGPU)]; ok {
			count = int(q.Value())
		}
	}

	if !found {
		obj := &gryviav1.GryviaGpuNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:   req.Name,
				Labels: map[string]string{AutoRegisteredLabel: "true"},
			},
		}
		applyDiscovered(&obj.Spec, req.Name, d, count)
		if err := r.Create(ctx, obj); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, err
		}
		log.Info("Registered GryviaGpuNode", "gpuType", obj.Spec.GpuType, "gpuCount", obj.Spec.GpuCount)
		return ctrl.Result{}, nil
	}

	if !auto {
		log.V(1).Info("GryviaGpuNode exists and is hand-made; not touching it", "gryviagpunode", existing.Name)
		return ctrl.Result{}, nil
	}

	desired := existing.Spec
	applyDiscovered(&desired, req.Name, d, count)
	if reflect.DeepEqual(desired, existing.Spec) {
		return ctrl.Result{}, nil
	}
	existing.Spec = desired
	if err := r.Update(ctx, existing); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("Updated auto-registered GryviaGpuNode", "gpuType", desired.GpuType, "gpuCount", desired.GpuCount)
	return ctrl.Result{}, nil
}

// applyDiscovered overwrites only the fields owned by discovery.
func applyDiscovered(spec *gryviav1.GryviaGpuNodeSpec, nodeName string, d discovery.Discovered, count int) {
	spec.NodeName = nodeName
	spec.GpuType = d.GPUType
	spec.GpuCount = count
	spec.MemoryGB = d.MemoryGB
	spec.RDMA = d.RDMA
	spec.SRIOV = d.SRIOV
}

// SetupWithManager sets up the controller with the Manager.
func (r *NodeDiscoveryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("node-discovery").
		For(&corev1.Node{}).
		Complete(r)
}
