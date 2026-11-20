// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controllers

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	gryviav1 "github.com/zyvorai/gryvia/operators/quota-operator/api/v1"
)

var updateGolden = flag.Bool("update", false, "rewrite testdata/*.golden.yaml")

func kueueBoolPtr(b bool) *bool { return &b }

func kueueTenant(name string, gpus int) *gryviav1.GryviaTenant {
	t := &gryviav1.GryviaTenant{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if gpus > 0 {
		t.Spec.Quotas = &gryviav1.TenantQuotas{ConcurrentGPUs: gpus}
	}
	return t
}

func kueueSku(name, gpuType string, enabled bool) *gryviav1.GryviaGpuSku {
	return &gryviav1.GryviaGpuSku{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       gryviav1.GryviaGpuSkuSpec{GpuType: gpuType, HourlyRate: 1, Enabled: kueueBoolPtr(enabled)},
	}
}

func renderYAML(t *testing.T, objs []*unstructured.Unstructured) string {
	t.Helper()
	var b strings.Builder
	for i, o := range objs {
		out, err := yaml.Marshal(o.Object)
		if err != nil {
			t.Fatal(err)
		}
		if i > 0 {
			b.WriteString("---\n")
		}
		b.Write(out)
	}
	return b.String()
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden.yaml")
	if *updateGolden {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./controllers -run Golden -update)", err)
	}
	if string(want) != got {
		t.Errorf("%s differs from golden (rerun with -update if intended):\n--- got ---\n%s", path, got)
	}
}

func TestKueueObjects_Golden(t *testing.T) {
	quotaFor := func(ns string, gpus int) gryviav1.GryviaQuota {
		return gryviav1.GryviaQuota{
			ObjectMeta: metav1.ObjectMeta{Name: "team"},
			Spec: gryviav1.GryviaQuotaSpec{Team: "team", Namespaces: []string{"other", ns},
				GPUQuota: gryviav1.GPUQuotaSpec{MaxGPUs: gpus}},
		}
	}
	skus := []gryviav1.GryviaGpuSku{*kueueSku("h100", "H100", true), *kueueSku("a100", "A100", true),
		*kueueSku("a100-b", "A100", true), *kueueSku("old", "V100", false), *kueueSku("l4", "L4 (24GB)", true)}

	cases := []struct {
		name   string
		r      GryviaKueueReconciler
		tenant *gryviav1.GryviaTenant
		quotas []gryviav1.GryviaQuota
		skus   []gryviav1.GryviaGpuSku
	}{
		{name: "kueue_tenant_gpu", tenant: kueueTenant("q1", 2)},
		{name: "kueue_tenant_gpu_types", r: GryviaKueueReconciler{GPUTypeFlavors: true}, tenant: kueueTenant("q1", 2), skus: skus},
		{name: "kueue_tenant_quota_fallback", tenant: kueueTenant("ml", 0), quotas: []gryviav1.GryviaQuota{quotaFor("tenant-ml", 8)}},
		{name: "kueue_tenant_unlimited", tenant: kueueTenant("free", 0)},
		{name: "kueue_tenant_cpu_resource", r: GryviaKueueReconciler{QuotaResources: []string{"cpu"}}, tenant: kueueTenant("q1", 2)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			checkGolden(t, c.name, renderYAML(t, c.r.desired(c.tenant, c.quotas, c.skus)))
		})
	}
}

func TestFlavorName(t *testing.T) {
	for in, want := range map[string]string{"H100": "gryvia-h100", "L4 (24GB)": "gryvia-l4-24gb", "A100_80": "gryvia-a100-80"} {
		if got := FlavorName(in); got != want {
			t.Errorf("FlavorName(%q) = %q, want %q", in, got, want)
		}
	}
}

func newKueueReconciler(objs ...client.Object) (*GryviaKueueReconciler, client.Client) {
	c := fake.NewClientBuilder().WithScheme(newQuotaTestScheme()).WithObjects(objs...).Build()
	return &GryviaKueueReconciler{Client: c, Scheme: newQuotaTestScheme()}, c
}

func tenantNS(name string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-" + name}}
}

func getK(t *testing.T, c client.Client, kind, ns, name string) (*unstructured.Unstructured, bool) {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(kueueGVK(kind))
	err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, u)
	if err != nil {
		return nil, false
	}
	return u, true
}

func reconcileTenant(t *testing.T, r *GryviaKueueReconciler, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func nominal(t *testing.T, cq *unstructured.Unstructured) string {
	t.Helper()
	groups, _, _ := unstructured.NestedSlice(cq.Object, "spec", "resourceGroups")
	fl := groups[0].(map[string]interface{})["flavors"].([]interface{})
	res := fl[0].(map[string]interface{})["resources"].([]interface{})
	return res[0].(map[string]interface{})["nominalQuota"].(string)
}

func TestKueueReconcile_CreateUpdateDelete(t *testing.T) {
	tenant := kueueTenant("q1", 2)
	r, c := newKueueReconciler(tenant, tenantNS("q1"))

	reconcileTenant(t, r, "q1")
	cq, ok := getK(t, c, "ClusterQueue", "", "gryvia-q1")
	if !ok {
		t.Fatal("ClusterQueue not created")
	}
	if _, ok := getK(t, c, "LocalQueue", "tenant-q1", "gryvia"); !ok {
		t.Fatal("LocalQueue not created")
	}
	if _, ok := getK(t, c, "ResourceFlavor", "", "gryvia-default"); !ok {
		t.Fatal("default ResourceFlavor not created")
	}
	if nominal(t, cq) != "2" {
		t.Errorf("nominal = %s", nominal(t, cq))
	}
	got := &gryviav1.GryviaTenant{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "q1"}, got)
	if !hasFinalizer(got, KueueFinalizer) {
		t.Error("tenant needs the Kueue finalizer to clean up cluster-scoped objects")
	}

	// Update: quota raised; a server-side default field must not be treated as drift.
	cq.Object["spec"].(map[string]interface{})["flavorFungibility"] = map[string]interface{}{"whenCanBorrow": "MayStopSearch"}
	if err := c.Update(context.Background(), cq); err != nil {
		t.Fatal(err)
	}
	reconcileTenant(t, r, "q1")
	cq, _ = getK(t, c, "ClusterQueue", "", "gryvia-q1")
	if _, has, _ := unstructured.NestedMap(cq.Object, "spec", "flavorFungibility"); !has {
		t.Error("unchanged spec must not be rewritten (would drop server defaults)")
	}
	got.Spec.Quotas.ConcurrentGPUs = 5
	if err := c.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	reconcileTenant(t, r, "q1")
	cq, _ = getK(t, c, "ClusterQueue", "", "gryvia-q1")
	if nominal(t, cq) != "5" {
		t.Errorf("nominal after update = %s, want 5", nominal(t, cq))
	}

	// Delete: the tenant carries a deletionTimestamp; queues go, shared flavor stays, finalizer is removed.
	// (The fake client refuses to set a deletionTimestamp without a finalizer, ours is there.)
	if err := c.Delete(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	reconcileTenant(t, r, "q1")
	if _, ok := getK(t, c, "ClusterQueue", "", "gryvia-q1"); ok {
		t.Error("ClusterQueue must be deleted with the tenant")
	}
	if _, ok := getK(t, c, "LocalQueue", "tenant-q1", "gryvia"); ok {
		t.Error("LocalQueue must be deleted with the tenant")
	}
	if _, ok := getK(t, c, "ResourceFlavor", "", "gryvia-default"); !ok {
		t.Error("shared ResourceFlavor must stay")
	}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "q1"}, &gryviav1.GryviaTenant{}); err == nil {
		t.Error("tenant must be gone once the finalizer is removed")
	}
}

func hasFinalizer(t *gryviav1.GryviaTenant, f string) bool {
	for _, x := range t.Finalizers {
		if x == f {
			return true
		}
	}
	return false
}

func TestKueueReconcile_WaitsForNamespace(t *testing.T) {
	r, c := newKueueReconciler(kueueTenant("q2", 1))
	res := reconcileTenant(t, r, "q2")
	if res.RequeueAfter != 5*time.Second {
		t.Errorf("requeue = %v", res.RequeueAfter)
	}
	if _, ok := getK(t, c, "LocalQueue", "tenant-q2", "gryvia"); ok {
		t.Error("LocalQueue created without namespace")
	}
}

func TestKueueReconcile_LeavesUnmanagedObjectsAlone(t *testing.T) {
	lq := &unstructured.Unstructured{Object: map[string]interface{}{"spec": map[string]interface{}{"clusterQueue": "mine"}}}
	lq.SetGroupVersionKind(kueueGVK("LocalQueue"))
	lq.SetNamespace("tenant-q3")
	lq.SetName("gryvia")
	r, c := newKueueReconciler(kueueTenant("q3", 1), tenantNS("q3"), lq)
	reconcileTenant(t, r, "q3")
	got, _ := getK(t, c, "LocalQueue", "tenant-q3", "gryvia")
	if cq, _, _ := unstructured.NestedString(got.Object, "spec", "clusterQueue"); cq != "mine" {
		t.Errorf("unmanaged LocalQueue was modified: %s", cq)
	}
	// and it survives tenant cleanup
	if err := r.cleanup(context.Background(), "q3"); err != nil {
		t.Fatal(err)
	}
	if _, ok := getK(t, c, "LocalQueue", "tenant-q3", "gryvia"); !ok {
		t.Error("cleanup deleted an object it does not manage")
	}
}

func TestKueueReconcile_QuotaFallbackAndResources(t *testing.T) {
	quota := &gryviav1.GryviaQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "team"},
		Spec: gryviav1.GryviaQuotaSpec{Team: "team", Namespaces: []string{"tenant-q4"},
			GPUQuota: gryviav1.GPUQuotaSpec{MaxGPUs: 16}},
	}
	r, c := newKueueReconciler(kueueTenant("q4", 0), tenantNS("q4"), quota)
	r.QuotaResources = []string{"cpu"}
	reconcileTenant(t, r, "q4")
	cq, _ := getK(t, c, "ClusterQueue", "", "gryvia-q4")
	if nominal(t, cq) != "16" {
		t.Errorf("nominal = %s, want GryviaQuota maxGPUs 16", nominal(t, cq))
	}
	if cq.GetAnnotations()[kueueQuotaSourceAnnotation] != "quota" {
		t.Errorf("annotations = %v", cq.GetAnnotations())
	}
	groups, _, _ := unstructured.NestedSlice(cq.Object, "spec", "resourceGroups")
	if len(groups) != 2 {
		t.Fatalf("resource groups = %d, want cpu group + memory group", len(groups))
	}
	cov := groups[1].(map[string]interface{})["coveredResources"].([]interface{})
	if len(cov) != 1 || cov[0] != "memory" {
		t.Errorf("second group covers %v, want [memory] (cpu is the quota resource)", cov)
	}
}

func TestSubsetEqual(t *testing.T) {
	want := map[string]interface{}{"a": "1", "l": []interface{}{map[string]interface{}{"x": int64(1)}}}
	have := map[string]interface{}{"a": "1", "extra": "default", "l": []interface{}{map[string]interface{}{"x": int64(1), "y": "d"}}}
	if !subsetEqual(want, have) {
		t.Error("defaults added by the server must not count as drift")
	}
	have["a"] = "2"
	if subsetEqual(want, have) {
		t.Error("changed value must count as drift")
	}
	if subsetEqual(map[string]interface{}{"l": []interface{}{"a", "b"}}, map[string]interface{}{"l": []interface{}{"a"}}) {
		t.Error("different list length must count as drift")
	}
}
