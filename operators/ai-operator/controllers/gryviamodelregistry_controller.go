// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controllers

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

const (
	// Model registry phases
	PhaseRegistered = "Registered"
	PhaseDeploying  = "Deploying"
	PhaseServing    = "Serving"

	// Condition types for model registry
	ConditionModelRegistered = "ModelRegistered"
	ConditionModelServing    = "ModelServing"
)

// GryviaModelRegistryReconciler reconciles a GryviaModelRegistry object.
//
// A registry entry is metadata. Only when spec.autoServe is true and spec.stage is "production" does the controller
// create a GryviaInferenceService "<entry>-serving" (owned by the entry) and mirror its endpoint into
// status.servingEndpoint. Moving the entry to "archived" (or turning autoServe off) removes that service.
//
// With servingConfig.serviceName the versions of a model share one service instead, and a new production version is
// rolled out as its canary (see reconcileShared). With a promotionPolicy a staging entry whose metric beats every
// production entry of the same modelName is moved to production by the controller.
type GryviaModelRegistryReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Log    logr.Logger

	// AutoServeGPUCount is the gpuCount of an auto-served service whose servingConfig sets none (the
	// --autoserve-default-gpu-count flag, default 1). 0 serves on CPU.
	AutoServeGPUCount int32

	// Recorder emits RolledBack and RollbackRefused events (optional).
	Recorder record.EventRecorder
}

//+kubebuilder:rbac:groups=gryvia.io,resources=gryviamodelregistries,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviamodelregistries/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviamodelregistries/finalizers,verbs=update
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviainferenceservices,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;patch
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func servingName(model *gryviav1.GryviaModelRegistry) string { return childName(model.Name, "serving") }

// Reconcile drives one GryviaModelRegistry towards its spec.
func (r *GryviaModelRegistryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("gryviamodelregistry", req.NamespacedName)

	model := &gryviav1.GryviaModelRegistry{}
	if err := r.Get(ctx, req.NamespacedName, model); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !model.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil // the serving service is garbage-collected through its owner reference
	}

	orig := model.DeepCopy()
	res, err := r.reconcileModel(ctx, model)
	if perr := patchStatus(ctx, r.Client, model, orig); perr != nil {
		log.Error(perr, "Failed to patch model registry status")
		if err == nil {
			err = perr
		}
	}
	return res, err
}

func (r *GryviaModelRegistryReconciler) reconcileModel(ctx context.Context, model *gryviav1.GryviaModelRegistry) (ctrl.Result, error) {
	if model.Status.RegisteredAt == nil {
		now := metav1.Now()
		model.Status.RegisteredAt = &now
	}
	if model.Status.Phase == "" {
		model.Status.Phase = PhaseRegistered
	}
	setCondition(&model.Status.Conditions, model.Generation, ConditionModelRegistered, metav1.ConditionTrue, "Registered",
		fmt.Sprintf("Model %s version %s registered", model.Spec.ModelName, model.Spec.Version))

	if err := r.applyPromotionPolicy(ctx, model); err != nil {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, err
	}
	if done, err := r.applyRollback(ctx, model); err != nil || done {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, err
	}

	serve := model.Spec.AutoServe && model.Spec.Stage == gryviav1.ModelStageProduction
	if model.Spec.Stage == gryviav1.ModelStageProduction {
		r.trackPreviousVersion(ctx, model)
	}

	if !serve {
		return ctrl.Result{}, r.stopServing(ctx, model)
	}

	ensure := r.ensureInferenceService
	if shared := sharedServiceName(model); shared != "" {
		ensure = func(ctx context.Context, m *gryviav1.GryviaModelRegistry) error {
			return r.reconcileShared(ctx, m, shared)
		}
	}
	if err := ensure(ctx, model); err != nil {
		if isConfigError(err) {
			model.Status.Phase = PhaseFailed
			model.Status.Message = err.Error()
			setCondition(&model.Status.Conditions, model.Generation, ConditionModelServing, metav1.ConditionFalse, "Failed", err.Error())
			return ctrl.Result{}, nil
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}
	if sharedServiceName(model) == "" {
		r.syncInferenceHealth(ctx, model)
	}

	if model.Status.Phase == PhaseServing || model.Status.Phase == PhaseRolledBack {
		return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
	}
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// stopServing removes the auto-created serving service when the entry no longer qualifies (archived, autoServe
// turned off, moved out of production) and clears the serving status.
func (r *GryviaModelRegistryReconciler) stopServing(ctx context.Context, model *gryviav1.GryviaModelRegistry) error {
	name := servingName(model)
	infer := &gryviav1.GryviaInferenceService{}
	err := r.Get(ctx, types.NamespacedName{Namespace: model.Namespace, Name: name}, infer)
	if err == nil && metav1.IsControlledBy(infer, model) {
		if derr := r.Delete(ctx, infer); derr != nil && !errors.IsNotFound(derr) {
			return derr
		}
	} else if err != nil && !errors.IsNotFound(err) {
		return err
	}
	if model.Status.InferenceServiceName != "" || model.Status.ServingEndpoint != "" {
		model.Status.InferenceServiceName = ""
		model.Status.ServingEndpoint = ""
		model.Status.Health = ""
		model.Status.DeployedAt = nil
		model.Status.Message = "Serving stopped"
	}
	if model.Status.Phase != PhaseRolledBack {
		model.Status.Phase = PhaseRegistered
		setCondition(&model.Status.Conditions, model.Generation, ConditionModelServing, metav1.ConditionFalse, "NotServing", "Model is not auto-served")
	}
	return nil
}

// servingSpec is the InferenceService spec derived from the entry and its servingConfig.
func (r *GryviaModelRegistryReconciler) servingSpec(model *gryviav1.GryviaModelRegistry) gryviav1.GryviaInferenceServiceSpec {
	spec := gryviav1.GryviaInferenceServiceSpec{
		ModelRef: model.Name,
		Backend:  gryviav1.BackendVLLM,
		Replicas: 1,
		GPUCount: r.AutoServeGPUCount,
	}
	if sc := model.Spec.ServingConfig; sc != nil {
		if sc.Backend != "" {
			spec.Backend = gryviav1.InferenceBackend(sc.Backend)
		}
		if sc.Replicas > 0 {
			spec.Replicas = sc.Replicas
		}
		if sc.GPUCount > 0 {
			spec.GPUCount = sc.GPUCount
		}
		spec.GPUType = sc.GPUType
		spec.Args = sc.Args
		spec.ServicePort = sc.ServicePort
	}
	return spec
}

// servingSettingsDiffer reports whether the serving settings the registry owns differ between two specs.
func servingSettingsDiffer(a, b gryviav1.GryviaInferenceServiceSpec) bool {
	return a.Backend != b.Backend || a.Replicas != b.Replicas || a.GPUCount != b.GPUCount ||
		a.GPUType != b.GPUType || a.ServicePort != b.ServicePort || !reflect.DeepEqual(a.Args, b.Args)
}

// copyServingSettings copies the serving settings the registry owns from desired into spec.
func copyServingSettings(spec *gryviav1.GryviaInferenceServiceSpec, desired gryviav1.GryviaInferenceServiceSpec) {
	spec.Backend, spec.Replicas, spec.Args = desired.Backend, desired.Replicas, desired.Args
	spec.GPUCount, spec.GPUType, spec.ServicePort = desired.GPUCount, desired.GPUType, desired.ServicePort
}

// ensureInferenceService creates the serving GryviaInferenceService, or updates it when servingConfig changed.
func (r *GryviaModelRegistryReconciler) ensureInferenceService(ctx context.Context, model *gryviav1.GryviaModelRegistry) error {
	name := servingName(model)
	desired := r.servingSpec(model)

	existing := &gryviav1.GryviaInferenceService{}
	err := r.Get(ctx, types.NamespacedName{Namespace: model.Namespace, Name: name}, existing)
	switch {
	case err == nil:
		if !metav1.IsControlledBy(existing, model) {
			return configError{fmt.Errorf("inference service %q already exists and is not owned by this model", name)}
		}
		if servingSettingsDiffer(existing.Spec, desired) {
			base := existing.DeepCopy()
			copyServingSettings(&existing.Spec, desired)
			if err := r.Patch(ctx, existing, client.MergeFrom(base)); err != nil {
				return err
			}
		}
	case errors.IsNotFound(err):
		infer := &gryviav1.GryviaInferenceService{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: model.Namespace,
				Labels: map[string]string{
					"gryvia.io/model":     model.Name,
					"gryvia.io/component": "inference",
				},
			},
			Spec: desired,
		}
		if err := controllerutil.SetControllerReference(model, infer, r.Scheme); err != nil {
			return err
		}
		if err := r.Create(ctx, infer); err != nil {
			if errors.IsInvalid(err) {
				return configError{err}
			}
			if !errors.IsAlreadyExists(err) {
				return fmt.Errorf("failed to create inference service: %w", err)
			}
		}
		model.Status.Phase = PhaseDeploying
		model.Status.Message = "Deploying inference service"
		setCondition(&model.Status.Conditions, model.Generation, ConditionModelServing, metav1.ConditionFalse, "Deploying", "Inference service being deployed")
	default:
		return err
	}
	model.Status.InferenceServiceName = name
	return nil
}

// syncInferenceHealth mirrors the serving service's state into the entry.
func (r *GryviaModelRegistryReconciler) syncInferenceHealth(ctx context.Context, model *gryviav1.GryviaModelRegistry) {
	infer := &gryviav1.GryviaInferenceService{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: model.Namespace, Name: model.Status.InferenceServiceName}, infer); err != nil {
		if errors.IsNotFound(err) {
			model.Status.Health = "Unknown"
			model.Status.Phase = PhaseDeploying // recreated by the next pass
		}
		return
	}
	switch infer.Status.Phase {
	case PhaseReady:
		model.Status.Phase = PhaseServing
		model.Status.Health = "Healthy"
		model.Status.Message = ""
		model.Status.ServingEndpoint = infer.Status.Endpoint
		if model.Status.DeployedAt == nil {
			now := metav1.Now()
			model.Status.DeployedAt = &now
		}
		setCondition(&model.Status.Conditions, model.Generation, ConditionModelServing, metav1.ConditionTrue, "Serving", "Model is being served")
	case PhaseFailed:
		model.Status.Phase = PhaseFailed
		model.Status.Health = "Unhealthy"
		model.Status.Message = infer.Status.Message
		setCondition(&model.Status.Conditions, model.Generation, ConditionModelServing, metav1.ConditionFalse, "Failed", infer.Status.Message)
	default:
		model.Status.Phase = PhaseDeploying
		model.Status.Health = "Unknown"
		model.Status.Message = "Deploying inference service"
	}
}

// trackPreviousVersion records another production version of the same model name, for rollback bookkeeping.
func (r *GryviaModelRegistryReconciler) trackPreviousVersion(ctx context.Context, model *gryviav1.GryviaModelRegistry) {
	if model.Status.PreviousVersion != "" {
		return
	}
	models := &gryviav1.GryviaModelRegistryList{}
	if err := r.List(ctx, models, client.InNamespace(model.Namespace)); err != nil {
		return
	}
	sort.Slice(models.Items, func(i, j int) bool { return models.Items[i].Name < models.Items[j].Name })
	for _, m := range models.Items {
		if m.Name != model.Name && m.Spec.ModelName == model.Spec.ModelName && m.Spec.Version != model.Spec.Version &&
			m.Spec.Stage == gryviav1.ModelStageProduction {
			model.Status.PreviousVersion = m.Spec.Version
			return
		}
	}
}

// entriesOfSharedService maps a shared GryviaInferenceService to the entries that name it in
// servingConfig.serviceName.
func (r *GryviaModelRegistryReconciler) entriesOfSharedService(ctx context.Context, obj client.Object) []reconcile.Request {
	if obj.GetLabels()[labelManagedBy] != managedByRegistry {
		return nil
	}
	list := &gryviav1.GryviaModelRegistryList{}
	if err := r.List(ctx, list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var out []reconcile.Request
	for _, m := range list.Items {
		if sharedServiceName(&m) == obj.GetName() {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: m.Namespace, Name: m.Name}})
		}
	}
	return out
}

// SetupWithManager sets up the controller with the Manager.
func (r *GryviaModelRegistryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gryviav1.GryviaModelRegistry{}).
		Owns(&gryviav1.GryviaInferenceService{}).
		Watches(&gryviav1.GryviaInferenceService{}, handler.EnqueueRequestsFromMapFunc(r.entriesOfSharedService)).
		Complete(r)
}
