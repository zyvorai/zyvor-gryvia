package controllers

import (
	"context"
	gryviav1 "github.com/zyvorai/gryvia/operators/quota-operator/api/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
	"time"
)

func TestChargebackFrozenPricesCurrencyAndDuplicates(t *testing.T) {
	rec := budgetRec("one", "ns", "tenant", 25, 2, "USD", true)
	rec.Spec.JobUID = "j1"
	quota := &gryviav1.GryviaQuota{ObjectMeta: metav1.ObjectMeta{Name: "q"}, Spec: gryviav1.GryviaQuotaSpec{Team: "team", Namespaces: []string{"ns"}}}
	cb := &gryviav1.GryviaChargeback{Spec: gryviav1.GryviaChargebackSpec{Period: gryviav1.ChargebackPeriod{Type: "monthly"}, CostCenters: []gryviav1.CostCenter{{ID: "cc", Teams: []string{"team"}}}}}
	c := fake.NewClientBuilder().WithScheme(newQuotaTestScheme()).WithObjects(rec, quota).Build()
	r := &GryviaChargebackReconciler{Client: c}
	if err := r.calculate(context.Background(), cb, budgetNow); err != nil {
		t.Fatal(err)
	}
	if cb.Status.CurrentPeriod.TotalCost != 25 || cb.Status.CurrentPeriod.CostCenters[0].Cost != 25 {
		t.Fatal("historic cost was repriced")
	}
	cb.Spec.Pricing.Currency = "INR"
	if err := r.calculate(context.Background(), cb, budgetNow); err == nil {
		t.Fatal("mixed currency silently converted")
	}
	cb.Spec.Pricing.Currency = "USD"
	second := rec.DeepCopy()
	second.Name = "duplicate"
	second.ResourceVersion = ""
	if err := c.Create(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if err := r.calculate(context.Background(), cb, budgetNow); err == nil {
		t.Fatal("double billed duplicate UID")
	}
}
func TestBudgetEventsAreIdempotent(t *testing.T) {
	b := &gryviav1.GryviaBudget{ObjectMeta: metav1.ObjectMeta{Name: "limit", UID: "b"}, Status: gryviav1.GryviaBudgetStatus{Alerts: []gryviav1.BudgetAlertEvent{{Timestamp: metav1.NewTime(time.Now()), Threshold: 80, Message: "crossed"}}}}
	c := fake.NewClientBuilder().WithScheme(newQuotaTestScheme()).Build()
	r := &GryviaBudgetReconciler{Client: c}
	for i := 0; i < 2; i++ {
		if err := r.publishAlerts(context.Background(), b); err != nil {
			t.Fatal(err)
		}
	}
	events := &corev1.EventList{}
	if err := c.List(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	if len(events.Items) != 1 || events.Items[0].Namespace != "default" {
		t.Fatal("event duplicated or invalid cluster-event namespace")
	}
}
func TestKueueAdvancedConfiguration(t *testing.T) {
	r := &GryviaKueueReconciler{FairSharing: true, TopologyName: "rack", AdmissionCheck: "multikueue"}
	objs := r.desired(&gryviav1.GryviaTenant{ObjectMeta: metav1.ObjectMeta{Name: "team"}}, nil, nil)
	topology, _, _ := unstructured.NestedString(objs[0].Object, "spec", "topologyName")
	if topology != "rack" {
		t.Fatal("topology missing")
	}
	for _, obj := range objs {
		if obj.GetKind() == "ClusterQueue" {
			weight, _, _ := unstructured.NestedString(obj.Object, "spec", "fairSharing", "weight")
			checks, _, _ := unstructured.NestedSlice(obj.Object, "spec", "admissionChecksStrategy", "admissionChecks")
			if weight != "1" || len(checks) != 1 || checks[0].(map[string]interface{})["name"] != "multikueue" {
				t.Fatal("advanced settings missing")
			}
		}
	}
}

func TestKueueGeneratedTopology(t *testing.T) {
	r := &GryviaKueueReconciler{TopologyName: "gryvia", GenerateTopology: true}
	objs := r.desired(&gryviav1.GryviaTenant{ObjectMeta: metav1.ObjectMeta{Name: "team"}}, nil, nil)
	if objs[0].GetKind() != "Topology" || objs[0].GetName() != "gryvia" || objs[0].GetLabels()[kueueManagedByLabel] != kueueManagedBy {
		t.Fatalf("first object %s %s %v", objs[0].GetKind(), objs[0].GetName(), objs[0].GetLabels())
	}
	levels, _, _ := unstructured.NestedSlice(objs[0].Object, "spec", "levels")
	if len(levels) != 3 || levels[0].(map[string]interface{})["nodeLabel"] != "gryvia.io/ib-block" || levels[2].(map[string]interface{})["nodeLabel"] != "kubernetes.io/hostname" {
		t.Fatalf("levels %v", levels)
	}
	if _, found, _ := unstructured.NestedString(objs[0].Object, "spec", "topologyName"); found {
		t.Fatal("topologyName belongs on ResourceFlavors only")
	}
	for _, o := range objs[1:] {
		if o.GetKind() == "ResourceFlavor" {
			if name, _, _ := unstructured.NestedString(o.Object, "spec", "topologyName"); name != "gryvia" {
				t.Fatalf("flavor %s topologyName %q", o.GetName(), name)
			}
		}
	}
	r.GenerateTopology = false
	if objs := r.desired(&gryviav1.GryviaTenant{ObjectMeta: metav1.ObjectMeta{Name: "team"}}, nil, nil); objs[0].GetKind() == "Topology" {
		t.Fatal("no Topology without --kueue-generate-topology")
	}
}
