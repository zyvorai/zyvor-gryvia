// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controllers

import (
	"fmt"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	InferenceGPUMetric        = "gryvia_gpu_utilization"
	InferenceRPSMetric        = "gryvia_inference_requests_per_second"
	ConditionAutoscalingReady = "AutoscalingReady"
)

func int32Ptr(v int32) *int32 { return &v }

// Pods metrics average across the target Deployment's pods, never across tenants
// or the canary. HPA takes the largest recommendation when both are configured.
func inferenceHPAMetrics(a *gryviav1.AutoscalingConfig) ([]autoscalingv2.MetricSpec, error) {
	if a.TargetGPUUtilization < 0 || a.TargetGPUUtilization > 100 || a.TargetRequestsPerSecond < 0 {
		return nil, fmt.Errorf("targetGPUUtilization must be 0-100 and targetRequestsPerSecond must be non-negative")
	}
	metrics := []autoscalingv2.MetricSpec{}
	for _, m := range []struct {
		name  string
		value int32
	}{{InferenceGPUMetric, a.TargetGPUUtilization}, {InferenceRPSMetric, a.TargetRequestsPerSecond}} {
		if m.value == 0 {
			continue
		}
		q := resource.NewQuantity(int64(m.value), resource.DecimalSI)
		metrics = append(metrics, autoscalingv2.MetricSpec{Type: autoscalingv2.PodsMetricSourceType, Pods: &autoscalingv2.PodsMetricSource{
			Metric: autoscalingv2.MetricIdentifier{Name: m.name}, Target: autoscalingv2.MetricTarget{Type: autoscalingv2.AverageValueMetricType, AverageValue: q},
		}})
	}
	if len(metrics) == 0 {
		metrics = append(metrics, autoscalingv2.MetricSpec{Type: autoscalingv2.ResourceMetricSourceType, Resource: &autoscalingv2.ResourceMetricSource{
			Name: corev1.ResourceCPU, Target: autoscalingv2.MetricTarget{Type: autoscalingv2.UtilizationMetricType, AverageUtilization: int32Ptr(80)},
		}})
	}
	return metrics, nil
}

func reportHPAMetricStatus(svc *gryviav1.GryviaInferenceService, hpa *autoscalingv2.HorizontalPodAutoscaler, exists bool) {
	status, reason, msg := metav1.ConditionUnknown, "WaitingForHPA", "Waiting for the HPA controller to resolve scaling metrics"
	// The upstream HPA controller leaves status.observedGeneration unset; only a set, older value marks the status stale.
	stale := hpa.Status.ObservedGeneration != nil && *hpa.Status.ObservedGeneration < hpa.Generation
	if exists && !stale {
		for _, c := range hpa.Status.Conditions {
			if c.Type == autoscalingv2.ScalingActive {
				status, reason, msg = metav1.ConditionStatus(c.Status), c.Reason, c.Message
				break
			}
		}
	}
	setCondition(&svc.Status.Conditions, svc.Generation, ConditionAutoscalingReady, status, reason, msg)
}
