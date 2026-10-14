package controllers

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	gryviav1 "github.com/zyvorai/gryvia/operators/quota-operator/api/v1"
)

func clusterQueueOf(t *testing.T, r *GryviaKueueReconciler, tenant *gryviav1.GryviaTenant) *unstructured.Unstructured {
	t.Helper()
	for _, o := range r.desired(tenant, nil, nil) {
		if o.GetKind() == "ClusterQueue" {
			return o
		}
	}
	t.Fatal("no ClusterQueue generated")
	return nil
}

func TestFairShareWeightLimitsAndCohort(t *testing.T) {
	borrow, lend := 4, 2
	tenant := &gryviav1.GryviaTenant{
		ObjectMeta: metav1.ObjectMeta{Name: "research"},
		Spec: gryviav1.GryviaTenantSpec{Quotas: &gryviav1.TenantQuotas{
			ConcurrentGPUs: 16, FairShareWeight: 3, BorrowingLimit: &borrow, LendingLimit: &lend, Cohort: "org-a",
		}},
	}
	cq := clusterQueueOf(t, &GryviaKueueReconciler{FairSharing: true}, tenant)

	if w, _, _ := unstructured.NestedString(cq.Object, "spec", "fairSharing", "weight"); w != "3" {
		t.Errorf("weight = %q, want 3", w)
	}
	if c, _, _ := unstructured.NestedString(cq.Object, "spec", "cohort"); c != "org-a" {
		t.Errorf("cohort = %q, want org-a", c)
	}
	groups, _, _ := unstructured.NestedSlice(cq.Object, "spec", "resourceGroups")
	gpuGroup := groups[0].(map[string]interface{})
	flavors := gpuGroup["flavors"].([]interface{})
	res := flavors[0].(map[string]interface{})["resources"].([]interface{})[0].(map[string]interface{})
	if res["nominalQuota"] != "16" || res["borrowingLimit"] != "4" || res["lendingLimit"] != "2" {
		t.Errorf("GPU resource = %v, want nominal 16 borrow 4 lend 2", res)
	}
	// the unlimited cpu/memory group must not get limits
	rest := groups[1].(map[string]interface{})["flavors"].([]interface{})[0].(map[string]interface{})["resources"].([]interface{})
	for _, r := range rest {
		m := r.(map[string]interface{})
		if _, ok := m["borrowingLimit"]; ok {
			t.Errorf("cpu/memory resource got a borrowingLimit: %v", m)
		}
	}
}

func TestFairShareDefaults(t *testing.T) {
	tenant := &gryviav1.GryviaTenant{ObjectMeta: metav1.ObjectMeta{Name: "plain"}}
	cq := clusterQueueOf(t, &GryviaKueueReconciler{FairSharing: true}, tenant)
	if w, _, _ := unstructured.NestedString(cq.Object, "spec", "fairSharing", "weight"); w != "1" {
		t.Errorf("default weight = %q, want 1", w)
	}
	if c, _, _ := unstructured.NestedString(cq.Object, "spec", "cohort"); c != KueueCohort {
		t.Errorf("default cohort = %q, want %q", c, KueueCohort)
	}
	// without --kueue-fair-sharing a weight on the tenant is ignored
	tenant.Spec.Quotas = &gryviav1.TenantQuotas{FairShareWeight: 5}
	cq = clusterQueueOf(t, &GryviaKueueReconciler{}, tenant)
	if _, found, _ := unstructured.NestedString(cq.Object, "spec", "fairSharing", "weight"); found {
		t.Error("fairSharing set although the flag is off")
	}
}
