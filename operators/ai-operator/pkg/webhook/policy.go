// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package webhook

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

// The quota, tenant and SKU kinds belong to the quota operator's module, so they are read as unstructured
// objects here instead of importing that module.
var (
	quotaGVK  = schema.GroupVersionKind{Group: "gryvia.io", Version: "v1alpha1", Kind: "GryviaQuotaList"}
	tenantGVK = schema.GroupVersionKind{Group: "gryvia.io", Version: "v1alpha1", Kind: "GryviaTenantList"}
	skuGVK    = schema.GroupVersionKind{Group: "gryvia.io", Version: "v1alpha1", Kind: "GryviaGpuSkuList"}
)

const tenantNamespacePrefix = "tenant-"

// gpuPolicy is what the quotas and the tenant allow for one namespace. Zero values mean "no limit".
type gpuPolicy struct {
	allowedTypes  []string // union over the quotas covering the namespace; empty = any
	maxGPUsPerJob int64    // the smallest positive limit over those quotas
	tenant        string
	allowedSkus   []string // the tenant's allowed SKUs; empty = every enabled SKU
	skus          []sku    // every SKU when at least one exists
}

type sku struct {
	name, gpuType string
	enabled       bool
}

// checkGPUPolicy returns the reasons a job is not allowed, or nil. It is pure so it can be tested directly.
func checkGPUPolicy(job *gryviav1.GryviaAIJob, p gpuPolicy) []string {
	var reasons []string
	gpuType := job.Spec.GpuType
	anyType := gpuType == "" || strings.EqualFold(gpuType, "any")

	if !anyType && len(p.allowedTypes) > 0 && !containsFold(p.allowedTypes, gpuType) {
		reasons = append(reasons, fmt.Sprintf("GPU type %q is not allowed for this namespace (allowed: %s)",
			gpuType, strings.Join(p.allowedTypes, ", ")))
	}
	if total := job.Spec.TotalGPUs(); p.maxGPUsPerJob > 0 && int64(total) > p.maxGPUsPerJob {
		reasons = append(reasons, fmt.Sprintf("%d GPUs exceeds the per-job limit of %d", total, p.maxGPUsPerJob))
	}
	if p.tenant != "" && len(p.allowedSkus) > 0 && !anyType {
		ok := false
		for _, s := range p.skus {
			if s.enabled && strings.EqualFold(s.gpuType, gpuType) &&
				(containsFold(p.allowedSkus, s.name) || containsFold(p.allowedSkus, s.gpuType)) {
				ok = true
				break
			}
		}
		if !ok {
			reasons = append(reasons, fmt.Sprintf("tenant %q has no enabled catalog SKU for GPU type %q (allowed SKUs: %s)",
				p.tenant, gpuType, strings.Join(p.allowedSkus, ", ")))
		}
	}
	return reasons
}

func containsFold(list []string, v string) bool {
	for _, x := range list {
		if strings.EqualFold(x, v) {
			return true
		}
	}
	return false
}

// loadPolicy reads the quotas covering namespace and its tenant. Any read failure (missing CRDs, RBAC, timeouts)
// returns ok=false so admission never blocks on a stale or unreachable view; the quota operator's reactive
// enforcement still applies.
func (v *GryviaAIJobValidator) loadPolicy(ctx context.Context, namespace string) (gpuPolicy, bool) {
	if v.Client == nil {
		return gpuPolicy{}, false
	}
	p, err := loadGPUPolicy(ctx, v.Client, namespace)
	if err != nil {
		v.log.V(1).Info("quota lookup skipped", "error", err.Error())
		return p, false
	}
	return p, true
}

// loadGPUPolicy is loadPolicy without the fail-open policy: it returns the read error.
func loadGPUPolicy(ctx context.Context, c client.Client, namespace string) (gpuPolicy, error) {
	var p gpuPolicy
	quotas := &unstructured.UnstructuredList{}
	quotas.SetGroupVersionKind(quotaGVK)
	if err := c.List(ctx, quotas); err != nil {
		return p, err
	}
	for _, q := range quotas.Items {
		nss, _, _ := unstructured.NestedStringSlice(q.Object, "spec", "namespaces")
		if !containsFold(nss, namespace) {
			continue
		}
		types, _, _ := unstructured.NestedStringSlice(q.Object, "spec", "gpuQuota", "allowedGPUTypes")
		p.allowedTypes = append(p.allowedTypes, types...)
		if m, found, _ := unstructured.NestedInt64(q.Object, "spec", "gpuQuota", "maxGPUsPerJob"); found && m > 0 &&
			(p.maxGPUsPerJob == 0 || m < p.maxGPUsPerJob) {
			p.maxGPUsPerJob = m
		}
	}

	if strings.HasPrefix(namespace, tenantNamespacePrefix) {
		name := strings.TrimPrefix(namespace, tenantNamespacePrefix)
		tenants := &unstructured.UnstructuredList{}
		tenants.SetGroupVersionKind(tenantGVK)
		if err := c.List(ctx, tenants); err == nil {
			for _, t := range tenants.Items {
				if t.GetName() != name {
					continue
				}
				p.tenant = name
				p.allowedSkus, _, _ = unstructured.NestedStringSlice(t.Object, "spec", "allowedSkus")
			}
		}
		if p.tenant != "" && len(p.allowedSkus) > 0 {
			skus := &unstructured.UnstructuredList{}
			skus.SetGroupVersionKind(skuGVK)
			if err := c.List(ctx, skus, &client.ListOptions{}); err != nil {
				return p, err
			}
			for _, s := range skus.Items {
				gt, _, _ := unstructured.NestedString(s.Object, "spec", "gpuType")
				enabled, found, _ := unstructured.NestedBool(s.Object, "spec", "enabled")
				p.skus = append(p.skus, sku{name: s.GetName(), gpuType: gt, enabled: !found || enabled})
			}
		}
	}
	return p, nil
}
