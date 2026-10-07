package controllers

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

const (
	// Inference service phases
	PhaseDeployingInfer = "Deploying"
	PhaseReady          = "Ready"
	PhaseRollingBack    = "RollingBack"
	PhaseScaledToZero   = "ScaledToZero"

	// Condition types for inference service
	ConditionInferenceReady   = "InferenceReady"
	ConditionCanaryActive     = "CanaryActive"
	ConditionHealthy          = "Healthy"
	ConditionAutoscalingValid = "AutoscalingValid"
	ConditionScaleToZero      = "ScaleToZero"

	annotationSpecHash        = "gryvia.io/spec-hash"
	annotationModelVersion    = "gryvia.io/model-version"
	annotationPromoted        = "gryvia.io/promoted-version"
	annotationRolledBack      = "gryvia.io/rolled-back-version"
	labelTrack                = "gryvia.io/track"
	trackStable, trackCanary  = "stable", "canary"
	canaryHealthPending       = "Pending"
	canaryHealthHealthy       = "Healthy"
	canaryHealthUnhealthy     = "Unhealthy"
	canaryHealthPromoted      = "Promoted"
	canaryHealthRolledBack    = "RolledBack"
	defaultCPURequest         = "250m"
	defaultHealthIntervalSecs = 30
)

// DefaultInferenceImages are the images used when spec.image is empty and no --inference-image-<backend> flag is
// set. They are unpinned/unverified defaults: pin your own for production.
var DefaultInferenceImages = map[gryviav1.InferenceBackend]string{
	gryviav1.BackendVLLM:        "vllm/vllm-openai:latest",
	gryviav1.BackendTriton:      "nvcr.io/nvidia/tritonserver:24.01-py3",
	gryviav1.BackendTensorRTLLM: "nvcr.io/nvidia/tritonserver:24.01-trtllm-python-py3",
	gryviav1.BackendTorchServe:  "pytorch/torchserve:latest-gpu",
}

// backendDefaults: the port and the readiness path each server answers on out of the box.
var backendDefaults = map[gryviav1.InferenceBackend]struct {
	port int32
	path string
}{
	gryviav1.BackendVLLM:        {8000, "/health"},
	gryviav1.BackendTriton:      {8000, "/v2/health/ready"},
	gryviav1.BackendTensorRTLLM: {8000, "/v2/health/ready"},
	gryviav1.BackendTorchServe:  {8080, "/ping"},
}

// GryviaInferenceServiceReconciler reconciles a GryviaInferenceService object.
//
// It creates a stable Deployment "<name>-inference", a ClusterIP Service of the same name, an optional HPA
// "<name>-inference-hpa" and, while spec.canary is enabled, a canary Deployment "<name>-canary". The Service
// selects stable and canary pods alike, so the traffic split is proportional to the replica counts (the canary
// gets about weight% of the pods): this is not weighted routing, which needs a mesh or an ingress controller.
type GryviaInferenceServiceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Log    logr.Logger

	// Images overrides DefaultInferenceImages per backend (used when spec.image is empty).
	Images map[gryviav1.InferenceBackend]string
	// HealthPath, when set, replaces the per-backend default readiness path (spec.healthCheck.path still wins).
	HealthPath string
	// CanaryStartupGrace is how long a new canary may take to become ready before it counts as failing.
	CanaryStartupGrace time.Duration
	// Clock returns the current time (time.Now when nil); tests move it.
	Clock func() time.Time
	// GatewayRouting allows opt-in HTTPRoute reconciliation; off by default.
	GatewayRouting bool
	// PrometheusURL is administrator configured; empty disables SLO evaluation.
	PrometheusURL  string
	TelemetryImage string
}

//+kubebuilder:rbac:groups=gryvia.io,resources=gryviainferenceservices,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviainferenceservices/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviainferenceservices/finalizers,verbs=update
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviamodelregistries,verbs=get;list;watch
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete

func inferPrimaryName(svc *gryviav1.GryviaInferenceService) string {
	return childName(svc.Name, "inference")
}
func inferCanaryName(svc *gryviav1.GryviaInferenceService) string {
	return childName(svc.Name, "canary")
}
func inferHPAName(svc *gryviav1.GryviaInferenceService) string {
	return childName(svc.Name, "inference", "hpa")
}

func (r *GryviaInferenceServiceReconciler) port(svc *gryviav1.GryviaInferenceService) int32 {
	if svc.Spec.ServicePort > 0 {
		return svc.Spec.ServicePort
	}
	if d, ok := backendDefaults[svc.Spec.Backend]; ok {
		return d.port
	}
	return 8080
}

func (r *GryviaInferenceServiceReconciler) healthPath(svc *gryviav1.GryviaInferenceService) string {
	if svc.Spec.HealthCheck != nil && svc.Spec.HealthCheck.Path != "" {
		return svc.Spec.HealthCheck.Path
	}
	if r.HealthPath != "" {
		return r.HealthPath
	}
	if d, ok := backendDefaults[svc.Spec.Backend]; ok {
		return d.path
	}
	return "/health"
}

func (r *GryviaInferenceServiceReconciler) image(svc *gryviav1.GryviaInferenceService) (string, error) {
	if svc.Spec.Image != "" {
		return svc.Spec.Image, nil
	}
	if img := r.Images[svc.Spec.Backend]; img != "" {
		return img, nil
	}
	if img := DefaultInferenceImages[svc.Spec.Backend]; img != "" {
		return img, nil
	}
	return "", fmt.Errorf("unknown backend %q and no spec.image", svc.Spec.Backend)
}

// Reconcile drives one GryviaInferenceService towards its spec.
func (r *GryviaInferenceServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("gryviainferenceservice", req.NamespacedName)

	svc := &gryviav1.GryviaInferenceService{}
	if err := r.Get(ctx, req.NamespacedName, svc); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !svc.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil // children are garbage-collected through their owner reference
	}

	orig := svc.DeepCopy()
	res, err := r.reconcileService(ctx, svc)
	if perr := patchStatus(ctx, r.Client, svc, orig); perr != nil {
		log.Error(perr, "Failed to patch inference service status")
		if err == nil {
			err = perr
		}
	}
	return res, err
}

func (r *GryviaInferenceServiceReconciler) failSvc(svc *gryviav1.GryviaInferenceService, err error) {
	svc.Status.Phase = PhaseFailed
	svc.Status.Message = err.Error()
	setCondition(&svc.Status.Conditions, svc.Generation, ConditionInferenceReady, metav1.ConditionFalse, "Invalid", err.Error())
}

func (r *GryviaInferenceServiceReconciler) reconcileService(ctx context.Context, svc *gryviav1.GryviaInferenceService) (ctrl.Result, error) {
	if svc.Status.Phase == PhaseFailed {
		svc.Status.Message = "" // a failure message from an earlier pass no longer applies once the spec is valid again
	}
	if svc.Spec.ModelRef == "" {
		r.failSvc(svc, fmt.Errorf("spec.modelRef is required"))
		return ctrl.Result{}, nil
	}
	if _, err := r.image(svc); err != nil {
		r.failSvc(svc, err)
		return ctrl.Result{}, nil
	}

	if err := validateRoutingOptions(svc); err != nil {
		r.failSvc(svc, err)
		return ctrl.Result{}, nil
	}
	if _, err := analysisConfig(svc); err != nil {
		r.failSvc(svc, err)
		return ctrl.Result{}, nil
	}
	model := r.lookupModel(ctx, svc)

	wasZero := svc.Status.ScaledToZeroAt != nil
	atZero, idleWait, err := r.reconcileScaleToZero(ctx, svc)
	if err != nil {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, err
	}

	primary, err := r.ensureDeployment(ctx, svc, model, atZero, wasZero)
	if err != nil {
		if isConfigError(err) {
			r.failSvc(svc, err)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{RequeueAfter: 15 * time.Second}, err
	}
	if err := r.ensureService(ctx, svc); err != nil {
		if isConfigError(err) {
			r.failSvc(svc, err)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{RequeueAfter: 15 * time.Second}, err
	}
	// At zero the HPA is left as it is: it does not scale a Deployment with 0 replicas.
	var sloWait time.Duration
	if !atZero {
		sloWait = r.reconcileSLO(ctx, svc, primary, clock(r.Clock))
		if err := r.reconcileHPA(ctx, svc); err != nil {
			return ctrl.Result{RequeueAfter: 15 * time.Second}, err
		}
	}

	next := 30 * time.Second
	if idleWait > 0 {
		next = minDuration(next, idleWait)
	}
	if sloWait > 0 {
		next = minDuration(next, sloWait)
	}
	canaryWait, err := r.reconcileCanary(ctx, svc, primary, model)
	if err != nil {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, err
	}
	if canaryWait > 0 {
		next = minDuration(next, canaryWait)
	}

	if err := r.reconcileRouting(ctx, svc); err != nil {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, err
	}

	// Re-read the primary: a promotion may just have changed it.
	live := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: inferPrimaryName(svc)}, live); err == nil {
		primary = live
	}
	r.syncStatus(svc, primary, atZero)
	return ctrl.Result{RequeueAfter: next}, nil
}

// scaleToZeroEnabled reports whether idle scale-down applies; a canary rules it out.
func scaleToZeroEnabled(svc *gryviav1.GryviaInferenceService) bool {
	z := svc.Spec.ScaleToZero
	return z != nil && z.Enabled && !(svc.Spec.Canary != nil && svc.Spec.Canary.Enabled)
}

// annotationTime parses an RFC 3339 annotation; the zero time when it is missing or malformed.
func annotationTime(svc *gryviav1.GryviaInferenceService, key string) time.Time {
	t, err := time.Parse(time.RFC3339, svc.Annotations[key])
	if err != nil {
		return time.Time{}
	}
	return t
}

// reconcileScaleToZero decides whether the service should be at zero replicas now. It consumes a wake request
// (removing the annotation), scales an idle service down, and returns how long until the idle deadline.
func (r *GryviaInferenceServiceReconciler) reconcileScaleToZero(ctx context.Context, svc *gryviav1.GryviaInferenceService) (bool, time.Duration, error) {
	z := svc.Spec.ScaleToZero
	if z == nil || !z.Enabled {
		svc.Status.ScaledToZeroAt = nil
		svc.Status.LastWakeAt = nil
		removeCondition(&svc.Status.Conditions, ConditionScaleToZero)
		return false, 0, nil
	}
	if !scaleToZeroEnabled(svc) {
		svc.Status.ScaledToZeroAt = nil
		setCondition(&svc.Status.Conditions, svc.Generation, ConditionScaleToZero, metav1.ConditionFalse, "Unsupported",
			"scaleToZero is not applied while a canary is enabled")
		return false, 0, nil
	}
	now := clock(r.Clock)
	if _, ok := svc.Annotations[gryviav1.AnnotationWakeRequested]; ok {
		patch := []byte(fmt.Sprintf(`{"metadata":{"annotations":{%q:null}}}`, gryviav1.AnnotationWakeRequested))
		target := &gryviav1.GryviaInferenceService{ObjectMeta: metav1.ObjectMeta{Name: svc.Name, Namespace: svc.Namespace}}
		if err := r.Patch(ctx, target, client.RawPatch(types.MergePatchType, patch)); err != nil && !errors.IsNotFound(err) {
			return false, 0, err
		}
		t := metav1.NewTime(now)
		svc.Status.LastWakeAt = &t
		if svc.Status.ScaledToZeroAt != nil {
			svc.Status.ScaledToZeroAt = nil
			svc.Status.Message = "Woken by a request through the LLM gateway"
		}
	}
	if svc.Status.ScaledToZeroAt != nil {
		setCondition(&svc.Status.Conditions, svc.Generation, ConditionScaleToZero, metav1.ConditionTrue, "Idle",
			"Scaled to zero; a request through the LLM gateway wakes it")
		return true, 0, nil
	}

	// The idle clock runs from the later of the last proxied request and the last wake-up, which is first set when
	// scale-to-zero is turned on (so enabling it on a long-idle service does not scale it down at once).
	if svc.Status.LastWakeAt == nil {
		t := metav1.NewTime(now)
		svc.Status.LastWakeAt = &t
	}
	activity := svc.Status.LastWakeAt.Time
	if t := annotationTime(svc, gryviav1.AnnotationLastRequest); t.After(activity) {
		activity = t
	}
	deadline := activity.Add(z.Idle())
	// A service that is not serving yet gets the cold-start time on top, so a slow start is not cut short.
	if svc.Status.ReadyReplicas == 0 {
		deadline = deadline.Add(z.ColdStart())
	}
	if now.Before(deadline) {
		setCondition(&svc.Status.Conditions, svc.Generation, ConditionScaleToZero, metav1.ConditionFalse, "Active",
			fmt.Sprintf("Scales to zero after %s without requests", z.Idle()))
		return false, deadline.Sub(now) + time.Second, nil
	}
	t := metav1.NewTime(now)
	svc.Status.ScaledToZeroAt = &t
	svc.Status.Message = fmt.Sprintf("Scaled to zero after %s without requests", z.Idle())
	setCondition(&svc.Status.Conditions, svc.Generation, ConditionScaleToZero, metav1.ConditionTrue, "Idle",
		"Scaled to zero; a request through the LLM gateway wakes it")
	return true, 0, nil
}

// lookupModel returns the registry entry the service serves (nil when it is missing: the service still runs).
func (r *GryviaInferenceServiceReconciler) lookupModel(ctx context.Context, svc *gryviav1.GryviaInferenceService) *gryviav1.GryviaModelRegistry {
	m := &gryviav1.GryviaModelRegistry{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: svc.Spec.ModelRef}, m); err != nil {
		return nil
	}
	return m
}

// modelForVersion is the registry entry whose artifacts pods serving version should load: the entry named by
// version when it is one (a canary or promoted GryviaModelRegistry name), else the service's own model.
func (r *GryviaInferenceServiceReconciler) modelForVersion(ctx context.Context, svc *gryviav1.GryviaInferenceService,
	model *gryviav1.GryviaModelRegistry, version string) *gryviav1.GryviaModelRegistry {
	if version == "" || version == svc.Spec.ModelRef {
		return model
	}
	m := &gryviav1.GryviaModelRegistry{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: version}, m); err != nil {
		return model
	}
	return m
}

// ensureDeployment creates the stable Deployment or brings it back to the desired pod template.
func (r *GryviaInferenceServiceReconciler) ensureDeployment(ctx context.Context, svc *gryviav1.GryviaInferenceService, model *gryviav1.GryviaModelRegistry, atZero, wasZero bool) (*appsv1.Deployment, error) {
	name := inferPrimaryName(svc)
	deploy := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: name}, deploy)
	if err != nil && !errors.IsNotFound(err) {
		return nil, err
	}
	notFound := errors.IsNotFound(err)
	if !notFound && !metav1.IsControlledBy(deploy, svc) {
		return nil, configError{fmt.Errorf("deployment %q already exists and is not owned by this service", name)}
	}

	version := ""
	if !notFound {
		version = deploy.Annotations[annotationPromoted]
	}
	model = r.modelForVersion(ctx, svc, model, version)
	replicas := r.primaryReplicas(svc)
	if atZero {
		replicas = 0
	}
	desired, err := r.buildDeployment(svc, model, name, trackStable, version, replicas)
	if err != nil {
		return nil, configError{err}
	}

	if err := r.enrichTelemetry(svc, desired, deploy.UID); err != nil {
		return nil, configError{err}
	}
	if notFound {
		if err := controllerutil.SetControllerReference(svc, desired, r.Scheme); err != nil {
			return nil, err
		}
		if err := r.Create(ctx, desired); err != nil {
			return nil, err
		}
		if err := r.persistTelemetryUID(ctx, svc, desired, model); err != nil {
			return nil, err
		}
		svc.Status.DeploymentName = name
		return desired, nil
	}

	base := deploy.DeepCopy()
	changed := false
	if deploy.Annotations[annotationSpecHash] != desired.Annotations[annotationSpecHash] {
		deploy.Spec.Template = desired.Spec.Template
		deploy.Spec.Selector = base.Spec.Selector // immutable
		if deploy.Annotations == nil {
			deploy.Annotations = map[string]string{}
		}
		deploy.Annotations[annotationSpecHash] = desired.Annotations[annotationSpecHash]
		changed = true
	}
	// With an HPA the autoscaler owns the replica count: only reset it when there is none, when scaling to zero,
	// or when waking from zero (the HPA does not scale a Deployment up from 0).
	current := int32(-1)
	if deploy.Spec.Replicas != nil {
		current = *deploy.Spec.Replicas
	}
	owned := !autoscalingEnabled(svc) || atZero || (current == 0 && (wasZero || scaleToZeroEnabled(svc)))
	if owned && current != *desired.Spec.Replicas {
		deploy.Spec.Replicas = desired.Spec.Replicas
		changed = true
	}
	if changed {
		if err := r.Patch(ctx, deploy, client.MergeFrom(base)); err != nil {
			return nil, err
		}
	}
	svc.Status.DeploymentName = name
	return deploy, nil
}

func autoscalingEnabled(svc *gryviav1.GryviaInferenceService) bool {
	return svc.Spec.Autoscaling != nil && svc.Spec.Autoscaling.Enabled
}

// primaryReplicas is the replica count for a new stable Deployment (clamped into the HPA range when there is one).
func (r *GryviaInferenceServiceReconciler) primaryReplicas(svc *gryviav1.GryviaInferenceService) int32 {
	n := svc.Spec.Replicas
	if n <= 0 {
		n = 1
	}
	if autoscalingEnabled(svc) {
		if lo, hi, ok := autoscalingRange(svc); ok {
			if n < lo {
				n = lo
			}
			if n > hi {
				n = hi
			}
		}
	}
	return n
}

// buildDeployment constructs a stable or canary Deployment. version is the model version the pods serve (the
// promoted or canary version); empty for a service that never promoted a canary.
func (r *GryviaInferenceServiceReconciler) buildDeployment(svc *gryviav1.GryviaInferenceService, model *gryviav1.GryviaModelRegistry,
	name, track, version string, replicas int32) (*appsv1.Deployment, error) {
	image, err := r.image(svc)
	if err != nil {
		return nil, err
	}
	port := r.port(svc)
	path := r.healthPath(svc)

	selector := map[string]string{
		"gryvia.io/inference": svc.Name,
		"gryvia.io/component": "inference-server",
		labelTrack:            track,
	}

	container := corev1.Container{
		Name:  "inference",
		Image: image,
		Args:  svc.Spec.Args,
		Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: port, Protocol: corev1.ProtocolTCP}},
		Env: []corev1.EnvVar{
			{Name: "MODEL_NAME", Value: svc.Spec.ModelRef},
			{Name: "BACKEND", Value: string(svc.Spec.Backend)},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(defaultCPURequest)},
			Limits:   corev1.ResourceList{},
		},
		// Model servers can take minutes to load a model: the startup probe holds liveness back for up to 30 min.
		StartupProbe: &corev1.Probe{
			ProbeHandler:     corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: path, Port: intstr.FromInt32(port)}},
			PeriodSeconds:    5,
			FailureThreshold: 360,
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler:  corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: path, Port: intstr.FromInt32(port)}},
			PeriodSeconds: 10,
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler:  corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: path, Port: intstr.FromInt32(port)}},
			PeriodSeconds: 30,
		},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: boolPtr(false),
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
	}
	// gpuCount 0 (or unset) means a CPU service: no GPU limit.
	if svc.Spec.GPUCount > 0 {
		container.Resources.Limits["nvidia.com/gpu"] = *resource.NewQuantity(int64(svc.Spec.GPUCount), resource.DecimalSI)
	}
	if version != "" {
		container.Env = append(container.Env, corev1.EnvVar{Name: "MODEL_VERSION", Value: version})
	}
	if track == trackCanary {
		container.Env = append(container.Env, corev1.EnvVar{Name: "CANARY_MODEL", Value: version})
	}

	var volumes []corev1.Volume
	if model != nil {
		art := model.Spec.Artifacts
		if art.S3Path != "" {
			container.Env = append(container.Env, corev1.EnvVar{Name: "MODEL_S3_PATH", Value: art.S3Path})
		}
		if art.Format != "" {
			container.Env = append(container.Env, corev1.EnvVar{Name: "MODEL_FORMAT", Value: art.Format})
		}
		if art.PVCName != "" {
			volumes = append(volumes, corev1.Volume{Name: "model", VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: art.PVCName, ReadOnly: true},
			}})
			container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
				Name: "model", MountPath: "/models", SubPath: art.SubPath, ReadOnly: true,
			})
			container.Env = append(container.Env, corev1.EnvVar{Name: "MODEL_PATH", Value: "/models"})
		}
	}
	if svc.Spec.GPUCount > 0 {
		volumes = append(volumes, corev1.Volume{Name: "shm", VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory},
		}})
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "shm", MountPath: "/dev/shm"})
	}

	var nodeSelector map[string]string
	if svc.Spec.GPUType != "" && svc.Spec.GPUType != "any" {
		nodeSelector = map[string]string{"gryvia.io/gpu": svc.Spec.GPUType}
	}

	podLabels := selector
	if sloActive(svc) {
		podLabels = map[string]string{labelJob: sloMetricsJob(svc)}
		for k, v := range selector {
			podLabels[k] = v
		}
	}
	template := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
		Spec: corev1.PodSpec{
			Containers:                   []corev1.Container{container},
			Volumes:                      volumes,
			NodeSelector:                 nodeSelector,
			AutomountServiceAccountToken: boolPtr(false),
		},
	}
	labels := map[string]string{}
	for k, v := range selector {
		labels[k] = v
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: svc.Namespace,
			Labels:    labels,
			Annotations: map[string]string{
				annotationSpecHash:     specHash(template),
				annotationModelVersion: version,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: selector},
			Template: template,
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType},
		},
	}, nil
}

// ensureService creates the ClusterIP Service (stable and canary pods behind one name) and records the endpoint.
func (r *GryviaInferenceServiceReconciler) ensureService(ctx context.Context, svc *gryviav1.GryviaInferenceService) error {
	name := inferPrimaryName(svc)
	port := r.port(svc)
	k8sSvc := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: name}, k8sSvc)
	switch {
	case errors.IsNotFound(err):
		k8sSvc = &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: svc.Namespace,
				Labels:    map[string]string{"gryvia.io/inference": svc.Name, "gryvia.io/component": "inference-service"},
			},
			Spec: corev1.ServiceSpec{
				Selector: map[string]string{"gryvia.io/inference": svc.Name, "gryvia.io/component": "inference-server"},
				Ports: []corev1.ServicePort{{
					Name: "http", Port: port, TargetPort: intstr.FromString("http"), Protocol: corev1.ProtocolTCP,
				}},
				Type: corev1.ServiceTypeClusterIP,
			},
		}
		if err := controllerutil.SetControllerReference(svc, k8sSvc, r.Scheme); err != nil {
			return err
		}
		if err := r.Create(ctx, k8sSvc); err != nil {
			if errors.IsInvalid(err) {
				return configError{err}
			}
			if !errors.IsAlreadyExists(err) {
				return err
			}
		}
	case err != nil:
		return err
	case !metav1.IsControlledBy(k8sSvc, svc):
		return configError{fmt.Errorf("service %q already exists and is not owned by this inference service", name)}
	default:
		if len(k8sSvc.Spec.Ports) == 1 && k8sSvc.Spec.Ports[0].Port != port {
			base := k8sSvc.DeepCopy()
			k8sSvc.Spec.Ports[0].Port = port
			if err := r.Patch(ctx, k8sSvc, client.MergeFrom(base)); err != nil {
				return err
			}
		}
	}
	svc.Status.ServiceName = name
	svc.Status.Endpoint = fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", name, svc.Namespace, port)
	return nil
}

// autoscalingRange validates the HPA bounds.
func autoscalingRange(svc *gryviav1.GryviaInferenceService) (lo, hi int32, ok bool) {
	a := svc.Spec.Autoscaling
	lo = a.MinReplicas
	if lo <= 0 {
		lo = 1
	}
	hi = a.MaxReplicas
	return lo, hi, hi >= 1 && lo <= hi
}

// reconcileHPA creates, updates or removes the HPA. Explicit GPU/RPS targets
// use custom per-pod metrics; otherwise CPU utilisation remains the default.
func (r *GryviaInferenceServiceReconciler) reconcileHPA(ctx context.Context, svc *gryviav1.GryviaInferenceService) error {
	name := inferHPAName(svc)
	existing := &autoscalingv2.HorizontalPodAutoscaler{}
	err := r.Get(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: name}, existing)
	if err != nil && !errors.IsNotFound(err) {
		return err
	}
	exists := err == nil
	if exists && !metav1.IsControlledBy(existing, svc) {
		setCondition(&svc.Status.Conditions, svc.Generation, ConditionAutoscalingValid, metav1.ConditionFalse, "NameCollision",
			fmt.Sprintf("HPA %q already exists and is not owned by this inference service", name))
		return nil
	}

	if !autoscalingEnabled(svc) {
		if exists {
			if err := r.Delete(ctx, existing); err != nil && !errors.IsNotFound(err) {
				return err
			}
		}
		removeCondition(&svc.Status.Conditions, ConditionAutoscalingValid)
		removeCondition(&svc.Status.Conditions, ConditionAutoscalingReady)
		return nil
	}

	lo, hi, ok := autoscalingRange(svc)
	metricSpecs, metricErr := inferenceHPAMetrics(svc.Spec.Autoscaling)
	if !ok || metricErr != nil {
		msg := fmt.Sprintf("invalid autoscaling: need maxReplicas >= 1 and minReplicas <= maxReplicas (min %d, max %d); no HPA created",
			svc.Spec.Autoscaling.MinReplicas, svc.Spec.Autoscaling.MaxReplicas)
		reason := "InvalidRange"
		if metricErr != nil {
			reason, msg = "InvalidMetrics", metricErr.Error()
		}
		setCondition(&svc.Status.Conditions, svc.Generation, ConditionAutoscalingValid, metav1.ConditionFalse, reason, msg)
		setCondition(&svc.Status.Conditions, svc.Generation, ConditionAutoscalingReady, metav1.ConditionFalse, reason, msg)
		if exists {
			if err := r.Delete(ctx, existing); err != nil && !errors.IsNotFound(err) {
				return err
			}
		}
		return nil
	}
	msg := "HPA scales on CPU utilisation (80%)"
	if svc.Spec.Autoscaling.TargetGPUUtilization > 0 || svc.Spec.Autoscaling.TargetRequestsPerSecond > 0 {
		msg = "HPA uses per-pod custom GPU/RPS metrics; a custom.metrics.k8s.io adapter is required"
	}
	setCondition(&svc.Status.Conditions, svc.Generation, ConditionAutoscalingValid, metav1.ConditionTrue, "Valid", msg)
	reportHPAMetricStatus(svc, existing, exists)
	spec := autoscalingv2.HorizontalPodAutoscalerSpec{
		ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: inferPrimaryName(svc)},
		MinReplicas:    int32Ptr(sloFloor(svc, lo, hi)),
		MaxReplicas:    hi,
		Metrics:        metricSpecs,
		Behavior: &autoscalingv2.HorizontalPodAutoscalerBehavior{
			ScaleDown: &autoscalingv2.HPAScalingRules{StabilizationWindowSeconds: int32Ptr(300)},
		},
	}
	if !exists {
		hpa := &autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{
				Name:        name,
				Namespace:   svc.Namespace,
				Labels:      map[string]string{"gryvia.io/inference": svc.Name},
				Annotations: map[string]string{annotationSpecHash: specHash(spec)},
			},
			Spec: spec,
		}
		if err := controllerutil.SetControllerReference(svc, hpa, r.Scheme); err != nil {
			return err
		}
		if err := r.Create(ctx, hpa); err != nil && !errors.IsAlreadyExists(err) {
			return err
		}
		return nil
	}
	if existing.Annotations[annotationSpecHash] != specHash(spec) {
		base := existing.DeepCopy()
		existing.Spec = spec
		if existing.Annotations == nil {
			existing.Annotations = map[string]string{}
		}
		existing.Annotations[annotationSpecHash] = specHash(spec)
		setCondition(&svc.Status.Conditions, svc.Generation, ConditionAutoscalingReady, metav1.ConditionUnknown, "Reconciling", "Waiting for the HPA controller to evaluate updated scaling metrics")
		return r.Patch(ctx, existing, client.MergeFrom(base))
	}
	return nil
}

func removeCondition(conds *[]metav1.Condition, condType string) {
	out := (*conds)[:0]
	for _, c := range *conds {
		if c.Type != condType {
			out = append(out, c)
		}
	}
	*conds = out
}

// canaryReplicas gives the canary the share of pods that matches its weight: with p stable replicas and a canary
// weight w (percent), c = ceil(p*w/(100-w)) so that c/(p+c) is about w. At least one pod.
func canaryReplicas(stable, weight int32) int32 {
	if weight >= 100 {
		return stable
	}
	if weight < 1 {
		return 1
	}
	c := (stable*weight + (100 - weight) - 1) / (100 - weight)
	if c < 1 {
		c = 1
	}
	return c
}

// reconcileCanary runs the canary lifecycle: create, health-check, promote (auto), roll back (auto). It returns
// how soon the next check is due.
func (r *GryviaInferenceServiceReconciler) reconcileCanary(ctx context.Context, svc *gryviav1.GryviaInferenceService,
	primary *appsv1.Deployment, model *gryviav1.GryviaModelRegistry) (time.Duration, error) {
	now := clock(r.Clock)
	name := inferCanaryName(svc)

	existing := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: name}, existing)
	if err != nil && !errors.IsNotFound(err) {
		return 0, err
	}
	exists := err == nil
	if exists && !metav1.IsControlledBy(existing, svc) {
		svc.Status.CanaryStatus = &gryviav1.CanaryStatus{Active: false, Health: "Unknown"}
		setCondition(&svc.Status.Conditions, svc.Generation, ConditionCanaryActive, metav1.ConditionFalse, "NameCollision", "Canary Deployment is owned by another resource; it was left untouched")
		return 0, nil
	}
	deleteCanary := func() error {
		if !exists {
			return nil
		}
		if err := r.Delete(ctx, existing); err != nil && !errors.IsNotFound(err) {
			return err
		}
		exists = false
		return nil
	}

	c := svc.Spec.Canary
	if cfg, _ := analysisConfig(svc); cfg == nil || c == nil || !c.Enabled || primary.Annotations[annotationPromoted] == c.ModelVersion || primary.Annotations[annotationRolledBack] == c.ModelVersion {
		removeCondition(&svc.Status.Conditions, ConditionCanaryAnalysis)
	}
	if c == nil || !c.Enabled {
		if err := deleteCanary(); err != nil {
			return 0, err
		}
		svc.Status.CanaryStatus = nil
		svc.Status.ConsecutiveFailures = 0
		removeCondition(&svc.Status.Conditions, ConditionCanaryActive)
		return 0, nil
	}
	version := c.ModelVersion

	// A canary version that was already promoted or rolled back stays finished until the user picks a new version.
	if primary.Annotations[annotationPromoted] == version {
		if err := deleteCanary(); err != nil {
			return 0, err
		}
		svc.Status.CanaryStatus = &gryviav1.CanaryStatus{Active: false, Health: canaryHealthPromoted}
		setCondition(&svc.Status.Conditions, svc.Generation, ConditionCanaryActive, metav1.ConditionFalse, "Promoted", "Canary "+version+" was promoted")
		return 0, nil
	}
	if primary.Annotations[annotationRolledBack] == version {
		if err := deleteCanary(); err != nil {
			return 0, err
		}
		svc.Status.CanaryStatus = &gryviav1.CanaryStatus{Active: false, Health: canaryHealthRolledBack}
		setCondition(&svc.Status.Conditions, svc.Generation, ConditionCanaryActive, metav1.ConditionFalse, "RolledBack", "Canary "+version+" was rolled back")
		return 0, nil
	}

	replicas := canaryReplicas(r.primaryReplicas(svc), c.Weight)
	model = r.modelForVersion(ctx, svc, model, version)
	desired, err := r.buildDeployment(svc, model, name, trackCanary, version, replicas)
	if err != nil {
		return 0, err
	}
	if err := r.enrichTelemetry(svc, desired, existing.UID); err != nil {
		return 0, err
	}
	newCanary := !exists || existing.Annotations[annotationModelVersion] != version
	switch {
	case !exists:
		if err := controllerutil.SetControllerReference(svc, desired, r.Scheme); err != nil {
			return 0, err
		}
		if err := r.Create(ctx, desired); err != nil {
			return 0, err
		}
		if err := r.persistTelemetryUID(ctx, svc, desired, model); err != nil {
			return 0, err
		}
		existing = desired
	default:
		base := existing.DeepCopy()
		if existing.Annotations[annotationSpecHash] != desired.Annotations[annotationSpecHash] {
			existing.Spec.Template = desired.Spec.Template
			existing.Annotations[annotationSpecHash] = desired.Annotations[annotationSpecHash]
			existing.Annotations[annotationModelVersion] = version
		}
		existing.Spec.Replicas = desired.Spec.Replicas
		if err := r.Patch(ctx, existing, client.MergeFrom(base)); err != nil {
			return 0, err
		}
	}

	cs := svc.Status.CanaryStatus
	if newCanary || cs == nil || !cs.Active || cs.StartedAt == nil {
		t := metav1.NewTime(now)
		cs = &gryviav1.CanaryStatus{Active: true, StartedAt: &t}
		svc.Status.ConsecutiveFailures = 0
		svc.Status.LastHealthCheck = nil
	}
	cs.Weight = c.Weight
	cs.DeploymentName = name
	cs.ReadyReplicas = existing.Status.ReadyReplicas
	// An old Deployment status must not authorize traffic to a newly rolled version.
	if existing.Status.ObservedGeneration < existing.Generation || (existing.Generation > 0 && (existing.Status.UpdatedReplicas < *existing.Spec.Replicas || existing.Status.Replicas != *existing.Spec.Replicas)) {
		cs.ReadyReplicas = 0
	}
	svc.Status.CanaryStatus = cs
	setCondition(&svc.Status.Conditions, svc.Generation, ConditionCanaryActive, metav1.ConditionTrue, "CanaryDeployed",
		fmt.Sprintf("Canary %s at about %d%% of the pods", version, c.Weight))

	age := now.Sub(cs.StartedAt.Time)
	interval := time.Duration(defaultHealthIntervalSecs) * time.Second
	if svc.Spec.HealthCheck != nil && svc.Spec.HealthCheck.IntervalSeconds > 0 {
		interval = time.Duration(svc.Spec.HealthCheck.IntervalSeconds) * time.Second
	}

	cfg, _ := analysisConfig(svc) // validated before resource reconciliation
	analysisHealthy, analysisFailed := true, false
	if cfg != nil {
		analysisHealthy, analysisFailed = r.evaluateCanary(ctx, svc, existing, cfg, now)
	}
	switch {
	case cs.ReadyReplicas > 0 && !analysisHealthy && !analysisFailed:
		cs.Health = canaryHealthPending
		svc.Status.ConsecutiveFailures = 0
	case cs.ReadyReplicas > 0 && analysisHealthy:
		cs.Health = canaryHealthHealthy
		svc.Status.ConsecutiveFailures = 0
	case !analysisFailed && age <= r.CanaryStartupGrace:
		cs.Health = canaryHealthPending
	default:
		cs.Health = canaryHealthUnhealthy
		// One failure per health-check interval, however often the object is reconciled.
		if svc.Status.LastHealthCheck == nil || now.Sub(svc.Status.LastHealthCheck.Time) >= interval {
			svc.Status.ConsecutiveFailures++
			t := metav1.NewTime(now)
			svc.Status.LastHealthCheck = &t
		}
	}

	// Automatic rollback: enough consecutive failed checks remove the canary and pin its version as rejected.
	if svc.Spec.HealthCheck != nil && svc.Spec.HealthCheck.AutoRollback {
		threshold := svc.Spec.HealthCheck.FailureThreshold
		if threshold <= 0 {
			threshold = 3
		}
		if svc.Status.ConsecutiveFailures >= threshold {
			if err := r.annotatePrimary(ctx, primary, annotationRolledBack, version); err != nil {
				return 0, err
			}
			if err := deleteCanary(); err != nil {
				return 0, err
			}
			svc.Status.Message = fmt.Sprintf("Canary %s rolled back after %d failed health checks", version, svc.Status.ConsecutiveFailures)
			setCondition(&svc.Status.Conditions, svc.Generation, ConditionHealthy, metav1.ConditionFalse, "CanaryRolledBack", svc.Status.Message)
			setCondition(&svc.Status.Conditions, svc.Generation, ConditionCanaryActive, metav1.ConditionFalse, "RolledBack", svc.Status.Message)
			svc.Status.CanaryStatus = &gryviav1.CanaryStatus{Active: false, Health: canaryHealthRolledBack}
			svc.Status.ConsecutiveFailures = 0
			return time.Second, nil
		}
	}

	promotionReady := true
	if cfg != nil {
		promotionReady, err = r.analysisPromotionReady(ctx, svc, existing, cfg, analysisHealthy, now)
		if err != nil {
			return 0, err
		}
	}
	if routingRequested(svc) {
		routeReady, routeErr := r.routePromotionReady(ctx, svc, existing, now)
		promotionReady = promotionReady && routeReady
		err = routeErr
		if err != nil {
			return 0, err
		}
	}

	// Automatic promotion: the canary has been healthy for promoteAfterSeconds (0 = as soon as it is ready).
	if c.AutoPromote && cs.Health == canaryHealthHealthy && age >= time.Duration(c.PromoteAfterSeconds)*time.Second && promotionReady {
		if err := r.annotatePrimary(ctx, primary, annotationPromoted, version); err != nil {
			return 0, err
		}
		if err := deleteCanary(); err != nil {
			return 0, err
		}
		svc.Status.Message = "Canary promoted: " + version
		svc.Status.CanaryStatus = &gryviav1.CanaryStatus{Active: false, Health: canaryHealthPromoted}
		setCondition(&svc.Status.Conditions, svc.Generation, ConditionCanaryActive, metav1.ConditionFalse, "Promoted", svc.Status.Message)
		return time.Second, nil // the next pass rolls the promoted version out to the stable Deployment
	}

	if cs.Health == canaryHealthHealthy && c.AutoPromote && !routingRequested(svc) && cfg == nil {
		if left := time.Duration(c.PromoteAfterSeconds)*time.Second - age; left > 0 {
			return left + time.Second, nil
		}
	}
	return interval, nil
}

// annotatePrimary records a decision (promoted or rejected version) on the stable Deployment's metadata.
func (r *GryviaInferenceServiceReconciler) annotatePrimary(ctx context.Context, primary *appsv1.Deployment, key, value string) error {
	base := primary.DeepCopy()
	if primary.Annotations == nil {
		primary.Annotations = map[string]string{}
	}
	primary.Annotations[key] = value
	return r.Patch(ctx, primary, client.MergeFrom(base))
}

// syncStatus fills phase, readyReplicas and health from the stable Deployment.
func (r *GryviaInferenceServiceReconciler) syncStatus(svc *gryviav1.GryviaInferenceService, primary *appsv1.Deployment, atZero bool) {
	if atZero {
		svc.Status.ReadyReplicas = primary.Status.ReadyReplicas
		svc.Status.Phase = PhaseScaledToZero
		svc.Status.HealthStatus = "Unknown"
		setCondition(&svc.Status.Conditions, svc.Generation, ConditionInferenceReady, metav1.ConditionFalse, "ScaledToZero",
			"Scaled to zero for being idle")
		return
	}
	want := int32(1)
	if primary.Spec.Replicas != nil {
		want = *primary.Spec.Replicas
	}
	ready := primary.Status.ReadyReplicas
	svc.Status.ReadyReplicas = ready
	switch {
	case ready > 0 && ready >= want:
		svc.Status.Phase = PhaseReady
		svc.Status.HealthStatus = canaryHealthHealthy
		setCondition(&svc.Status.Conditions, svc.Generation, ConditionInferenceReady, metav1.ConditionTrue, "Ready", "All replicas are ready")
	case ready > 0:
		svc.Status.Phase = PhaseReady
		svc.Status.HealthStatus = canaryHealthHealthy
		setCondition(&svc.Status.Conditions, svc.Generation, ConditionInferenceReady, metav1.ConditionFalse, "PartiallyReady",
			fmt.Sprintf("%d/%d replicas ready", ready, want))
	default:
		svc.Status.Phase = PhaseDeployingInfer
		svc.Status.HealthStatus = "Unknown"
		setCondition(&svc.Status.Conditions, svc.Generation, ConditionInferenceReady, metav1.ConditionFalse, "Deploying", "No replica is ready yet")
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *GryviaInferenceServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gryviav1.GryviaInferenceService{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&autoscalingv2.HorizontalPodAutoscaler{}).
		Complete(r)
}
