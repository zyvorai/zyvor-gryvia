// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	gryviav1 "github.com/zyvorai/gryvia/operators/quota-operator/api/v1"
)

// The fake client stores the quota operator's narrow typed job, so it cannot show fields it
// does not declare. Instead the tests capture the exact JSON merge patch the controllers send
// to the status subresource and apply it (RFC 7386) to a status as the AI operator writes it.
// A field survives if and only if the patch does not mention it.

// fullStatus is a GryviaAIJob status as the AI operator writes it: it carries fields the
// quota operator's narrow type does not declare.
func fullStatus(phase string) map[string]interface{} {
	return map[string]interface{}{
		"phase":          phase,
		"gpusAllocated":  float64(16),
		"replicasReady":  float64(2),
		"message":        "waiting for nodes",
		"nodesAllocated": []interface{}{"n1", "n2"},
		"placementExplanation": []interface{}{
			map[string]interface{}{"node": "n1", "baseScore": float64(10), "fabricPenalty": float64(0), "finalScore": float64(10)},
		},
		"metrics":   map[string]interface{}{"gpuUtilization": "83"},
		"startTime": "2026-01-01T00:00:00Z",
		"conditions": []interface{}{
			map[string]interface{}{"type": "Scheduled", "status": "True", "reason": "Scheduled", "message": "ok", "lastTransitionTime": "2026-01-01T00:00:00Z"},
		},
	}
}

func typedJob(name, ns, phase string) *gryviav1.GryviaAIJob {
	return &gryviav1.GryviaAIJob{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       gryviav1.GryviaAIJobSpec{GPUs: 16, GpuType: "H100"},
		Status: gryviav1.GryviaAIJobStatus{Phase: phase, NodesAllocated: []string{"n1", "n2"},
			Conditions: []metav1.Condition{{Type: "Scheduled", Status: metav1.ConditionTrue, Reason: "Scheduled", LastTransitionTime: metav1.Now()}}},
	}
}

type patchRecorder struct{ status [][]byte }

// recordingClient fails any Status().Update of a job: only patches are allowed.
func recordingClient(rec *patchRecorder, objs ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(newQuotaTestScheme()).WithObjects(objs...).
		WithStatusSubresource(&gryviav1.GryviaAIJob{}, &gryviav1.GryviaQuota{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if _, ok := obj.(*gryviav1.GryviaAIJob); ok && sub == "status" {
					data, err := patch.Data(obj)
					if err != nil {
						return err
					}
					rec.status = append(rec.status, data)
				}
				return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if _, ok := obj.(*gryviav1.GryviaAIJob); ok {
					return fmt.Errorf("job status must be patched, never updated (a full update drops fields owned by the AI operator)")
				}
				return c.SubResource(sub).Update(ctx, obj, opts...)
			},
		}).Build()
}

// mergePatch applies an RFC 7386 JSON merge patch.
func mergePatch(target, patch map[string]interface{}) {
	for k, v := range patch {
		if v == nil {
			delete(target, k)
			continue
		}
		if pm, ok := v.(map[string]interface{}); ok {
			tm, _ := target[k].(map[string]interface{})
			if tm == nil {
				tm = map[string]interface{}{}
			}
			mergePatch(tm, pm)
			target[k] = tm
			continue
		}
		target[k] = v
	}
}

// applied applies the single recorded patch to fullStatus and returns the resulting status.
func applied(t *testing.T, rec *patchRecorder, phase string) map[string]interface{} {
	t.Helper()
	if len(rec.status) != 1 {
		t.Fatalf("expected exactly one status patch, got %d", len(rec.status))
	}
	var p map[string]interface{}
	if err := json.Unmarshal(rec.status[0], &p); err != nil {
		t.Fatal(err)
	}
	patchStatus, _ := p["status"].(map[string]interface{})
	for k := range p {
		if k != "status" {
			t.Errorf("patch touches %q outside status: %s", k, rec.status[0])
		}
	}
	for k := range patchStatus {
		switch k {
		case "phase", "message", "conditions":
		default:
			t.Errorf("patch touches status.%s, which the quota operator does not own: %s", k, rec.status[0])
		}
	}
	doc := map[string]interface{}{"status": fullStatus(phase)}
	mergePatch(doc, p)
	st, _ := doc["status"].(map[string]interface{})
	return st
}

// assertOwnedFieldsSurvive checks everything the AI operator owns is still there and the
// job is Rejected.
func assertOwnedFieldsSurvive(t *testing.T, st map[string]interface{}, wantPhase string) {
	t.Helper()
	if st["phase"] != wantPhase {
		t.Errorf("phase = %v, want %s", st["phase"], wantPhase)
	}
	for _, k := range []string{"gpusAllocated", "replicasReady", "nodesAllocated", "placementExplanation", "metrics", "startTime"} {
		if !reflect.DeepEqual(st[k], fullStatus("")[k]) {
			t.Errorf("status.%s changed or dropped: %v", k, st[k])
		}
	}
	if wantPhase != "Rejected" {
		return
	}
	found := map[string]bool{}
	conds, _ := st["conditions"].([]interface{})
	for _, c := range conds {
		found[c.(map[string]interface{})["type"].(string)] = true
	}
	if !found["Scheduled"] || !found["Rejected"] {
		t.Errorf("conditions = %v, want Scheduled kept and Rejected added", conds)
	}
}

func TestPatchJobPhase_SendsOnlyOwnedFields(t *testing.T) {
	rec := &patchRecorder{}
	c := recordingClient(rec, typedJob("j", "ml", "Pending"))
	job := &gryviav1.GryviaAIJob{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "j", Namespace: "ml"}, job); err != nil {
		t.Fatal(err)
	}
	err := patchJobPhase(context.Background(), c, job, "Rejected", "too big", &metav1.Condition{
		Type: "Rejected", Status: metav1.ConditionTrue, Reason: "QuotaExceeded", Message: "too big", LastTransitionTime: metav1.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	st := applied(t, rec, "Pending")
	assertOwnedFieldsSurvive(t, st, "Rejected")
	if st["message"] != "too big" {
		t.Errorf("message = %v", st["message"])
	}
	got := &gryviav1.GryviaAIJob{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "j", Namespace: "ml"}, got)
	if got.Status.Phase != "Rejected" {
		t.Errorf("stored phase = %s", got.Status.Phase)
	}
}

func TestQuotaEnforcement_PatchesOnlyOwnedFields(t *testing.T) {
	quota := newTestQuota("q")
	quota.Spec.GPUQuota.AllowedGPUTypes = []string{"A100"} // H100 is not allowed
	rec := &patchRecorder{}
	c := recordingClient(rec, quota, typedJob("j", "ml-prod", "Pending"))
	r := &GryviaQuotaReconciler{Client: c, Scheme: newQuotaTestScheme()}
	if err := r.enforceQuota(context.Background(), quota); err != nil {
		t.Fatal(err)
	}
	assertOwnedFieldsSurvive(t, applied(t, rec, "Pending"), "Rejected")
}

func TestBudgetEnforcement_PatchesOnlyOwnedFields(t *testing.T) {
	fb := &gryviav1.GryviaBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "b"},
		Spec: gryviav1.GryviaBudgetSpec{
			Scope:       gryviav1.BudgetScope{Type: "namespace", Name: "ml-prod"},
			Enforcement: &gryviav1.BudgetEnforcement{Action: "block"},
		},
	}
	rec := &patchRecorder{}
	c := recordingClient(rec, typedJob("j", "ml-prod", "Queued"))
	r := &GryviaBudgetReconciler{Client: c, Scheme: newQuotaTestScheme()}
	if err := r.enforceBudget(context.Background(), fb); err != nil {
		t.Fatal(err)
	}
	assertOwnedFieldsSurvive(t, applied(t, rec, "Queued"), "Rejected")
}

// Legacy QuotaPolicy behavior is covered by api_contracts_test.go: it reports
// UnsupportedAPI and no longer performs dormant job-state mutations.

func TestUsageRecord_PreemptedIsTerminal(t *testing.T) {
	for _, p := range []string{"Succeeded", "Failed", "Cancelled", "Preempted"} {
		if !isTerminalPhase(p) {
			t.Errorf("%s must finalize the usage record", p)
		}
	}
	for _, p := range []string{"Pending", "Running", "Queued", "Rejected"} {
		if isTerminalPhase(p) {
			t.Errorf("%s must not finalize the usage record", p)
		}
	}
}
