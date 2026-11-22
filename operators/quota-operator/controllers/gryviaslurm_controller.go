// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

// Slurm partitions for tenants (opt-in: quota-operator flag --slurm-integration, default off).
//
// For every GryviaTenant this controller maintains, through the unstructured client only (no Slinky Go dependency;
// API version slinky.slurm.net/v1beta1, SchedMD's slurm-operator), a NodeSet "gryvia-<tenant>" in the namespace of
// the Slurm Controller CR, with its partition enabled. The slurm-operator names the partition after the NodeSet, so
// each tenant gets the partition gryvia-<tenant> backed by its own slurmd pods. The node count comes from the tenant
// quota (concurrentGPUs, else a GryviaQuota maxGPUs, divided by --slurm-gpus-per-node) or --slurm-nodes-per-tenant,
// and the partition line limits a job to those nodes (MaxNodes).
//
// The NodeSet lives in the Slurm namespace, not the tenant's, so it is tied to the tenant with the label
// gryvia.io/tenant and a finalizer that deletes it. NodeSets without our managed-by label are left alone.
// Finished jobs are read back into GryviaUsageRecords by SlurmAccounting (slurm_accounting.go).
//
// Verified by unit tests with a fake client and the kind workflow .github/workflows/e2e-slurm.yml (CPU only).

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gryviav1 "github.com/zyvorai/gryvia/operators/quota-operator/api/v1"
)

const (
	slinkyAPIVersion = "slinky.slurm.net/v1beta1"

	// SlurmFinalizer keeps the tenant until its NodeSet is deleted.
	SlurmFinalizer = "gryvia.io/slurm-finalizer"
	// DefaultSlurmdImage is the slurmd image of the pinned slurm chart (1.2.3, Slurm 26.05).
	DefaultSlurmdImage = "ghcr.io/slinkyproject/slurmd:26.05-ubuntu26.04"
)

// NodeSetGVK is Slinky's NodeSet.
var NodeSetGVK = schema.GroupVersionKind{Group: "slinky.slurm.net", Version: "v1beta1", Kind: "NodeSet"}

// SlurmPartitionName is the NodeSet, and so the Slurm partition, of a tenant.
func SlurmPartitionName(tenant string) string { return "gryvia-" + tenant }

// GryviaSlurmReconciler creates the Slurm NodeSet (and partition) of a GryviaTenant.
type GryviaSlurmReconciler struct {
	client.Client

	// Namespace and Controller locate the Slinky Controller CR the NodeSets join.
	Namespace  string
	Controller string
	// SlurmdImage is the slurmd container image (default DefaultSlurmdImage).
	SlurmdImage string
	// NodesPerTenant is the node count of a tenant without a GPU quota, or when GPUsPerNode is 0 (default 1).
	NodesPerTenant int32
	// GPUsPerNode is requested (nvidia.com/gpu) by every slurmd pod; the tenant's GPU quota divided by it, rounded
	// up, is the node count. 0 runs CPU-only nodes.
	GPUsPerNode int32
	// Oversubscribe lets several slurmd pods share a Kubernetes node (Slinky's oversubscribeNode). For test
	// clusters; Slurm then sees each pod as a node with the whole host's CPUs.
	Oversubscribe bool
}

//+kubebuilder:rbac:groups=gryvia.io,resources=gryviatenants,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaquotas,verbs=get;list;watch
//+kubebuilder:rbac:groups=slinky.slurm.net,resources=nodesets,verbs=get;list;watch;create;update;patch;delete

// slurmNodes is the tenant's node count and where it came from (tenant, quota or default).
func (r *GryviaSlurmReconciler) slurmNodes(tenant *gryviav1.GryviaTenant, quotas []gryviav1.GryviaQuota) (int32, string) {
	def := r.NodesPerTenant
	if def < 1 {
		def = 1
	}
	if r.GPUsPerNode < 1 {
		return def, "default"
	}
	q := tenantQuota(tenant, quotas)
	if q.Source == "unlimited" {
		return def, "default"
	}
	var gpus int32
	if _, err := fmt.Sscan(q.Nominal, &gpus); err != nil || gpus < 1 {
		return def, "default"
	}
	return (gpus + r.GPUsPerNode - 1) / r.GPUsPerNode, q.Source
}

// BuildNodeSet returns the tenant's NodeSet.
func (r *GryviaSlurmReconciler) BuildNodeSet(tenant *gryviav1.GryviaTenant, quotas []gryviav1.GryviaQuota) *unstructured.Unstructured {
	nodes, source := r.slurmNodes(tenant, quotas)
	image := r.SlurmdImage
	if image == "" {
		image = DefaultSlurmdImage
	}
	slurmd := map[string]interface{}{"image": image}
	if r.GPUsPerNode > 0 {
		slurmd["resources"] = map[string]interface{}{
			"limits": map[string]interface{}{"nvidia.com/gpu": fmt.Sprintf("%d", r.GPUsPerNode)},
		}
	}
	labels := managedLabels(tenant.Name)
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": slinkyAPIVersion,
		"kind":       "NodeSet",
		"metadata": map[string]interface{}{
			"name":      SlurmPartitionName(tenant.Name),
			"namespace": r.Namespace,
			"labels":    labels,
		},
		"spec": map[string]interface{}{
			"controllerRef":     map[string]interface{}{"name": r.Controller},
			"replicas":          int64(nodes),
			"scalingMode":       "StatefulSet",
			"oversubscribeNode": r.Oversubscribe,
			"slurmd":            slurmd,
			"partition": map[string]interface{}{
				"enabled": true,
				"config":  fmt.Sprintf("MaxNodes=%d", nodes),
			},
			"template": map[string]interface{}{
				"metadata": map[string]interface{}{"labels": map[string]interface{}{kueueTenantLabel: tenant.Name}},
				"spec":     map[string]interface{}{"nodeSelector": map[string]interface{}{"kubernetes.io/os": "linux"}},
			},
		},
	}}
	obj.SetAnnotations(map[string]string{kueueQuotaSourceAnnotation: source})
	return obj
}

func (r *GryviaSlurmReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	tenant := &gryviav1.GryviaTenant{}
	if err := r.Get(ctx, req.NamespacedName, tenant); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !tenant.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(tenant, SlurmFinalizer) {
			return ctrl.Result{}, nil
		}
		if err := r.cleanup(ctx, tenant.Name); err != nil {
			return ctrl.Result{}, err
		}
		base := tenant.DeepCopy()
		controllerutil.RemoveFinalizer(tenant, SlurmFinalizer)
		return ctrl.Result{}, r.Patch(ctx, tenant, client.MergeFrom(base))
	}

	if !controllerutil.ContainsFinalizer(tenant, SlurmFinalizer) {
		base := tenant.DeepCopy()
		controllerutil.AddFinalizer(tenant, SlurmFinalizer)
		if err := r.Patch(ctx, tenant, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
	}

	quotas := &gryviav1.GryviaQuotaList{}
	if err := r.List(ctx, quotas); err != nil {
		return ctrl.Result{}, fmt.Errorf("list GryviaQuota: %w", err)
	}
	want := r.BuildNodeSet(tenant, quotas.Items)
	if err := (&GryviaKueueReconciler{Client: r.Client}).apply(ctx, want); err != nil {
		if meta.IsNoMatchError(err) {
			log.FromContext(ctx).Info("Slinky CRDs not found: is the slurm-operator installed?", "kind", "NodeSet")
			return ctrl.Result{RequeueAfter: time.Minute}, nil
		}
		return ctrl.Result{}, fmt.Errorf("NodeSet %s: %w", want.GetName(), err)
	}
	return ctrl.Result{RequeueAfter: 2 * time.Minute}, nil
}

// cleanup deletes the tenant's NodeSet if we manage it. A missing CRD or NodeSet counts as done.
func (r *GryviaSlurmReconciler) cleanup(ctx context.Context, tenant string) error {
	have := &unstructured.Unstructured{}
	have.SetGroupVersionKind(NodeSetGVK)
	err := r.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: SlurmPartitionName(tenant)}, have)
	if errors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if have.GetLabels()[kueueManagedByLabel] != kueueManagedBy || have.GetLabels()[kueueTenantLabel] != tenant {
		return nil
	}
	if err := r.Delete(ctx, have); err != nil && !errors.IsNotFound(err) {
		return err
	}
	return nil
}

func (r *GryviaSlurmReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("gryviaslurm").
		For(&gryviav1.GryviaTenant{}).
		Complete(r)
}
