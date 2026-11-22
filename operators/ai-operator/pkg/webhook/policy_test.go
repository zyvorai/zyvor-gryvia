// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package webhook

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

func job(gpuType string, gpus int32) *gryviav1.GryviaAIJob {
	j := &gryviav1.GryviaAIJob{}
	j.Spec.GpuType = gpuType
	j.Spec.GPUs = gpus
	return j
}

func TestCheckGPUPolicy(t *testing.T) {
	skus := []sku{{"h100-80g", "H100", true}, {"l40", "L40", true}, {"old", "V100", false}}
	tests := []struct {
		name string
		job  *gryviav1.GryviaAIJob
		p    gpuPolicy
		want string // substring of one reason, "" = allowed
	}{
		{"no policy", job("H100", 8), gpuPolicy{}, ""},
		{"type allowed", job("h100", 1), gpuPolicy{allowedTypes: []string{"H100", "A100"}}, ""},
		{"type denied", job("L40", 1), gpuPolicy{allowedTypes: []string{"H100"}}, "not allowed for this namespace"},
		{"any type is not a violation", job("any", 1), gpuPolicy{allowedTypes: []string{"H100"}}, ""},
		{"empty type is not a violation", job("", 1), gpuPolicy{allowedTypes: []string{"H100"}}, ""},
		{"per-job limit", job("H100", 9), gpuPolicy{maxGPUsPerJob: 8}, "per-job limit of 8"},
		{"at the limit", job("H100", 8), gpuPolicy{maxGPUsPerJob: 8}, ""},
		{"tenant sku ok", job("H100", 1), gpuPolicy{tenant: "acme", allowedSkus: []string{"h100-80g"}, skus: skus}, ""},
		{"tenant sku missing", job("L40", 1), gpuPolicy{tenant: "acme", allowedSkus: []string{"h100-80g"}, skus: skus}, "no enabled catalog SKU"},
		{"tenant sku disabled", job("V100", 1), gpuPolicy{tenant: "acme", allowedSkus: []string{"old"}, skus: skus}, "no enabled catalog SKU"},
		{"tenant without allowedSkus is unrestricted", job("L40", 1), gpuPolicy{tenant: "acme", skus: skus}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(checkGPUPolicy(tc.job, tc.p), "; ")
			if tc.want == "" && got != "" {
				t.Fatalf("expected allowed, got %q", got)
			}
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("expected reason containing %q, got %q", tc.want, got)
			}
		})
	}
}

func obj(kind, name string, spec map[string]interface{}) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]interface{}{"spec": spec}}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "gryvia.io", Version: "v1alpha1", Kind: kind})
	u.SetName(name)
	return u
}

func TestLoadPolicyFromObjects(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, k := range []string{"GryviaQuota", "GryviaTenant", "GryviaGpuSku"} {
		gvk := schema.GroupVersionKind{Group: "gryvia.io", Version: "v1alpha1", Kind: k}
		scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind(k+"List"), &unstructured.UnstructuredList{})
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		obj("GryviaQuota", "q1", map[string]interface{}{
			"namespaces": []interface{}{"tenant-acme"},
			"gpuQuota":   map[string]interface{}{"allowedGPUTypes": []interface{}{"H100"}, "maxGPUsPerJob": int64(4)},
		}),
		obj("GryviaQuota", "other", map[string]interface{}{
			"namespaces": []interface{}{"elsewhere"},
			"gpuQuota":   map[string]interface{}{"maxGPUsPerJob": int64(1)},
		}),
		obj("GryviaTenant", "acme", map[string]interface{}{"allowedSkus": []interface{}{"h100-80g"}}),
		obj("GryviaGpuSku", "h100-80g", map[string]interface{}{"gpuType": "H100"}),
	).Build()

	v := NewGryviaAIJobValidator(c)
	p, ok := v.loadPolicy(context.Background(), "tenant-acme")
	if !ok {
		t.Fatal("policy should load")
	}
	if p.maxGPUsPerJob != 4 || len(p.allowedTypes) != 1 || p.tenant != "acme" || len(p.skus) != 1 || !p.skus[0].enabled {
		t.Fatalf("unexpected policy: %+v", p)
	}
	if reasons := checkGPUPolicy(job("H100", 8), p); len(reasons) != 1 {
		t.Fatalf("want exactly the per-job reason, got %v", reasons)
	}

	// A namespace no quota covers has no restrictions.
	free, ok := v.loadPolicy(context.Background(), "default")
	if !ok || len(checkGPUPolicy(job("L40", 64), free)) != 0 {
		t.Fatalf("uncovered namespace must be unrestricted: %+v", free)
	}
}

func TestLoadPolicyFailsOpen(t *testing.T) {
	// Missing CRDs, RBAC errors or timeouts make the list fail: admission must not block on it.
	failing := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("forbidden")
		},
	}).Build()
	v := NewGryviaAIJobValidator(failing)
	if _, ok := v.loadPolicy(context.Background(), "tenant-acme"); ok {
		t.Fatal("a failed lookup must report ok=false so the job is allowed")
	}
	if _, ok := NewGryviaAIJobValidator(nil).loadPolicy(context.Background(), "x"); ok {
		t.Fatal("nil client must skip policy")
	}
}
