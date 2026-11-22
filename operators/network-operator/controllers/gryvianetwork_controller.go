// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gryviav1 "github.com/zyvorai/gryvia/operators/network-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/network-operator/pkg/multus"
	"github.com/zyvorai/gryvia/operators/network-operator/pkg/rdma"
	"github.com/zyvorai/gryvia/operators/network-operator/pkg/sriov"
)

const (
	gryviaNetworkFinalizer = "gryvia.io/network-finalizer"
)

// GryviaNetworkReconciler reconciles a GryviaNetwork object
type GryviaNetworkReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=gryvia.io,resources=gryvianetworks,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gryvia.io,resources=gryvianetworks/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryvianetworks/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=k8s.cni.cncf.io,resources=network-attachment-definitions,verbs=get;list;watch;create;update;patch;delete

func (r *GryviaNetworkReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the GryviaNetwork instance
	network := &gryviav1.GryviaNetwork{}
	err := r.Get(ctx, req.NamespacedName, network)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("GryviaNetwork resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get GryviaNetwork")
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !network.ObjectMeta.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, network)
	}

	// Add finalizer if it doesn't exist
	if !controllerutil.ContainsFinalizer(network, gryviaNetworkFinalizer) {
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := r.Get(ctx, req.NamespacedName, network); err != nil {
				return err
			}
			if controllerutil.ContainsFinalizer(network, gryviaNetworkFinalizer) {
				return nil
			}
			controllerutil.AddFinalizer(network, gryviaNetworkFinalizer)
			return r.Update(ctx, network)
		}); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	logger.Info("Reconciling GryviaNetwork", "name", network.Name, "type", network.Spec.NetworkType)

	// Reconcile the network configuration
	result, err := r.reconcileNetwork(ctx, network)
	if err != nil {
		logger.Error(err, "Failed to reconcile network")
		r.updateStatus(ctx, network, "Failed", err.Error())
		return result, err
	}

	// Update status to Ready
	r.updateStatus(ctx, network, "Ready", "Network configured successfully")

	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

func (r *GryviaNetworkReconciler) reconcileNetwork(ctx context.Context, network *gryviav1.GryviaNetwork) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Get matching nodes
	nodes, err := r.getMatchingNodes(ctx, network)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get matching nodes: %w", err)
	}

	if len(nodes) == 0 {
		logger.Info("No matching nodes found", "nodeSelector", network.Spec.NodeSelector)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	logger.Info("Found matching nodes", "count", len(nodes))

	// Update total nodes in status
	network.Status.TotalNodes = len(nodes)

	// Configure based on network type
	switch network.Spec.NetworkType {
	case "rdma":
		if err := r.configureRDMA(ctx, network, nodes); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to configure RDMA: %w", err)
		}
	case "sriov":
		if err := r.configureSRIOV(ctx, network, nodes); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to configure SR-IOV: %w", err)
		}
	case "standard":
		logger.Info("Standard network type - no special configuration needed")
	default:
		return ctrl.Result{}, fmt.Errorf("unknown network type: %s", network.Spec.NetworkType)
	}

	// Ensure Multus NetworkAttachmentDefinition
	if err := r.ensureNetworkAttachment(ctx, network); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to create network attachment: %w", err)
	}

	network.Status.ConfiguredNodes = len(nodes)

	return ctrl.Result{}, nil
}

func (r *GryviaNetworkReconciler) getMatchingNodes(ctx context.Context, network *gryviav1.GryviaNetwork) ([]corev1.Node, error) {
	nodeList := &corev1.NodeList{}

	listOpts := []client.ListOption{}
	if len(network.Spec.NodeSelector) > 0 {
		listOpts = append(listOpts, client.MatchingLabels(network.Spec.NodeSelector))
	}

	if err := r.List(ctx, nodeList, listOpts...); err != nil {
		return nil, err
	}

	return nodeList.Items, nil
}

func (r *GryviaNetworkReconciler) configureRDMA(ctx context.Context, network *gryviav1.GryviaNetwork, nodes []corev1.Node) error {
	logger := log.FromContext(ctx)

	if network.Spec.RDMA == nil {
		return fmt.Errorf("RDMA configuration is required for rdma network type")
	}

	logger.Info("Configuring RDMA network", "mode", network.Spec.RDMA.Mode)

	// Install RDMA device plugin
	if err := rdma.InstallDevicePlugin(ctx, r.Client, network, r.Scheme); err != nil {
		return fmt.Errorf("failed to install RDMA device plugin: %w", err)
	}

	// Configure RDMA on each node, tracking failures
	var failedNodeNames []string
	for _, node := range nodes {
		if err := rdma.ConfigureNode(ctx, r.Client, &node, network.Spec.RDMA); err != nil {
			logger.Error(err, "Failed to configure RDMA on node", "node", node.Name)
			failedNodeNames = append(failedNodeNames, node.Name)
			continue
		}
		logger.Info("Configured RDMA on node", "node", node.Name)
	}

	if len(failedNodeNames) > 0 {
		return fmt.Errorf("failed to configure RDMA on %d/%d nodes: %v", len(failedNodeNames), len(nodes), failedNodeNames)
	}

	return nil
}

func (r *GryviaNetworkReconciler) configureSRIOV(ctx context.Context, network *gryviav1.GryviaNetwork, nodes []corev1.Node) error {
	logger := log.FromContext(ctx)

	if network.Spec.SRIOV == nil {
		return fmt.Errorf("SR-IOV configuration is required for sriov network type")
	}

	logger.Info("Configuring SR-IOV network", "interface", network.Spec.SRIOV.PhysicalInterface)

	// Install SR-IOV device plugin
	if err := sriov.InstallDevicePlugin(ctx, r.Client, network.Name, network.Spec.SRIOV); err != nil {
		return fmt.Errorf("failed to install SR-IOV device plugin: %w", err)
	}

	// Configure SR-IOV on each node, tracking failures
	var failedNodeNames []string
	for _, node := range nodes {
		if err := sriov.ConfigureNode(ctx, r.Client, &node, network.Spec.SRIOV); err != nil {
			logger.Error(err, "Failed to configure SR-IOV on node", "node", node.Name)
			failedNodeNames = append(failedNodeNames, node.Name)
			continue
		}
		logger.Info("Configured SR-IOV on node", "node", node.Name)
	}

	if len(failedNodeNames) > 0 {
		return fmt.Errorf("failed to configure SR-IOV on %d/%d nodes: %v", len(failedNodeNames), len(nodes), failedNodeNames)
	}

	return nil
}

func (r *GryviaNetworkReconciler) ensureNetworkAttachment(ctx context.Context, network *gryviav1.GryviaNetwork) error {
	logger := log.FromContext(ctx)

	logger.Info("Creating NetworkAttachmentDefinition", "name", network.Name)

	if err := multus.CreateNetworkAttachment(ctx, r.Client, network); err != nil {
		return err
	}

	return nil
}

func (r *GryviaNetworkReconciler) handleDeletion(ctx context.Context, network *gryviav1.GryviaNetwork) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if controllerutil.ContainsFinalizer(network, gryviaNetworkFinalizer) {
		logger.Info("Running cleanup for GryviaNetwork", "name", network.Name)

		// Explicitly clean up DaemonSets and ConfigMaps since cross-namespace owner
		// references don't work (resources are in kube-system, owner is cluster-scoped).
		// Resource names are scoped by the GryviaNetwork name to avoid conflicts
		// when multiple GryviaNetworks exist.
		daemonSetsToClean := []string{
			rdma.RDMADevicePluginDaemonSetName(network.Name),
			sriov.SRIOVDevicePluginDaemonSetName(network.Name),
			sriov.SRIOVCNIDaemonSetName(network.Name),
		}
		for _, name := range daemonSetsToClean {
			ds := &appsv1.DaemonSet{}
			if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: "kube-system"}, ds); err == nil {
				if delErr := r.Delete(ctx, ds); delErr != nil {
					logger.Error(delErr, "Failed to delete DaemonSet during cleanup", "daemonset", name)
				} else {
					logger.Info("Deleted DaemonSet during cleanup", "daemonset", name)
				}
			}
		}
		configMapsToClean := []string{
			rdma.RDMAConfigMapName(network.Name),
			sriov.SRIOVConfigMapName(network.Name),
		}
		for _, name := range configMapsToClean {
			cm := &corev1.ConfigMap{}
			if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: "kube-system"}, cm); err == nil {
				if delErr := r.Delete(ctx, cm); delErr != nil {
					logger.Error(delErr, "Failed to delete ConfigMap during cleanup", "configmap", name)
				}
			}
		}

		// Remove node labels applied by this network.
		nodes, err := r.getMatchingNodes(ctx, network)
		if err != nil {
			logger.Error(err, "Failed to list nodes during cleanup")
		} else {
			for _, node := range nodes {
				// Clean up RDMA labels
				delete(node.Labels, "gryvia.io/rdma")
				delete(node.Labels, "gryvia.io/rdma-mode")
				// Clean up SR-IOV labels: exact match for the base key plus
				// prefix match with trailing "-" for dynamic keys like
				// "gryvia.io/sriov-<resourceName>".
				delete(node.Labels, "gryvia.io/sriov")
				for k := range node.Labels {
					if strings.HasPrefix(k, "gryvia.io/sriov-") {
						delete(node.Labels, k)
					}
				}
				// Clean up annotations (nil-safe)
				if node.Annotations != nil {
					delete(node.Annotations, "gryvia.io/rdma-devices")
					delete(node.Annotations, "gryvia.io/sriov-interface")
					delete(node.Annotations, "gryvia.io/sriov-numvfs")
				}
				if updateErr := r.Update(ctx, &node); updateErr != nil {
					logger.Error(updateErr, "Failed to remove labels from node", "node", node.Name)
				}
			}
		}

		// Remove finalizer
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := r.Get(ctx, types.NamespacedName{Name: network.Name}, network); err != nil {
				return err
			}
			if !controllerutil.ContainsFinalizer(network, gryviaNetworkFinalizer) {
				return nil
			}
			controllerutil.RemoveFinalizer(network, gryviaNetworkFinalizer)
			return r.Update(ctx, network)
		}); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

func (r *GryviaNetworkReconciler) updateStatus(ctx context.Context, network *gryviav1.GryviaNetwork, phase, message string) {
	network.Status.Phase = phase
	network.Status.LastUpdated = metav1.Now()

	condition := metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             phase,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	}

	if phase == "Failed" {
		condition.Status = metav1.ConditionFalse
	}

	meta.SetStatusCondition(&network.Status.Conditions, condition)

	if err := r.Status().Update(ctx, network); err != nil {
		log.FromContext(ctx).Error(err, "Failed to update GryviaNetwork status")
	}
}

// SetupWithManager sets up the controller with the Manager
func (r *GryviaNetworkReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// NOTE: Owns() for DaemonSet and ConfigMap is not used here because those
	// resources live in kube-system while GryviaNetwork is cluster-scoped.
	// Cross-namespace owner references cannot be set, so cleanup is handled
	// via the finalizer in handleDeletion instead.
	return ctrl.NewControllerManagedBy(mgr).
		For(&gryviav1.GryviaNetwork{}).
		Complete(r)
}
