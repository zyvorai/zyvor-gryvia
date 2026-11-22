// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gryviav1 "github.com/zyvorai/gryvia/operators/network-intelligence/api/v1"
	"github.com/zyvorai/gryvia/operators/network-intelligence/pkg/sources"
)

// GryviaFlowPolicyReconciler reconciles a GryviaFlowPolicy object
//
// A policy labelled gryvia.io/suggested=true is a SUGGESTION (written by the GryviaAutoPolicy controller):
// it is never translated to a CiliumNetworkPolicy until the label is removed (gryvia policy apply). A Service
// endpoint without labels is resolved to the Service's pod selector; when it cannot be resolved the policy
// is reported Failed instead of becoming a selector-less (namespace-wide) policy. matchedFlows is the count
// of Netra flow records of the source pods in the last 15 minutes and is left unset when Netra is not configured.
type GryviaFlowPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Netra is optional; nil or unconfigured leaves matchedFlows unset.
	Netra sources.FlowHistory
}

const (
	// LabelSuggested marks a GryviaFlowPolicy that is only a suggestion.
	LabelSuggested = "gryvia.io/suggested"
	// matchedFlowsWindow is the Netra history window behind status.matchedFlows.
	matchedFlowsWindow = 15 * time.Minute
	matchedFlowsLimit  = 5000
	namespaceLabel     = "k8s:io.kubernetes.pod.namespace"
)

//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaflowpolicies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaflowpolicies/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaflowpolicies/finalizers,verbs=update
//+kubebuilder:rbac:groups=cilium.io,resources=ciliumnetworkpolicies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *GryviaFlowPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the GryviaFlowPolicy instance
	policy := &gryviav1.GryviaFlowPolicy{}
	if err := r.Get(ctx, req.NamespacedName, policy); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("GryviaFlowPolicy resource not found, ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get GryviaFlowPolicy")
		return ctrl.Result{}, err
	}

	logger.Info("Reconciling GryviaFlowPolicy",
		"name", policy.Name,
		"action", policy.Spec.Action,
		"intent", policy.Spec.Intent,
	)

	if policy.Labels[LabelSuggested] == "true" {
		_ = r.setStatus(ctx, req.NamespacedName, func(p *gryviav1.GryviaFlowPolicy) {
			p.Status.Phase = "Suggested"
			p.Status.Enforced = false
			setCondition(&p.Status.Conditions, p.Generation, "Enforceable", metav1.ConditionFalse, "Suggested",
				"this policy is a suggestion; no CiliumNetworkPolicy exists until the "+LabelSuggested+" label is removed (gryvia network policy apply)")
		})
		return ctrl.Result{}, nil
	}

	srcLabels, srcErr := r.resolveLabels(ctx, policy.Spec.Source, policy.Namespace)
	dstLabels, dstErr := r.resolveLabels(ctx, policy.Spec.Destination, policy.Namespace)
	if err := firstErr(srcErr, dstErr); err != nil {
		_ = r.setStatus(ctx, req.NamespacedName, func(p *gryviav1.GryviaFlowPolicy) {
			p.Status.Phase = "Failed"
			p.Status.Enforced = false
			setCondition(&p.Status.Conditions, p.Generation, "Enforceable", metav1.ConditionFalse, "UnresolvedEndpoint", err.Error())
		})
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
	}

	// Translate GryviaFlowPolicy to CiliumNetworkPolicy
	ciliumPolicy, err := r.buildCiliumNetworkPolicy(policy, srcLabels, dstLabels)
	if err != nil {
		_ = r.setStatus(ctx, req.NamespacedName, func(p *gryviav1.GryviaFlowPolicy) { p.Status.Phase = "Failed" })
		return ctrl.Result{}, fmt.Errorf("failed to build CiliumNetworkPolicy: %w", err)
	}
	if err := controllerutil.SetControllerReference(policy, ciliumPolicy, r.Scheme); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to set owner reference: %w", err)
	}

	ciliumPolicyName := ciliumPolicy.GetName()
	existingPolicy := &unstructured.Unstructured{}
	existingPolicy.SetGroupVersionKind(schema.GroupVersionKind{Group: "cilium.io", Version: "v2", Kind: "CiliumNetworkPolicy"})

	err = r.Get(ctx, types.NamespacedName{Name: ciliumPolicyName, Namespace: policy.Namespace}, existingPolicy)
	switch {
	case errors.IsNotFound(err):
		logger.Info("Creating CiliumNetworkPolicy", "name", ciliumPolicyName)
		if createErr := r.Create(ctx, ciliumPolicy); createErr != nil {
			_ = r.setStatus(ctx, req.NamespacedName, func(p *gryviav1.GryviaFlowPolicy) {
				p.Status.Phase, p.Status.CiliumPolicyRef, p.Status.Enforced = "Failed", ciliumPolicyName, false
			})
			return ctrl.Result{}, fmt.Errorf("failed to create CiliumNetworkPolicy: %w", createErr)
		}
	case err != nil:
		return ctrl.Result{}, fmt.Errorf("failed to check existing CiliumNetworkPolicy: %w", err)
	default:
		logger.Info("Updating CiliumNetworkPolicy", "name", ciliumPolicyName)
		ciliumPolicy.SetResourceVersion(existingPolicy.GetResourceVersion())
		if updateErr := r.Update(ctx, ciliumPolicy); updateErr != nil {
			_ = r.setStatus(ctx, req.NamespacedName, func(p *gryviav1.GryviaFlowPolicy) {
				p.Status.Phase, p.Status.CiliumPolicyRef, p.Status.Enforced = "Failed", ciliumPolicyName, false
			})
			return ctrl.Result{}, fmt.Errorf("failed to update CiliumNetworkPolicy: %w", updateErr)
		}
	}

	matched, mset, merr := r.matchedFlows(ctx, policy, srcLabels)

	uerr := r.setStatus(ctx, req.NamespacedName, func(p *gryviav1.GryviaFlowPolicy) {
		p.Status.Phase = "Enforced"
		p.Status.CiliumPolicyRef = ciliumPolicyName
		p.Status.Enforced = true
		p.Status.LastApplied = metav1.Now()
		setCondition(&p.Status.Conditions, p.Generation, "Enforceable", metav1.ConditionTrue, "Applied", "CiliumNetworkPolicy "+ciliumPolicyName+" applied")
		if mset {
			p.Status.MatchedFlows = matched
		} else {
			p.Status.MatchedFlows = 0
		}
		setSource(&p.Status.Conditions, p.Generation, merr, sources.Stats{},
			"matchedFlows = Netra flow records of the source pods (to the port, if set) in the last 15 minutes")
	})
	if uerr != nil && !errors.IsNotFound(uerr) {
		return ctrl.Result{}, uerr
	}
	logger.Info("GryviaFlowPolicy reconciled", "ciliumPolicy", ciliumPolicyName, "matchedFlows", matched, "matchedFlowsKnown", mset)
	return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
}

func (r *GryviaFlowPolicyReconciler) setStatus(ctx context.Context, key types.NamespacedName, mutate func(*gryviav1.GryviaFlowPolicy)) error {
	return updateStatus(ctx, r.Client, key, func() *gryviav1.GryviaFlowPolicy { return &gryviav1.GryviaFlowPolicy{} }, mutate)
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// resolveLabels returns the pod labels selecting an endpoint: its own labels, else the selector of its
// Service (in the endpoint's namespace, default the policy's). An endpoint with neither is nil (all pods).
// Cross-namespace endpoints also select the namespace.
func (r *GryviaFlowPolicyReconciler) resolveLabels(ctx context.Context, ep *gryviav1.FlowEndpoint, policyNS string) (map[string]string, error) {
	if ep == nil {
		return nil, nil
	}
	labels := map[string]string{}
	for k, v := range ep.Labels {
		labels[k] = v
	}
	ns := ep.Namespace
	if ns == "" {
		ns = policyNS
	}
	if len(labels) == 0 && ep.Service != "" {
		svc := &corev1.Service{}
		if err := r.Get(ctx, types.NamespacedName{Name: ep.Service, Namespace: ns}, svc); err != nil {
			return nil, fmt.Errorf("service %s/%s cannot be resolved: %w", ns, ep.Service, err)
		}
		if len(svc.Spec.Selector) == 0 {
			return nil, fmt.Errorf("service %s/%s has no pod selector; set labels on the endpoint", ns, ep.Service)
		}
		for k, v := range svc.Spec.Selector {
			labels[k] = v
		}
	}
	if len(labels) == 0 {
		return nil, nil
	}
	if ns != policyNS {
		labels[namespaceLabel] = ns
	}
	return labels, nil
}

// matchedFlows counts the Netra records of the source pods in the last 15 minutes. known=false (with an
// error explaining why) when Netra is not configured/reachable or the source pods cannot be identified.
func (r *GryviaFlowPolicyReconciler) matchedFlows(ctx context.Context, policy *gryviav1.GryviaFlowPolicy, srcLabels map[string]string) (int64, bool, error) {
	if r.Netra == nil || !r.Netra.Configured() {
		return 0, false, &sources.SourceError{Source: "netra", Reason: sources.ReasonNotConfigured,
			Message: "matchedFlows needs Netra (set GRYVIA_NETRA_URL on the operator); it is left unset"}
	}
	if len(srcLabels) == 0 {
		return 0, false, &sources.SourceError{Source: "netra", Reason: sources.ReasonNoData,
			Message: "the policy has no source labels or Service, so its flows cannot be identified; matchedFlows is left unset"}
	}
	srcNS := policy.Namespace
	if policy.Spec.Source != nil && policy.Spec.Source.Namespace != "" {
		srcNS = policy.Spec.Source.Namespace
	}
	pods, err := r.podNames(ctx, srcNS, srcLabels)
	if err != nil {
		return 0, false, err
	}
	recs, err := r.Netra.History(ctx, matchedFlowsWindow, matchedFlowsLimit)
	if err != nil {
		return 0, false, err
	}
	var n int64
	for _, rec := range recs {
		if rec.Namespace != srcNS || !pods[rec.Pod] {
			continue
		}
		if policy.Spec.Destination != nil && policy.Spec.Destination.Port > 0 && rec.Port != policy.Spec.Destination.Port {
			continue
		}
		if p := strings.ToLower(policy.Spec.Protocol); p != "" && p != "any" && rec.Protocol != "" && !strings.EqualFold(rec.Protocol, p) {
			continue
		}
		n++
	}
	return n, true, nil
}

func (r *GryviaFlowPolicyReconciler) podNames(ctx context.Context, ns string, labels map[string]string) (map[string]bool, error) {
	sel := map[string]string{}
	for k, v := range labels {
		if k != namespaceLabel {
			sel[k] = v
		}
	}
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(ns), client.MatchingLabels(sel)); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, p := range pods.Items {
		out[p.Name] = true
	}
	return out, nil
}

// buildCiliumNetworkPolicy translates a GryviaFlowPolicy into an unstructured CiliumNetworkPolicy
func (r *GryviaFlowPolicyReconciler) buildCiliumNetworkPolicy(policy *gryviav1.GryviaFlowPolicy, srcLabels, dstLabels map[string]string) (*unstructured.Unstructured, error) {
	ciliumPolicyName := fmt.Sprintf("ffp-%s", policy.Name)

	// Build endpoint selector from source labels
	endpointSelector := map[string]interface{}{}
	if len(srcLabels) > 0 {
		matchLabels := make(map[string]interface{})
		for k, v := range srcLabels {
			matchLabels[k] = v
		}
		endpointSelector["matchLabels"] = matchLabels
	}

	// Build ingress/egress rules based on action
	spec := map[string]interface{}{
		"endpointSelector": endpointSelector,
	}

	// Build port rules
	var toPorts []interface{}
	if policy.Spec.Destination != nil && policy.Spec.Destination.Port > 0 {
		portRule := map[string]interface{}{
			"ports": []interface{}{
				map[string]interface{}{
					"port":     fmt.Sprintf("%d", policy.Spec.Destination.Port),
					"protocol": mapProtocol(policy.Spec.Protocol),
				},
			},
		}
		toPorts = append(toPorts, portRule)
	}

	// Build destination selector
	destSelector := map[string]interface{}{}
	if len(dstLabels) > 0 {
		matchLabels := make(map[string]interface{})
		for k, v := range dstLabels {
			matchLabels[k] = v
		}
		destSelector["matchLabels"] = matchLabels
	}

	switch policy.Spec.Action {
	case "allow":
		egressRule := map[string]interface{}{}
		if len(destSelector) > 0 {
			egressRule["toEndpoints"] = []interface{}{destSelector}
		}
		if len(toPorts) > 0 {
			egressRule["toPorts"] = toPorts
		}
		spec["egress"] = []interface{}{egressRule}
	case "deny":
		egressDenyRule := map[string]interface{}{}
		if len(destSelector) > 0 {
			egressDenyRule["toEndpoints"] = []interface{}{destSelector}
		}
		if len(toPorts) > 0 {
			egressDenyRule["toPorts"] = toPorts
		}
		spec["egressDeny"] = []interface{}{egressDenyRule}
	case "log":
		// For log action, create an allow rule with visibility annotations
		egressRule := map[string]interface{}{}
		if len(destSelector) > 0 {
			egressRule["toEndpoints"] = []interface{}{destSelector}
		}
		if len(toPorts) > 0 {
			egressRule["toPorts"] = toPorts
		}
		spec["egress"] = []interface{}{egressRule}
	}

	// Build annotations based on intent
	annotations := map[string]interface{}{
		"gryvia.io/managed-by": "netpredator",
		"gryvia.io/intent":     policy.Spec.Intent,
	}
	annotations = r.applyIntentAnnotations(annotations, policy.Spec.Intent)

	ciliumObj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "cilium.io/v2",
			"kind":       "CiliumNetworkPolicy",
			"metadata": map[string]interface{}{
				"name":        ciliumPolicyName,
				"namespace":   policy.Namespace,
				"annotations": annotations,
			},
			"spec": spec,
		},
	}

	return ciliumObj, nil
}

// applyIntentAnnotations maps intent types to Cilium-specific annotations
func (r *GryviaFlowPolicyReconciler) applyIntentAnnotations(annotations map[string]interface{}, intent string) map[string]interface{} {
	switch intent {
	case "low-latency":
		annotations["cilium.io/priority"] = "high"
		annotations["gryvia.io/qos-class"] = "low-latency"
		annotations["gryvia.io/dscp"] = "46" // EF (Expedited Forwarding)
	case "high-throughput":
		annotations["cilium.io/priority"] = "normal"
		annotations["gryvia.io/qos-class"] = "high-throughput"
		annotations["gryvia.io/dscp"] = "34" // AF41
	case "secure":
		annotations["cilium.io/priority"] = "high"
		annotations["gryvia.io/qos-class"] = "secure"
		annotations["gryvia.io/encryption"] = "wireguard"
	default:
		annotations["gryvia.io/qos-class"] = "default"
	}
	return annotations
}

// mapProtocol converts the policy protocol to Cilium protocol format
func mapProtocol(protocol string) string {
	switch protocol {
	case "tcp":
		return "TCP"
	case "udp":
		return "UDP"
	case "icmp":
		return "ICMP"
	default:
		return "TCP"
	}
}

// SetupWithManager sets up the controller with the Manager
func (r *GryviaFlowPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gryviav1.GryviaFlowPolicy{}).
		Complete(r)
}
