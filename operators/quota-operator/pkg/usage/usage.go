// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package usage

import (
	"context"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	gryviav1 "github.com/zyvorai/gryvia/operators/quota-operator/api/v1"
)

// CalculateUsage calculates current resource usage for a quota
func CalculateUsage(ctx context.Context, k8sClient client.Client, quota *gryviav1.GryviaQuota) (*gryviav1.QuotaUsage, error) {
	usage := &gryviav1.QuotaUsage{}

	// Get all AI jobs in quota namespaces
	totalGPUs := int32(0)
	runningJobs := 0
	queuedJobs := 0
	gpuHours := 0.0

	for _, nsName := range quota.Spec.Namespaces {
		jobs := &gryviav1.GryviaAIJobList{}
		if err := k8sClient.List(ctx, jobs, client.InNamespace(nsName)); err != nil {
			return nil, err
		}

		for _, job := range jobs.Items {
			switch job.Status.Phase {
			case "Running":
				totalGPUs += job.Spec.TotalGPUs()
				runningJobs++

				// Calculate GPU hours
				if job.Status.StartTime != nil && !job.Status.StartTime.IsZero() {
					duration := time.Since(job.Status.StartTime.Time)
					hours := duration.Hours()
					gpuHours += hours * float64(job.Spec.TotalGPUs())
				}

			case "Pending", "Queued":
				queuedJobs++
			}
		}
	}

	usage.AllocatedGPUs = int(totalGPUs)
	usage.RunningJobs = runningJobs
	usage.QueuedJobs = queuedJobs
	usage.GPUHours = gpuHours

	return usage, nil
}

// GetMonthlyGPUHours calculates total GPU hours for the current month
func GetMonthlyGPUHours(ctx context.Context, k8sClient client.Client, quota *gryviav1.GryviaQuota) (float64, error) {
	// Get start of current month
	now := time.Now()
	startOfMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())

	totalHours := 0.0

	for _, nsName := range quota.Spec.Namespaces {
		jobs := &gryviav1.GryviaAIJobList{}
		if err := k8sClient.List(ctx, jobs, client.InNamespace(nsName)); err != nil {
			return 0, err
		}

		for _, job := range jobs.Items {
			// Skip jobs without a start time
			if job.Status.StartTime == nil || job.Status.StartTime.IsZero() {
				continue
			}

			// For jobs started before this month, count hours from start of month
			effectiveStart := job.Status.StartTime.Time
			if effectiveStart.Before(startOfMonth) {
				effectiveStart = startOfMonth
			}

			// Calculate duration
			var duration time.Duration
			if job.Status.CompletionTime == nil || job.Status.CompletionTime.IsZero() {
				// Still running
				duration = time.Since(effectiveStart)
			} else {
				// Completed
				duration = job.Status.CompletionTime.Time.Sub(effectiveStart)
			}

			hours := duration.Hours()
			totalHours += hours * float64(job.Spec.TotalGPUs())
		}
	}

	return totalHours, nil
}
