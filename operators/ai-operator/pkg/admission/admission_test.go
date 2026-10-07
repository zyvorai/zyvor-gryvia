package admission

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

var now = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

func gvk(kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: "gryvia.io", Version: "v1alpha1", Kind: kind}
}

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = gryviav1.AddToScheme(s)
	for _, k := range []string{"GryviaQuota", "GryviaBudget", "GryviaUsageRecord", "GryviaGpuSku", "GryviaTenant", "GryviaReservation"} {
		s.AddKnownTypeWithName(gvk(k), &unstructured.Unstructured{})
		s.AddKnownTypeWithName(gvk(k+"List"), &unstructured.UnstructuredList{})
	}
	return s
}

func obj(kind, ns, name string, spec map[string]interface{}) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]interface{}{"spec": spec}}
	u.SetGroupVersionKind(gvk(kind))
	u.SetName(name)
	if ns != "" {
		u.SetNamespace(ns)
	}
	return u
}

func quota(name, team string, nss []string, gpuQuota, budget map[string]interface{}) *unstructured.Unstructured {
	ns := make([]interface{}, len(nss))
	for i, n := range nss {
		ns[i] = n
	}
	spec := map[string]interface{}{"team": team, "namespaces": ns}
	if gpuQuota != nil {
		spec["gpuQuota"] = gpuQuota
	}
	if budget != nil {
		spec["budget"] = budget
	}
	return obj("GryviaQuota", "", name, spec)
}

func budgetObj(name, scopeType, scopeName string, cost float64, block bool, alerts ...map[string]interface{}) *unstructured.Unstructured {
	spec := map[string]interface{}{
		"scope":  map[string]interface{}{"type": scopeType, "name": scopeName},
		"period": map[string]interface{}{"type": "monthly"},
		"limits": map[string]interface{}{"costUSD": cost},
	}
	if block {
		spec["enforcement"] = map[string]interface{}{"enabled": true, "action": "block"}
	}
	if len(alerts) > 0 {
		var as []interface{}
		for _, a := range alerts {
			as = append(as, a)
		}
		spec["alerts"] = as
	}
	return obj("GryviaBudget", "", name, spec)
}

func usage(ns, name string, cost, hours float64, currency string, open bool) *unstructured.Unstructured {
	spec := map[string]interface{}{
		"tenant": strings.TrimPrefix(ns, "tenant-"), "start": now.Add(-3 * time.Hour).Format(time.RFC3339),
		"gpuHours": hours, "cost": cost, "currency": currency, "final": !open,
	}
	if !open {
		spec["end"] = now.Add(-time.Hour).Format(time.RFC3339)
	}
	return obj("GryviaUsageRecord", ns, name, spec)
}

func skuObj(name, gpuType string, rate float64, currency string) *unstructured.Unstructured {
	return obj("GryviaGpuSku", "", name, map[string]interface{}{"gpuType": gpuType, "hourlyRate": rate, "currency": currency})
}

func mkJob(ns, gpuType string, gpus int32) *gryviav1.GryviaAIJob {
	return &gryviav1.GryviaAIJob{
		ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: ns, UID: "uid-j"},
		Spec:       gryviav1.GryviaAIJobSpec{Type: "training", GpuType: gpuType, GPUs: gpus, Image: "x"},
	}
}

func gate(objs ...client.Object) *Gate {
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(objs...).Build()
	return &Gate{Client: c, Now: func() time.Time { return now }}
}

func eval(t *testing.T, g *Gate, j *gryviav1.GryviaAIJob) Decision {
	t.Helper()
	d, err := g.Evaluate(context.Background(), j)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	return d
}

func hard(limit float64) map[string]interface{} {
	return map[string]interface{}{"monthlyBudget": limit, "hardLimit": true}
}

func TestNoObjectsAllows(t *testing.T) {
	if d := eval(t, gate(), mkJob("tenant-a", "H100", 8)); !d.Allow || len(d.Reasons) != 0 {
		t.Errorf("%+v", d)
	}
}

func TestCPUOnlyJobIsNeverGated(t *testing.T) {
	g := gate(quota("q", "a", []string{"tenant-a"}, map[string]interface{}{"maxGPUs": int64(0), "allowedGPUTypes": []interface{}{"A100"}}, hard(0.01)),
		usage("tenant-a", "u", 999, 1, "USD", false))
	if d := eval(t, g, mkJob("tenant-a", "", 0)); !d.Allow {
		t.Errorf("%+v", d)
	}
}

func TestHardQuotaBudgetUnderAndOver(t *testing.T) {
	q := quota("q", "a", []string{"tenant-a"}, nil, hard(1000))
	// 4 H100 for 1h (default) at the default 8.00 = 32.
	under := gate(q, usage("tenant-a", "u1", 100, 10, "USD", false))
	if d := eval(t, under, mkJob("tenant-a", "H100", 4)); !d.Allow {
		t.Errorf("under budget: %+v", d)
	}
	over := gate(q, usage("tenant-a", "u1", 990, 100, "USD", false))
	d := eval(t, over, mkJob("tenant-a", "H100", 4))
	if d.Allow || d.Code != CodeBudget || !strings.Contains(d.Message(), "spend 990.00 + forecast 32.00") {
		t.Errorf("over budget: %+v", d)
	}
}

func TestForecastAloneCrossesTheLimit(t *testing.T) {
	// Spend is 0, but a 16 GPU x 24h job costs 16*24*8 = 3072 > 1000.
	g := gate(quota("q", "a", []string{"tenant-a"}, nil, hard(1000)))
	j := mkJob("tenant-a", "H100", 16)
	j.Spec.Timeout = "24h"
	if d := eval(t, g, j); d.Allow || d.Code != CodeBudget {
		t.Errorf("forecast must count: %+v", d)
	}
	j.Spec.Timeout = "1h" // 128 fits
	if d := eval(t, g, j); !d.Allow {
		t.Errorf("1h job fits: %+v", d)
	}
}

func TestDistributedForecastUsesNodesTimesGPUsPerNode(t *testing.T) {
	g := gate(quota("q", "a", []string{"tenant-a"}, nil, hard(200)))
	j := mkJob("tenant-a", "H100", 8)
	j.Spec.Distributed = &gryviav1.DistributedConfig{Enabled: true, Nodes: 4, GpusPerNode: 8}
	// 32 GPUs x 1h x 8 = 256 > 200 (spec.gpus alone would be 64 and pass).
	d := eval(t, g, j)
	if d.Allow || !strings.Contains(d.Message(), "forecast 256.00 (32 GPUs") {
		t.Errorf("%+v", d)
	}
}

func TestSoftBudgetOnlyWarns(t *testing.T) {
	soft := map[string]interface{}{"monthlyBudget": float64(10), "hardLimit": false}
	g := gate(quota("q", "a", []string{"tenant-a"}, nil, soft))
	d := eval(t, g, mkJob("tenant-a", "H100", 8))
	if !d.Allow || len(d.Warnings) != 1 || !strings.Contains(d.Warnings[0], "soft limit") {
		t.Errorf("%+v", d)
	}
	// GryviaBudget without enforcement is soft too.
	g = gate(budgetObj("b", "tenant", "a", 10, false))
	d = eval(t, g, mkJob("tenant-a", "H100", 8))
	if !d.Allow || len(d.Warnings) != 1 {
		t.Errorf("%+v", d)
	}
}

func TestGryviaBudgetHardAndBlockAlert(t *testing.T) {
	// Enforcement block: refuse at 100 %.
	g := gate(budgetObj("b", "namespace", "tenant-a", 100, true), usage("tenant-a", "u", 90, 5, "USD", false))
	if d := eval(t, g, mkJob("tenant-a", "H100", 2)); d.Allow { // 90 + 16 > 100
		t.Errorf("%+v", d)
	}
	if d := eval(t, g, mkJob("tenant-a", "T4", 2)); !d.Allow { // 90 + 2 = 92
		t.Errorf("%+v", d)
	}
	// A block alert at 50 % lowers the block point.
	g = gate(budgetObj("b", "namespace", "tenant-a", 100, true, map[string]interface{}{"threshold": float64(50), "actions": []interface{}{"block"}}),
		usage("tenant-a", "u", 45, 5, "USD", false))
	d := eval(t, g, mkJob("tenant-a", "T4", 8)) // 45 + 8 = 53 > 50
	if d.Allow || !strings.Contains(d.Message(), "block point (50%") {
		t.Errorf("%+v", d)
	}
	// Records of other tenants and namespaces do not count.
	g = gate(budgetObj("b", "namespace", "tenant-a", 100, true), usage("tenant-b", "u", 5000, 5, "USD", false))
	if d := eval(t, g, mkJob("tenant-a", "H100", 1)); !d.Allow {
		t.Errorf("%+v", d)
	}
}

func TestBudgetOfAnotherScopeIsIgnored(t *testing.T) {
	g := gate(budgetObj("b", "namespace", "tenant-b", 1, true), budgetObj("u", "user", "alice", 1, true))
	if d := eval(t, g, mkJob("tenant-a", "H100", 8)); !d.Allow || len(d.Warnings) != 0 {
		t.Errorf("%+v", d)
	}
}

func TestTeamBudgetCoversQuotaNamespaces(t *testing.T) {
	g := gate(quota("q", "vision", []string{"v1", "v2"}, nil, nil), budgetObj("b", "team", "vision", 100, true),
		usage("v2", "u", 99, 5, "USD", false))
	if d := eval(t, g, mkJob("v1", "T4", 8)); d.Allow {
		t.Errorf("team spend across namespaces must count: %+v", d)
	}
}

func TestMixedCurrencyReportedNotEnforced(t *testing.T) {
	g := gate(quota("q", "a", []string{"tenant-a"}, nil, hard(10)),
		usage("tenant-a", "u1", 500, 5, "EUR", false), usage("tenant-a", "u2", 5, 1, "USD", false))
	d := eval(t, g, mkJob("tenant-a", "H100", 8))
	if !d.Allow || len(d.Warnings) != 1 || !strings.Contains(d.Warnings[0], "mixed currencies") {
		t.Errorf("%+v", d)
	}
	// Non-USD only is also not comparable with a USD limit.
	g = gate(quota("q", "a", []string{"tenant-a"}, nil, hard(10)), usage("tenant-a", "u1", 500, 5, "EUR", false))
	if d := eval(t, g, mkJob("tenant-a", "H100", 8)); !d.Allow || !strings.Contains(d.Warnings[0], "EUR") {
		t.Errorf("%+v", d)
	}
	// SKU priced in EUR: the forecast cannot be compared either.
	g = gate(quota("q", "a", []string{"tenant-a"}, nil, hard(10)), skuObj("h100", "H100", 8, "EUR"))
	if d := eval(t, g, mkJob("tenant-a", "H100", 8)); !d.Allow || !strings.Contains(d.Warnings[0], "priced in EUR") {
		t.Errorf("%+v", d)
	}
}

func TestSkuRateUsedForForecast(t *testing.T) {
	g := gate(quota("q", "a", []string{"tenant-a"}, nil, hard(100)), skuObj("h100", "H100", 30, "USD"))
	// 4 GPUs x 1h x 30 = 120 > 100 (the default table's 8 would pass).
	if d := eval(t, g, mkJob("tenant-a", "H100", 4)); d.Allow {
		t.Errorf("%+v", d)
	}
	// A GPU type the catalog does not price cannot be forecast: warn, allow.
	d := eval(t, g, mkJob("tenant-a", "T4", 4))
	if !d.Allow || len(d.Warnings) != 1 || !strings.Contains(d.Warnings[0], "no enabled SKU") {
		t.Errorf("%+v", d)
	}
}

func TestOpenRecordsCount(t *testing.T) {
	g := gate(quota("q", "a", []string{"tenant-a"}, nil, hard(100)), usage("tenant-a", "open", 95, 5, "USD", true))
	if d := eval(t, g, mkJob("tenant-a", "H100", 1)); d.Allow { // 95 + 8 > 100
		t.Errorf("open (still running) records must count: %+v", d)
	}
}

func TestQuotaGPULimits(t *testing.T) {
	q := quota("q", "a", []string{"tenant-a"}, map[string]interface{}{
		"maxGPUs": int64(16), "maxGPUsPerJob": int64(8), "allowedGPUTypes": []interface{}{"H100"}}, nil)
	g := gate(q)
	if d := eval(t, g, mkJob("tenant-a", "H100", 8)); !d.Allow {
		t.Errorf("%+v", d)
	}
	d := eval(t, g, mkJob("tenant-a", "H100", 9))
	if d.Allow || d.Code != CodeQuota || !strings.Contains(d.Message(), "per-job limit of 8") {
		t.Errorf("per job: %+v", d)
	}
	d = eval(t, g, mkJob("tenant-a", "T4", 1))
	if d.Allow || !strings.Contains(d.Message(), "not allowed") {
		t.Errorf("type: %+v", d)
	}
	// Distributed total counts against the per-job limit.
	j := mkJob("tenant-a", "H100", 4)
	j.Spec.Distributed = &gryviav1.DistributedConfig{Enabled: true, Nodes: 3, GpusPerNode: 4}
	if d := eval(t, g, j); d.Allow || !strings.Contains(d.Message(), "12 GPUs exceeds the per-job limit of 8") {
		t.Errorf("distributed per-job: %+v", d)
	}
}

func TestQuotaConcurrentGPUs(t *testing.T) {
	q := quota("q", "a", []string{"tenant-a", "tenant-a2"}, map[string]interface{}{"maxGPUs": int64(16)}, nil)
	running := mkJob("tenant-a2", "H100", 8)
	running.Name, running.UID = "running", "uid-r"
	running.Status.Phase = "Running"
	scheduling := mkJob("tenant-a", "H100", 4)
	scheduling.Name, scheduling.UID = "sched", "uid-s"
	scheduling.Status.Phase = "Scheduling"
	pending := mkJob("tenant-a", "H100", 64)
	pending.Name, pending.UID = "pending", "uid-p"
	pending.Status.Phase = "Pending" // holds nothing
	done := mkJob("tenant-a", "H100", 64)
	done.Name, done.UID = "done", "uid-d"
	done.Status.Phase = "Succeeded"
	g := gate(q, running, scheduling, pending, done)

	if d := eval(t, g, mkJob("tenant-a", "H100", 4)); !d.Allow { // 12 in use + 4 = 16
		t.Errorf("exactly at the limit: %+v", d)
	}
	d := eval(t, g, mkJob("tenant-a", "H100", 5)) // 17 > 16
	if d.Allow || d.Code != CodeQuota || !strings.Contains(d.Message(), "16 concurrent GPUs; 12 are in use") {
		t.Errorf("%+v", d)
	}
}

func TestTenantSkuRestriction(t *testing.T) {
	tenant := obj("GryviaTenant", "", "a", map[string]interface{}{"allowedSkus": []interface{}{"l40"}})
	g := gate(tenant, skuObj("l40", "L40", 2, "USD"), skuObj("h100", "H100", 8, "USD"))
	if d := eval(t, g, mkJob("tenant-a", "H100", 1)); d.Allow || !strings.Contains(d.Message(), "no enabled catalog SKU") {
		t.Errorf("%+v", d)
	}
	if d := eval(t, g, mkJob("tenant-a", "L40", 1)); !d.Allow {
		t.Errorf("%+v", d)
	}
}

func TestFailsOpenOnLookupErrors(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("no matches for kind GryviaQuota")
		},
	}).Build()
	g := &Gate{Client: c, Now: func() time.Time { return now }}
	if _, err := g.Evaluate(context.Background(), mkJob("tenant-a", "H100", 8)); err == nil {
		t.Error("a failed lookup must be reported so the caller can fail open")
	}
}

func TestMissingBudgetCRDStillEnforcesQuotaBudget(t *testing.T) {
	// The GryviaBudget list fails but the quota budget is still applied.
	base := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(quota("q", "a", []string{"tenant-a"}, nil, hard(10))).Build()
	c := interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, l client.ObjectList, opts ...client.ListOption) error {
			if u, ok := l.(*unstructured.UnstructuredList); ok && u.GroupVersionKind().Kind == "GryviaBudgetList" {
				return errors.New("no matches for kind GryviaBudget")
			}
			return cl.List(ctx, l, opts...)
		},
	})
	g := &Gate{Client: c, Now: func() time.Time { return now }}
	d, err := g.Evaluate(context.Background(), mkJob("tenant-a", "H100", 8))
	if err != nil {
		t.Fatal(err)
	}
	if d.Allow {
		t.Errorf("the quota's hard budget must still apply: %+v", d)
	}
}

func TestPeriodsAndSum(t *testing.T) {
	p, err := periodFor("monthly", "", "", now)
	if err != nil || !p.start.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) || !p.end.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("monthly: %+v %v", p, err)
	}
	c, err := periodFor("custom", "2026-08-10", "2026-08-20", now)
	if err != nil || !c.end.Equal(time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("custom: %+v %v", c, err)
	}
	if _, err := periodFor("custom", "", "2026-08-20", now); err == nil {
		t.Error("custom needs a start")
	}
	// Proration: 10h record, 4h before the month began.
	start := p.start.Add(-4 * time.Hour)
	end := p.start.Add(6 * time.Hour)
	tot := sum([]record{{namespace: "n", start: start, end: &end, cost: 100, gpuHours: 10, currency: "USD"}}, scope{namespaces: []string{"n"}}, p, now)
	if tot.cost < 59.999 || tot.cost > 60.001 || tot.gpuHours < 5.999 || tot.gpuHours > 6.001 {
		t.Errorf("proration: %+v", tot)
	}
}

func TestCustomPeriodBudget(t *testing.T) {
	b := budgetObj("b", "namespace", "tenant-a", 100, true)
	b.Object["spec"].(map[string]interface{})["period"] = map[string]interface{}{"type": "custom", "startDate": "2026-08-01", "endDate": "2026-08-31"}
	old := usage("tenant-a", "old", 500, 5, "USD", false)
	old.Object["spec"].(map[string]interface{})["start"] = time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	old.Object["spec"].(map[string]interface{})["end"] = time.Date(2026, 8, 10, 1, 0, 0, 0, time.UTC).Format(time.RFC3339)
	g := gate(b, old)
	// The custom period ended on Aug 31: it no longer applies to a job starting on Sep 15.
	if d := eval(t, g, mkJob("tenant-a", "T4", 1)); !d.Allow {
		t.Errorf("an ended custom period must not block: %+v", d)
	}
	// Inside the window the same spend blocks.
	b.Object["spec"].(map[string]interface{})["period"] = map[string]interface{}{"type": "custom", "startDate": "2026-09-01", "endDate": "2026-09-30"}
	cur := usage("tenant-a", "cur", 500, 5, "USD", false)
	g = gate(b, cur)
	if d := eval(t, g, mkJob("tenant-a", "T4", 1)); d.Allow {
		t.Errorf("spend inside the custom window must count: %+v", d)
	}
}

func TestPreflightEnforcement(t *testing.T) {
	s := newScheme()
	_ = corev1.AddToScheme(s)
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: map[string]string{"gryvia.io/gpu": "A100", "gryvia.io/gpu-memory": "80"}},
		Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
			"nvidia.com/gpu": *resource.NewQuantity(8, resource.DecimalSI)}},
	}
	g := &Gate{Client: fake.NewClientBuilder().WithScheme(s).WithObjects(n).Build()}
	job := &gryviav1.GryviaAIJob{ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "ns"},
		Spec: gryviav1.GryviaAIJobSpec{GPUs: 1, GpuType: "A100"}}

	if d, err := g.Preflight(context.Background(), job); err != nil || !d.Allow {
		t.Fatalf("no annotations: %+v %v", d, err)
	}
	job.Annotations = map[string]string{"gryvia.io/model-params-billions": "70"}
	d, err := g.Preflight(context.Background(), job)
	if err != nil || d.Allow || d.Code != CodePreflight || !strings.Contains(d.Message(), "memory") {
		t.Fatalf("70B fp16 on one GPU: %+v %v", d, err)
	}
	job.Annotations["gryvia.io/tensor-parallel"] = "8"
	job.Spec.GPUs = 8
	if d, err := g.Preflight(context.Background(), job); err != nil || !d.Allow {
		t.Fatalf("tensor parallel 8 fits: %+v %v", d, err)
	}

	failing := &Gate{Client: interceptor.NewClient(fake.NewClientBuilder().WithScheme(s).Build(), interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("boom")
		}})}
	if _, err := failing.Preflight(context.Background(), job); err == nil {
		t.Fatal("a node lookup failure must be returned so the caller fails open")
	}
}
