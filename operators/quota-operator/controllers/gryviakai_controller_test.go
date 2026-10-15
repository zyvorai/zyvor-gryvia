package controllers

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gryviav1 "github.com/zyvorai/gryvia/operators/quota-operator/api/v1"
)

func newKAIReconciler(objs ...client.Object) (*GryviaKAIReconciler, client.Client) {
	c := fake.NewClientBuilder().WithScheme(newQuotaTestScheme()).WithObjects(objs...).Build()
	return &GryviaKAIReconciler{Client: c, Scheme: newQuotaTestScheme()}, c
}

func getKAIQueue(t *testing.T, c client.Client, name string) (*unstructured.Unstructured, bool) {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "scheduling.run.ai", Version: "v2", Kind: "Queue"})
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, u); err != nil {
		return nil, false
	}
	return u, true
}

func reconcileKAI(t *testing.T, r *GryviaKAIReconciler, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func gpuOf(t *testing.T, q *unstructured.Unstructured) (quota, limit, weight int64) {
	t.Helper()
	g, _, _ := unstructured.NestedMap(q.Object, "spec", "resources", "gpu")
	return g["quota"].(int64), g["limit"].(int64), g["overQuotaWeight"].(int64)
}

func TestKAIReconcile_CreateUpdateDelete(t *testing.T) {
	tenant := kueueTenant("k1", 2)
	r, c := newKAIReconciler(tenant, tenantNS("k1"))
	reconcileKAI(t, r, "k1")

	parent, ok := getKAIQueue(t, c, KAIParentQueue)
	if !ok {
		t.Fatal("parent queue not created")
	}
	if q, l, _ := gpuOf(t, parent); q != -1 || l != -1 {
		t.Errorf("parent gpu quota/limit = %d/%d, want -1/-1", q, l)
	}
	leaf, ok := getKAIQueue(t, c, "gryvia-k1")
	if !ok {
		t.Fatal("tenant queue not created")
	}
	if p, _, _ := unstructured.NestedString(leaf.Object, "spec", "parentQueue"); p != KAIParentQueue {
		t.Errorf("parentQueue = %q", p)
	}
	if q, l, w := gpuOf(t, leaf); q != 2 || l != -1 || w != 1 {
		t.Errorf("gpu quota/limit/weight = %d/%d/%d, want 2/-1/1", q, l, w)
	}
	for _, res := range []string{"cpu", "memory"} {
		m, _, _ := unstructured.NestedMap(leaf.Object, "spec", "resources", res)
		if m["quota"].(int64) != -1 || m["limit"].(int64) != -1 {
			t.Errorf("%s must be unlimited (KAI treats an unset limit as 0): %v", res, m)
		}
	}
	ns := &corev1.Namespace{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "tenant-k1"}, ns)
	if ns.Labels[KAIQueueNamespaceLabel] != "gryvia-k1" {
		t.Errorf("namespace label = %q", ns.Labels[KAIQueueNamespaceLabel])
	}

	got := &gryviav1.GryviaTenant{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "k1"}, got)
	if !hasFinalizer(got, KAIFinalizer) {
		t.Fatal("tenant needs the KAI finalizer")
	}
	borrow := 3
	got.Spec.Quotas.ConcurrentGPUs, got.Spec.Quotas.BorrowingLimit, got.Spec.Quotas.FairShareWeight = 4, &borrow, 2
	if err := c.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	reconcileKAI(t, r, "k1")
	leaf, _ = getKAIQueue(t, c, "gryvia-k1")
	if q, l, w := gpuOf(t, leaf); q != 4 || l != 7 || w != 2 {
		t.Errorf("after update gpu quota/limit/weight = %d/%d/%d, want 4/7/2", q, l, w)
	}

	if err := c.Delete(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	reconcileKAI(t, r, "k1")
	if _, ok := getKAIQueue(t, c, "gryvia-k1"); ok {
		t.Error("tenant queue must be deleted with the tenant")
	}
	if _, ok := getKAIQueue(t, c, KAIParentQueue); !ok {
		t.Error("shared parent queue must stay")
	}
}

func TestKAIReconcile_UnlimitedTenantAndGryviaQuota(t *testing.T) {
	r, c := newKAIReconciler(kueueTenant("free", 0), tenantNS("free"))
	reconcileKAI(t, r, "free")
	leaf, _ := getKAIQueue(t, c, "gryvia-free")
	if q, l, _ := gpuOf(t, leaf); q != -1 || l != -1 {
		t.Errorf("tenant without quota: %d/%d, want -1/-1", q, l)
	}

	quota := &gryviav1.GryviaQuota{ObjectMeta: metav1.ObjectMeta{Name: "team"},
		Spec: gryviav1.GryviaQuotaSpec{Namespaces: []string{"tenant-capped"}, GPUQuota: gryviav1.GPUQuotaSpec{MaxGPUs: 6}}}
	r, c = newKAIReconciler(kueueTenant("capped", 0), tenantNS("capped"), quota)
	reconcileKAI(t, r, "capped")
	leaf, _ = getKAIQueue(t, c, "gryvia-capped")
	if q, _, _ := gpuOf(t, leaf); q != 6 {
		t.Errorf("GryviaQuota fallback = %d, want 6", q)
	}
}

func TestKAIReconcile_WaitsForNamespaceAndLeavesUnmanagedQueues(t *testing.T) {
	mine := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{"resources": map[string]interface{}{"gpu": map[string]interface{}{"quota": int64(9)}}}}}
	mine.SetGroupVersionKind(schema.GroupVersionKind{Group: "scheduling.run.ai", Version: "v2", Kind: "Queue"})
	mine.SetName("gryvia-k3")
	r, c := newKAIReconciler(kueueTenant("k3", 1), mine)
	if res := reconcileKAI(t, r, "k3"); res.RequeueAfter == 0 {
		t.Error("must requeue until the tenant namespace exists")
	}
	got, _ := getKAIQueue(t, c, "gryvia-k3")
	if q, _, _ := unstructured.NestedInt64(got.Object, "spec", "resources", "gpu", "quota"); q != 9 {
		t.Errorf("unmanaged queue was modified: quota %d", q)
	}
	if err := r.cleanup(context.Background(), "k3"); err != nil {
		t.Fatal(err)
	}
	if _, ok := getKAIQueue(t, c, "gryvia-k3"); !ok {
		t.Error("cleanup deleted a queue it does not manage")
	}
}
