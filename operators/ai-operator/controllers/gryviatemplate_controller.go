// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1 "k8s.io/api/core/v1"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

// GryviaTemplateReconciler reconciles a GryviaTemplate object
type GryviaTemplateReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Log    logr.Logger
}

//+kubebuilder:rbac:groups=gryvia.io,resources=gryviatemplates,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviatemplates/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviatemplates/finalizers,verbs=update
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaaijobs,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop
func (r *GryviaTemplateReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("gryviatemplate", req.NamespacedName)

	// Fetch the GryviaTemplate instance
	template := &gryviav1.GryviaTemplate{}
	err := r.Get(ctx, req.NamespacedName, template)
	if err != nil {
		if errors.IsNotFound(err) {
			log.Info("GryviaTemplate resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get GryviaTemplate")
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !template.ObjectMeta.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// Reconcile the template
	result, err := r.reconcileTemplate(ctx, template)
	if err != nil {
		log.Error(err, "Failed to reconcile template")
		return result, err
	}

	return result, nil
}

func (r *GryviaTemplateReconciler) reconcileTemplate(ctx context.Context, template *gryviav1.GryviaTemplate) (ctrl.Result, error) {
	log := r.Log.WithValues("gryviatemplate", template.Name)

	// Validate template parameters
	if err := r.validateTemplate(template); err != nil {
		log.Error(err, "Template validation failed")
		r.updateCondition(template, "Valid", metav1.ConditionFalse, "ValidationFailed", err.Error())
		if updateErr := r.Status().Update(ctx, template); updateErr != nil {
			log.Error(updateErr, "Failed to update status")
		}
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
	}

	r.updateCondition(template, "Valid", metav1.ConditionTrue, "Valid", "Template is valid")

	// Count jobs instantiated from this template
	jobList := &gryviav1.GryviaAIJobList{}
	if err := r.List(ctx, jobList); err != nil {
		log.Error(err, "Failed to list GryviaAIJobs")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}

	var instantiatedCount int32
	var lastInstantiatedTime *metav1.Time
	for _, job := range jobList.Items {
		labels := job.GetLabels()
		if labels != nil && labels["gryvia.io/template"] == template.Name {
			instantiatedCount++
			if lastInstantiatedTime == nil || job.CreationTimestamp.After(lastInstantiatedTime.Time) {
				t := job.CreationTimestamp
				lastInstantiatedTime = &t
			}
		}
	}

	template.Status.InstantiatedJobs = instantiatedCount
	template.Status.LastInstantiatedTime = lastInstantiatedTime

	r.updateCondition(template, "Ready", metav1.ConditionTrue, "Ready",
		fmt.Sprintf("Template ready, %d jobs instantiated", instantiatedCount))

	// Update status
	if err := r.Status().Update(ctx, template); err != nil {
		log.Error(err, "Failed to update status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
}

func (r *GryviaTemplateReconciler) validateTemplate(template *gryviav1.GryviaTemplate) error {
	// Validate category
	validCategories := map[string]bool{
		"training": true, "inference": true, "development": true, "benchmark": true,
	}
	if !validCategories[template.Spec.Category] {
		return fmt.Errorf("invalid category %q, must be one of: training, inference, development, benchmark", template.Spec.Category)
	}

	// Validate defaults
	if template.Spec.Defaults.Image == "" {
		return fmt.Errorf("template defaults must specify an image")
	}

	// Validate parameters
	for _, param := range template.Spec.Parameters {
		if param.Name == "" {
			return fmt.Errorf("parameter name cannot be empty")
		}

		if param.Validation != nil {
			if param.Validation.Min != nil && param.Validation.Max != nil {
				if *param.Validation.Min > *param.Validation.Max {
					return fmt.Errorf("parameter %q: min (%v) cannot be greater than max (%v)",
						param.Name, *param.Validation.Min, *param.Validation.Max)
				}
			}

			// Validate default value against enum
			if len(param.Validation.Enum) > 0 && param.Default != "" {
				found := false
				for _, v := range param.Validation.Enum {
					if v == param.Default {
						found = true
						break
					}
				}
				if !found {
					return fmt.Errorf("parameter %q: default value %q not in enum %v",
						param.Name, param.Default, param.Validation.Enum)
				}
			}
		}
	}

	return nil
}

// InstantiateJob creates a GryviaAIJob from this template with the given parameters.
// This method is intended to be called by external controllers or webhooks.
func (r *GryviaTemplateReconciler) InstantiateJob(ctx context.Context, template *gryviav1.GryviaTemplate, jobName, namespace string, params map[string]string) (*gryviav1.GryviaAIJob, error) {
	// Validate required parameters
	for _, param := range template.Spec.Parameters {
		if param.Required {
			if _, ok := params[param.Name]; !ok {
				if param.Default == "" {
					return nil, fmt.Errorf("required parameter %q not provided", param.Name)
				}
			}
		}
	}

	// Validate parameter values
	for _, param := range template.Spec.Parameters {
		value, ok := params[param.Name]
		if !ok {
			continue
		}

		if err := r.validateParameterValue(param, value); err != nil {
			return nil, fmt.Errorf("parameter %q validation failed: %w", param.Name, err)
		}
	}

	// Build merged parameters (defaults + overrides)
	mergedParams := make(map[string]string)
	for _, param := range template.Spec.Parameters {
		if param.Default != "" {
			mergedParams[param.Name] = param.Default
		}
	}
	for k, v := range params {
		mergedParams[k] = v
	}

	// Create the job from template defaults
	job := &gryviav1.GryviaAIJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: namespace,
			Labels: map[string]string{
				"gryvia.io/template": template.Name,
				"gryvia.io/category": template.Spec.Category,
			},
		},
		Spec: gryviav1.GryviaAIJobSpec{
			Type:  template.Spec.Category,
			Image: template.Spec.Defaults.Image,
		},
	}

	// Apply resource defaults
	if template.Spec.Defaults.Resources != nil {
		job.Spec.GpuType = template.Spec.Defaults.Resources.GpuType
		job.Spec.GPUs = template.Spec.Defaults.Resources.GpuCount
		if template.Spec.Defaults.Resources.Memory != "" {
			memQty, err := resource.ParseQuantity(template.Spec.Defaults.Resources.Memory)
			if err == nil {
				job.Spec.Resources.Requests = corev1.ResourceList{
					corev1.ResourceMemory: memQty,
				}
			}
		}
	}

	// Apply command with parameter substitution
	if len(template.Spec.Defaults.Command) > 0 {
		command := make([]string, len(template.Spec.Defaults.Command))
		for i, cmd := range template.Spec.Defaults.Command {
			command[i] = r.substituteParams(cmd, mergedParams)
		}
		job.Spec.Command = command
	}

	// Apply env vars
	if len(template.Spec.Defaults.Env) > 0 {
		envVars := make([]corev1.EnvVar, len(template.Spec.Defaults.Env))
		for i, e := range template.Spec.Defaults.Env {
			envVars[i] = corev1.EnvVar{
				Name:  e.Name,
				Value: r.substituteParams(e.Value, mergedParams),
			}
		}
		job.Spec.Env = envVars
	}

	if err := r.Create(ctx, job); err != nil {
		return nil, fmt.Errorf("failed to create job from template: %w", err)
	}

	return job, nil
}

func (r *GryviaTemplateReconciler) validateParameterValue(param gryviav1.TemplateParameter, value string) error {
	if param.Validation == nil {
		return nil
	}

	// Check enum
	if len(param.Validation.Enum) > 0 {
		found := false
		for _, v := range param.Validation.Enum {
			if v == value {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("value %q not in allowed values %v", value, param.Validation.Enum)
		}
	}

	// Check numeric ranges
	if param.Type == "integer" || param.Type == "number" {
		numVal, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return fmt.Errorf("expected numeric value, got %q", value)
		}

		if param.Validation.Min != nil && numVal < *param.Validation.Min {
			return fmt.Errorf("value %v is below minimum %v", numVal, *param.Validation.Min)
		}
		if param.Validation.Max != nil && numVal > *param.Validation.Max {
			return fmt.Errorf("value %v is above maximum %v", numVal, *param.Validation.Max)
		}
	}

	return nil
}

func (r *GryviaTemplateReconciler) substituteParams(template string, params map[string]string) string {
	result := template
	for key, value := range params {
		result = strings.ReplaceAll(result, "{{ ."+key+" }}", value)
		result = strings.ReplaceAll(result, "{{."+key+"}}", value)
	}
	return result
}

func (r *GryviaTemplateReconciler) updateCondition(template *gryviav1.GryviaTemplate, condType string, status metav1.ConditionStatus, reason, message string) {
	condition := metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: template.Generation,
		LastTransitionTime: metav1.Now(),
	}

	for _, cond := range template.Status.Conditions {
		if cond.Type == condType && cond.Status == status {
			condition.LastTransitionTime = cond.LastTransitionTime
			break
		}
	}

	found := false
	for i, cond := range template.Status.Conditions {
		if cond.Type == condType {
			template.Status.Conditions[i] = condition
			found = true
			break
		}
	}
	if !found {
		template.Status.Conditions = append(template.Status.Conditions, condition)
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *GryviaTemplateReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gryviav1.GryviaTemplate{}).
		Complete(r)
}
