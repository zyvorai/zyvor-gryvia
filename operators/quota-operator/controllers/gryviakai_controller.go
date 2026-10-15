package controllers

// KAI Scheduler queues for tenants (opt-in: quota-operator flag --kai-integration, default off).
//
// For every GryviaTenant this controller maintains, through the unstructured client only (no KAI Go dependency;
// API version scheduling.run.ai/v2):
//
//   - the parent Queue "gryvia" (unlimited quota and limit), shared by all tenants, and
//   - a leaf Queue "gryvia-<tenant>" under it whose GPU quota (guaranteed share) is the tenant's
//     spec.quotas.concurrentGPUs, else the maxGPUs of a GryviaQuota listing the tenant namespace, else unlimited.
//     Its limit is quota + spec.quotas.borrowingLimit, or unlimited: like the Kueue cohort, a tenant borrows idle
//     GPUs above its quota and KAI reclaims them for tenants under theirs. spec.quotas.fairShareWeight becomes
//     overQuotaWeight. CPU and memory are unlimited.
//
// It also labels the tenant namespace gryvia.io/kai-queue=gryvia-<tenant>; the ai-operator (--kai-scheduler) puts
// a job that names no queue into the queue of its namespace. spec.parentTenant, lendingLimit and cohort are not
// mapped: KAI only runs jobs in leaf queues, so a tenant that is also a parent could not run jobs.
//
// Cluster-scoped queues cannot have a namespaced owner: they carry the label gryvia.io/tenant and a finalizer on the
// GryviaTenant deletes them. Queues that exist without our managed-by label are never modified or deleted.
//
// Verified by unit tests with a fake client and on kind by scripts/e2e-kai.sh (tenant step).

import (
	"context"
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gryviav1 "github.com/zyvorai/gryvia/operators/quota-operator/api/v1"
)

const (
	kaiAPIVersion = "scheduling.run.ai/v2"

	// KAIFinalizer keeps the tenant until its KAI queue is deleted.
	KAIFinalizer = "gryvia.io/kai-finalizer"
	// KAIParentQueue is the parent of every tenant queue.
	KAIParentQueue = "gryvia"
	// KAIQueueNamespaceLabel on a tenant namespace names the KAI queue of jobs there that name none.
	KAIQueueNamespaceLabel = "gryvia.io/kai-queue"

	kaiUnlimited int64 = -1
)

// KAIQueueName is the KAI leaf queue of a tenant.
func KAIQueueName(tenant string) string { return "gryvia-" + tenant }

// GryviaKAIReconciler creates the KAI queue of a GryviaTenant.
type GryviaKAIReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=gryvia.io,resources=gryviatenants,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaquotas,verbs=get;list;watch
//+kubebuilder:rbac:groups=scheduling.run.ai,resources=queues,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=namespaces,verbs=get;patch

func kaiResource(quota, limit, weight int64) map[string]interface{} {
	return map[string]interface{}{"quota": quota, "limit": limit, "overQuotaWeight": weight}
}

func newKAIQueue(name, parent string, labels map[string]interface{}, gpu map[string]interface{}) *unstructured.Unstructured {
	spec := map[string]interface{}{
		"resources": map[string]interface{}{
			"cpu":    kaiResource(kaiUnlimited, kaiUnlimited, 1),
			"memory": kaiResource(kaiUnlimited, kaiUnlimited, 1),
			"gpu":    gpu,
		},
	}
	if parent != "" {
		spec["parentQueue"] = parent
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": kaiAPIVersion, "kind": "Queue",
		"metadata": map[string]interface{}{"name": name, "labels": labels},
		"spec":     spec,
	}}
}

// BuildKAIParentQueue returns the shared parent queue "gryvia".
func BuildKAIParentQueue() *unstructured.Unstructured {
	return newKAIQueue(KAIParentQueue, "", map[string]interface{}{kueueManagedByLabel: kueueManagedBy},
		kaiResource(kaiUnlimited, kaiUnlimited, 1))
}

// BuildKAIQueue returns the tenant's leaf queue for the quota q (see tenantQuota).
func BuildKAIQueue(tenant string, q quotaSource) *unstructured.Unstructured {
	quota, limit := kaiUnlimited, kaiUnlimited
	if q.Source != "unlimited" {
		if n, err := strconv.ParseInt(q.Nominal, 10, 64); err == nil {
			quota = n
			if q.BorrowingLimit != nil {
				limit = n + int64(*q.BorrowingLimit)
			}
		}
	}
	weight := int64(1)
	if q.Weight > 0 {
		weight = int64(q.Weight)
	}
	obj := newKAIQueue(KAIQueueName(tenant), KAIParentQueue, managedLabels(tenant), kaiResource(quota, limit, weight))
	obj.SetAnnotations(map[string]string{kueueQuotaSourceAnnotation: q.Source})
	return obj
}

func (r *GryviaKAIReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	tenant := &gryviav1.GryviaTenant{}
	if err := r.Get(ctx, req.NamespacedName, tenant); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !tenant.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(tenant, KAIFinalizer) {
			return ctrl.Result{}, nil
		}
		if err := r.cleanup(ctx, tenant.Name); err != nil {
			return ctrl.Result{}, err
		}
		base := tenant.DeepCopy()
		controllerutil.RemoveFinalizer(tenant, KAIFinalizer)
		return ctrl.Result{}, r.Patch(ctx, tenant, client.MergeFrom(base))
	}

	if !controllerutil.ContainsFinalizer(tenant, KAIFinalizer) {
		base := tenant.DeepCopy()
		controllerutil.AddFinalizer(tenant, KAIFinalizer)
		if err := r.Patch(ctx, tenant, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
	}

	quotas := &gryviav1.GryviaQuotaList{}
	if err := r.List(ctx, quotas); err != nil {
		return ctrl.Result{}, fmt.Errorf("list GryviaQuota: %w", err)
	}
	for _, obj := range []*unstructured.Unstructured{BuildKAIParentQueue(), BuildKAIQueue(tenant.Name, tenantQuota(tenant, quotas.Items))} {
		if err := r.apply(ctx, obj); err != nil {
			if meta.IsNoMatchError(err) {
				logger.Info("KAI Queue CRD not found: is KAI Scheduler installed?")
				return ctrl.Result{RequeueAfter: time.Minute}, nil
			}
			return ctrl.Result{}, fmt.Errorf("Queue %s: %w", obj.GetName(), err)
		}
	}

	// The namespace is created by the tenant controller.
	ns := &corev1.Namespace{}
	if err := r.Get(ctx, types.NamespacedName{Name: tenantNamespaceName(tenant.Name)}, ns); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}
	if want := KAIQueueName(tenant.Name); ns.Labels[KAIQueueNamespaceLabel] != want {
		base := ns.DeepCopy()
		if ns.Labels == nil {
			ns.Labels = map[string]string{}
		}
		ns.Labels[KAIQueueNamespaceLabel] = want
		if err := r.Patch(ctx, ns, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: 2 * time.Minute}, nil
}

// apply creates the queue or updates its spec and our annotations when they differ. Queues without our managed-by
// label are left alone.
func (r *GryviaKAIReconciler) apply(ctx context.Context, want *unstructured.Unstructured) error {
	have := &unstructured.Unstructured{}
	have.SetGroupVersionKind(want.GroupVersionKind())
	err := r.Get(ctx, client.ObjectKeyFromObject(want), have)
	if errors.IsNotFound(err) {
		return r.Create(ctx, want)
	}
	if err != nil {
		return err
	}
	if have.GetLabels()[kueueManagedByLabel] != kueueManagedBy {
		log.FromContext(ctx).Info("KAI queue exists and is not managed by Gryvia, leaving it alone", "name", want.GetName())
		return nil
	}
	wantSpec, _, _ := unstructured.NestedMap(want.Object, "spec")
	haveSpec, _, _ := unstructured.NestedMap(have.Object, "spec")
	sameAnno := true
	for k, v := range want.GetAnnotations() {
		if have.GetAnnotations()[k] != v {
			sameAnno = false
		}
	}
	if subsetEqual(wantSpec, haveSpec) && sameAnno {
		return nil
	}
	_ = unstructured.SetNestedMap(have.Object, wantSpec, "spec")
	if a := want.GetAnnotations(); a != nil {
		merged := have.GetAnnotations()
		if merged == nil {
			merged = map[string]string{}
		}
		for k, v := range a {
			merged[k] = v
		}
		have.SetAnnotations(merged)
	}
	return r.Update(ctx, have)
}

// cleanup deletes the tenant's queue (only if we manage it). The parent queue is shared and stays; the tenant
// namespace is deleted with the tenant, so its label is left.
func (r *GryviaKAIReconciler) cleanup(ctx context.Context, tenant string) error {
	want := BuildKAIQueue(tenant, quotaSource{Source: "unlimited"})
	have := &unstructured.Unstructured{}
	have.SetGroupVersionKind(want.GroupVersionKind())
	err := r.Get(ctx, client.ObjectKeyFromObject(want), have)
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

func (r *GryviaKAIReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("gryviakai").
		For(&gryviav1.GryviaTenant{}).
		Complete(r)
}
