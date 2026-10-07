package admission

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	gryviav1 "github.com/zyvorai/gryvia/operators/quota-operator/api/v1"
)

func invoice(state string) *gryviav1.GryviaInvoice {
	at := metav1.NewTime(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	inv := &gryviav1.GryviaInvoice{
		ObjectMeta: metav1.ObjectMeta{Name: "inv-acme-202609-1"},
		Spec: gryviav1.GryviaInvoiceSpec{
			Tenant: "acme", Period: "2026-09", Number: 1, State: state, Currency: "USD", Subtotal: "12.50",
			Lines: []gryviav1.InvoiceLine{{Sku: "H100", Jobs: 1, GpuHours: "5.000000", Rate: "2.500000", Amount: "12.50", Currency: "USD"}},
		},
	}
	if state != gryviav1.InvoiceDraft {
		inv.Spec.FinalizedAt, inv.Spec.FinalizedBy = &at, "admin"
	}
	return inv
}

func TestInvoiceTransitions(t *testing.T) {
	at := metav1.NewTime(time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
	cases := []struct {
		name   string
		old    *gryviav1.GryviaInvoice
		mutate func(*gryviav1.GryviaInvoice)
		deny   string
	}{
		{"draft edits", invoice("Draft"), func(i *gryviav1.GryviaInvoice) { i.Spec.Subtotal = "13" }, ""},
		{"finalize", invoice("Draft"), func(i *gryviav1.GryviaInvoice) {
			i.Spec.State, i.Spec.FinalizedAt, i.Spec.FinalizedBy = "Finalized", &at, "admin"
		}, ""},
		{"finalize without actor", invoice("Draft"), func(i *gryviav1.GryviaInvoice) { i.Spec.State = "Finalized" }, "finalizedAt and finalizedBy"},
		{"draft to paid", invoice("Draft"), func(i *gryviav1.GryviaInvoice) { i.Spec.State, i.Spec.PaidAt = "Paid", &at }, "can only be finalized"},
		{"finalized line edit", invoice("Finalized"), func(i *gryviav1.GryviaInvoice) { i.Spec.Lines[0].Amount = "1" }, "frozen"},
		{"finalized subtotal edit", invoice("Finalized"), func(i *gryviav1.GryviaInvoice) { i.Spec.Subtotal = "1" }, "frozen"},
		{"record payment", invoice("Finalized"), func(i *gryviav1.GryviaInvoice) {
			i.Spec.Payment = &gryviav1.InvoicePayment{Provider: "stripe", InvoiceID: "in_1"}
		}, ""},
		{"pay", invoice("Finalized"), func(i *gryviav1.GryviaInvoice) { i.Spec.State, i.Spec.PaidAt = "Paid", &at }, ""},
		{"pay without time", invoice("Finalized"), func(i *gryviav1.GryviaInvoice) { i.Spec.State = "Paid" }, "needs paidAt"},
		{"pay and edit", invoice("Finalized"), func(i *gryviav1.GryviaInvoice) {
			i.Spec.State, i.Spec.PaidAt, i.Spec.Subtotal = "Paid", &at, "0"
		}, "frozen"},
		{"void", invoice("Finalized"), func(i *gryviav1.GryviaInvoice) {
			i.Spec.State, i.Spec.VoidedAt, i.Spec.VoidedBy, i.Spec.VoidReason = "Void", &at, "admin", "wrong rate"
		}, ""},
		{"void without reason", invoice("Finalized"), func(i *gryviav1.GryviaInvoice) {
			i.Spec.State, i.Spec.VoidedAt, i.Spec.VoidedBy = "Void", &at, "admin"
		}, "voidReason"},
		{"back to draft", invoice("Finalized"), func(i *gryviav1.GryviaInvoice) { i.Spec.State = "Draft" }, "only become Paid or Void"},
		{"paid immutable", invoice("Paid"), func(i *gryviav1.GryviaInvoice) { i.Spec.State = "Void" }, "immutable"},
		{"void immutable", invoice("Void"), func(i *gryviav1.GryviaInvoice) { i.Spec.Subtotal = "0" }, "immutable"},
		{"paid metadata only", invoice("Paid"), func(i *gryviav1.GryviaInvoice) { i.Labels = map[string]string{"x": "y"} }, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			next := c.old.DeepCopy()
			c.mutate(next)
			err := ValidateInvoiceUpdate(c.old, next)
			if c.deny == "" && err != nil {
				t.Fatalf("denied: %v", err)
			}
			if c.deny != "" && (err == nil || !strings.Contains(err.Error(), c.deny)) {
				t.Fatalf("err = %v, want %q", err, c.deny)
			}
		})
	}
}

func TestInvoiceDeleteOnlyDraft(t *testing.T) {
	if err := ValidateInvoiceDelete(invoice("Draft")); err != nil {
		t.Error(err)
	}
	for _, s := range []string{"Finalized", "Paid", "Void"} {
		if err := ValidateInvoiceDelete(invoice(s)); err == nil {
			t.Errorf("deleting a %s invoice allowed", s)
		}
	}
}

func billingReview(t *testing.T, h admission.Handler, op admissionv1.Operation, old, next runtime.Object) admission.Response {
	t.Helper()
	req := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{Operation: op, Name: "x"}}
	if next != nil {
		b, _ := json.Marshal(next)
		req.Object = runtime.RawExtension{Raw: b}
	}
	if old != nil {
		b, _ := json.Marshal(old)
		req.OldObject = runtime.RawExtension{Raw: b}
	}
	return h.Handle(context.Background(), req)
}

func TestLedgerEntryHandler(t *testing.T) {
	s := runtime.NewScheme()
	_ = gryviav1.AddToScheme(s)
	h := NewLedgerEntryHandler(s)
	e := &gryviav1.GryviaLedgerEntry{ObjectMeta: metav1.ObjectMeta{Name: "le-1"},
		Spec: gryviav1.GryviaLedgerEntrySpec{Tenant: "acme", Sequence: 1, Cost: "1.000000", Hash: "h"}}
	edited := e.DeepCopy()
	edited.Spec.Cost = "0.000000"
	if r := billingReview(t, h, admissionv1.Update, e, edited); r.Allowed {
		t.Error("ledger spec edit allowed")
	}
	labelled := e.DeepCopy()
	labelled.Labels = map[string]string{"exported": "true"}
	if r := billingReview(t, h, admissionv1.Update, e, labelled); !r.Allowed {
		t.Errorf("metadata-only update denied: %v", r.Result)
	}
	if r := billingReview(t, h, admissionv1.Delete, e, nil); r.Allowed {
		t.Error("ledger delete allowed")
	}
	if r := billingReview(t, h, admissionv1.Create, nil, e); !r.Allowed {
		t.Error("ledger create denied")
	}
}

func TestInvoiceHandler(t *testing.T) {
	s := runtime.NewScheme()
	_ = gryviav1.AddToScheme(s)
	h := NewInvoiceHandler(s)
	fin := invoice("Finalized")
	edited := fin.DeepCopy()
	edited.Spec.Subtotal = "0"
	if r := billingReview(t, h, admissionv1.Update, fin, edited); r.Allowed {
		t.Error("finalized edit allowed")
	}
	if r := billingReview(t, h, admissionv1.Delete, fin, nil); r.Allowed {
		t.Error("finalized delete allowed")
	}
	if r := billingReview(t, h, admissionv1.Delete, invoice("Draft"), nil); !r.Allowed {
		t.Error("draft delete denied")
	}
}
