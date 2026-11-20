// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controllers

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

const (
	PhaseCanary     = "Canary"
	PhaseRolledBack = "RolledBack"

	ConditionPromotionGate = "PromotionGate"
	ConditionRolledBack    = "RolledBack"

	// annotationRollbackRequested on a production entry asks the controller to roll its shared service back to
	// status.previousVersion (set by the gateway, the CLI and workflow registry steps; removed once handled).
	annotationRollbackRequested = "gryvia.io/rollback-requested"

	PromotionPromoted = "Promoted"
	PromotionRejected = "Rejected"
	PromotionWaiting  = "Waiting"

	defaultAutoCanaryWeight   = 10
	defaultAutoPromoteSeconds = 300

	labelServingGroup = "gryvia.io/serving-group"
	labelManagedBy    = "gryvia.io/managed-by"
	managedByRegistry = "gryvia-model-registry"
	directionMinimize = "minimize"
	promotionMaxPeers = 1000
)

// applyPromotionPolicy decides whether a staging entry with a promotionPolicy moves to production, and moves it
// (a patch of spec.stage, as the gateway's promote action does).
func (r *GryviaModelRegistryReconciler) applyPromotionPolicy(ctx context.Context, model *gryviav1.GryviaModelRegistry) error {
	if model.Spec.PromotionPolicy == nil || model.Spec.Stage != gryviav1.ModelStageStaging {
		return nil
	}
	decision, msg, err := r.promotionDecision(ctx, model)
	if err != nil {
		return err
	}
	model.Status.PromotionDecision = decision
	status := metav1.ConditionFalse
	if decision == PromotionPromoted {
		status = metav1.ConditionTrue
	}
	setCondition(&model.Status.Conditions, model.Generation, ConditionPromotionGate, status, decision, msg)
	if decision != PromotionPromoted {
		return nil
	}
	// The patch response carries the stored status; keep what this pass computed.
	computed := model.Status.DeepCopy()
	base := model.DeepCopy()
	model.Spec.Stage = gryviav1.ModelStageProduction
	if err := r.Patch(ctx, model, client.MergeFrom(base)); err != nil {
		return err
	}
	model.Status = *computed
	model.Status.Message = msg
	return nil
}

// promotionDecision compares the entry's metric against every production entry of the same modelName.
func (r *GryviaModelRegistryReconciler) promotionDecision(ctx context.Context, model *gryviav1.GryviaModelRegistry) (string, string, error) {
	p := model.Spec.PromotionPolicy
	raw, ok := model.Spec.Metadata[p.Metric]
	if !ok || strings.TrimSpace(raw) == "" {
		return PromotionWaiting, fmt.Sprintf("metadata %q is not set yet", p.Metric), nil
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return PromotionRejected, fmt.Sprintf("metadata %q = %q is not a number", p.Metric, raw), nil
	}
	minimize := p.Direction == directionMinimize
	better := func(a, b float64) float64 { // how much a improves on b
		if minimize {
			return b - a
		}
		return a - b
	}
	delta := 0.0
	if p.MinDelta != "" {
		if delta, err = strconv.ParseFloat(p.MinDelta, 64); err != nil || delta < 0 {
			return PromotionRejected, fmt.Sprintf("promotionPolicy.minDelta %q is not a non-negative number", p.MinDelta), nil
		}
	}
	if p.Threshold != "" {
		th, err := strconv.ParseFloat(p.Threshold, 64)
		if err != nil {
			return PromotionRejected, fmt.Sprintf("promotionPolicy.threshold %q is not a number", p.Threshold), nil
		}
		if better(v, th) < 0 {
			return PromotionRejected, fmt.Sprintf("%s %s does not reach the threshold %s", p.Metric, raw, p.Threshold), nil
		}
	}

	list := &gryviav1.GryviaModelRegistryList{}
	if err := r.List(ctx, list, client.InNamespace(model.Namespace), client.Limit(promotionMaxPeers)); err != nil {
		return "", "", err
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	var bestName, bestRaw string
	var best float64
	for _, m := range list.Items {
		if m.Name == model.Name || m.Spec.ModelName != model.Spec.ModelName || m.Spec.Stage != gryviav1.ModelStageProduction {
			continue
		}
		pr, ok := m.Spec.Metadata[p.Metric]
		pv, err := strconv.ParseFloat(strings.TrimSpace(pr), 64)
		if !ok || err != nil {
			return PromotionRejected, fmt.Sprintf("production entry %s has no numeric %q to compare against; promote by hand", m.Name, p.Metric), nil
		}
		if bestName == "" || better(pv, best) > 0 {
			bestName, bestRaw, best = m.Name, pr, pv
		}
	}
	if bestName == "" {
		return PromotionPromoted, fmt.Sprintf("Promoted: %s %s and no production version of %s yet", p.Metric, raw, model.Spec.ModelName), nil
	}
	if gain := better(v, best); gain <= 0 || gain < delta {
		return PromotionRejected, fmt.Sprintf("%s %s does not beat %s (%s) by %s", p.Metric, raw, bestName, bestRaw, orZero(p.MinDelta)), nil
	}
	return PromotionPromoted, fmt.Sprintf("Promoted: %s %s beats %s (%s)", p.Metric, raw, bestName, bestRaw), nil
}

// rollbackBreach reports whether spec.metadata[rollbackPolicy.metric] crossed the threshold. A missing or
// non-numeric metric is not a breach.
func rollbackBreach(model *gryviav1.GryviaModelRegistry) (bool, string) {
	p := model.Spec.RollbackPolicy
	if p == nil {
		return false, ""
	}
	raw := strings.TrimSpace(model.Spec.Metadata[p.Metric])
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return false, ""
	}
	th, err := strconv.ParseFloat(strings.TrimSpace(p.Threshold), 64)
	if err != nil {
		return false, ""
	}
	if p.Direction == directionMinimize {
		if v > th {
			return true, fmt.Sprintf("%s %s is above the rollback threshold %s", p.Metric, raw, p.Threshold)
		}
		return false, ""
	}
	if v < th {
		return true, fmt.Sprintf("%s %s is below the rollback threshold %s", p.Metric, raw, p.Threshold)
	}
	return false, ""
}

// previousEntry finds the entry status.previousVersion names: an entry name (recorded when a canary was promoted)
// or, failing that, a version of the same modelName. It must serve the same shared service.
func (r *GryviaModelRegistryReconciler) previousEntry(ctx context.Context, model *gryviav1.GryviaModelRegistry, service string) (*gryviav1.GryviaModelRegistry, error) {
	prev := model.Status.PreviousVersion
	if prev == "" {
		return nil, nil
	}
	m := &gryviav1.GryviaModelRegistry{}
	err := r.Get(ctx, types.NamespacedName{Namespace: model.Namespace, Name: prev}, m)
	if err == nil && m.Name != model.Name && m.Spec.ModelName == model.Spec.ModelName && sharedServiceName(m) == service {
		return m, nil
	}
	if err != nil && !errors.IsNotFound(err) {
		return nil, err
	}
	list := &gryviav1.GryviaModelRegistryList{}
	if err := r.List(ctx, list, client.InNamespace(model.Namespace), client.Limit(promotionMaxPeers)); err != nil {
		return nil, err
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	for i := range list.Items {
		c := &list.Items[i]
		if c.Name != model.Name && c.Spec.ModelName == model.Spec.ModelName && c.Spec.Version == prev && sharedServiceName(c) == service {
			return c, nil
		}
	}
	return nil, nil
}

// applyRollback rolls the shared service back to the previous version when a rollback was requested
// (annotation) or the rollbackPolicy metric crossed its threshold: the service's modelRef becomes the previous
// entry (moved back to production), and this entry is archived. It reports whether it rolled back; then the
// status is final for this pass.
func (r *GryviaModelRegistryReconciler) applyRollback(ctx context.Context, model *gryviav1.GryviaModelRegistry) (bool, error) {
	requested := model.Annotations[annotationRollbackRequested]
	breach, why := rollbackBreach(model)
	if requested == "" && !breach {
		return false, nil
	}
	reason := "PolicyBreached"
	if requested != "" {
		reason = "Requested"
		why = "rollback requested by " + requested
	}
	refuse := func(cause, msg string) (bool, error) {
		setCondition(&model.Status.Conditions, model.Generation, ConditionRolledBack, metav1.ConditionFalse, cause, msg)
		if requested != "" {
			r.event(model, "Warning", "RollbackRefused", msg)
			return false, r.clearRollbackRequest(ctx, model)
		}
		return false, nil
	}
	service := sharedServiceName(model)
	if model.Spec.Stage != gryviav1.ModelStageProduction || !model.Spec.AutoServe || service == "" {
		if requested == "" {
			return false, nil // the policy only applies to the serving version
		}
		return refuse("NotServing", "Rollback needs an auto-served production entry with servingConfig.serviceName")
	}
	prev, err := r.previousEntry(ctx, model, service)
	if err != nil {
		return false, err
	}
	if prev == nil {
		return refuse("NoPreviousVersion", fmt.Sprintf("Cannot roll back (%s): no previous version of %s on %s", why, model.Spec.ModelName, service))
	}
	svc := &gryviav1.GryviaInferenceService{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: model.Namespace, Name: service}, svc); err != nil {
		if errors.IsNotFound(err) {
			return refuse("NotServing", fmt.Sprintf("Shared inference service %s does not exist", service))
		}
		return false, err
	}
	if svc.Labels[labelManagedBy] != managedByRegistry {
		return refuse("NotServing", fmt.Sprintf("Inference service %s is not managed by the model registry", service))
	}
	// A retry after a partial rollback finds the service already on the previous entry.
	if svc.Spec.ModelRef != model.Name && svc.Spec.ModelRef != prev.Name {
		if requested == "" {
			return false, nil
		}
		return refuse("NotStable", fmt.Sprintf("%s is not the stable version of %s (%s is)", model.Name, service, svc.Spec.ModelRef))
	}

	if prev.Spec.Stage != gryviav1.ModelStageProduction || !prev.Spec.AutoServe {
		base := prev.DeepCopy()
		prev.Spec.Stage = gryviav1.ModelStageProduction
		prev.Spec.AutoServe = true
		if err := r.Patch(ctx, prev, client.MergeFrom(base)); err != nil {
			return false, err
		}
	}
	if svc.Spec.ModelRef != prev.Name || svc.Spec.Canary != nil {
		base := svc.DeepCopy()
		svc.Spec.ModelRef = prev.Name
		svc.Spec.Canary = nil
		if err := r.Patch(ctx, svc, client.MergeFrom(base)); err != nil {
			return false, err
		}
	}
	// The stable Deployment serves its promoted-version annotation over modelRef; drop it so the pods roll back.
	d := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: inferPrimaryName(svc)}, d); err == nil {
		if v, ok := d.Annotations[annotationPromoted]; ok && v != prev.Name {
			base := d.DeepCopy()
			delete(d.Annotations, annotationPromoted)
			if err := r.Patch(ctx, d, client.MergeFrom(base)); err != nil {
				return false, err
			}
		}
	} else if !errors.IsNotFound(err) {
		return false, err
	}

	msg := fmt.Sprintf("Rolled back: %s serves %s again (%s)", service, prev.Name, why)
	computed := model.Status.DeepCopy()
	base := model.DeepCopy()
	model.Spec.Stage = gryviav1.ModelStageArchived
	delete(model.Annotations, annotationRollbackRequested)
	if err := r.Patch(ctx, model, client.MergeFrom(base)); err != nil {
		return false, err
	}
	model.Status = *computed
	model.Status.Phase = PhaseRolledBack
	model.Status.Message = msg
	model.Status.InferenceServiceName = ""
	model.Status.ServingEndpoint = ""
	model.Status.Health = ""
	model.Status.DeployedAt = nil
	setCondition(&model.Status.Conditions, model.Generation, ConditionRolledBack, metav1.ConditionTrue, reason, msg)
	setCondition(&model.Status.Conditions, model.Generation, ConditionModelServing, metav1.ConditionFalse, "RolledBack", msg)
	r.event(model, "Normal", "RolledBack", msg)
	return true, nil
}

func (r *GryviaModelRegistryReconciler) clearRollbackRequest(ctx context.Context, model *gryviav1.GryviaModelRegistry) error {
	computed := model.Status.DeepCopy()
	base := model.DeepCopy()
	delete(model.Annotations, annotationRollbackRequested)
	if err := r.Patch(ctx, model, client.MergeFrom(base)); err != nil {
		return err
	}
	model.Status = *computed
	return nil
}

func (r *GryviaModelRegistryReconciler) event(model *gryviav1.GryviaModelRegistry, kind, reason, msg string) {
	if r.Recorder != nil {
		r.Recorder.Event(model, kind, reason, msg)
	}
}

func orZero(s string) string {
	if s == "" {
		return "more than 0"
	}
	return "at least " + s
}

func sharedServiceName(model *gryviav1.GryviaModelRegistry) string {
	if model.Spec.ServingConfig == nil {
		return ""
	}
	return model.Spec.ServingConfig.ServiceName
}

// reconcileShared serves the entry through the GryviaInferenceService shared by the versions of a model. The
// service is labelled, not owned, so archiving the version that created it does not delete it.
//
//   - no service yet: create it with this entry as modelRef;
//   - this entry is the modelRef: keep the serving settings in sync and mirror health;
//   - another entry is the modelRef: run this entry as the canary (autoPromote, autoRollback). Once promoted, the
//     entry becomes the modelRef and the entry it replaced is archived. A rolled-back entry stays RolledBack.
func (r *GryviaModelRegistryReconciler) reconcileShared(ctx context.Context, model *gryviav1.GryviaModelRegistry, name string) error {
	if !stepNameRE.MatchString(name) || len(name) > maxDNSLabel {
		return configError{fmt.Errorf("servingConfig.serviceName %q is not a lowercase DNS label", name)}
	}
	model.Status.InferenceServiceName = name
	svc := &gryviav1.GryviaInferenceService{}
	err := r.Get(ctx, types.NamespacedName{Namespace: model.Namespace, Name: name}, svc)
	if errors.IsNotFound(err) {
		svc = &gryviav1.GryviaInferenceService{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: model.Namespace,
				Labels: map[string]string{
					labelServingGroup:     name,
					labelManagedBy:        managedByRegistry,
					"gryvia.io/component": "inference",
				},
			},
			Spec: r.servingSpec(model),
		}
		svc.Spec.HealthCheck = &gryviav1.HealthCheckConfig{AutoRollback: true}
		if err := r.Create(ctx, svc); err != nil {
			if errors.IsInvalid(err) {
				return configError{err}
			}
			return err
		}
		model.Status.Phase = PhaseDeploying
		model.Status.Message = "Deploying shared inference service " + name
		setCondition(&model.Status.Conditions, model.Generation, ConditionModelServing, metav1.ConditionFalse, "Deploying", model.Status.Message)
		return nil
	}
	if err != nil {
		return err
	}
	if svc.Labels[labelManagedBy] != managedByRegistry || svc.Labels[labelServingGroup] != name {
		return configError{fmt.Errorf("inference service %q exists and is not managed by the model registry", name)}
	}

	if svc.Spec.ModelRef == model.Name {
		desired := r.servingSpec(model)
		if servingSettingsDiffer(svc.Spec, desired) {
			base := svc.DeepCopy()
			copyServingSettings(&svc.Spec, desired)
			if err := r.Patch(ctx, svc, client.MergeFrom(base)); err != nil {
				return err
			}
		}
		r.syncInferenceHealth(ctx, model)
		return nil
	}

	c := svc.Spec.Canary
	cs := svc.Status.CanaryStatus
	// The stable Deployment's annotations are the decision record; the canary status can lag a version behind.
	promoted, rolledBack, err := r.canaryDecisions(ctx, svc)
	if err != nil {
		return err
	}
	switch {
	case c != nil && c.Enabled && c.ModelVersion == model.Name && promoted == model.Name:
		previous := svc.Spec.ModelRef
		base := svc.DeepCopy()
		svc.Spec.ModelRef = model.Name
		svc.Spec.Canary = nil
		if err := r.Patch(ctx, svc, client.MergeFrom(base)); err != nil {
			return err
		}
		if err := r.archiveReplaced(ctx, model, previous, name); err != nil {
			return err
		}
		model.Status.Phase = PhaseDeploying
		model.Status.PreviousVersion = previous
		model.Status.Message = fmt.Sprintf("Canary promoted: %s replaces %s on %s", model.Name, previous, name)
		setCondition(&model.Status.Conditions, model.Generation, ConditionModelServing, metav1.ConditionFalse, "Promoted", model.Status.Message)
	case c != nil && c.Enabled && c.ModelVersion == model.Name && rolledBack == model.Name:
		model.Status.Phase = PhaseRolledBack
		model.Status.Health = "Unhealthy"
		model.Status.Message = fmt.Sprintf("Canary on %s was rolled back; %s keeps serving", name, svc.Spec.ModelRef)
		setCondition(&model.Status.Conditions, model.Generation, ConditionModelServing, metav1.ConditionFalse, "RolledBack", model.Status.Message)
	case c != nil && c.Enabled && c.ModelVersion == model.Name:
		model.Status.Phase = PhaseCanary
		model.Status.Health = "Unknown"
		if cs != nil && cs.Health != "" {
			model.Status.Health = cs.Health
		}
		model.Status.Message = fmt.Sprintf("Canary on %s at %d%% of the pods; %s is stable", name, c.Weight, svc.Spec.ModelRef)
		model.Status.ServingEndpoint = svc.Status.Endpoint
		setCondition(&model.Status.Conditions, model.Generation, ConditionModelServing, metav1.ConditionFalse, "Canary", model.Status.Message)
	case c != nil && c.Enabled && c.ModelVersion != promoted && c.ModelVersion != rolledBack && r.canaryBusy(ctx, svc):
		model.Status.Phase = PhaseDeploying
		model.Status.Message = fmt.Sprintf("Waiting: %s is the canary on %s", c.ModelVersion, name)
		setCondition(&model.Status.Conditions, model.Generation, ConditionModelServing, metav1.ConditionFalse, "Waiting", model.Status.Message)
	default:
		sc := model.Spec.ServingConfig
		weight, after := int32(defaultAutoCanaryWeight), int64(defaultAutoPromoteSeconds)
		if sc.CanaryWeight > 0 && sc.CanaryWeight < 100 {
			weight = sc.CanaryWeight
		}
		if sc.PromoteAfterSeconds > 0 {
			after = sc.PromoteAfterSeconds
		}
		base := svc.DeepCopy()
		svc.Spec.Canary = &gryviav1.CanaryConfig{Enabled: true, Weight: weight, ModelVersion: model.Name, AutoPromote: true, PromoteAfterSeconds: after}
		if svc.Spec.HealthCheck == nil {
			svc.Spec.HealthCheck = &gryviav1.HealthCheckConfig{AutoRollback: true}
		}
		if err := r.Patch(ctx, svc, client.MergeFrom(base)); err != nil {
			return err
		}
		model.Status.Phase = PhaseCanary
		model.Status.Health = "Unknown"
		model.Status.Message = fmt.Sprintf("Started as canary on %s at %d%% of the pods", name, weight)
		setCondition(&model.Status.Conditions, model.Generation, ConditionModelServing, metav1.ConditionFalse, "Canary", model.Status.Message)
	}
	return nil
}

// canaryDecisions reads the promoted and rolled-back versions the inference controller recorded on the stable
// Deployment ("" before the Deployment exists).
func (r *GryviaModelRegistryReconciler) canaryDecisions(ctx context.Context, svc *gryviav1.GryviaInferenceService) (string, string, error) {
	d := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: inferPrimaryName(svc)}, d); err != nil {
		if errors.IsNotFound(err) {
			return "", "", nil
		}
		return "", "", err
	}
	return d.Annotations[annotationPromoted], d.Annotations[annotationRolledBack], nil
}

// canaryBusy reports whether another version's undecided canary is still wanted (its entry exists, is in
// production and auto-served).
func (r *GryviaModelRegistryReconciler) canaryBusy(ctx context.Context, svc *gryviav1.GryviaInferenceService) bool {
	other := &gryviav1.GryviaModelRegistry{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: svc.Spec.Canary.ModelVersion}, other); err != nil {
		return !errors.IsNotFound(err)
	}
	return other.Spec.Stage == gryviav1.ModelStageProduction && other.Spec.AutoServe
}

// archiveReplaced moves the entry a promoted canary replaced to archived, if it is a production entry of the same
// shared service.
func (r *GryviaModelRegistryReconciler) archiveReplaced(ctx context.Context, model *gryviav1.GryviaModelRegistry, previous, service string) error {
	if previous == "" || previous == model.Name {
		return nil
	}
	old := &gryviav1.GryviaModelRegistry{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: model.Namespace, Name: previous}, old); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if old.Spec.Stage != gryviav1.ModelStageProduction || sharedServiceName(old) != service {
		return nil
	}
	base := old.DeepCopy()
	old.Spec.Stage = gryviav1.ModelStageArchived
	return r.Patch(ctx, old, client.MergeFrom(base))
}
