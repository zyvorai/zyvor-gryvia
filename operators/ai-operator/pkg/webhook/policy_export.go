// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package webhook

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

// GPUPolicy is what the quotas and the tenant catalog allow for one namespace. It is exported
// so the AIJob controller's admission gate (pkg/admission) applies exactly the rules the
// webhook applies, from one implementation.
type GPUPolicy = gpuPolicy

// LoadGPUPolicy reads the policy for a namespace. Unlike the webhook, it returns the read
// error and leaves the fail-open decision to the caller.
func LoadGPUPolicy(ctx context.Context, c client.Client, namespace string) (GPUPolicy, error) {
	return loadGPUPolicy(ctx, c, namespace)
}

// CheckGPUPolicy returns the reasons a job is not allowed by the policy, or nil.
func CheckGPUPolicy(job *gryviav1.GryviaAIJob, p GPUPolicy) []string { return checkGPUPolicy(job, p) }

// AllowedSkus is the tenant's allowedSkus list (empty: every enabled SKU).
func (p gpuPolicy) AllowedSkus() []string { return p.allowedSkus }
