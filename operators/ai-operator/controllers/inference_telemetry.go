// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controllers

import (
	"context"
	"fmt"
	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const AnnotationInferenceTelemetry = "gryvia.io/inference-telemetry"

// Hash before injecting the self-referential revision label. UID is included so
// recreated Deployments cannot reuse the previous incarnation's samples.
func (r *GryviaInferenceServiceReconciler) enrichTelemetry(svc *gryviav1.GryviaInferenceService, d *appsv1.Deployment, uid types.UID) error {
	v := svc.Annotations[AnnotationInferenceTelemetry]
	if v != "" && v != "true" && v != "false" {
		return fmt.Errorf("%s must be true or false", AnnotationInferenceTelemetry)
	}
	if v != "true" {
		return nil
	}
	if r.TelemetryImage == "" {
		return fmt.Errorf("telemetry requires operator inference-telemetry-image")
	}
	if r.port(svc) == 18080 || r.port(svc) == 18081 {
		return fmt.Errorf("telemetry ports 18080/18081 conflict with servicePort")
	}
	base := d.Spec.Template.DeepCopy()
	base.Spec.Containers = base.Spec.Containers[:1]
	base.Spec.Containers[0].Ports[0].Name = "http"
	base.Annotations = nil
	hash := specHash(struct {
		Template corev1.PodTemplateSpec
		Image    string
		UID      types.UID
	}{*base, r.TelemetryImage, uid})
	base.Spec.Containers[0].Ports[0].Name = "backend-http"
	env := []corev1.EnvVar{
		{Name: "GRYVIA_POD", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
		{Name: "GRYVIA_UPSTREAM", Value: fmt.Sprintf("http://127.0.0.1:%d", r.port(svc))},
		{Name: "GRYVIA_NAMESPACE", Value: svc.Namespace}, {Name: "GRYVIA_INFERENCE", Value: svc.Name},
		{Name: "GRYVIA_TRACK", Value: d.Labels[labelTrack]}, {Name: "GRYVIA_MODEL_VERSION", Value: d.Annotations[annotationModelVersion]},
		{Name: "GRYVIA_DEPLOYMENT_UID", Value: string(uid)}, {Name: "GRYVIA_REVISION", Value: hash},
	}
	base.Spec.Containers = append(base.Spec.Containers, corev1.Container{Name: "telemetry", Image: r.TelemetryImage, Args: []string{"inference-proxy"}, Env: env,
		Ports:           []corev1.ContainerPort{{Name: "http", ContainerPort: 18080}, {Name: "metrics", ContainerPort: 18081}},
		ReadinessProbe:  &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt(18081)}}},
		Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("32Mi")}, Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")}},
		SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: boolPtr(false), RunAsNonRoot: boolPtr(true)},
	})
	grace := int64(130)
	base.Spec.TerminationGracePeriodSeconds = &grace
	base.Annotations = map[string]string{"prometheus.io/scrape": "true", "prometheus.io/port": "18081", "prometheus.io/path": "/metrics"}
	d.Spec.Template = *base
	d.Annotations[annotationSpecHash] = hash
	return nil
}

func (r *GryviaInferenceServiceReconciler) persistTelemetryUID(ctx context.Context, svc *gryviav1.GryviaInferenceService, d *appsv1.Deployment, model *gryviav1.GryviaModelRegistry) error {
	if svc.Annotations[AnnotationInferenceTelemetry] != "true" {
		return nil
	}
	fresh, err := r.buildDeployment(svc, model, d.Name, d.Labels[labelTrack], d.Annotations[annotationModelVersion], *d.Spec.Replicas)
	if err != nil {
		return err
	}
	if err := r.enrichTelemetry(svc, fresh, d.UID); err != nil {
		return err
	}
	base := d.DeepCopy()
	d.Spec.Template = fresh.Spec.Template
	d.Annotations[annotationSpecHash] = fresh.Annotations[annotationSpecHash]
	return r.Patch(ctx, d, client.MergeFrom(base))
}
