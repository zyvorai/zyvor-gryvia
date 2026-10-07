package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	gryviav1 "github.com/zyvorai/gryvia/operators/quota-operator/api/v1"
)

const (
	// AnnotationLedgerEntry on a sealed GryviaUsageRecord names the GryviaLedgerEntry that copies it.
	AnnotationLedgerEntry = "gryvia.io/ledger-entry"
	// LabelUsageRecordUID on a GryviaLedgerEntry is the UID of the usage record it copies.
	LabelUsageRecordUID = "gryvia.io/usage-record-uid"
	ledgerEntryPrefix   = "le-"
)

// GryviaLedgerReconciler appends one GryviaLedgerEntry per sealed GryviaUsageRecord (--billing-ledger). Entries
// are named after the record's UID, so a record is never copied twice, and chained per tenant through PrevHash.
// The chain head is read without the cache (Reader) and the controller runs one reconcile at a time under leader
// election, so two entries never get the same sequence.
type GryviaLedgerReconciler struct {
	client.Client
	// Reader reads ledger entries without the informer cache; nil uses Client.
	Reader client.Reader
	Scheme *runtime.Scheme
	Now    func() time.Time
}

//+kubebuilder:rbac:groups=gryvia.io,resources=gryvialedgerentries,verbs=get;list;watch;create
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviausagerecords,verbs=get;list;watch;patch

func (r *GryviaLedgerReconciler) reader() client.Reader {
	if r.Reader != nil {
		return r.Reader
	}
	return r.Client
}

func (r *GryviaLedgerReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *GryviaLedgerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	rec := &gryviav1.GryviaUsageRecord{}
	if err := r.Get(ctx, req.NamespacedName, rec); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !rec.Spec.Final || rec.Annotations[AnnotationLedgerEntry] != "" {
		return ctrl.Result{}, nil
	}
	name := ledgerEntryPrefix + string(rec.UID)
	err := r.reader().Get(ctx, types.NamespacedName{Name: name}, &gryviav1.GryviaLedgerEntry{})
	if errors.IsNotFound(err) {
		entry, err := r.nextEntry(ctx, rec, name)
		if err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, entry); err != nil && !errors.IsAlreadyExists(err) {
			return ctrl.Result{}, err
		}
		log.FromContext(ctx).Info("ledger entry appended", "entry", name, "tenant", entry.Spec.Tenant, "sequence", entry.Spec.Sequence)
	} else if err != nil {
		return ctrl.Result{}, err
	}
	patch := client.MergeFrom(rec.DeepCopy())
	if rec.Annotations == nil {
		rec.Annotations = map[string]string{}
	}
	rec.Annotations[AnnotationLedgerEntry] = name
	return ctrl.Result{}, r.Patch(ctx, rec, patch)
}

// nextEntry builds the entry after the tenant's current chain head.
func (r *GryviaLedgerReconciler) nextEntry(ctx context.Context, rec *gryviav1.GryviaUsageRecord, name string) (*gryviav1.GryviaLedgerEntry, error) {
	tenant := ledgerTenant(rec)
	if errs := validation.IsValidLabelValue(tenant); tenant == "" || len(errs) > 0 {
		return nil, fmt.Errorf("usage record %s/%s: tenant %q cannot be a label value", rec.Namespace, rec.Name, tenant)
	}
	list := &gryviav1.GryviaLedgerEntryList{}
	if err := r.reader().List(ctx, list, client.MatchingLabels{labelTenant: tenant}); err != nil {
		return nil, err
	}
	var head *gryviav1.GryviaLedgerEntry
	for i := range list.Items {
		if head == nil || list.Items[i].Spec.Sequence > head.Spec.Sequence {
			head = &list.Items[i]
		}
	}
	spec := gryviav1.GryviaLedgerEntrySpec{
		Tenant:      tenant,
		Sequence:    1,
		UsageRecord: gryviav1.UsageRecordRef{Namespace: rec.Namespace, Name: rec.Name, UID: string(rec.UID)},
		Kind:        rec.Spec.Kind,
		Job:         rec.Spec.Job,
		Sku:         rec.Spec.Sku,
		Start:       ledgerTime(&rec.Spec.Start),
		End:         ledgerTime(rec.Spec.End),
		GpuHours:    ledgerDecimal(rec.Spec.GpuHours),
		Rate:        ledgerDecimal(rec.Spec.Rate),
		Cost:        ledgerDecimal(rec.Spec.Cost),
		Currency:    rec.Spec.Currency,
		RecordedAt:  r.now().UTC().Format(time.RFC3339),
	}
	if spec.Kind == "" {
		spec.Kind = "gpu"
	}
	if spec.Sku == "" {
		spec.Sku = rec.Spec.GpuType
	}
	if spec.Currency == "" {
		spec.Currency = "USD"
	}
	if head != nil {
		spec.Sequence, spec.PrevHash = head.Spec.Sequence+1, head.Spec.Hash
	}
	spec.Hash = LedgerHash(spec)
	return &gryviav1.GryviaLedgerEntry{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{
			labelTenant: tenant, LabelUsageRecordUID: string(rec.UID),
		}},
		Spec: spec,
	}, nil
}

// ledgerTenant matches the gateway's invoice attribution: spec.tenant, else the namespace without "tenant-".
func ledgerTenant(rec *gryviav1.GryviaUsageRecord) string {
	if rec.Spec.Tenant != "" {
		return rec.Spec.Tenant
	}
	return strings.TrimPrefix(rec.Namespace, "tenant-")
}

// LedgerHash is the hex SHA-256 of the entry's hash input.
func LedgerHash(spec gryviav1.GryviaLedgerEntrySpec) string {
	sum := sha256.Sum256([]byte(gryviav1.LedgerHashInput(spec)))
	return hex.EncodeToString(sum[:])
}

func ledgerTime(t *metav1.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func ledgerDecimal(v float64) string {
	return strconv.FormatFloat(v, 'f', 6, 64)
}

// SetupWithManager watches usage records; only sealed records without an entry matter.
func (r *GryviaLedgerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	sealed := predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return needsLedger(e.Object) },
		UpdateFunc:  func(e event.UpdateEvent) bool { return needsLedger(e.ObjectNew) },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(e event.GenericEvent) bool { return needsLedger(e.Object) },
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("gryvialedger").
		For(&gryviav1.GryviaUsageRecord{}, builder.WithPredicates(sealed)).
		Complete(r)
}

func needsLedger(obj client.Object) bool {
	rec, ok := obj.(*gryviav1.GryviaUsageRecord)
	return ok && rec.Spec.Final && rec.Annotations[AnnotationLedgerEntry] == ""
}
