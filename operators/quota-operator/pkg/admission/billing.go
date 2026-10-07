package admission

import (
	"context"
	"fmt"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	gryviav1 "github.com/zyvorai/gryvia/operators/quota-operator/api/v1"
)

// LedgerEntryPath and InvoicePath are the URL paths of the billing validating handlers.
const (
	LedgerEntryPath = "/validate-gryvia-io-v1alpha1-gryvialedgerentry"
	InvoicePath     = "/validate-gryvia-io-v1alpha1-gryviainvoice"
)

// ValidateLedgerEntryUpdate rejects any change to a ledger entry's spec; metadata (labels, annotations) may change.
func ValidateLedgerEntryUpdate(oldE, newE *gryviav1.GryviaLedgerEntry) error {
	if !equality.Semantic.DeepEqual(oldE.Spec, newE.Spec) {
		return fmt.Errorf("GryviaLedgerEntry %s (tenant %s, sequence %d) is append-only; spec is immutable",
			oldE.Name, oldE.Spec.Tenant, oldE.Spec.Sequence)
	}
	return nil
}

// ValidateInvoiceUpdate lets a Draft change freely (except straight to Paid), and a Finalized invoice only record
// its payment, become Paid or become Void. Paid and Void invoices are immutable.
func ValidateInvoiceUpdate(oldI, newI *gryviav1.GryviaInvoice) error {
	o, n := oldI.Spec, newI.Spec
	switch o.State {
	case gryviav1.InvoiceDraft:
		switch n.State {
		case gryviav1.InvoiceDraft:
			return nil
		case gryviav1.InvoiceFinalized:
			if n.FinalizedAt == nil || n.FinalizedBy == "" {
				return fmt.Errorf("finalizing GryviaInvoice %s needs finalizedAt and finalizedBy", oldI.Name)
			}
			return nil
		}
		return fmt.Errorf("GryviaInvoice %s: a Draft can only be finalized (or deleted), not set to %s", oldI.Name, n.State)
	case gryviav1.InvoiceFinalized:
		allowed := o
		switch n.State {
		case gryviav1.InvoiceFinalized:
		case gryviav1.InvoicePaid:
			if n.PaidAt == nil {
				return fmt.Errorf("paying GryviaInvoice %s needs paidAt", oldI.Name)
			}
			allowed.State, allowed.PaidAt = n.State, n.PaidAt
		case gryviav1.InvoiceVoid:
			if n.VoidedAt == nil || n.VoidedBy == "" || n.VoidReason == "" {
				return fmt.Errorf("voiding GryviaInvoice %s needs voidedAt, voidedBy and voidReason", oldI.Name)
			}
			allowed.State, allowed.VoidedAt, allowed.VoidedBy, allowed.VoidReason = n.State, n.VoidedAt, n.VoidedBy, n.VoidReason
		default:
			return fmt.Errorf("GryviaInvoice %s is Finalized; it can only become Paid or Void", oldI.Name)
		}
		if o.Payment == nil && n.Payment != nil {
			allowed.Payment = n.Payment
		}
		if !equality.Semantic.DeepEqual(allowed, n) {
			return fmt.Errorf("GryviaInvoice %s is Finalized; its lines, totals and payment reference are frozen", oldI.Name)
		}
		return nil
	}
	if !equality.Semantic.DeepEqual(o, n) {
		return fmt.Errorf("GryviaInvoice %s is %s; it is immutable", oldI.Name, o.State)
	}
	return nil
}

// ValidateInvoiceDelete allows deleting only a Draft.
func ValidateInvoiceDelete(inv *gryviav1.GryviaInvoice) error {
	if inv.Spec.State != gryviav1.InvoiceDraft {
		return fmt.Errorf("GryviaInvoice %s is %s; only a Draft can be deleted (void it instead)", inv.Name, inv.Spec.State)
	}
	return nil
}

// LedgerEntryHandler rejects updates of a ledger entry's spec and every delete.
type LedgerEntryHandler struct{ decoder admission.Decoder }

// NewLedgerEntryHandler builds the ledger entry handler; scheme must have gryviav1 registered.
func NewLedgerEntryHandler(scheme *runtime.Scheme) *LedgerEntryHandler {
	return &LedgerEntryHandler{decoder: admission.NewDecoder(scheme)}
}

func (h *LedgerEntryHandler) Handle(_ context.Context, req admission.Request) admission.Response {
	switch req.Operation {
	case admissionv1.Delete:
		return admission.Denied(fmt.Sprintf("GryviaLedgerEntry %s is append-only; it cannot be deleted", req.Name))
	case admissionv1.Update:
		oldE, newE := &gryviav1.GryviaLedgerEntry{}, &gryviav1.GryviaLedgerEntry{}
		if err := h.decoder.DecodeRaw(req.OldObject, oldE); err != nil {
			return admission.Errored(http.StatusBadRequest, fmt.Errorf("decode oldObject: %w", err))
		}
		if err := h.decoder.Decode(req, newE); err != nil {
			return admission.Errored(http.StatusBadRequest, fmt.Errorf("decode object: %w", err))
		}
		if err := ValidateLedgerEntryUpdate(oldE, newE); err != nil {
			return admission.Denied(err.Error())
		}
	}
	return admission.Allowed("")
}

// InvoiceHandler enforces ValidateInvoiceUpdate and ValidateInvoiceDelete.
type InvoiceHandler struct{ decoder admission.Decoder }

// NewInvoiceHandler builds the invoice handler; scheme must have gryviav1 registered.
func NewInvoiceHandler(scheme *runtime.Scheme) *InvoiceHandler {
	return &InvoiceHandler{decoder: admission.NewDecoder(scheme)}
}

func (h *InvoiceHandler) Handle(_ context.Context, req admission.Request) admission.Response {
	switch req.Operation {
	case admissionv1.Delete:
		inv := &gryviav1.GryviaInvoice{}
		if err := h.decoder.DecodeRaw(req.OldObject, inv); err != nil {
			return admission.Errored(http.StatusBadRequest, fmt.Errorf("decode oldObject: %w", err))
		}
		if err := ValidateInvoiceDelete(inv); err != nil {
			return admission.Denied(err.Error())
		}
	case admissionv1.Update:
		oldI, newI := &gryviav1.GryviaInvoice{}, &gryviav1.GryviaInvoice{}
		if err := h.decoder.DecodeRaw(req.OldObject, oldI); err != nil {
			return admission.Errored(http.StatusBadRequest, fmt.Errorf("decode oldObject: %w", err))
		}
		if err := h.decoder.Decode(req, newI); err != nil {
			return admission.Errored(http.StatusBadRequest, fmt.Errorf("decode object: %w", err))
		}
		if err := ValidateInvoiceUpdate(oldI, newI); err != nil {
			return admission.Denied(err.Error())
		}
	}
	return admission.Allowed("")
}
