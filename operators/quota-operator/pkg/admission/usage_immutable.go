package admission

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	gryviav1 "github.com/zyvorai/gryvia/operators/quota-operator/api/v1"
)

// UsageRecordValidator rejects mutations of a GryviaUsageRecord once spec.final
// is true. Running records may still grow (end, gpuHours, cost) because the
// quota operator writes them. Deletes are allowed so operators can GC, unless
// BlockSealedDelete (the billing ledger is on) protects final records.
type UsageRecordValidator struct {
	BlockSealedDelete bool
}

func (UsageRecordValidator) ValidateCreate(_ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (v UsageRecordValidator) ValidateDelete(obj runtime.Object) (admission.Warnings, error) {
	rec, ok := obj.(*gryviav1.GryviaUsageRecord)
	if !ok {
		return nil, fmt.Errorf("expected GryviaUsageRecord, got %T", obj)
	}
	if v.BlockSealedDelete && rec.Spec.Final {
		return nil, fmt.Errorf("GryviaUsageRecord %s/%s is final and billed through the ledger; it cannot be deleted",
			rec.Namespace, rec.Name)
	}
	return nil, nil
}

func (UsageRecordValidator) ValidateUpdate(oldObj, newObj runtime.Object) (admission.Warnings, error) {
	oldRec, ok := oldObj.(*gryviav1.GryviaUsageRecord)
	if !ok {
		return nil, fmt.Errorf("expected GryviaUsageRecord, got %T", oldObj)
	}
	newRec, ok := newObj.(*gryviav1.GryviaUsageRecord)
	if !ok {
		return nil, fmt.Errorf("expected GryviaUsageRecord, got %T", newObj)
	}
	if !oldRec.Spec.Final {
		return nil, nil
	}
	if !equality.Semantic.DeepEqual(oldRec.Spec, newRec.Spec) {
		return nil, fmt.Errorf("GryviaUsageRecord %s/%s is final (job %s); spec is immutable",
			oldRec.Namespace, oldRec.Name, oldRec.Spec.Job)
	}
	return nil, nil
}

// FinalCondition is the status condition the quota operator should set when
// it seals a record, so dashboards do not have to infer from spec.final.
func FinalCondition(now metav1.Time) metav1.Condition {
	return metav1.Condition{
		Type:               "Sealed",
		Status:             metav1.ConditionTrue,
		Reason:             "JobFinished",
		Message:            "usage record is final; spec mutations are rejected",
		LastTransitionTime: now,
	}
}
