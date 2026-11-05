package controllers

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gryviav1 "github.com/zyvorai/gryvia/operators/quota-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/quota-operator/pkg/budget"
)

const (
	gryviaTenantFinalizer = "gryvia.io/tenant-finalizer"
)

// GryviaTenantReconciler reconciles a GryviaTenant object
type GryviaTenantReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// TenantRBAC creates RoleBindings for members and OIDC groups (operator flag
	// --tenant-rbac, default false; needs the rolebindings and bind RBAC from the chart).
	TenantRBAC bool
}

//+kubebuilder:rbac:groups=gryvia.io,resources=gryviatenants,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviatenants/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviatenants/finalizers,verbs=update
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaaijobs,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;patch
//+kubebuilder:rbac:groups="",resources=resourcequotas,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=limitranges,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=events,verbs=get;create;patch

func (r *GryviaTenantReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the GryviaTenant instance
	tenant := &gryviav1.GryviaTenant{}
	err := r.Get(ctx, req.NamespacedName, tenant)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("GryviaTenant resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get GryviaTenant")
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !tenant.ObjectMeta.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, tenant)
	}

	// Add finalizer if it doesn't exist
	if !controllerutil.ContainsFinalizer(tenant, gryviaTenantFinalizer) {
		controllerutil.AddFinalizer(tenant, gryviaTenantFinalizer)
		if err := r.Update(ctx, tenant); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	logger.Info("Reconciling GryviaTenant", "tenant", tenant.Name, "members", len(tenant.Spec.Members))

	// Reconcile the tenant
	result, err := r.reconcileTenant(ctx, tenant)
	if err != nil {
		logger.Error(err, "Failed to reconcile tenant")
		r.updateCondition(tenant, "Ready", metav1.ConditionFalse, "ReconcileFailed", err.Error())
		if statusErr := r.Status().Update(ctx, tenant); statusErr != nil {
			logger.Error(statusErr, "Failed to update status after reconcile failure")
		}
		return result, err
	}

	// Update status
	if err := r.Status().Update(ctx, tenant); err != nil {
		logger.Error(err, "Failed to update GryviaTenant status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: 2 * time.Minute}, nil
}

func (r *GryviaTenantReconciler) reconcileTenant(ctx context.Context, tenant *gryviav1.GryviaTenant) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Ensure namespace exists for tenant
	nsName := fmt.Sprintf("tenant-%s", tenant.Name)
	if err := r.ensureNamespace(ctx, tenant, nsName); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to ensure namespace: %w", err)
	}

	// RoleBindings for members and OIDC groups (gryviatenant_rbac.go)
	if !r.TenantRBAC {
		// disabled: nothing to do
	} else if invalid, err := r.reconcileTenantRBAC(ctx, tenant, nsName); err != nil {
		logger.Error(err, "Failed to reconcile tenant RoleBindings")
		r.updateCondition(tenant, "RBACReady", metav1.ConditionFalse, "RoleBindingFailed", err.Error())
	} else if len(invalid) > 0 {
		r.updateCondition(tenant, "RBACReady", metav1.ConditionTrue, "UnknownRole",
			"unknown roles treated as viewer: "+strings.Join(invalid, ", "))
	} else {
		r.updateCondition(tenant, "RBACReady", metav1.ConditionTrue, "RoleBindingsApplied", "RoleBindings match spec.members and spec.oidcGroups")
	}

	// Apply ResourceQuota to the namespace
	if tenant.Spec.Quotas != nil {
		if err := r.ensureResourceQuota(ctx, tenant, nsName); err != nil {
			logger.Error(err, "Failed to ensure ResourceQuota")
		}
	}

	// Apply LimitRange to the namespace
	if err := r.ensureLimitRange(ctx, tenant, nsName); err != nil {
		logger.Error(err, "Failed to ensure LimitRange")
	}

	// Apply NetworkPolicy for isolation
	if tenant.Spec.NetworkPolicy != nil && tenant.Spec.NetworkPolicy.Isolated {
		if err := r.ensureNetworkPolicy(ctx, tenant, nsName); err != nil {
			logger.Error(err, "Failed to ensure NetworkPolicy")
		}
	}

	// Calculate current resource usage
	usage, err := r.calculateUsage(ctx, tenant, nsName)
	if err != nil {
		logger.Error(err, "Failed to calculate usage")
	} else {
		tenant.Status.CurrentUsage = usage
	}

	// Calculate quota utilization
	tenant.Status.QuotaUtilization = r.calculateUtilization(tenant)

	// Set member count
	tenant.Status.MemberCount = len(tenant.Spec.Members)

	// Set managed namespaces
	tenant.Status.NamespacesManaged = []string{nsName}

	// Determine health
	tenant.Status.Health = r.determineHealth(tenant)

	r.updateCondition(tenant, "Ready", metav1.ConditionTrue, "TenantActive",
		fmt.Sprintf("Tenant %s: %d members, health=%s", tenant.Spec.DisplayName, tenant.Status.MemberCount, tenant.Status.Health))

	return ctrl.Result{}, nil
}

func (r *GryviaTenantReconciler) ensureNamespace(ctx context.Context, tenant *gryviav1.GryviaTenant, nsName string) error {
	ns := &corev1.Namespace{}
	err := r.Get(ctx, types.NamespacedName{Name: nsName}, ns)
	if err != nil {
		if errors.IsNotFound(err) {
			// Create the namespace
			ns = &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: nsName,
					Labels: map[string]string{
						"gryvia.io/tenant":     tenant.Name,
						"gryvia.io/managed-by": "gryvia-tenant-controller",
					},
				},
			}
			// Label values cannot hold free text, so the display name is not copied.
			if tenant.Spec.Governance != nil && tenant.Spec.Governance.DataClassification != "" {
				ns.Labels["gryvia.io/data-classification"] = tenant.Spec.Governance.DataClassification
			}
			return r.Create(ctx, ns)
		}
		return err
	}

	// Update labels
	if ns.Labels == nil {
		ns.Labels = make(map[string]string)
	}
	if ns.Labels["gryvia.io/tenant"] == tenant.Name && ns.Labels["gryvia.io/managed-by"] == "gryvia-tenant-controller" {
		return nil
	}
	ns.Labels["gryvia.io/tenant"] = tenant.Name
	ns.Labels["gryvia.io/managed-by"] = "gryvia-tenant-controller"
	return r.Update(ctx, ns)
}

func (r *GryviaTenantReconciler) ensureResourceQuota(ctx context.Context, tenant *gryviav1.GryviaTenant, nsName string) error {
	quotaName := fmt.Sprintf("%s-quota", tenant.Name)

	rq := &corev1.ResourceQuota{}
	err := r.Get(ctx, types.NamespacedName{Name: quotaName, Namespace: nsName}, rq)
	if err != nil && !errors.IsNotFound(err) {
		return err
	}

	hard := corev1.ResourceList{}

	if tenant.Spec.Quotas != nil {
		if tenant.Spec.Quotas.ConcurrentGPUs > 0 {
			hard["requests.nvidia.com/gpu"] = resource.MustParse(fmt.Sprintf("%d", tenant.Spec.Quotas.ConcurrentGPUs))
		}
	}

	if errors.IsNotFound(err) {
		rq = &corev1.ResourceQuota{
			ObjectMeta: metav1.ObjectMeta{
				Name:      quotaName,
				Namespace: nsName,
				Labels: map[string]string{
					"gryvia.io/tenant":     tenant.Name,
					"gryvia.io/managed-by": "gryvia-tenant-controller",
				},
			},
			Spec: corev1.ResourceQuotaSpec{
				Hard: hard,
			},
		}
		return r.Create(ctx, rq)
	}

	rq.Spec.Hard = hard
	return r.Update(ctx, rq)
}

func (r *GryviaTenantReconciler) ensureLimitRange(ctx context.Context, tenant *gryviav1.GryviaTenant, nsName string) error {
	lrName := fmt.Sprintf("%s-limits", tenant.Name)

	lr := &corev1.LimitRange{}
	err := r.Get(ctx, types.NamespacedName{Name: lrName, Namespace: nsName}, lr)
	if err != nil && !errors.IsNotFound(err) {
		return err
	}

	limits := []corev1.LimitRangeItem{
		{
			Type: corev1.LimitTypeContainer,
			Default: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			},
			DefaultRequest: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse("2Gi"),
			},
		},
	}

	if errors.IsNotFound(err) {
		lr = &corev1.LimitRange{
			ObjectMeta: metav1.ObjectMeta{
				Name:      lrName,
				Namespace: nsName,
				Labels: map[string]string{
					"gryvia.io/tenant":     tenant.Name,
					"gryvia.io/managed-by": "gryvia-tenant-controller",
				},
			},
			Spec: corev1.LimitRangeSpec{
				Limits: limits,
			},
		}
		return r.Create(ctx, lr)
	}

	lr.Spec.Limits = limits
	return r.Update(ctx, lr)
}

func (r *GryviaTenantReconciler) ensureNetworkPolicy(ctx context.Context, tenant *gryviav1.GryviaTenant, nsName string) error {
	npName := fmt.Sprintf("%s-isolation", tenant.Name)

	np := &networkingv1.NetworkPolicy{}
	err := r.Get(ctx, types.NamespacedName{Name: npName, Namespace: nsName}, np)
	if err != nil && !errors.IsNotFound(err) {
		return err
	}

	// Build ingress rules allowing traffic from self and allowed tenants
	ingressPeers := []networkingv1.NetworkPolicyPeer{
		{
			// Allow traffic from same namespace
			PodSelector: &metav1.LabelSelector{},
		},
	}

	if tenant.Spec.NetworkPolicy != nil {
		for _, allowedTenant := range tenant.Spec.NetworkPolicy.AllowedTenants {
			ingressPeers = append(ingressPeers, networkingv1.NetworkPolicyPeer{
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"gryvia.io/tenant": allowedTenant,
					},
				},
			})
		}
	}

	spec := networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{},
		PolicyTypes: []networkingv1.PolicyType{
			networkingv1.PolicyTypeIngress,
			networkingv1.PolicyTypeEgress,
		},
		Ingress: []networkingv1.NetworkPolicyIngressRule{
			{From: ingressPeers},
		},
		Egress: []networkingv1.NetworkPolicyEgressRule{
			{}, // Allow all egress by default
		},
	}

	if errors.IsNotFound(err) {
		np = &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name:      npName,
				Namespace: nsName,
				Labels: map[string]string{
					"gryvia.io/tenant":     tenant.Name,
					"gryvia.io/managed-by": "gryvia-tenant-controller",
				},
			},
			Spec: spec,
		}
		return r.Create(ctx, np)
	}

	np.Spec = spec
	return r.Update(ctx, np)
}

func (r *GryviaTenantReconciler) calculateUsage(ctx context.Context, tenant *gryviav1.GryviaTenant, nsName string) (*gryviav1.TenantCurrentUsage, error) {
	usage := &gryviav1.TenantCurrentUsage{}

	jobList := &gryviav1.GryviaAIJobList{}
	if err := r.List(ctx, jobList, client.InNamespace(nsName)); err != nil {
		return nil, err
	}

	now := time.Now()
	startOfMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())

	for _, job := range jobList.Items {
		switch job.Status.Phase {
		case "Running":
			usage.RunningJobs++
			eff := job.Spec.EffectiveGPUs(job.Annotations)
			usage.UsedGPUs += int(math.Ceil(eff))

			if job.Status.StartTime != nil {
				effectiveStart := job.Status.StartTime.Time
				if effectiveStart.Before(startOfMonth) {
					effectiveStart = startOfMonth
				}
				hours := time.Since(effectiveStart).Hours()
				gpuHours := hours * eff
				usage.GPUHours += gpuHours
				usage.CostUSD += gpuHours * budget.GetGPURate(job.Spec.GpuType)
			}

		case "Succeeded", "Failed":
			if job.Status.StartTime != nil && job.Status.CompletionTime != nil {
				effectiveStart := job.Status.StartTime.Time
				if effectiveStart.Before(startOfMonth) {
					effectiveStart = startOfMonth
				}
				if job.Status.CompletionTime.Time.After(startOfMonth) {
					hours := job.Status.CompletionTime.Time.Sub(effectiveStart).Hours()
					if hours > 0 {
						eff := job.Spec.EffectiveGPUs(job.Annotations)
						gpuHours := hours * eff
						usage.GPUHours += gpuHours
						usage.CostUSD += gpuHours * budget.GetGPURate(job.Spec.GpuType)
					}
				}
			}
		}
	}

	return usage, nil
}

func (r *GryviaTenantReconciler) calculateUtilization(tenant *gryviav1.GryviaTenant) *gryviav1.TenantQuotaUtilization {
	util := &gryviav1.TenantQuotaUtilization{}

	if tenant.Status.CurrentUsage == nil || tenant.Spec.Quotas == nil {
		return util
	}

	if tenant.Spec.Quotas.GPUHours != nil && tenant.Spec.Quotas.GPUHours.Monthly > 0 {
		util.GPUHoursPercent = (tenant.Status.CurrentUsage.GPUHours / tenant.Spec.Quotas.GPUHours.Monthly) * 100
	}

	if tenant.Spec.Quotas.CostUSD != nil && tenant.Spec.Quotas.CostUSD.Monthly > 0 {
		util.CostPercent = (tenant.Status.CurrentUsage.CostUSD / tenant.Spec.Quotas.CostUSD.Monthly) * 100
	}

	if tenant.Spec.Quotas.ConcurrentJobs > 0 {
		util.JobsPercent = (float64(tenant.Status.CurrentUsage.RunningJobs) / float64(tenant.Spec.Quotas.ConcurrentJobs)) * 100
	}

	return util
}

func (r *GryviaTenantReconciler) determineHealth(tenant *gryviav1.GryviaTenant) string {
	if tenant.Status.QuotaUtilization == nil {
		return "healthy"
	}

	maxUtil := tenant.Status.QuotaUtilization.GPUHoursPercent
	if tenant.Status.QuotaUtilization.CostPercent > maxUtil {
		maxUtil = tenant.Status.QuotaUtilization.CostPercent
	}
	if tenant.Status.QuotaUtilization.JobsPercent > maxUtil {
		maxUtil = tenant.Status.QuotaUtilization.JobsPercent
	}

	if maxUtil >= 100 {
		return "quota-exceeded"
	}
	if maxUtil >= 80 {
		return "warning"
	}
	return "healthy"
}

func (r *GryviaTenantReconciler) handleDeletion(ctx context.Context, tenant *gryviav1.GryviaTenant) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if controllerutil.ContainsFinalizer(tenant, gryviaTenantFinalizer) {
		logger.Info("Running cleanup for GryviaTenant", "tenant", tenant.Name)

		// Clean up managed namespace labels (do not delete the namespace itself)
		nsName := fmt.Sprintf("tenant-%s", tenant.Name)
		ns := &corev1.Namespace{}
		err := r.Get(ctx, types.NamespacedName{Name: nsName}, ns)
		if err == nil {
			if ns.Labels != nil {
				delete(ns.Labels, "gryvia.io/tenant")
				delete(ns.Labels, "gryvia.io/managed-by")
				if updateErr := r.Update(ctx, ns); updateErr != nil {
					logger.Error(updateErr, "Failed to remove labels from namespace", "namespace", nsName)
				}
			}
		}

		// Remove finalizer
		controllerutil.RemoveFinalizer(tenant, gryviaTenantFinalizer)
		if err := r.Update(ctx, tenant); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func (r *GryviaTenantReconciler) updateCondition(tenant *gryviav1.GryviaTenant, condType string, status metav1.ConditionStatus, reason, message string) {
	condition := metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	}
	meta.SetStatusCondition(&tenant.Status.Conditions, condition)
}

// SetupWithManager sets up the controller with the Manager.
func (r *GryviaTenantReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gryviav1.GryviaTenant{}).
		Complete(r)
}
