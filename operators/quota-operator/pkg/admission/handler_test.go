package admission

import (
	"context"
	"encoding/json"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	gryviav1 "github.com/zyvorai/gryvia/operators/quota-operator/api/v1"
)

func review(t *testing.T, op admissionv1.Operation, old, next *gryviav1.GryviaUsageRecord) admission.Response {
	return reviewWith(t, false, op, old, next)
}

func reviewWith(t *testing.T, blockSealedDelete bool, op admissionv1.Operation, old, next *gryviav1.GryviaUsageRecord) admission.Response {
	t.Helper()
	s := runtime.NewScheme()
	if err := gryviav1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	req := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{Operation: op}}
	if next != nil {
		b, _ := json.Marshal(next)
		req.Object = runtime.RawExtension{Raw: b}
	}
	if old != nil {
		b, _ := json.Marshal(old)
		req.OldObject = runtime.RawExtension{Raw: b}
	}
	return NewUsageRecordHandler(s, blockSealedDelete).Handle(context.Background(), req)
}

func TestHandlerDeniesSealedMutation(t *testing.T) {
	if r := review(t, admissionv1.Update, rec(true, 1.5), rec(true, 0.1)); r.Allowed {
		t.Fatal("expected denial of a spec change on a sealed record")
	}
}

func TestHandlerAllowsRunningGrowthCreateAndDelete(t *testing.T) {
	if r := review(t, admissionv1.Update, rec(false, 0.2), rec(false, 0.4)); !r.Allowed {
		t.Fatalf("running record growth denied: %v", r.Result)
	}
	if r := review(t, admissionv1.Create, nil, rec(true, 1)); !r.Allowed {
		t.Fatal("create denied")
	}
	if r := review(t, admissionv1.Delete, rec(true, 1), nil); !r.Allowed {
		t.Fatal("delete denied")
	}
}

func TestHandlerBlocksSealedDeleteWithLedger(t *testing.T) {
	if r := reviewWith(t, true, admissionv1.Delete, rec(true, 1), nil); r.Allowed {
		t.Fatal("deleting a sealed record allowed with the ledger on")
	}
	if r := reviewWith(t, true, admissionv1.Delete, rec(false, 1), nil); !r.Allowed {
		t.Fatal("deleting an open record denied")
	}
}
