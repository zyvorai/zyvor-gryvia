package controllers

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gryviav1 "github.com/zyvorai/gryvia/operators/quota-operator/api/v1"
)

func sealedRecord(name, uid, tenant string, cost float64, final bool) *gryviav1.GryviaUsageRecord {
	start := metav1.NewTime(time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC))
	end := metav1.NewTime(time.Date(2026, 9, 3, 12, 30, 0, 0, time.UTC))
	return &gryviav1.GryviaUsageRecord{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tenant-" + tenant, UID: types.UID(uid)},
		Spec: gryviav1.GryviaUsageRecordSpec{
			Tenant: tenant, Job: "train-" + name, JobUID: uid, GpuType: "H100", Gpus: 2,
			Start: start, End: &end, GpuHours: 5, Rate: cost / 5, Cost: cost, Final: final,
		},
	}
}

func ledgerFixture(objs ...client.Object) (*GryviaLedgerReconciler, client.Client) {
	scheme := newQuotaTestScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &GryviaLedgerReconciler{Client: c, Scheme: scheme,
		Now: func() time.Time { return time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC) }}, c
}

func reconcileRecord(t *testing.T, r *GryviaLedgerReconciler, rec *gryviav1.GryviaUsageRecord) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: rec.Namespace, Name: rec.Name}}); err != nil {
		t.Fatal(err)
	}
}

func tenantEntries(t *testing.T, c client.Client, tenant string) []gryviav1.GryviaLedgerEntry {
	t.Helper()
	list := &gryviav1.GryviaLedgerEntryList{}
	if err := c.List(context.Background(), list, client.MatchingLabels{labelTenant: tenant}); err != nil {
		t.Fatal(err)
	}
	return list.Items
}

func TestLedgerChainsSealedRecordsPerTenant(t *testing.T) {
	a1 := sealedRecord("a1", "uid-a1", "acme", 12.5, true)
	a2 := sealedRecord("a2", "uid-a2", "acme", 0.1, true)
	b1 := sealedRecord("b1", "uid-b1", "beta", 3, true)
	r, c := ledgerFixture(a1, a2, b1)
	for _, rec := range []*gryviav1.GryviaUsageRecord{a1, b1, a2} {
		reconcileRecord(t, r, rec)
	}

	e1 := &gryviav1.GryviaLedgerEntry{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "le-uid-a1"}, e1); err != nil {
		t.Fatal(err)
	}
	s := e1.Spec
	if s.Sequence != 1 || s.PrevHash != "" || s.Cost != "12.500000" || s.Rate != "2.500000" || s.GpuHours != "5.000000" ||
		s.Start != "2026-09-03T10:00:00Z" || s.End != "2026-09-03T12:30:00Z" || s.Kind != "gpu" || s.Sku != "H100" ||
		s.Currency != "USD" || s.RecordedAt != "2026-10-01T00:00:00Z" || s.UsageRecord.UID != "uid-a1" {
		t.Errorf("entry 1 = %+v", s)
	}
	if s.Hash != LedgerHash(s) || len(s.Hash) != 64 {
		t.Errorf("hash %q does not match its input", s.Hash)
	}
	e2 := &gryviav1.GryviaLedgerEntry{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "le-uid-a2"}, e2); err != nil {
		t.Fatal(err)
	}
	if e2.Spec.Sequence != 2 || e2.Spec.PrevHash != s.Hash || e2.Spec.Cost != "0.100000" {
		t.Errorf("entry 2 = %+v, want sequence 2 chained to %s", e2.Spec, s.Hash)
	}
	if beta := tenantEntries(t, c, "beta"); len(beta) != 1 || beta[0].Spec.Sequence != 1 || beta[0].Spec.PrevHash != "" {
		t.Errorf("beta chain = %+v, want its own sequence 1", beta)
	}

	got := &gryviav1.GryviaUsageRecord{}
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: a1.Namespace, Name: a1.Name}, got)
	if got.Annotations[AnnotationLedgerEntry] != "le-uid-a1" {
		t.Errorf("record annotation = %v", got.Annotations)
	}
}

func TestLedgerSkipsOpenAndAlreadyCopiedRecords(t *testing.T) {
	open := sealedRecord("open", "uid-open", "acme", 1, false)
	r, c := ledgerFixture(open)
	reconcileRecord(t, r, open)
	if n := len(tenantEntries(t, c, "acme")); n != 0 {
		t.Fatalf("open record got %d entries", n)
	}

	// The entry was created but the annotation write was lost: the next reconcile only annotates.
	sealed := sealedRecord("s", "uid-s", "acme", 1, true)
	r, c = ledgerFixture(sealed)
	reconcileRecord(t, r, sealed)
	got := &gryviav1.GryviaUsageRecord{}
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: sealed.Namespace, Name: sealed.Name}, got)
	delete(got.Annotations, AnnotationLedgerEntry)
	if err := c.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	reconcileRecord(t, r, sealed)
	if n := len(tenantEntries(t, c, "acme")); n != 1 {
		t.Errorf("entries = %d, want 1 (no duplicate)", n)
	}
}

func TestLedgerTenantFallsBackToNamespace(t *testing.T) {
	rec := sealedRecord("n", "uid-n", "acme", 1, true)
	rec.Spec.Tenant = ""
	r, c := ledgerFixture(rec)
	reconcileRecord(t, r, rec)
	if got := tenantEntries(t, c, "acme"); len(got) != 1 || got[0].Spec.Tenant != "acme" {
		t.Errorf("entries = %+v, want one for tenant acme from namespace tenant-acme", got)
	}
}

func TestLedgerRejectsTenantThatIsNotALabelValue(t *testing.T) {
	rec := sealedRecord("x", "uid-x", "acme", 1, true)
	rec.Spec.Tenant = "not a label"
	r, _ := ledgerFixture(rec)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: rec.Namespace, Name: rec.Name}}); err == nil {
		t.Error("expected an error for a tenant that cannot be a label value")
	}
}

// The hash input layout is shared with the gateway's verifier (services/api-gateway/routers/ledger.py).
func TestLedgerHashGolden(t *testing.T) {
	spec := gryviav1.GryviaLedgerEntrySpec{
		Tenant: "acme", Sequence: 2, PrevHash: "abc",
		UsageRecord: gryviav1.UsageRecordRef{Namespace: "tenant-acme", Name: "usage-1", UID: "u1"},
		Kind:        "gpu", Job: "train", Sku: "H100", Start: "2026-09-03T10:00:00Z", End: "2026-09-03T12:30:00Z",
		GpuHours: "5.000000", Rate: "2.500000", Cost: "12.500000", Currency: "USD", RecordedAt: "2026-10-01T00:00:00Z",
	}
	const want = "43516e7b3b191a079bd53edfb1138e394a588ee14a9571d316ada1c598da86ac"
	if got := LedgerHash(spec); got != want {
		t.Errorf("hash = %s, want %s", got, want)
	}
}
