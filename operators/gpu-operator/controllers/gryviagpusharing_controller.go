// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gryviav1 "github.com/zyvorai/gryvia/operators/gpu-operator/api/v1"
)

const (
	defaultSharingReconcileInterval = 30 * time.Second
)

// GryviaGPUSharingPolicyReconciler reconciles a GryviaGPUSharingPolicy object
type GryviaGPUSharingPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Log    logr.Logger

	// DevicePluginConfigName / DevicePluginConfigNamespace name the ConfigMap the NVIDIA device
	// plugin reads (the GPU Operator's devicePlugin.config.name). When both are set, a time-slicing
	// policy writes the data key "gryvia-ts-<replicas>" there and labels nodes with that key, so a
	// policy's maxPodsPerGPU is what the plugin is told. When empty the node label is the fixed
	// "gryvia-time-slicing" key and the replica count comes from whatever the chart put there.
	DevicePluginConfigName      string
	DevicePluginConfigNamespace string
}

//+kubebuilder:rbac:groups=gryvia.io,resources=gryviagpusharingpolicies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviagpusharingpolicies/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviagpusharingpolicies/finalizers,verbs=update
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviagpunodes,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch
//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete

func (r *GryviaGPUSharingPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("gryviagpusharingpolicy", req.NamespacedName)

	// Fetch the GryviaGPUSharingPolicy instance
	policy := &gryviav1.GryviaGPUSharingPolicy{}
	err := r.Get(ctx, req.NamespacedName, policy)
	if err != nil {
		if errors.IsNotFound(err) {
			log.Info("GryviaGPUSharingPolicy resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get GryviaGPUSharingPolicy")
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !policy.ObjectMeta.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	log.Info("Reconciling GryviaGPUSharingPolicy", "strategy", policy.Spec.Strategy)

	// Reconcile the GPU sharing policy
	result, err := r.reconcileGPUSharing(ctx, policy)
	if err != nil {
		log.Error(err, "Failed to reconcile GPU sharing policy")
		return result, err
	}

	return result, nil
}

func (r *GryviaGPUSharingPolicyReconciler) reconcileGPUSharing(ctx context.Context, policy *gryviav1.GryviaGPUSharingPolicy) (ctrl.Result, error) {
	log := r.Log.WithValues("policy", policy.Name)

	// Find matching GPU nodes
	matchingNodes, err := r.findMatchingNodes(ctx, policy)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to find matching nodes: %w", err)
	}

	// Update affected nodes in status
	var affectedNodeNames []string
	for _, node := range matchingNodes {
		affectedNodeNames = append(affectedNodeNames, node.Spec.NodeName)
	}
	policy.Status.AffectedNodes = affectedNodeNames

	// Calculate total physical GPUs
	totalGPUs := 0
	for _, node := range matchingNodes {
		totalGPUs += node.Spec.GpuCount
	}
	policy.Status.TotalGPUs = totalGPUs

	// Calculate effective GPUs based on sharing strategy
	policy.Status.EffectiveGPUs = r.calculateEffectiveGPUs(policy, totalGPUs)

	// Configure sharing on matching nodes based on strategy
	var res sharingResult
	switch policy.Spec.Strategy {
	case "time-slicing":
		res = r.configureTimeSlicing(ctx, policy, matchingNodes)
	case "mig":
		res = r.configureMIG(ctx, policy, matchingNodes)
	case "fractional":
		res = r.configureFractional(ctx, policy, matchingNodes)
	default:
		res = sharingResult{Disabled: true}
	}
	if err := res.err(); err != nil {
		log.Error(err, "Failed to configure GPU sharing", "strategy", policy.Spec.Strategy)
	}

	// Track per-pod GPU utilization
	r.trackPerPodUtilization(ctx, policy, matchingNodes)

	// Calculate current allocations
	policy.Status.CurrentAllocations = r.calculateAllocations(ctx, policy, matchingNodes)

	// Enforce fair sharing with utilization limits
	r.enforceFairSharing(ctx, policy, matchingNodes)

	// Update condition from what was actually written to the nodes
	status, reason, msg := sharingCondition(policy, res, len(matchingNodes))
	r.updateSharingCondition(policy, "Ready", status, reason, msg)

	// Update status
	if err := r.Status().Update(ctx, policy); err != nil {
		log.Error(err, "Failed to update GPU sharing policy status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: defaultSharingReconcileInterval}, nil
}

// sharingCondition maps a configure outcome to the Ready condition. Ready is
// True only when every matching node was labelled; the effective-GPU figure is
// an estimate computed from the policy, not a measurement of the device plugin.
func sharingCondition(policy *gryviav1.GryviaGPUSharingPolicy, res sharingResult, matched int) (metav1.ConditionStatus, string, string) {
	switch {
	case res.ConfigErr != nil:
		return metav1.ConditionFalse, "ConfigWriteFailed", res.ConfigErr.Error()
	case res.Disabled:
		return metav1.ConditionFalse, "StrategyNotEnabled",
			fmt.Sprintf("strategy %q is not enabled in the policy; no node labels were written", policy.Spec.Strategy)
	case len(res.Failed) > 0:
		return metav1.ConditionFalse, "LabelWriteFailed",
			fmt.Sprintf("labelled %d of %d nodes; write failed on %v", res.Applied, matched, res.Failed)
	case matched == 0:
		return metav1.ConditionFalse, "NoMatchingNodes", "no GryviaGpuNode matches the policy nodeSelector"
	case res.Applied == 0:
		return metav1.ConditionFalse, "NodesNotFound",
			fmt.Sprintf("none of the %d matching GryviaGpuNodes has a Kubernetes Node", matched)
	}
	msg := fmt.Sprintf("labelled %d of %d nodes; estimated %d effective GPUs from %d physical (computed from the policy; the NVIDIA device plugin must apply the matching config)",
		res.Applied, matched, policy.Status.EffectiveGPUs, policy.Status.TotalGPUs)
	if len(res.Missing) > 0 {
		msg += fmt.Sprintf("; no Kubernetes Node for %v", res.Missing)
	}
	return metav1.ConditionTrue, "LabelsApplied", msg
}

func (r *GryviaGPUSharingPolicyReconciler) findMatchingNodes(ctx context.Context, policy *gryviav1.GryviaGPUSharingPolicy) ([]gryviav1.GryviaGpuNode, error) {
	gpuNodeList := &gryviav1.GryviaGpuNodeList{}
	if err := r.List(ctx, gpuNodeList); err != nil {
		return nil, fmt.Errorf("failed to list GryviaGpuNodes: %w", err)
	}

	var matching []gryviav1.GryviaGpuNode
	for _, node := range gpuNodeList.Items {
		if r.nodeMatchesSelector(node, policy.Spec.NodeSelector) {
			matching = append(matching, node)
		}
	}

	return matching, nil
}

func (r *GryviaGPUSharingPolicyReconciler) nodeMatchesSelector(node gryviav1.GryviaGpuNode, selector map[string]string) bool {
	if len(selector) == 0 {
		return true
	}

	nodeLabels := node.Labels
	if nodeLabels == nil {
		nodeLabels = make(map[string]string)
	}

	// Also check against node spec fields
	effectiveLabels := make(map[string]string)
	for k, v := range nodeLabels {
		effectiveLabels[k] = v
	}
	effectiveLabels["gryvia.io/gpu-type"] = node.Spec.GpuType

	for key, value := range selector {
		if effectiveLabels[key] != value {
			return false
		}
	}

	return true
}

func (r *GryviaGPUSharingPolicyReconciler) calculateEffectiveGPUs(policy *gryviav1.GryviaGPUSharingPolicy, totalPhysical int) int {
	switch policy.Spec.Strategy {
	case "time-slicing":
		if policy.Spec.TimeSlicing != nil && policy.Spec.TimeSlicing.MaxPodsPerGPU > 0 {
			return totalPhysical * policy.Spec.TimeSlicing.MaxPodsPerGPU
		}
		return totalPhysical * 4 // Default 4x

	case "fractional":
		multiplier := 4 // Default 1/4
		if policy.Spec.FractionalGPU != nil {
			switch policy.Spec.FractionalGPU.Granularity {
			case "1/2":
				multiplier = 2
			case "1/4":
				multiplier = 4
			case "1/8":
				multiplier = 8
			case "1/16":
				multiplier = 16
			}
		}
		return totalPhysical * multiplier

	case "mig":
		if policy.Spec.MIG != nil {
			totalProfiles := 0
			for _, profile := range policy.Spec.MIG.Profiles {
				totalProfiles += profile.Count
			}
			if totalProfiles > 0 {
				return totalPhysical * totalProfiles
			}
		}
		return totalPhysical * 7 // Default A100 MIG profiles

	default:
		return totalPhysical
	}
}

// sharingResult says how many nodes a configure step labelled and why the rest
// were not, so the policy condition reflects what was actually written.
type sharingResult struct {
	Applied   int
	Missing   []string // GryviaGpuNodes whose Kubernetes Node does not exist
	Failed    []string // nodes where the label write failed
	Disabled  bool     // the strategy block is absent or not enabled
	ConfigErr error    // the device-plugin ConfigMap could not be written
}

func (res sharingResult) err() error {
	if res.ConfigErr != nil {
		return res.ConfigErr
	}
	if len(res.Failed) == 0 {
		return nil
	}
	return fmt.Errorf("label write failed on %d node(s): %v", len(res.Failed), res.Failed)
}

// labelNodes applies mutate to the Kubernetes Node behind each GryviaGpuNode and
// records the outcome per node.
func (r *GryviaGPUSharingPolicyReconciler) labelNodes(ctx context.Context, log logr.Logger, nodes []gryviav1.GryviaGpuNode, mutate func(map[string]string)) sharingResult {
	var res sharingResult
	for _, node := range nodes {
		k8sNode := &corev1.Node{}
		if err := r.Get(ctx, client.ObjectKey{Name: node.Spec.NodeName}, k8sNode); err != nil {
			if errors.IsNotFound(err) {
				res.Missing = append(res.Missing, node.Spec.NodeName)
				continue
			}
			log.Error(err, "Failed to get node", "node", node.Spec.NodeName)
			res.Failed = append(res.Failed, node.Spec.NodeName)
			continue
		}
		if k8sNode.Labels == nil {
			k8sNode.Labels = make(map[string]string)
		}
		mutate(k8sNode.Labels)
		if err := r.Update(ctx, k8sNode); err != nil {
			log.Error(err, "Failed to label node", "node", node.Spec.NodeName)
			res.Failed = append(res.Failed, node.Spec.NodeName)
			continue
		}
		res.Applied++
	}
	return res
}

func (r *GryviaGPUSharingPolicyReconciler) configureTimeSlicing(ctx context.Context, policy *gryviav1.GryviaGPUSharingPolicy, nodes []gryviav1.GryviaGpuNode) sharingResult {
	if policy.Spec.TimeSlicing == nil || !policy.Spec.TimeSlicing.Enabled {
		return sharingResult{Disabled: true}
	}
	log := r.Log.WithValues("policy", policy.Name, "strategy", "time-slicing")
	key := "gryvia-time-slicing"
	if r.DevicePluginConfigName != "" && r.DevicePluginConfigNamespace != "" {
		k, err := r.ensureTimeSlicingConfig(ctx, timeSlicingReplicas(policy))
		if err != nil {
			log.Error(err, "Failed to write the device-plugin time-slicing ConfigMap")
			return sharingResult{ConfigErr: fmt.Errorf("device-plugin ConfigMap %s/%s: %w", r.DevicePluginConfigNamespace, r.DevicePluginConfigName, err)}
		}
		key = k
	}
	return r.labelNodes(ctx, log, nodes, func(l map[string]string) {
		// The NVIDIA device plugin reads nvidia.com/device-plugin.config.
		l["gryvia.io/gpu-sharing"] = "time-slicing"
		l["gryvia.io/max-pods-per-gpu"] = fmt.Sprintf("%d", policy.Spec.TimeSlicing.MaxPodsPerGPU)
		l["nvidia.com/device-plugin.config"] = key
	})
}

// timeSlicingReplicas is the replica count a time-slicing policy asks the device plugin for
// (the same default as calculateEffectiveGPUs).
func timeSlicingReplicas(policy *gryviav1.GryviaGPUSharingPolicy) int {
	if policy.Spec.TimeSlicing != nil && policy.Spec.TimeSlicing.MaxPodsPerGPU > 0 {
		return policy.Spec.TimeSlicing.MaxPodsPerGPU
	}
	return 4
}

// ensureTimeSlicingConfig makes sure the device-plugin ConfigMap has the data key for the
// replica count and returns the key. It only adds or corrects its own "gryvia-ts-<n>" keys; other
// keys (including the chart's "gryvia-time-slicing") are left alone. A missing ConfigMap is
// created.
func (r *GryviaGPUSharingPolicyReconciler) ensureTimeSlicingConfig(ctx context.Context, replicas int) (string, error) {
	key := fmt.Sprintf("gryvia-ts-%d", replicas)
	body := fmt.Sprintf("version: v1\nflags:\n  migStrategy: none\nsharing:\n  timeSlicing:\n    renameByDefault: false\n    resources:\n      - name: nvidia.com/gpu\n        replicas: %d\n", replicas)
	nn := client.ObjectKey{Namespace: r.DevicePluginConfigNamespace, Name: r.DevicePluginConfigName}
	cm := &corev1.ConfigMap{}
	err := r.Get(ctx, nn, cm)
	if errors.IsNotFound(err) {
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: nn.Namespace, Name: nn.Name,
				Labels: map[string]string{"app.kubernetes.io/managed-by": "gryvia-gpu-operator"}},
			Data: map[string]string{key: body},
		}
		return key, r.Create(ctx, cm)
	}
	if err != nil {
		return "", err
	}
	if cm.Data[key] == body {
		return key, nil
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[key] = body
	return key, r.Update(ctx, cm)
}

func (r *GryviaGPUSharingPolicyReconciler) configureMIG(ctx context.Context, policy *gryviav1.GryviaGPUSharingPolicy, nodes []gryviav1.GryviaGpuNode) sharingResult {
	if policy.Spec.MIG == nil || !policy.Spec.MIG.Enabled {
		return sharingResult{Disabled: true}
	}
	log := r.Log.WithValues("policy", policy.Name, "strategy", "mig")
	return r.labelNodes(ctx, log, nodes, func(l map[string]string) {
		// nvidia.com/mig.config is what the GPU Operator MIG manager acts on.
		// The first profile name must be a mig-parted config (for example all-1g.10gb).
		l["gryvia.io/gpu-sharing"] = "mig"
		l["gryvia.io/mig-enabled"] = "true"
		for i, profile := range policy.Spec.MIG.Profiles {
			l[fmt.Sprintf("gryvia.io/mig-%s", profile.Name)] = fmt.Sprintf("%d", profile.Count)
			if i == 0 && profile.Name != "" {
				l["nvidia.com/mig.config"] = profile.Name
			}
		}
	})
}

func (r *GryviaGPUSharingPolicyReconciler) configureFractional(ctx context.Context, policy *gryviav1.GryviaGPUSharingPolicy, nodes []gryviav1.GryviaGpuNode) sharingResult {
	if policy.Spec.FractionalGPU == nil || !policy.Spec.FractionalGPU.Enabled {
		return sharingResult{Disabled: true}
	}
	log := r.Log.WithValues("policy", policy.Name, "strategy", "fractional")
	return r.labelNodes(ctx, log, nodes, func(l map[string]string) {
		l["gryvia.io/gpu-sharing"] = "fractional"
		l["gryvia.io/gpu-granularity"] = policy.Spec.FractionalGPU.Granularity
		if o := policy.Spec.FractionalGPU.Oversubscription; o != nil && o.Enabled {
			l["gryvia.io/gpu-oversubscription"] = "true"
			l["gryvia.io/gpu-oversubscription-ratio"] = fmt.Sprintf("%.1f", o.MaxRatio)
		}
	})
}

func (r *GryviaGPUSharingPolicyReconciler) trackPerPodUtilization(ctx context.Context, policy *gryviav1.GryviaGPUSharingPolicy, nodes []gryviav1.GryviaGpuNode) {
	var podUtils []gryviav1.GPUPodUtilization

	for _, node := range nodes {
		// List pods running on this node
		podList := &corev1.PodList{}
		if err := r.List(ctx, podList, client.MatchingFields{"spec.nodeName": node.Spec.NodeName}); err != nil {
			// If field selectors are not indexed, fall back to listing all pods
			// and filtering manually
			allPods := &corev1.PodList{}
			if err := r.List(ctx, allPods); err != nil {
				continue
			}
			for _, pod := range allPods.Items {
				if pod.Spec.NodeName == node.Spec.NodeName && pod.Status.Phase == corev1.PodRunning {
					// Check if this pod is using GPU resources
					for _, container := range pod.Spec.Containers {
						if gpuReq, ok := container.Resources.Requests["nvidia.com/gpu"]; ok {
							if gpuReq.Value() > 0 {
								podUtils = append(podUtils, gryviav1.GPUPodUtilization{
									PodName:   pod.Name,
									Namespace: pod.Namespace,
									NodeName:  node.Spec.NodeName,
								})
							}
						}
					}
				}
			}
			continue
		}

		for _, pod := range podList.Items {
			if pod.Status.Phase != corev1.PodRunning {
				continue
			}
			for _, container := range pod.Spec.Containers {
				if gpuReq, ok := container.Resources.Requests["nvidia.com/gpu"]; ok {
					if gpuReq.Value() > 0 {
						podUtils = append(podUtils, gryviav1.GPUPodUtilization{
							PodName:   pod.Name,
							Namespace: pod.Namespace,
							NodeName:  node.Spec.NodeName,
						})
					}
				}
			}
		}
	}

	// Only keep the most recent 100 entries
	if len(podUtils) > 100 {
		podUtils = podUtils[:100]
	}

	policy.Status.PerPodUtilization = podUtils
}

func (r *GryviaGPUSharingPolicyReconciler) calculateAllocations(ctx context.Context, policy *gryviav1.GryviaGPUSharingPolicy, nodes []gryviav1.GryviaGpuNode) *gryviav1.GPUSharingAllocations {
	allocs := &gryviav1.GPUSharingAllocations{
		Physical: policy.Status.TotalGPUs,
	}

	// Count fractional allocations from per-pod tracking
	allocs.Fractional = len(policy.Status.PerPodUtilization)

	// Calculate utilization from GPU node status
	totalUtil := 0.0
	gpuCount := 0
	for _, node := range nodes {
		for _, gpuStatus := range node.Status.GpuStatus {
			totalUtil += float64(gpuStatus.Utilization) / 100.0
			gpuCount++
		}
	}
	if gpuCount > 0 {
		allocs.Utilization = totalUtil / float64(gpuCount)
	}

	return allocs
}

func (r *GryviaGPUSharingPolicyReconciler) enforceFairSharing(ctx context.Context, policy *gryviav1.GryviaGPUSharingPolicy, nodes []gryviav1.GryviaGpuNode) {
	log := r.Log.WithValues("policy", policy.Name)

	// Check tenant quotas
	if len(policy.Spec.TenantQuotas) == 0 {
		return
	}

	// Count current allocations per tenant from pod labels
	tenantAllocations := make(map[string]int)
	for _, pu := range policy.Status.PerPodUtilization {
		// Look up the pod to find tenant label
		pod := &corev1.Pod{}
		if err := r.Get(ctx, client.ObjectKey{Name: pu.PodName, Namespace: pu.Namespace}, pod); err != nil {
			continue
		}
		tenant := pod.Labels["gryvia.io/tenant"]
		if tenant == "" {
			tenant = pod.Labels["gryvia.io/team"]
		}
		if tenant != "" {
			tenantAllocations[tenant]++
		}
	}

	// Log warnings for tenants exceeding their quotas
	for _, tq := range policy.Spec.TenantQuotas {
		current := tenantAllocations[tq.Tenant]
		if tq.MaxFractionalGPUs > 0 && current > tq.MaxFractionalGPUs {
			log.Info("Tenant exceeding GPU sharing quota",
				"tenant", tq.Tenant,
				"currentAllocations", current,
				"maxFractionalGPUs", tq.MaxFractionalGPUs,
			)
		}
	}
}

func (r *GryviaGPUSharingPolicyReconciler) updateSharingCondition(policy *gryviav1.GryviaGPUSharingPolicy, condType string, status metav1.ConditionStatus, reason, message string) {
	condition := metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: policy.Generation,
		LastTransitionTime: metav1.Now(),
	}

	found := false
	for i, cond := range policy.Status.Conditions {
		if cond.Type == condType {
			policy.Status.Conditions[i] = condition
			found = true
			break
		}
	}
	if !found {
		policy.Status.Conditions = append(policy.Status.Conditions, condition)
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *GryviaGPUSharingPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gryviav1.GryviaGPUSharingPolicy{}).
		Complete(r)
}
