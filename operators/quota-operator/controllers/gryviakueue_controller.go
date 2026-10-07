package controllers

// Kueue objects for tenants (opt-in: quota-operator flag --kueue-integration, default off).
//
// For every GryviaTenant this controller maintains, through the unstructured client only (no Kueue Go
// dependency; API version kueue.x-k8s.io/v1beta1):
//
//   - a LocalQueue "gryvia" in the namespace tenant-<name>, pointing at
//   - a ClusterQueue "gryvia-<tenant>" in the cohort "gryvia" (tenants borrow unused quota from each
//     other and reclaim it by preemption), whose namespaceSelector matches only the tenant namespace, and
//   - the ResourceFlavor "gryvia-default" (+ one "gryvia-<gpu-type>" per enabled GryviaGpuSku when
//     --kueue-gpu-type-flavors is on).
//
// The ai-operator (also with --kueue-integration) then creates each job's batch Job suspended in that queue,
// and Kueue admits all pods of a job together or not at all. See docs/kueue-integration.md.
//
// Cluster-scoped objects cannot have a namespaced owner, so they are tied to the tenant with the label
// gryvia.io/tenant and a finalizer on the GryviaTenant that deletes them. Objects that exist without
// our managed-by label are never modified or deleted.
//
// Verified only by unit tests with a fake client (golden YAML) and the kind workflow
// .github/workflows/e2e-kueue.yml; nothing has run against GPUs.

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gryviav1 "github.com/zyvorai/gryvia/operators/quota-operator/api/v1"
)

const (
	kueueGroup      = "kueue.x-k8s.io"
	kueueAPIVersion = "kueue.x-k8s.io/v1beta1"

	// KueueFinalizer keeps the tenant until its ClusterQueue and LocalQueue are deleted.
	KueueFinalizer = "gryvia.io/kueue-finalizer"

	kueueManagedByLabel = "app.kubernetes.io/managed-by"
	kueueManagedBy      = "gryvia-quota-operator"
	kueueTenantLabel    = "gryvia.io/tenant"
	// kueueQuotaSourceAnnotation records where the nominal quota came from: tenant, quota or unlimited.
	kueueQuotaSourceAnnotation = "gryvia.io/quota-source"

	// KueueCohort is the cohort all tenant ClusterQueues join.
	KueueCohort = "gryvia"
	// KueueLocalQueueName is the LocalQueue created in every tenant namespace.
	KueueLocalQueueName = "gryvia"
	// KueueDefaultFlavor is the flavor without node labels that holds the tenant quota.
	KueueDefaultFlavor = "gryvia-default"
	// KueueComputeFlavor holds the unlimited cpu/memory declarations. It is a flavor of its own because
	// Kueue (as far as we know, unverified) does not accept one flavor in two resource groups.
	KueueComputeFlavor = "gryvia-compute"

	// Kueue only admits a workload whose every requested resource is covered by the ClusterQueue, so
	// cpu and memory are always declared. They get these effectively unlimited nominal quotas unless
	// they are the quota resource themselves (--kueue-quota-resources).
	kueueUnlimitedCPU    = "100000"
	kueueUnlimitedMemory = "1000Ti"
	// kueueUnlimitedQuota is the nominal quota of a tenant with neither spec.quotas.concurrentGPUs
	// nor a GryviaQuota: no limit (0 would starve every job).
	kueueUnlimitedQuota = "1000000"

	// DefaultKueueQuotaResources is the resource the tenant quota (concurrentGPUs) is applied to.
	DefaultKueueQuotaResources = "nvidia.com/gpu"
	// gpuNodeLabel is the node label carrying the GPU type (same as the ai-operator's node selector).
	gpuNodeLabel = "gryvia.io/gpu"
)

var nonDNS = regexp.MustCompile(`[^a-z0-9-]+`)

// GryviaKueueReconciler creates the Kueue objects of a GryviaTenant.
type GryviaKueueReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// QuotaResources are the resources the tenant's nominal quota is applied to (all with the same
	// number; intended for one resource). Default: nvidia.com/gpu. The kind e2e uses cpu.
	QuotaResources []string
	// GPUTypeFlavors also creates one ResourceFlavor per enabled GryviaGpuSku gpuType and puts them
	// (plus the default) in the ClusterQueue. Off by default: see docs/kueue-integration.md for why.
	GPUTypeFlavors bool
	FairSharing    bool
	TopologyName   string
	// GenerateTopology also creates the Topology TopologyName (levels gryvia.io/ib-block, gryvia.io/rack,
	// kubernetes.io/hostname) when it does not exist. An existing Topology not labelled as Gryvia's is left alone.
	GenerateTopology bool
	AdmissionCheck   string
}

//+kubebuilder:rbac:groups=gryvia.io,resources=gryviatenants,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaquotas;gryviagpuskus,verbs=get;list;watch
//+kubebuilder:rbac:groups=kueue.x-k8s.io,resources=clusterqueues;resourceflavors;localqueues,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=kueue.x-k8s.io,resources=topologies,verbs=get;create;update

func kueueGVK(kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: kueueGroup, Version: "v1beta1", Kind: kind}
}

func (r *GryviaKueueReconciler) quotaResources() []string {
	if len(r.QuotaResources) == 0 {
		return []string{DefaultKueueQuotaResources}
	}
	return r.QuotaResources
}

// ClusterQueueName is the ClusterQueue of a tenant.
func ClusterQueueName(tenant string) string { return "gryvia-" + tenant }

// FlavorName is the ResourceFlavor of a GPU type.
func FlavorName(gpuType string) string {
	n := nonDNS.ReplaceAllString(strings.ToLower(gpuType), "-")
	return "gryvia-" + strings.Trim(n, "-")
}

func managedLabels(tenant string) map[string]interface{} {
	return map[string]interface{}{kueueManagedByLabel: kueueManagedBy, kueueTenantLabel: tenant}
}

// quotaSource says what the nominal quota is and where it came from.
type quotaSource struct {
	Nominal string // quantity
	Source  string // tenant | quota | unlimited
}

// tenantQuota picks the nominal quota: spec.quotas.concurrentGPUs of the tenant, else maxGPUs of a
// GryviaQuota listing the tenant namespace, else unlimited.
func tenantQuota(tenant *gryviav1.GryviaTenant, quotas []gryviav1.GryviaQuota) quotaSource {
	if tenant.Spec.Quotas != nil && tenant.Spec.Quotas.ConcurrentGPUs > 0 {
		return quotaSource{fmt.Sprintf("%d", tenant.Spec.Quotas.ConcurrentGPUs), "tenant"}
	}
	ns := tenantNamespaceName(tenant.Name)
	for _, q := range quotas {
		for _, n := range q.Spec.Namespaces {
			if n == ns && q.Spec.GPUQuota.MaxGPUs > 0 {
				return quotaSource{fmt.Sprintf("%d", q.Spec.GPUQuota.MaxGPUs), "quota"}
			}
		}
	}
	return quotaSource{kueueUnlimitedQuota, "unlimited"}
}

func tenantNamespaceName(tenant string) string { return tenantNamespacePrefix + tenant }

// gpuTypes returns the sorted distinct gpuType of the enabled SKUs.
func gpuTypes(skus []gryviav1.GryviaGpuSku) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range skus {
		if s.Spec.Enabled != nil && !*s.Spec.Enabled {
			continue
		}
		t := s.Spec.GpuType
		// The type becomes a node label value: skip what could never be one.
		if t == "" || seen[t] || len(validation.IsValidLabelValue(t)) > 0 {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

func newObject(kind, name, namespace string, labels map[string]interface{}, spec map[string]interface{}) *unstructured.Unstructured {
	md := map[string]interface{}{"name": name}
	if namespace != "" {
		md["namespace"] = namespace
	}
	if labels != nil {
		md["labels"] = labels
	}
	obj := map[string]interface{}{"apiVersion": kueueAPIVersion, "kind": kind, "metadata": md}
	if spec != nil {
		obj["spec"] = spec
	}
	return &unstructured.Unstructured{Object: obj}
}

// TopologyLevels are the node labels of a generated Kueue Topology, from the widest domain to the node.
var TopologyLevels = []string{"gryvia.io/ib-block", "gryvia.io/rack", "kubernetes.io/hostname"}

// BuildTopology returns the cluster-scoped Kueue Topology Gryvia generates for topology-aware scheduling.
func BuildTopology(name string) *unstructured.Unstructured {
	levels := []interface{}{}
	for _, l := range TopologyLevels {
		levels = append(levels, map[string]interface{}{"nodeLabel": l})
	}
	return newObject("Topology", name, "", map[string]interface{}{kueueManagedByLabel: kueueManagedBy}, map[string]interface{}{"levels": levels})
}

// BuildFlavors returns the ResourceFlavors of a tenant's queue: the default one and, when types are
// given, one per GPU type. Flavors are shared by all tenants, so they carry no tenant label.
func BuildFlavors(gpuTypeNames []string) []*unstructured.Unstructured {
	shared := map[string]interface{}{kueueManagedByLabel: kueueManagedBy}
	out := []*unstructured.Unstructured{
		newObject("ResourceFlavor", KueueDefaultFlavor, "", shared, nil),
		newObject("ResourceFlavor", KueueComputeFlavor, "", shared, nil),
	}
	for _, t := range gpuTypeNames {
		out = append(out, newObject("ResourceFlavor", FlavorName(t), "", shared, map[string]interface{}{
			"nodeLabels": map[string]interface{}{gpuNodeLabel: t},
		}))
	}
	return out
}

// BuildClusterQueue returns the tenant's ClusterQueue. quotaResources get nominalQuota q.Nominal in
// the first resource group (flavors: one per gpu type in order, then the default flavor); cpu and
// memory not among them are declared with effectively unlimited quota in a second group.
func BuildClusterQueue(tenant string, q quotaSource, quotaResources, gpuTypeNames []string) *unstructured.Unstructured {
	flavorQuota := func(flavor string, res []string, nominal func(string) string) map[string]interface{} {
		rs := []interface{}{}
		for _, name := range res {
			rs = append(rs, map[string]interface{}{"name": name, "nominalQuota": nominal(name)})
		}
		return map[string]interface{}{"name": flavor, "resources": rs}
	}
	covered := func(res []string) []interface{} {
		c := []interface{}{}
		for _, n := range res {
			c = append(c, n)
		}
		return c
	}

	// Type flavors first: Kueue takes the first flavor that fits the job's node selector, so a job
	// naming a GPU type lands in its flavor; the default flavor (no node labels) comes last.
	flavors := []interface{}{}
	for _, t := range gpuTypeNames {
		flavors = append(flavors, flavorQuota(FlavorName(t), quotaResources, func(string) string { return q.Nominal }))
	}
	flavors = append(flavors, flavorQuota(KueueDefaultFlavor, quotaResources, func(string) string { return q.Nominal }))
	groups := []interface{}{map[string]interface{}{"coveredResources": covered(quotaResources), "flavors": flavors}}

	var rest []string
	for _, n := range []string{"cpu", "memory"} {
		if !containsString(quotaResources, n) {
			rest = append(rest, n)
		}
	}
	if len(rest) > 0 {
		groups = append(groups, map[string]interface{}{
			"coveredResources": covered(rest),
			"flavors": []interface{}{flavorQuota(KueueComputeFlavor, rest, func(n string) string {
				if n == "cpu" {
					return kueueUnlimitedCPU
				}
				return kueueUnlimitedMemory
			})},
		})
	}

	obj := newObject("ClusterQueue", ClusterQueueName(tenant), "", managedLabels(tenant), map[string]interface{}{
		"cohort": KueueCohort,
		"namespaceSelector": map[string]interface{}{
			"matchLabels": map[string]interface{}{"kubernetes.io/metadata.name": tenantNamespaceName(tenant)},
		},
		"queueingStrategy": "BestEffortFIFO",
		"preemption": map[string]interface{}{
			"withinClusterQueue":  "LowerPriority",
			"reclaimWithinCohort": "Any",
			"borrowWithinCohort":  map[string]interface{}{"policy": "LowerPriority"},
		},
		"resourceGroups": groups,
	})
	obj.SetAnnotations(map[string]string{kueueQuotaSourceAnnotation: q.Source})
	return obj
}

// BuildLocalQueue returns the LocalQueue "gryvia" of the tenant namespace.
func BuildLocalQueue(tenant string) *unstructured.Unstructured {
	return newObject("LocalQueue", KueueLocalQueueName, tenantNamespaceName(tenant), managedLabels(tenant), map[string]interface{}{
		"clusterQueue": ClusterQueueName(tenant),
	})
}

// Desired returns every object of a tenant in creation order: flavors, ClusterQueue, LocalQueue.
func (r *GryviaKueueReconciler) desired(tenant *gryviav1.GryviaTenant, quotas []gryviav1.GryviaQuota, skus []gryviav1.GryviaGpuSku) []*unstructured.Unstructured {
	var typeNames []string
	if r.GPUTypeFlavors {
		typeNames = gpuTypes(skus)
	}
	objs := BuildFlavors(typeNames)
	if r.TopologyName != "" && r.GenerateTopology {
		objs = append([]*unstructured.Unstructured{BuildTopology(r.TopologyName)}, objs...)
	}
	if r.TopologyName != "" {
		for _, f := range objs {
			if f.GetKind() != "ResourceFlavor" {
				continue
			}
			_ = unstructured.SetNestedField(f.Object, r.TopologyName, "spec", "topologyName")
			labels, _, _ := unstructured.NestedStringMap(f.Object, "spec", "nodeLabels")
			if labels == nil {
				labels = map[string]string{}
			}
			labels["kubernetes.io/os"] = "linux"
			_ = unstructured.SetNestedStringMap(f.Object, labels, "spec", "nodeLabels")
		}
	}
	cq := BuildClusterQueue(tenant.Name, tenantQuota(tenant, quotas), r.quotaResources(), typeNames)
	if r.FairSharing {
		_ = unstructured.SetNestedField(cq.Object, "1", "spec", "fairSharing", "weight")
	}
	if r.AdmissionCheck != "" {
		_ = unstructured.SetNestedSlice(cq.Object, []interface{}{map[string]interface{}{"name": r.AdmissionCheck}}, "spec", "admissionChecksStrategy", "admissionChecks")
	}
	objs = append(objs, cq)
	objs = append(objs, BuildLocalQueue(tenant.Name))
	return objs
}

func (r *GryviaKueueReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	tenant := &gryviav1.GryviaTenant{}
	if err := r.Get(ctx, req.NamespacedName, tenant); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !tenant.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(tenant, KueueFinalizer) {
			return ctrl.Result{}, nil
		}
		if err := r.cleanup(ctx, tenant.Name); err != nil {
			return ctrl.Result{}, err
		}
		base := tenant.DeepCopy()
		controllerutil.RemoveFinalizer(tenant, KueueFinalizer)
		return ctrl.Result{}, r.Patch(ctx, tenant, client.MergeFrom(base))
	}

	if !controllerutil.ContainsFinalizer(tenant, KueueFinalizer) {
		base := tenant.DeepCopy()
		controllerutil.AddFinalizer(tenant, KueueFinalizer)
		if err := r.Patch(ctx, tenant, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
	}

	// The LocalQueue needs the namespace, which the tenant controller creates.
	if err := r.Get(ctx, types.NamespacedName{Name: tenantNamespaceName(tenant.Name)}, &corev1.Namespace{}); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}

	quotas := &gryviav1.GryviaQuotaList{}
	if err := r.List(ctx, quotas); err != nil {
		return ctrl.Result{}, fmt.Errorf("list GryviaQuota: %w", err)
	}
	skus := &gryviav1.GryviaGpuSkuList{}
	if r.GPUTypeFlavors {
		if err := r.List(ctx, skus); err != nil {
			return ctrl.Result{}, fmt.Errorf("list GryviaGpuSku: %w", err)
		}
	}

	for _, obj := range r.desired(tenant, quotas.Items, skus.Items) {
		if err := r.apply(ctx, obj); err != nil {
			if meta.IsNoMatchError(err) {
				logger.Info("Kueue CRDs not found: is Kueue installed?", "kind", obj.GetKind())
				return ctrl.Result{RequeueAfter: time.Minute}, nil
			}
			return ctrl.Result{}, fmt.Errorf("%s %s: %w", obj.GetKind(), obj.GetName(), err)
		}
	}
	return ctrl.Result{RequeueAfter: 2 * time.Minute}, nil
}

// apply creates the object or updates its spec (and our labels/annotations) if it differs from what we
// want. Objects that exist without our managed-by label are left alone.
func (r *GryviaKueueReconciler) apply(ctx context.Context, want *unstructured.Unstructured) error {
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
		log.FromContext(ctx).Info("Kueue object exists and is not managed by Gryvia, leaving it alone",
			"kind", want.GetKind(), "name", want.GetName(), "namespace", want.GetNamespace())
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
	if wantSpec != nil {
		_ = unstructured.SetNestedMap(have.Object, wantSpec, "spec")
	}
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

// subsetEqual reports whether every field of want is present and equal in have (the API server adds
// defaults to have, which must not count as drift).
func subsetEqual(want, have interface{}) bool {
	switch w := want.(type) {
	case map[string]interface{}:
		h, ok := have.(map[string]interface{})
		if !ok {
			return false
		}
		for k, wv := range w {
			hv, present := h[k]
			if !present || !subsetEqual(wv, hv) {
				return false
			}
		}
		return true
	case []interface{}:
		h, ok := have.([]interface{})
		if !ok || len(h) != len(w) {
			return false
		}
		for i := range w {
			if !subsetEqual(w[i], h[i]) {
				return false
			}
		}
		return true
	default:
		return fmt.Sprint(want) == fmt.Sprint(have)
	}
}

// cleanup deletes the tenant's ClusterQueue and LocalQueue (only if we manage them). Flavors are shared
// between tenants and stay. Missing CRDs and missing objects count as done.
func (r *GryviaKueueReconciler) cleanup(ctx context.Context, tenant string) error {
	for _, obj := range []*unstructured.Unstructured{BuildLocalQueue(tenant), BuildClusterQueue(tenant, quotaSource{}, nil, nil)} {
		have := &unstructured.Unstructured{}
		have.SetGroupVersionKind(obj.GroupVersionKind())
		err := r.Get(ctx, client.ObjectKeyFromObject(obj), have)
		if errors.IsNotFound(err) || meta.IsNoMatchError(err) {
			continue
		}
		if err != nil {
			return err
		}
		if have.GetLabels()[kueueManagedByLabel] != kueueManagedBy || have.GetLabels()[kueueTenantLabel] != tenant {
			continue
		}
		if err := r.Delete(ctx, have); err != nil && !errors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (r *GryviaKueueReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("gryviakueue").
		For(&gryviav1.GryviaTenant{}).
		Complete(r)
}
