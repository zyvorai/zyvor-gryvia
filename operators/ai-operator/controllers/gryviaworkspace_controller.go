package controllers

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
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
	// Workspace condition types
	ConditionWorkspaceReady = "WorkspaceReady"
	ConditionWorkspaceIdle  = "WorkspaceIdle"

	// Default images, overridable per operator (flags) and per object (spec.image).
	DefaultJupyterImage = "quay.io/jupyter/pytorch-notebook:cuda12-latest"
	DefaultVSCodeImage  = "codercom/code-server:latest"

	defaultWorkspaceStorage = "50Gi"
)

// GryviaWorkspaceReconciler reconciles a GryviaWorkspace object.
//
// A workspace is a PVC (when spec.storage is set), one bare Pod and a ClusterIP Service, all named
// "<workspace>-workspace" and owned by the GryviaWorkspace. Pausing deletes the Pod and keeps the PVC and Service;
// resuming creates a fresh Pod that mounts the same PVC. Deleting the GryviaWorkspace garbage-collects all three
// (the PVC and its data with them).
type GryviaWorkspaceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Log    logr.Logger

	// JupyterImage and CodeImage are the images used when spec.image is empty (defaults above when empty).
	JupyterImage string
	CodeImage    string

	// Clock returns the current time (time.Now when nil); tests move it.
	Clock func() time.Time

	// Proxy, when set, serves workspaces through the api-gateway (see WorkspaceProxy).
	Proxy *WorkspaceProxy
	// HTTPClient polls workspace servers for activity (http.DefaultClient when nil).
	HTTPClient *http.Client
	// InternalURL overrides the in-cluster address used for activity polls; tests point it at a local server.
	InternalURL func(*gryviav1.GryviaWorkspace) string
	// ActivityProbe replaces the activity poll entirely; tests use it.
	ActivityProbe func(context.Context, *gryviav1.GryviaWorkspace) (time.Time, bool)
}

//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaworkspaces,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaworkspaces/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaworkspaces/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete

func workspaceChildName(ws *gryviav1.GryviaWorkspace) string { return childName(ws.Name, "workspace") }

func workspacePort(ws *gryviav1.GryviaWorkspace) int32 {
	if ws.Spec.Type == gryviav1.WorkspaceTypeVSCode {
		return 8080
	}
	return 8888
}

// Reconcile drives one GryviaWorkspace towards its spec.
func (r *GryviaWorkspaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("gryviaworkspace", req.NamespacedName)

	ws := &gryviav1.GryviaWorkspace{}
	if err := r.Get(ctx, req.NamespacedName, ws); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	// Children carry an owner reference, so the garbage collector removes them; nothing to clean up here.
	if !ws.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	orig := ws.DeepCopy()
	var res ctrl.Result
	var err error
	if ws.Spec.Paused {
		res, err = r.reconcilePaused(ctx, ws)
	} else {
		res, err = r.reconcileRunning(ctx, ws)
	}
	if perr := patchStatus(ctx, r.Client, ws, orig); perr != nil {
		log.Error(perr, "Failed to patch workspace status")
		if err == nil {
			err = perr
		}
	}
	return res, err
}

func (r *GryviaWorkspaceReconciler) fail(ws *gryviav1.GryviaWorkspace, reason string, err error) {
	ws.Status.Phase = string(gryviav1.WorkspacePhaseFailed)
	ws.Status.Message = fmt.Sprintf("%s: %v", reason, err)
	setCondition(&ws.Status.Conditions, ws.Generation, ConditionWorkspaceReady, metav1.ConditionFalse, "Failed", ws.Status.Message)
}

func (r *GryviaWorkspaceReconciler) reconcileRunning(ctx context.Context, ws *gryviav1.GryviaWorkspace) (ctrl.Result, error) {
	name := workspaceChildName(ws)

	if ws.Spec.Storage != "" {
		if err := r.ensurePVC(ctx, ws, name); err != nil {
			if isConfigError(err) {
				r.fail(ws, "Invalid workspace storage", err)
				return ctrl.Result{}, nil
			}
			return ctrl.Result{RequeueAfter: 15 * time.Second}, err
		}
	}

	pod, err := r.ensurePod(ctx, ws, name)
	if err != nil {
		if isConfigError(err) {
			r.fail(ws, "Invalid workspace", err)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{RequeueAfter: 15 * time.Second}, err
	}

	if err := r.ensureService(ctx, ws, name); err != nil {
		if isConfigError(err) {
			r.fail(ws, "Cannot create the workspace service", err)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{RequeueAfter: 15 * time.Second}, err
	}

	if err := r.ensureNetworkPolicy(ctx, ws, name); err != nil {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, err
	}

	return r.syncPodStatus(ctx, ws, pod)
}

// configError marks a problem the user has to fix (as opposed to a transient API error worth retrying).
type configError struct{ error }

func isConfigError(err error) bool {
	_, ok := err.(configError)
	return ok
}

// ensurePVC creates the workspace PVC once. An existing PVC is reused (this is how a resumed workspace keeps its
// data) but only when this workspace owns it.
func (r *GryviaWorkspaceReconciler) ensurePVC(ctx context.Context, ws *gryviav1.GryviaWorkspace, name string) error {
	pvc := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, types.NamespacedName{Namespace: ws.Namespace, Name: name}, pvc)
	if err == nil {
		if !metav1.IsControlledBy(pvc, ws) {
			return configError{fmt.Errorf("PVC %q already exists and is not owned by this workspace", name)}
		}
		ws.Status.PVCName = name
		return nil
	}
	if !errors.IsNotFound(err) {
		return err
	}

	size, perr := resource.ParseQuantity(ws.Spec.Storage)
	if perr != nil {
		return configError{fmt.Errorf("invalid storage size %q: %w", ws.Spec.Storage, perr)}
	}
	pvc = &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ws.Namespace,
			Labels:    map[string]string{"gryvia.io/workspace": ws.Name, "gryvia.io/component": "workspace-data"},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: size},
			},
		},
	}
	if ws.Spec.StorageClassName != "" {
		sc := ws.Spec.StorageClassName
		pvc.Spec.StorageClassName = &sc
	}
	if err := controllerutil.SetControllerReference(ws, pvc, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, pvc); err != nil && !errors.IsAlreadyExists(err) {
		return err
	}
	ws.Status.PVCName = name
	return nil
}

// ensurePod returns the workspace Pod, creating it when missing. A Pod that is being deleted is returned as is so
// the caller waits for it to disappear before a new one (same name) is created.
func (r *GryviaWorkspaceReconciler) ensurePod(ctx context.Context, ws *gryviav1.GryviaWorkspace, name string) (*corev1.Pod, error) {
	pod := &corev1.Pod{}
	err := r.Get(ctx, types.NamespacedName{Namespace: ws.Namespace, Name: name}, pod)
	if err == nil {
		if !metav1.IsControlledBy(pod, ws) {
			return nil, configError{fmt.Errorf("pod %q already exists and is not owned by this workspace", name)}
		}
		ws.Status.PodName = name
		return pod, nil
	}
	if !errors.IsNotFound(err) {
		return nil, err
	}

	pod, err = r.buildPod(ws, name)
	if err != nil {
		return nil, configError{err}
	}
	if err := controllerutil.SetControllerReference(ws, pod, r.Scheme); err != nil {
		return nil, err
	}
	if err := r.Create(ctx, pod); err != nil {
		return nil, err
	}
	ws.Status.PodName = name
	return pod, nil
}

func (r *GryviaWorkspaceReconciler) buildPod(ws *gryviav1.GryviaWorkspace, name string) (*corev1.Pod, error) {
	port := workspacePort(ws)
	container := corev1.Container{
		Name:  "workspace",
		Image: r.image(ws),
		Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: port, Protocol: corev1.ProtocolTCP}},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/", Port: intstr.FromInt32(port)}},
			InitialDelaySeconds: 5,
			PeriodSeconds:       10,
		},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: boolPtr(false),
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
	}

	res := corev1.ResourceRequirements{Requests: corev1.ResourceList{}, Limits: corev1.ResourceList{}}
	for _, q := range []struct {
		field string
		val   string
		dst   corev1.ResourceList
		name  corev1.ResourceName
	}{
		{"cpuRequest", ws.Spec.CPURequest, res.Requests, corev1.ResourceCPU},
		{"memRequest", ws.Spec.MemRequest, res.Requests, corev1.ResourceMemory},
		{"cpuLimit", ws.Spec.CPULimit, res.Limits, corev1.ResourceCPU},
		{"memLimit", ws.Spec.MemLimit, res.Limits, corev1.ResourceMemory},
	} {
		if q.val == "" {
			continue
		}
		v, err := resource.ParseQuantity(q.val)
		if err != nil {
			return nil, fmt.Errorf("invalid %s %q: %w", q.field, q.val, err)
		}
		q.dst[q.name] = v
	}
	// gpuCount 0 (or unset) means a CPU workspace: no GPU limit and no GPU node selector.
	if ws.Spec.GPUCount > 0 {
		res.Limits["nvidia.com/gpu"] = *resource.NewQuantity(int64(ws.Spec.GPUCount), resource.DecimalSI)
	}
	container.Resources = res

	keys := make([]string, 0, len(ws.Spec.Env))
	for k := range ws.Spec.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		container.Env = append(container.Env, corev1.EnvVar{Name: k, Value: ws.Spec.Env[k]})
	}
	r.applyProxy(ws, &container)

	var volumes []corev1.Volume
	if ws.Spec.Storage != "" {
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "workspace-data", MountPath: "/workspace"})
		volumes = append(volumes, corev1.Volume{Name: "workspace-data", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: name},
		}})
	}
	if ws.Spec.GPUCount > 0 {
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "shm", MountPath: "/dev/shm"})
		volumes = append(volumes, corev1.Volume{Name: "shm", VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory},
		}})
	}

	var nodeSelector map[string]string
	if ws.Spec.GPUType != "" && ws.Spec.GPUType != "any" {
		nodeSelector = map[string]string{"gryvia.io/gpu": ws.Spec.GPUType}
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ws.Namespace,
			Labels: map[string]string{
				"gryvia.io/workspace": ws.Name,
				"gryvia.io/component": "workspace-pod",
				"gryvia.io/type":      string(ws.Spec.Type),
			},
		},
		Spec: corev1.PodSpec{
			Containers:                   []corev1.Container{container},
			Volumes:                      volumes,
			NodeSelector:                 nodeSelector,
			RestartPolicy:                corev1.RestartPolicyAlways,
			AutomountServiceAccountToken: boolPtr(false),
		},
	}, nil
}

// ensureService creates the ClusterIP Service once and records the URL (the gateway path when proxied).
func (r *GryviaWorkspaceReconciler) ensureService(ctx context.Context, ws *gryviav1.GryviaWorkspace, name string) error {
	port := workspacePort(ws)
	svc := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Namespace: ws.Namespace, Name: name}, svc)
	if errors.IsNotFound(err) {
		svc = &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: ws.Namespace,
				Labels:    map[string]string{"gryvia.io/workspace": ws.Name, "gryvia.io/component": "workspace-service"},
			},
			Spec: corev1.ServiceSpec{
				Selector: map[string]string{"gryvia.io/workspace": ws.Name, "gryvia.io/component": "workspace-pod"},
				Ports: []corev1.ServicePort{{
					Name: "http", Port: port, TargetPort: intstr.FromString("http"), Protocol: corev1.ProtocolTCP,
				}},
				Type: corev1.ServiceTypeClusterIP,
			},
		}
		if err := controllerutil.SetControllerReference(ws, svc, r.Scheme); err != nil {
			return err
		}
		if err := r.Create(ctx, svc); err != nil {
			if errors.IsInvalid(err) {
				return configError{err}
			}
			if !errors.IsAlreadyExists(err) {
				return err
			}
		}
	} else if err != nil {
		return err
	} else if !metav1.IsControlledBy(svc, ws) {
		return configError{fmt.Errorf("service %q already exists and is not owned by this workspace", name)}
	}
	ws.Status.ServiceName = name
	ws.Status.URL = r.browserURL(ws)
	return nil
}

// syncPodStatus turns the Pod state into the phase the gateway and dashboard show, applies the idle timeout
// and the maximum lifetime.
func (r *GryviaWorkspaceReconciler) syncPodStatus(ctx context.Context, ws *gryviav1.GryviaWorkspace, pod *corev1.Pod) (ctrl.Result, error) {
	now := clock(r.Clock)

	// Re-read: ensurePod returns the Pod it just created without a status.
	live := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ws.Namespace, Name: pod.Name}, live); err != nil {
		if errors.IsNotFound(err) {
			ws.Status.Phase = string(gryviav1.WorkspacePhasePending)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}
	pod = live

	if !pod.DeletionTimestamp.IsZero() {
		ws.Status.Phase = string(gryviav1.WorkspacePhasePending)
		ws.Status.Message = "Waiting for the previous workspace pod to terminate"
		return ctrl.Result{RequeueAfter: 3 * time.Second}, nil
	}

	switch pod.Status.Phase {
	case corev1.PodFailed, corev1.PodSucceeded:
		// RestartPolicy Always never ends a healthy pod: this is an eviction or a node problem. Replace it.
		reason := pod.Status.Reason
		if reason == "" {
			reason = string(pod.Status.Phase)
		}
		if err := r.Delete(ctx, pod); err != nil && !errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		ws.Status.Phase = string(gryviav1.WorkspacePhasePending)
		ws.Status.Message = fmt.Sprintf("Workspace pod ended (%s); recreating", reason)
		setCondition(&ws.Status.Conditions, ws.Generation, ConditionWorkspaceReady, metav1.ConditionFalse, "PodFailed", ws.Status.Message)
		return ctrl.Result{RequeueAfter: 3 * time.Second}, nil
	case corev1.PodRunning:
		if !podReady(pod) {
			ws.Status.Phase = string(gryviav1.WorkspacePhasePending)
			ws.Status.Message = "Waiting for the workspace to become ready"
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	default:
		ws.Status.Phase = string(gryviav1.WorkspacePhasePending)
		ws.Status.Message = ""
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// The pod is running and ready.
	if ws.Status.StartTime == nil {
		t := metav1.NewTime(now)
		ws.Status.StartTime = &t
	}
	ws.Status.Phase = string(gryviav1.WorkspacePhaseRunning)
	ws.Status.Message = ""
	setCondition(&ws.Status.Conditions, ws.Generation, ConditionWorkspaceReady, metav1.ConditionTrue, "Running", "Workspace pod is running")

	// Activity: the newest of the session start, the RFC3339 time in the gryvia.io/last-activity annotation
	// (a user or a script can touch it), and what the server reports (Jupyter last_activity, code-server
	// lastHeartbeat). A server time in the future is capped at now.
	last := ws.Status.StartTime.Time
	if v, ok := ws.Annotations[annotationLastActivity]; ok {
		if t, err := time.Parse(time.RFC3339, v); err == nil && t.After(last) {
			last = t
		}
	}
	if t, ok := r.observeActivity(ctx, ws); ok && t.After(last) {
		if t.After(now) {
			t = now
		}
		last = t
	}
	lt := metav1.NewTime(last)
	ws.Status.LastActivity = &lt

	next := 60 * time.Second
	if ws.Spec.IdleTimeoutMinutes > 0 {
		timeout := time.Duration(ws.Spec.IdleTimeoutMinutes) * time.Minute
		if idle := now.Sub(last); idle > timeout {
			ws.Status.Phase = string(gryviav1.WorkspacePhaseIdle)
			ws.Status.Message = fmt.Sprintf("No activity for %s (idle timeout %s)", idle.Truncate(time.Minute), timeout)
			setCondition(&ws.Status.Conditions, ws.Generation, ConditionWorkspaceIdle, metav1.ConditionTrue, "Idle", ws.Status.Message)
			if ws.Annotations[annotationIdleAction] == "pause" {
				return r.autoPause(ctx, ws, "Paused after being idle")
			}
		} else {
			setCondition(&ws.Status.Conditions, ws.Generation, ConditionWorkspaceIdle, metav1.ConditionFalse, "Active", "Workspace is active")
			next = minDuration(next, timeout-idle+time.Second)
		}
	}

	if ws.Spec.MaxLifetimeHours > 0 {
		maxLife := time.Duration(ws.Spec.MaxLifetimeHours) * time.Hour
		life := now.Sub(ws.Status.StartTime.Time)
		if life > maxLife {
			return r.autoPause(ctx, ws, fmt.Sprintf("Paused: exceeded the maximum lifetime of %d hours", ws.Spec.MaxLifetimeHours))
		}
		next = minDuration(next, maxLife-life+time.Second)
	}
	return ctrl.Result{RequeueAfter: next}, nil
}

// autoPause pauses the workspace by setting spec.paused, exactly what the pause API does: the next reconcile
// deletes the Pod and keeps the PVC, and the user resumes it the usual way.
func (r *GryviaWorkspaceReconciler) autoPause(ctx context.Context, ws *gryviav1.GryviaWorkspace, why string) (ctrl.Result, error) {
	// Patch a copy: the response would overwrite the status changes made on ws so far.
	base := ws.DeepCopy()
	tmp := ws.DeepCopy()
	tmp.Spec.Paused = true
	if err := r.Patch(ctx, tmp, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}
	ws.Status.Message = why
	return ctrl.Result{RequeueAfter: time.Second}, nil
}

// reconcilePaused deletes the Pod (the PVC and the Service stay) and reports Paused. It is idempotent.
func (r *GryviaWorkspaceReconciler) reconcilePaused(ctx context.Context, ws *gryviav1.GryviaWorkspace) (ctrl.Result, error) {
	name := workspaceChildName(ws)
	pod := &corev1.Pod{}
	err := r.Get(ctx, types.NamespacedName{Namespace: ws.Namespace, Name: name}, pod)
	switch {
	case err == nil:
		if pod.DeletionTimestamp.IsZero() && metav1.IsControlledBy(pod, ws) {
			if derr := r.Delete(ctx, pod); derr != nil && !errors.IsNotFound(derr) {
				return ctrl.Result{RequeueAfter: 15 * time.Second}, derr
			}
		}
	case !errors.IsNotFound(err):
		return ctrl.Result{}, err
	}

	// Keep the reason recorded by an automatic pause ("Paused ..."); otherwise use the generic text.
	msg := ws.Status.Message
	if !strings.HasPrefix(msg, "Paused") {
		msg = "Workspace paused; storage is kept for resume"
	}
	ws.Status.Phase = string(gryviav1.WorkspacePhasePaused)
	ws.Status.Message = msg
	ws.Status.PodName = ""
	ws.Status.StartTime = nil
	setCondition(&ws.Status.Conditions, ws.Generation, ConditionWorkspaceReady, metav1.ConditionFalse, "Paused", "Workspace paused")
	setCondition(&ws.Status.Conditions, ws.Generation, ConditionWorkspaceIdle, metav1.ConditionFalse, "Paused", "Workspace paused")
	return ctrl.Result{}, nil
}

// image returns spec.image, else the operator default for the workspace type.
func (r *GryviaWorkspaceReconciler) image(ws *gryviav1.GryviaWorkspace) string {
	if ws.Spec.Image != "" {
		return ws.Spec.Image
	}
	if ws.Spec.Type == gryviav1.WorkspaceTypeVSCode {
		if r.CodeImage != "" {
			return r.CodeImage
		}
		return DefaultVSCodeImage
	}
	if r.JupyterImage != "" {
		return r.JupyterImage
	}
	return DefaultJupyterImage
}

func podReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func boolPtr(b bool) *bool { return &b }

func minDuration(a, b time.Duration) time.Duration {
	if b < a {
		return b
	}
	return a
}

// SetupWithManager sets up the controller with the Manager.
func (r *GryviaWorkspaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gryviav1.GryviaWorkspace{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Complete(r)
}
