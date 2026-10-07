package controllers

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

// GryviaFederationReconciler reconciles a GryviaFederation object
type GryviaFederationReconciler struct {
	client.Client
	Scheme               *runtime.Scheme
	Log                  logr.Logger
	CredentialsNamespace string
	AllowedServers       []string
	// Failover (--federation-failover) lets spec.failover.automatic fence failed members and move their Kueue
	// Workloads; off, the controller only probes.
	Failover bool
	Recorder record.EventRecorder
	// RemoteClient overrides how a member is reached (tests); nil connects through restConfigFor.
	RemoteClient func(ctx context.Context, c gryviav1.FederationCluster) (client.Client, error)
	// Now overrides the clock (tests).
	Now func() time.Time
}

//+kubebuilder:rbac:groups=gryvia.io,resources=gryviafederations,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviafederations/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviafederations/finalizers,verbs=update
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaaijobs,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *GryviaFederationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("gryviafederation", req.NamespacedName)

	// Fetch the GryviaFederation instance
	federation := &gryviav1.GryviaFederation{}
	err := r.Get(ctx, req.NamespacedName, federation)
	if err != nil {
		if errors.IsNotFound(err) {
			log.Info("GryviaFederation resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get GryviaFederation")
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !federation.ObjectMeta.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	log.Info("Reconciling GryviaFederation", "clusters", len(federation.Spec.Clusters))

	// Reconcile the federation
	result, err := r.reconcileFederation(ctx, federation)
	if err != nil {
		log.Error(err, "Failed to reconcile federation")
		return result, err
	}

	return result, nil
}

func (r *GryviaFederationReconciler) reconcileFederation(ctx context.Context, federation *gryviav1.GryviaFederation) (ctrl.Result, error) {
	log := r.Log.WithValues("federation", federation.Name)

	// Health check all member clusters
	now := metav1.NewTime(r.now())
	clusterStatuses := r.healthCheckClusters(ctx, federation)
	trackHealth(federation.Status.ClusterStatus, clusterStatuses, now)
	federation.Status.ClusterStatus = clusterStatuses
	failoverWait, failoverErr := r.reconcileFailover(ctx, federation, now)
	if failoverErr != nil {
		log.Error(failoverErr, "Failover")
		r.updateFederationCondition(federation, "FailoverFenced", metav1.ConditionUnknown, "FailoverError", failoverErr.Error())
	}
	clusterStatuses = federation.Status.ClusterStatus

	// Calculate aggregate stats
	federation.Status.AggregateStats = r.calculateAggregateStats(federation)

	// Remote workload dispatch is owned by MultiKueue, not fabricated local job counts.
	federation.Status.JobDistribution = nil

	r.updateFederationCondition(federation, "DispatchReady", metav1.ConditionUnknown, "UseMultiKueue", "Configure a Kueue MultiKueue admission check for workload dispatch")
	// Determine overall federation state
	federation.Status.State = r.determineFederationState(clusterStatuses)

	// Update condition
	ready := metav1.ConditionTrue
	for _, s := range clusterStatuses {
		if s.State != "healthy" {
			ready = metav1.ConditionFalse
		}
	}
	if len(clusterStatuses) == 0 {
		ready = metav1.ConditionFalse
	}
	r.updateFederationCondition(federation, "Ready", ready, "ClusterProbes",
		fmt.Sprintf("Federation %s: %d clusters, %d total GPUs",
			federation.Status.State,
			len(clusterStatuses),
			federation.Status.AggregateStats.TotalGPUs))

	// Update status
	if err := r.Status().Update(ctx, federation); err != nil {
		log.Error(err, "Failed to update federation status")
		return ctrl.Result{}, err
	}

	// Requeue based on health check interval
	requeueAfter := 30 * time.Second
	if federation.Spec.Failover != nil && federation.Spec.Failover.HealthCheck != nil {
		if interval, err := time.ParseDuration(federation.Spec.Failover.HealthCheck.Interval); err == nil {
			requeueAfter = interval
		}
	}

	if failoverWait > 0 && failoverWait < requeueAfter {
		requeueAfter = failoverWait
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, failoverErr
}

func (r *GryviaFederationReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *GryviaFederationReconciler) healthCheckClusters(ctx context.Context, federation *gryviav1.GryviaFederation) []gryviav1.FederationClusterStatus {
	var statuses []gryviav1.FederationClusterStatus

	for _, cluster := range federation.Spec.Clusters {
		if !cluster.Enabled {
			continue
		}

		status := gryviav1.FederationClusterStatus{
			Name: cluster.Name,
		}

		// Perform health check
		// In production, this would use the cluster's API server endpoint
		// and credentials to verify connectivity
		healthy := r.checkClusterHealth(ctx, cluster)

		now := metav1.Now()
		status.LastHealthCheck = &now

		if healthy {
			status.State = "healthy"
		} else {
			status.State = "unhealthy"
		}

		// Calculate utilization from capacity
		// Spec capacity is declared inventory, not a live usage measurement.
		status.Utilization = nil

		statuses = append(statuses, status)
	}

	return statuses
}

func (r *GryviaFederationReconciler) checkClusterHealth(ctx context.Context, c gryviav1.FederationCluster) bool {
	return r.probeCluster(ctx, c) == nil
}

func (r *GryviaFederationReconciler) calculateAggregateStats(federation *gryviav1.GryviaFederation) *gryviav1.FederationAggregateStats {
	stats := &gryviav1.FederationAggregateStats{}

	for _, cluster := range federation.Spec.Clusters {
		if !cluster.Enabled || cluster.Capacity == nil {
			continue
		}

		for _, gpuType := range cluster.Capacity.GPUTypes {
			stats.TotalGPUs += gpuType.Total
			stats.AvailableGPUs += gpuType.Available
		}

	}

	return stats
}

func (r *GryviaFederationReconciler) determineFederationState(statuses []gryviav1.FederationClusterStatus) string {
	if len(statuses) == 0 {
		return "unavailable"
	}

	healthyCount := 0
	for _, status := range statuses {
		if status.State == "healthy" {
			healthyCount++
		}
	}

	if healthyCount == len(statuses) {
		return "healthy"
	}
	if healthyCount == 0 {
		return "unavailable"
	}
	return "degraded"
}

func (r *GryviaFederationReconciler) updateFederationCondition(federation *gryviav1.GryviaFederation, condType string, status metav1.ConditionStatus, reason, message string) {
	condition := metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: federation.Generation,
		LastTransitionTime: metav1.Now(),
	}

	found := false
	for i, cond := range federation.Status.Conditions {
		if cond.Type == condType {
			federation.Status.Conditions[i] = condition
			found = true
			break
		}
	}
	if !found {
		federation.Status.Conditions = append(federation.Status.Conditions, condition)
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *GryviaFederationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gryviav1.GryviaFederation{}).
		Complete(r)
}
