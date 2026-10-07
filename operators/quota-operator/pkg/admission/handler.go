package admission

import (
	"context"
	"fmt"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	gryviav1 "github.com/zyvorai/gryvia/operators/quota-operator/api/v1"
)

// UsageRecordPath is the URL path the GryviaUsageRecord validating handler is served on.
const UsageRecordPath = "/validate-gryvia-io-v1alpha1-gryviausagerecord"

// UsageRecordHandler serves UsageRecordValidator as an admission webhook.
type UsageRecordHandler struct {
	decoder   admission.Decoder
	validator UsageRecordValidator
}

// NewUsageRecordHandler builds a handler that decodes with the given scheme,
// which must have gryviav1 registered. blockSealedDelete also rejects deleting final records.
func NewUsageRecordHandler(scheme *runtime.Scheme, blockSealedDelete bool) *UsageRecordHandler {
	return &UsageRecordHandler{decoder: admission.NewDecoder(scheme), validator: UsageRecordValidator{BlockSealedDelete: blockSealedDelete}}
}

// Handle allows everything except an UPDATE or DELETE that the validator rejects.
func (h *UsageRecordHandler) Handle(_ context.Context, req admission.Request) admission.Response {
	if req.Operation == admissionv1.Delete {
		oldRec := &gryviav1.GryviaUsageRecord{}
		if err := h.decoder.DecodeRaw(req.OldObject, oldRec); err != nil {
			return admission.Errored(http.StatusBadRequest, fmt.Errorf("decode oldObject: %w", err))
		}
		if _, err := h.validator.ValidateDelete(oldRec); err != nil {
			return admission.Denied(err.Error())
		}
		return admission.Allowed("")
	}
	if req.Operation != admissionv1.Update {
		return admission.Allowed("only updates and deletes are validated")
	}
	newRec := &gryviav1.GryviaUsageRecord{}
	if err := h.decoder.Decode(req, newRec); err != nil {
		return admission.Errored(http.StatusBadRequest, fmt.Errorf("decode object: %w", err))
	}
	oldRec := &gryviav1.GryviaUsageRecord{}
	if err := h.decoder.DecodeRaw(req.OldObject, oldRec); err != nil {
		return admission.Errored(http.StatusBadRequest, fmt.Errorf("decode oldObject: %w", err))
	}
	if _, err := h.validator.ValidateUpdate(oldRec, newRec); err != nil {
		return admission.Denied(err.Error())
	}
	return admission.Allowed("")
}
