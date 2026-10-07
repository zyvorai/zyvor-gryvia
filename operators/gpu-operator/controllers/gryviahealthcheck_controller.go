package controllers

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"k8s.io/client-go/util/retry"

	gryviav1 "github.com/zyvorai/gryvia/operators/gpu-operator/api/v1"
)

const (
	healthHealthy   = "healthy"
	healthDegraded  = "degraded"
	healthUnhealthy = "unhealthy"
	healthUnknown   = "unknown"

	checkStatusPass    = "pass"
	checkStatusWarning = "warning"
	checkStatusFail    = "fail"
)

// GryviaHealthCheckReconciler reconciles a GryviaHealthCheck object
type GryviaHealthCheckReconciler struct {
	client.Client
	Scheme            *runtime.Scheme
	Log               logr.Logger
	EnableRemediation bool
	Now               func() time.Time // nil = time.Now
}

//+kubebuilder:rbac:groups=gryvia.io,resources=gryviahealthchecks,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviahealthchecks/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviahealthchecks/finalizers,verbs=update
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviagpunodes,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch

// Reconcile is part of the main kubernetes reconciliation loop
func (r *GryviaHealthCheckReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("gryviahealthcheck", req.NamespacedName)

	// Fetch the GryviaHealthCheck instance
	hc := &gryviav1.GryviaHealthCheck{}
	err := r.Get(ctx, req.NamespacedName, hc)
	if err != nil {
		if errors.IsNotFound(err) {
			log.Info("GryviaHealthCheck resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get GryviaHealthCheck")
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !hc.ObjectMeta.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// Initialize status if needed
	if hc.Status.OverallHealth == "" {
		hc.Status.OverallHealth = healthUnknown
		if err := r.Status().Update(ctx, hc); err != nil {
			log.Error(err, "Failed to update initial status")
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Reconcile health check
	result, err := r.reconcileHealthCheck(ctx, hc)
	if err != nil {
		log.Error(err, "Failed to reconcile health check")
		return result, err
	}

	return result, nil
}

func (r *GryviaHealthCheckReconciler) reconcileHealthCheck(ctx context.Context, hc *gryviav1.GryviaHealthCheck) (ctrl.Result, error) {
	log := r.Log.WithValues("gryviahealthcheck", hc.Name)

	// Get target nodes
	nodes, err := r.getTargetNodes(ctx, hc)
	if err != nil {
		log.Error(err, "Failed to get target nodes")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}

	// Get GPU node info
	gpuNodes := &gryviav1.GryviaGpuNodeList{}
	if err := r.List(ctx, gpuNodes); err != nil {
		log.Error(err, "Failed to list GryviaGpuNodes")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}

	// Run health checks on each node
	var checkResults []gryviav1.HealthCheckResult
	var affectedResources []gryviav1.AffectedResource
	overallHealth := healthHealthy
	if len(nodes) == 0 {
		overallHealth = healthUnknown
	}
	now := metav1.Now()
	if r.Now != nil {
		now = metav1.NewTime(r.Now())
	}

	for _, node := range nodes {
		// Find matching GPU node
		var gpuNode *gryviav1.GryviaGpuNode
		for i := range gpuNodes.Items {
			if gpuNodes.Items[i].Spec.NodeName == node.Name {
				gpuNode = &gpuNodes.Items[i]
				break
			}
		}

		// Run each configured check
		for _, check := range hc.Spec.Checks {
			if !check.Enabled {
				continue
			}

			result := r.runCheck(check, gpuNode, &node)
			result.Timestamp = &now
			result.CheckName = check.Name + "@" + node.Name
			checkResults = append(checkResults, result)

			// Track affected resources
			if result.Status == checkStatusFail {
				overallHealth = healthUnhealthy
				affectedResources = append(affectedResources, gryviav1.AffectedResource{
					Type:   "node",
					Name:   node.Name,
					Status: healthUnhealthy,
				})
			} else if result.Status == checkStatusWarning && overallHealth != healthUnhealthy {
				overallHealth = healthDegraded
				affectedResources = append(affectedResources, gryviav1.AffectedResource{
					Type:   "node",
					Name:   node.Name,
					Status: healthDegraded,
				})
			}
		}
	}

	// Update status
	hc.Status.LastCheckTime = &now
	nextCheck := now.Add(r.getCheckInterval(hc))
	nextCheckTime := metav1.NewTime(nextCheck)
	hc.Status.NextCheckTime = &nextCheckTime
	hc.Status.OverallHealth = overallHealth
	hc.Status.CheckResults = checkResults
	hc.Status.AffectedResources = affectedResources

	var remediationErr error
	// Handle failures
	if overallHealth == healthUnhealthy || overallHealth == healthDegraded {
		if err := r.handleFailure(ctx, hc, nodes, affectedResources); err != nil {
			log.Error(err, "Failed to handle health check failure")
			remediationErr = err
		}
	}

	bad := map[string]bool{}
	for _, res := range affectedResources {
		if res.Type == "node" {
			bad[res.Name] = true
		}
	}
	recovery, recoveryErr := r.reconcileRecovery(ctx, hc, nodes, bad, now.Time)
	if recoveryErr != nil {
		log.Error(recoveryErr, "Failed to drive GPU recovery")
		if remediationErr == nil {
			remediationErr = recoveryErr
		}
	}

	if hc.Spec.OnFailure != nil {
		status, reason, msg := metav1.ConditionTrue, "NoActionRequired", "No remediation failure observed"
		if !r.EnableRemediation {
			status, reason, msg = metav1.ConditionFalse, "Disabled", "Enable operator GPU remediation capability before requesting mutations"
		}
		switch {
		case remediationErr != nil:
			status, reason, msg = metav1.ConditionFalse, "DrainBlocked", remediationErr.Error()
		case len(recovery.exhausted) > 0:
			status, reason, msg = metav1.ConditionFalse, "RemediationExhausted", strings.Join(recovery.exhausted, "; ")
		case len(recovery.recovering) > 0:
			status, reason, msg = metav1.ConditionFalse, "Recovering", strings.Join(recovery.recovering, "; ")
		case len(recovery.dryRun) > 0:
			status, reason, msg = metav1.ConditionFalse, "AgentDryRun", "reset agent is in dry-run: "+strings.Join(recovery.dryRun, "; ")
		case len(recovery.recovered) > 0:
			status, reason, msg = metav1.ConditionTrue, "Recovered", "quarantine lifted: "+strings.Join(recovery.recovered, ", ")
		}
		meta.SetStatusCondition(&hc.Status.Conditions, metav1.Condition{Type: "RemediationReady", Status: status, Reason: reason, Message: msg, ObservedGeneration: hc.Generation})
	}
	// Update node labels based on health status
	if err := r.updateNodeLabels(ctx, nodes, checkResults, overallHealth); err != nil {
		log.Error(err, "Failed to update node labels")
	}

	if len(hc.Status.RemediationHistory) > 20 {
		hc.Status.RemediationHistory = hc.Status.RemediationHistory[len(hc.Status.RemediationHistory)-20:]
	}
	// Update status
	if err := r.Status().Update(ctx, hc); err != nil {
		log.Error(err, "Failed to update status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: r.getCheckInterval(hc)}, remediationErr
}

func (r *GryviaHealthCheckReconciler) getTargetNodes(ctx context.Context, hc *gryviav1.GryviaHealthCheck) ([]corev1.Node, error) {
	nodeList := &corev1.NodeList{}

	switch hc.Spec.Target.Type {
	case "cluster":
		// Get all GPU nodes
		if err := r.List(ctx, nodeList, client.HasLabels{"gryvia.io/gpu"}); err != nil {
			return nil, err
		}

	case "node":
		// Get nodes matching selector
		if len(hc.Spec.Target.Selector) > 0 {
			if err := r.List(ctx, nodeList, client.MatchingLabels(hc.Spec.Target.Selector)); err != nil {
				return nil, err
			}
		} else {
			if err := r.List(ctx, nodeList, client.HasLabels{"gryvia.io/gpu"}); err != nil {
				return nil, err
			}
		}

	default:
		// For job and other types, get all GPU nodes as a fallback
		if err := r.List(ctx, nodeList, client.HasLabels{"gryvia.io/gpu"}); err != nil {
			return nil, err
		}
	}

	return nodeList.Items, nil
}

func (r *GryviaHealthCheckReconciler) runCheck(check gryviav1.HealthCheck, gpuNode *gryviav1.GryviaGpuNode, node *corev1.Node) gryviav1.HealthCheckResult {
	result := gryviav1.HealthCheckResult{
		CheckName: check.Name,
		Status:    checkStatusPass,
		Message:   "Check passed",
	}

	if gpuNode == nil || len(gpuNode.Status.GpuStatus) == 0 || gpuNode.Status.LastHealthCheck == nil || time.Since(gpuNode.Status.LastHealthCheck.Time) > 2*time.Minute || gpuNode.Status.LastHealthCheck.Time.After(time.Now().Add(time.Second)) {
		result.Status = checkStatusWarning
		result.Message = "No GryviaGpuNode found for node " + node.Name
		return result
	}

	switch check.Type {
	case "gpu-temperature":
		r.checkGpuTemperature(check, gpuNode, &result)

	case "gpu-utilization":
		r.checkGpuUtilization(check, gpuNode, &result)

	case "gpu-memory":
		r.checkGpuMemory(check, gpuNode, &result)

	case "gpu-ecc-errors":
		r.checkECCErrors(check, gpuNode, &result)

	case "gpu-nvlink":
		r.checkNVLink(check, gpuNode, &result)

	case "gpu-power":
		r.checkGpuPower(check, gpuNode, &result)

	case "pcie-bandwidth":
		result.Status = checkStatusWarning
		result.Value = "N/A"
		result.Message = "PCIe bandwidth check requires runtime measurement"

	case "clock-speeds":
		result.Status = checkStatusWarning
		result.Value = "N/A"
		result.Message = "Clock speed check requires runtime measurement"

	default:
		result.Status = checkStatusWarning
		result.Message = fmt.Sprintf("Unknown check type: %s", check.Type)
	}

	return result
}

func (r *GryviaHealthCheckReconciler) checkGpuTemperature(check gryviav1.HealthCheck, gpuNode *gryviav1.GryviaGpuNode, result *gryviav1.HealthCheckResult) {
	var maxTemp int
	for _, gpuStatus := range gpuNode.Status.GpuStatus {
		if gpuStatus.Temperature > maxTemp {
			maxTemp = gpuStatus.Temperature
		}
	}

	result.Value = fmt.Sprintf("%d", maxTemp)

	if check.Threshold != nil {
		thresholdStr := ""
		if check.Threshold.Max != nil {
			thresholdStr = fmt.Sprintf("max: %.0f", *check.Threshold.Max)
			if float64(maxTemp) > *check.Threshold.Max {
				result.Status = checkStatusFail
				result.Message = fmt.Sprintf("GPU temperature %d exceeds max threshold %.0f", maxTemp, *check.Threshold.Max)
				return
			}
		}
		if check.Threshold.Critical != nil {
			if float64(maxTemp) > *check.Threshold.Critical {
				result.Status = checkStatusFail
				result.Message = fmt.Sprintf("GPU temperature %d exceeds critical threshold %.0f", maxTemp, *check.Threshold.Critical)
				return
			}
		}
		if check.Threshold.Warning != nil {
			if float64(maxTemp) > *check.Threshold.Warning {
				result.Status = checkStatusWarning
				result.Message = fmt.Sprintf("GPU temperature %d exceeds warning threshold %.0f", maxTemp, *check.Threshold.Warning)
				return
			}
		}
		result.Threshold = thresholdStr
	}

	result.Message = fmt.Sprintf("GPU temperature %d within normal range", maxTemp)
}

func (r *GryviaHealthCheckReconciler) checkGpuUtilization(check gryviav1.HealthCheck, gpuNode *gryviav1.GryviaGpuNode, result *gryviav1.HealthCheckResult) {
	var maxUtil int
	for _, gpuStatus := range gpuNode.Status.GpuStatus {
		if gpuStatus.Utilization > maxUtil {
			maxUtil = gpuStatus.Utilization
		}
	}

	result.Value = fmt.Sprintf("%d", maxUtil)

	if check.Threshold != nil {
		if check.Threshold.Max != nil && float64(maxUtil) > *check.Threshold.Max {
			result.Status = checkStatusFail
			result.Message = fmt.Sprintf("GPU utilization %d%% exceeds max threshold %.0f%%", maxUtil, *check.Threshold.Max)
			return
		}
		if check.Threshold.Warning != nil && float64(maxUtil) > *check.Threshold.Warning {
			result.Status = checkStatusWarning
			result.Message = fmt.Sprintf("GPU utilization %d%% exceeds warning threshold %.0f%%", maxUtil, *check.Threshold.Warning)
			return
		}
		if check.Threshold.Min != nil && float64(maxUtil) < *check.Threshold.Min {
			result.Status = checkStatusWarning
			result.Message = fmt.Sprintf("GPU utilization %d%% below minimum threshold %.0f%%", maxUtil, *check.Threshold.Min)
			return
		}
	}

	result.Message = fmt.Sprintf("GPU utilization %d%% within normal range", maxUtil)
}

func (r *GryviaHealthCheckReconciler) checkGpuMemory(check gryviav1.HealthCheck, gpuNode *gryviav1.GryviaGpuNode, result *gryviav1.HealthCheckResult) {
	var maxMemPct float64
	for _, gpuStatus := range gpuNode.Status.GpuStatus {
		if gpuStatus.MemoryTotal > 0 {
			pct := float64(gpuStatus.MemoryUsed) / float64(gpuStatus.MemoryTotal) * 100
			if pct > maxMemPct {
				maxMemPct = pct
			}
		}
	}

	result.Value = fmt.Sprintf("%.1f", maxMemPct)

	if check.Threshold != nil {
		if check.Threshold.Max != nil && maxMemPct > *check.Threshold.Max {
			result.Status = checkStatusFail
			result.Message = fmt.Sprintf("GPU memory usage %.1f%% exceeds max threshold %.0f%%", maxMemPct, *check.Threshold.Max)
			return
		}
		if check.Threshold.Warning != nil && maxMemPct > *check.Threshold.Warning {
			result.Status = checkStatusWarning
			result.Message = fmt.Sprintf("GPU memory usage %.1f%% exceeds warning threshold %.0f%%", maxMemPct, *check.Threshold.Warning)
			return
		}
	}

	result.Message = fmt.Sprintf("GPU memory usage %.1f%% within normal range", maxMemPct)
}

func (r *GryviaHealthCheckReconciler) checkECCErrors(check gryviav1.HealthCheck, gpuNode *gryviav1.GryviaGpuNode, result *gryviav1.HealthCheckResult) {
	result.Status = checkStatusWarning
	result.Message = "ECC metrics are not available in GryviaGpuNode status"
}

func (r *GryviaHealthCheckReconciler) checkNVLink(check gryviav1.HealthCheck, gpuNode *gryviav1.GryviaGpuNode, result *gryviav1.HealthCheckResult) {
	// Check NVLink health via GPU node status
	healthyGPUs := 0
	totalGPUs := len(gpuNode.Status.GpuStatus)
	for _, gpuStatus := range gpuNode.Status.GpuStatus {
		if gpuStatus.Health == "Healthy" || gpuStatus.Health == "" {
			healthyGPUs++
		}
	}

	result.Value = fmt.Sprintf("%d/%d", healthyGPUs, totalGPUs)

	if healthyGPUs < totalGPUs {
		result.Status = checkStatusWarning
		result.Message = fmt.Sprintf("NVLink: %d/%d GPUs healthy", healthyGPUs, totalGPUs)
	} else {
		result.Message = "All NVLink connections healthy"
	}
}

func (r *GryviaHealthCheckReconciler) checkGpuPower(check gryviav1.HealthCheck, gpuNode *gryviav1.GryviaGpuNode, result *gryviav1.HealthCheckResult) {
	var maxPower int
	for _, gpuStatus := range gpuNode.Status.GpuStatus {
		if gpuStatus.PowerUsage > maxPower {
			maxPower = gpuStatus.PowerUsage
		}
	}

	result.Value = fmt.Sprintf("%d", maxPower)

	if check.Threshold != nil {
		if check.Threshold.Max != nil && float64(maxPower) > *check.Threshold.Max {
			result.Status = checkStatusFail
			result.Message = fmt.Sprintf("GPU power %dW exceeds max threshold %.0fW", maxPower, *check.Threshold.Max)
			return
		}
		if check.Threshold.Warning != nil && float64(maxPower) > *check.Threshold.Warning {
			result.Status = checkStatusWarning
			result.Message = fmt.Sprintf("GPU power %dW exceeds warning threshold %.0fW", maxPower, *check.Threshold.Warning)
			return
		}
	}

	result.Message = fmt.Sprintf("GPU power %dW within normal range", maxPower)
}

func (r *GryviaHealthCheckReconciler) handleFailure(ctx context.Context, hc *gryviav1.GryviaHealthCheck, nodes []corev1.Node, affected []gryviav1.AffectedResource) error {
	if hc.Spec.OnFailure == nil {
		return nil
	}
	if !r.EnableRemediation {
		return nil
	}
	for _, res := range affected {
		if res.Type != "node" {
			continue
		}
		if hc.Spec.OnFailure.Cordon || hc.Spec.OnFailure.Drain {
			if err := r.quarantine(ctx, hc, res.Name); err != nil {
				return err
			}
		}
		if hc.Spec.OnFailure.Drain {
			if err := r.drain(ctx, res.Name); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *GryviaHealthCheckReconciler) updateNodeLabels(ctx context.Context, nodes []corev1.Node, results []gryviav1.HealthCheckResult, overallHealth string) error {
	nodeHealth := map[string]string{}
	for _, result := range results {
		for _, node := range nodes {
			if result.CheckName != "" && strings.HasSuffix(result.CheckName, "@"+node.Name) {
				if result.Status == checkStatusFail {
					nodeHealth[node.Name] = healthUnhealthy
				} else if result.Status == checkStatusWarning && nodeHealth[node.Name] != healthUnhealthy {
					nodeHealth[node.Name] = healthUnknown
				}
			}
		}
	}

	for _, node := range nodes {
		health, ok := nodeHealth[node.Name]
		if !ok {
			health = healthHealthy
		}

		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			n := &corev1.Node{}
			if err := r.Get(ctx, types.NamespacedName{Name: node.Name}, n); err != nil {
				return err
			}

			if n.Labels == nil {
				n.Labels = make(map[string]string)
			}

			currentHealth := n.Labels["gryvia.io/gpu-health"]
			if currentHealth != health {
				n.Labels["gryvia.io/gpu-health"] = health
				return r.Update(ctx, n)
			}
			return nil
		}); err != nil {
			r.Log.Error(err, "Failed to update node health label", "node", node.Name)
		}
	}

	return nil
}

func (r *GryviaHealthCheckReconciler) getCheckInterval(hc *gryviav1.GryviaHealthCheck) time.Duration {
	// Default to 5 minutes
	interval := 5 * time.Minute

	// Parse cron-like schedule for simple intervals
	if hc.Spec.Schedule != "" {
		switch hc.Spec.Schedule {
		case "*/1 * * * *":
			interval = 1 * time.Minute
		case "*/2 * * * *":
			interval = 2 * time.Minute
		case "*/5 * * * *":
			interval = 5 * time.Minute
		case "*/10 * * * *":
			interval = 10 * time.Minute
		case "*/15 * * * *":
			interval = 15 * time.Minute
		}
	}

	return interval
}

// SetupWithManager sets up the controller with the Manager.
func (r *GryviaHealthCheckReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gryviav1.GryviaHealthCheck{}).
		Complete(r)
}
