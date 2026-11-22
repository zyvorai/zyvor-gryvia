// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package admission

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gryviav1 "github.com/zyvorai/gryvia/operators/quota-operator/api/v1"
)

func rec(final bool, hours float64) *gryviav1.GryviaUsageRecord {
	return &gryviav1.GryviaUsageRecord{
		ObjectMeta: metav1.ObjectMeta{Name: "usage-abc", Namespace: "tenant-acme"},
		Spec: gryviav1.GryviaUsageRecordSpec{
			Tenant:   "acme",
			Job:      "train-1",
			JobUID:   "abc",
			Gpus:     8,
			GpuHours: hours,
			Final:    final,
		},
	}
}

func TestFinalRecordRejectsSpecMutation(t *testing.T) {
	v := UsageRecordValidator{}
	old := rec(true, 1.5)
	next := rec(true, 0.1)
	if _, err := v.ValidateUpdate(old, next); err == nil {
		t.Fatal("expected rejection of gpuHours change on a final record")
	}
}

func TestFinalRecordAllowsMetadataOnly(t *testing.T) {
	v := UsageRecordValidator{}
	old := rec(true, 1.5)
	next := rec(true, 1.5)
	next.Labels = map[string]string{"exported": "true"}
	if _, err := v.ValidateUpdate(old, next); err != nil {
		t.Fatal(err)
	}
}

func TestRunningRecordAllowsGrowth(t *testing.T) {
	v := UsageRecordValidator{}
	if _, err := v.ValidateUpdate(rec(false, 0.2), rec(false, 0.4)); err != nil {
		t.Fatal(err)
	}
}

func TestUnsealRejected(t *testing.T) {
	v := UsageRecordValidator{}
	if _, err := v.ValidateUpdate(rec(true, 1.5), rec(false, 1.5)); err == nil {
		t.Fatal("clearing spec.final must be rejected")
	}
}
