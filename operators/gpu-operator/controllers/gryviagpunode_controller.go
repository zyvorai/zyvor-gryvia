// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"k8s.io/client-go/util/retry"

	gryviav1 "github.com/zyvorai/gryvia/operators/gpu-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/gpu-operator/pkg/discovery"
)

const (
	gryviaGpuNodeFinalizer = "gryvia.io/finalizer"

	// Status phases
	PhaseInitializing = "Initializing"
	PhaseReady        = "Ready"
	PhaseDegraded     = "Degraded"
	PhaseFailed       = "Failed"

	// Condition types
	ConditionDriversInstalled = "DriversInstalled"
	ConditionNodeLabeled      = "NodeLabeled"
	ConditionHealthy          = "Healthy"
)

const (
	dcgmPort    = "9400"
	dcgmTimeout = 2 * time.Second
)

// HTTPDoer is the subset of *http.Client used to scrape the DCGM exporter.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

var defaultHTTPClient HTTPDoer = &http.Client{Timeout: dcgmTimeout}

// GryviaGpuNodeReconciler reconciles a GryviaGpuNode object
type GryviaGpuNodeReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Log    logr.Logger

	// HTTPClient scrapes the DCGM exporter; nil uses a client with a 2s timeout.
	HTTPClient HTTPDoer
	// DCGMNamespace restricts the DCGM exporter pod search to one namespace
	// (all namespaces when empty).
	DCGMNamespace string
}

//+kubebuilder:rbac:groups=gryvia.io,resources=gryviagpunodes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviagpunodes/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviagpunodes/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop
func (r *GryviaGpuNodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("gryviagpunode", req.NamespacedName)

	// Fetch the GryviaGpuNode instance
	gryviaNode := &gryviav1.GryviaGpuNode{}
	err := r.Get(ctx, req.NamespacedName, gryviaNode)
	if err != nil {
		if errors.IsNotFound(err) {
			log.Info("GryviaGpuNode resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get GryviaGpuNode")
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !gryviaNode.ObjectMeta.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, gryviaNode)
	}

	// Add finalizer if it doesn't exist
	if !controllerutil.ContainsFinalizer(gryviaNode, gryviaGpuNodeFinalizer) {
		controllerutil.AddFinalizer(gryviaNode, gryviaGpuNodeFinalizer)
		if err := r.Update(ctx, gryviaNode); err != nil {
			return ctrl.Result{}, err
		}
		// Requeue to avoid working with a stale object after update
		return ctrl.Result{Requeue: true}, nil
	}

	// Initialize status if needed
	if gryviaNode.Status.Phase == "" {
		gryviaNode.Status.Phase = PhaseInitializing
		if err := r.Status().Update(ctx, gryviaNode); err != nil {
			log.Error(err, "Failed to update GryviaGpuNode status")
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Reconcile the GPU node
	result, err := r.reconcileGpuNode(ctx, gryviaNode)
	if err != nil {
		log.Error(err, "Failed to reconcile GPU node")
		return result, err
	}

	// Use the returned result's RequeueAfter if non-zero, otherwise use default interval
	if result.RequeueAfter > 0 {
		return result, nil
	}

	// Schedule next reconciliation based on health check interval
	interval := 60 * time.Second
	if gryviaNode.Spec.HealthCheck != nil && gryviaNode.Spec.HealthCheck.Enabled {
		if gryviaNode.Spec.HealthCheck.IntervalSeconds > 0 {
			interval = time.Duration(gryviaNode.Spec.HealthCheck.IntervalSeconds) * time.Second
		}
	}

	return ctrl.Result{RequeueAfter: interval}, nil
}

func (r *GryviaGpuNodeReconciler) reconcileGpuNode(ctx context.Context, gryviaNode *gryviav1.GryviaGpuNode) (ctrl.Result, error) {
	log := r.Log.WithValues("gryviagpunode", gryviaNode.Name)

	// Get the Kubernetes node this object describes (not the node the operator runs on).
	node := &corev1.Node{}
	err := r.Get(ctx, types.NamespacedName{Name: gryviaNode.Spec.NodeName}, node)
	if err != nil {
		log.Error(err, "Failed to get Kubernetes node")
		r.updateCondition(gryviaNode, ConditionNodeLabeled, metav1.ConditionFalse, "NodeNotFound", err.Error())
		gryviaNode.Status.Phase = PhaseFailed
		if updateErr := r.Status().Update(ctx, gryviaNode); updateErr != nil {
			log.Error(updateErr, "Failed to update status after node not found")
		}
		return ctrl.Result{}, err
	}

	// Driver and device plugin state comes from the NVIDIA GPU Operator's node
	// labels (GPU Feature Discovery) and the node's allocatable resources.
	alloc := map[string]string{}
	if q, ok := node.Status.Allocatable[corev1.ResourceName(discovery.ResourceGPU)]; ok {
		alloc[discovery.ResourceGPU] = q.String()
	}
	found, hasGPU := discovery.FromNode(node.Labels, alloc)
	if hasGPU {
		gryviaNode.Status.DriverVersion = found.DriverVersion
		gryviaNode.Status.CudaVersion = found.CUDAVersion
	} else {
		gryviaNode.Status.DriverVersion = ""
		gryviaNode.Status.CudaVersion = ""
	}

	phase, message := discovery.Readiness(gryviaNode.Spec.GpuCount, found.Allocatable, gryviaNode.Status.DriverVersion)
	if found.Allocatable >= 1 {
		r.updateCondition(gryviaNode, ConditionDriversInstalled, metav1.ConditionTrue, "DevicePluginReady",
			fmt.Sprintf("NVIDIA driver and device plugin are ready: %d GPU(s) allocatable", found.Allocatable))
	} else {
		r.updateCondition(gryviaNode, ConditionDriversInstalled, metav1.ConditionFalse, "WaitingForDrivers", message)
	}

	// Best-effort per-GPU status from the DCGM exporter on this node.
	gryviaNode.Status.GpuStatus = r.scrapeDCGM(ctx, gryviaNode.Spec.NodeName)
	worst := ""
	for _, g := range gryviaNode.Status.GpuStatus {
		if g.Health == discovery.HealthFailed {
			worst = discovery.HealthFailed
		} else if g.Health == discovery.HealthDegraded && worst == "" {
			worst = discovery.HealthDegraded
		}
	}
	switch {
	case worst != "":
		r.updateCondition(gryviaNode, ConditionHealthy, metav1.ConditionFalse, "GPUUnhealthy", "at least one GPU is "+strings.ToLower(worst)+" (temperature)")
	case len(gryviaNode.Status.GpuStatus) > 0:
		r.updateCondition(gryviaNode, ConditionHealthy, metav1.ConditionTrue, "Healthy", "All GPUs are healthy")
	default:
		r.updateCondition(gryviaNode, ConditionHealthy, metav1.ConditionUnknown, "NoMetrics", "no DCGM exporter metrics available for this node")
	}

	// Label the node
	labelFailed := false
	if err := r.labelNode(ctx, gryviaNode, node); err != nil {
		log.Error(err, "Failed to label node")
		labelFailed = true
		r.updateCondition(gryviaNode, ConditionNodeLabeled, metav1.ConditionFalse, "LabelFailed", err.Error())
	} else {
		r.updateCondition(gryviaNode, ConditionNodeLabeled, metav1.ConditionTrue, "Labeled", "Node labeled successfully")
	}

	if phase == discovery.PhaseReady {
		switch {
		case worst == discovery.HealthFailed:
			phase = PhaseFailed
		case worst != "" || labelFailed:
			phase = PhaseDegraded
		}
	}
	gryviaNode.Status.Phase = phase
	log.V(1).Info("GPU node reconciled", "phase", phase, "message", message)

	now := metav1.Now()
	gryviaNode.Status.LastHealthCheck = &now
	if err := r.Status().Update(ctx, gryviaNode); err != nil {
		log.Error(err, "Failed to update status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// scrapeDCGM returns per-GPU status from the DCGM exporter pod running on the
// node. It is best effort: any problem yields nil.
func (r *GryviaGpuNodeReconciler) scrapeDCGM(ctx context.Context, nodeName string) []gryviav1.GpuStatus {
	log := r.Log.WithValues("node", nodeName)
	pods := &corev1.PodList{}
	var opts []client.ListOption
	if r.DCGMNamespace != "" {
		opts = append(opts, client.InNamespace(r.DCGMNamespace))
	}
	if err := r.List(ctx, pods, opts...); err != nil {
		log.V(1).Info("Cannot list pods to find the DCGM exporter", "error", err.Error())
		return nil
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Spec.NodeName != nodeName || p.Status.PodIP == "" || p.DeletionTimestamp != nil {
			continue
		}
		if p.Labels["app.kubernetes.io/component"] != "dcgm-exporter" && !strings.Contains(p.Name, "dcgm-exporter") {
			continue
		}
		status, err := r.fetchDCGM(ctx, p.Status.PodIP)
		if err != nil {
			log.V(1).Info("DCGM exporter scrape failed", "pod", p.Name, "error", err.Error())
			continue
		}
		if len(status) > 0 {
			return status
		}
	}
	return nil
}

func (r *GryviaGpuNodeReconciler) fetchDCGM(ctx context.Context, ip string) ([]gryviav1.GpuStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, dcgmTimeout)
	defer cancel()
	url := "http://" + net.JoinHostPort(ip, dcgmPort) + "/metrics"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	doer := r.HTTPClient
	if doer == nil {
		doer = defaultHTTPClient
	}
	resp, err := doer.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	metrics := discovery.ParseDCGM(string(body))
	out := make([]gryviav1.GpuStatus, 0, len(metrics))
	for _, m := range metrics {
		out = append(out, gryviav1.GpuStatus{
			Index:       m.Index,
			UUID:        m.UUID,
			Health:      discovery.Health(m.Temperature),
			Temperature: m.Temperature,
			PowerUsage:  m.PowerUsage,
			MemoryUsed:  m.MemoryUsed,
			MemoryTotal: m.MemoryTotal(),
			Utilization: m.Utilization,
		})
	}
	return out, nil
}

func (r *GryviaGpuNodeReconciler) labelNode(ctx context.Context, gryviaNode *gryviav1.GryviaGpuNode, node *corev1.Node) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Re-fetch node to get latest version
		if err := r.Get(ctx, types.NamespacedName{Name: node.Name}, node); err != nil {
			return err
		}

		before := make(map[string]string, len(node.Labels))
		for k, v := range node.Labels {
			before[k] = v
		}

		// Apply labels to the Kubernetes node
		if node.Labels == nil {
			node.Labels = make(map[string]string)
		}

		// Standard Gryvia labels
		node.Labels["gryvia.io/gpu"] = gryviaNode.Spec.GpuType
		node.Labels["gryvia.io/gpu-count"] = fmt.Sprintf("%d", gryviaNode.Spec.GpuCount)
		node.Labels["gryvia.io/rdma"] = fmt.Sprintf("%t", gryviaNode.Spec.RDMA)
		node.Labels["gryvia.io/sriov"] = fmt.Sprintf("%t", gryviaNode.Spec.SRIOV)

		if gryviaNode.Spec.Interconnect != "" {
			node.Labels["gryvia.io/interconnect"] = gryviaNode.Spec.Interconnect
		}

		// Apply custom labels from spec (only allow gryvia.io/ prefix)
		for k, v := range gryviaNode.Spec.Labels {
			if strings.HasPrefix(k, "gryvia.io/") {
				node.Labels[k] = v
			}
		}

		// Update the node only when something changed
		if reflect.DeepEqual(before, node.Labels) {
			return nil
		}
		return r.Update(ctx, node)
	})
}

func (r *GryviaGpuNodeReconciler) handleDeletion(ctx context.Context, gryviaNode *gryviav1.GryviaGpuNode) (ctrl.Result, error) {
	if controllerutil.ContainsFinalizer(gryviaNode, gryviaGpuNodeFinalizer) {
		// Cleanup: remove labels from node
		node := &corev1.Node{}
		err := r.Get(ctx, types.NamespacedName{Name: gryviaNode.Spec.NodeName}, node)
		if err == nil {
			if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				if err := r.Get(ctx, types.NamespacedName{Name: gryviaNode.Spec.NodeName}, node); err != nil {
					return err
				}
				// Remove Gryvia labels
				if node.Labels != nil {
					delete(node.Labels, "gryvia.io/gpu")
					delete(node.Labels, "gryvia.io/gpu-count")
					delete(node.Labels, "gryvia.io/rdma")
					delete(node.Labels, "gryvia.io/sriov")
					delete(node.Labels, "gryvia.io/interconnect")

					// Remove custom labels with gryvia.io/ prefix from spec
					for k := range gryviaNode.Spec.Labels {
						if strings.HasPrefix(k, "gryvia.io/") {
							delete(node.Labels, k)
						}
					}
				}

				return r.Update(ctx, node)
			}); err != nil {
				r.Log.Error(err, "Failed to remove labels from node during cleanup", "node", gryviaNode.Spec.NodeName)
				return ctrl.Result{}, err
			}
		} else if !errors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("failed to get node %s during cleanup: %w", gryviaNode.Spec.NodeName, err)
		}

		// Remove finalizer
		controllerutil.RemoveFinalizer(gryviaNode, gryviaGpuNodeFinalizer)
		if err := r.Update(ctx, gryviaNode); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

func (r *GryviaGpuNodeReconciler) updateCondition(gryviaNode *gryviav1.GryviaGpuNode, condType string, status metav1.ConditionStatus, reason, message string) {
	condition := metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	}

	// Only update LastTransitionTime when status actually changes
	for _, cond := range gryviaNode.Status.Conditions {
		if cond.Type == condType && cond.Status == status {
			condition.LastTransitionTime = cond.LastTransitionTime
			break
		}
	}

	// Find and update existing condition or append new one
	found := false
	for i, cond := range gryviaNode.Status.Conditions {
		if cond.Type == condType {
			gryviaNode.Status.Conditions[i] = condition
			found = true
			break
		}
	}
	if !found {
		gryviaNode.Status.Conditions = append(gryviaNode.Status.Conditions, condition)
	}
}

// calculateBackoff returns an exponential backoff duration based on the number
// of consecutive health check failures (indicated by the Healthy condition being False).
func (r *GryviaGpuNodeReconciler) calculateBackoff(gryviaNode *gryviav1.GryviaGpuNode) time.Duration {
	const (
		minBackoff = 30 * time.Second
		maxBackoff = 10 * time.Minute
	)
	backoff := minBackoff
	for _, cond := range gryviaNode.Status.Conditions {
		if cond.Type == ConditionHealthy && cond.Status == metav1.ConditionFalse {
			elapsed := time.Since(cond.LastTransitionTime.Time)
			// Double backoff for each minute the condition has been false
			multiplier := int(elapsed.Minutes()) + 1
			backoff = time.Duration(multiplier) * minBackoff
			break
		}
	}
	if backoff > maxBackoff {
		backoff = maxBackoff
	}
	return backoff
}

func (r *GryviaGpuNodeReconciler) allConditionsTrue(gryviaNode *gryviav1.GryviaGpuNode) bool {
	requiredConditions := []string{ConditionDriversInstalled, ConditionNodeLabeled, ConditionHealthy}
	for _, reqCond := range requiredConditions {
		found := false
		for _, cond := range gryviaNode.Status.Conditions {
			if cond.Type == reqCond && cond.Status == metav1.ConditionTrue {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (r *GryviaGpuNodeReconciler) anyConditionFalse(gryviaNode *gryviav1.GryviaGpuNode) bool {
	for _, cond := range gryviaNode.Status.Conditions {
		if cond.Status == metav1.ConditionFalse {
			return true
		}
	}
	return false
}

// SetupWithManager sets up the controller with the Manager.
func (r *GryviaGpuNodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gryviav1.GryviaGpuNode{}).
		Complete(r)
}
