// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"fmt"
	appsv1 "k8s.io/api/apps/v1"
	"time"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	AnnotationGatewayParent   = "gryvia.io/gateway-parent"
	AnnotationGatewaySection  = "gryvia.io/gateway-section"
	AnnotationGatewayHostname = "gryvia.io/gateway-hostname"
	ConditionGatewayRouting   = "GatewayRouting"
)

var inferenceRouteGVK = schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "HTTPRoute"}

func routingRequested(svc *gryviav1.GryviaInferenceService) bool {
	return svc.Annotations[AnnotationGatewayParent] != ""
}
func inferRouteName(svc *gryviav1.GryviaInferenceService) string { return childName(svc.Name, "route") }
func inferTrackServiceName(svc *gryviav1.GryviaInferenceService, track string) string {
	return childName(svc.Name, "route", track)
}
func newInferenceRoute() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(inferenceRouteGVK)
	return u
}

// Parent Gateways are restricted to the service's namespace; cross-namespace
// routing requires a separately reviewed policy, not an arbitrary annotation.
func validateRoutingOptions(svc *gryviav1.GryviaInferenceService) error {
	a := svc.Annotations
	if !routingRequested(svc) {
		if a[AnnotationGatewaySection] != "" || a[AnnotationGatewayHostname] != "" {
			return fmt.Errorf("gateway section/hostname requires gateway-parent")
		}
		return nil
	}
	if len(validation.IsDNS1123Subdomain(a[AnnotationGatewayParent])) != 0 {
		return fmt.Errorf("gateway-parent must be a Gateway name in this namespace")
	}
	if v := a[AnnotationGatewaySection]; v != "" && len(validation.IsDNS1123Label(v)) != 0 {
		return fmt.Errorf("gateway-section must be a valid listener name")
	}
	if v := a[AnnotationGatewayHostname]; v != "" && len(validation.IsDNS1123Subdomain(v)) != 0 && len(validation.IsWildcardDNS1123Subdomain(v)) != 0 {
		return fmt.Errorf("gateway-hostname must be a valid DNS hostname")
	}
	if svc.Spec.Canary != nil && svc.Spec.Canary.Enabled && (svc.Spec.Canary.Weight < 0 || svc.Spec.Canary.Weight > 100) {
		return fmt.Errorf("canary weight must be 0-100")
	}
	return nil
}

func routeWeight(svc *gryviav1.GryviaInferenceService) int32 {
	cs := svc.Status.CanaryStatus
	if svc.Spec.Canary == nil || !svc.Spec.Canary.Enabled || cs == nil || !cs.Active || cs.ReadyReplicas == 0 {
		return 0
	}
	if cs.Health != canaryHealthHealthy {
		if cfg, _ := analysisConfig(svc); cfg == nil {
			return 0
		}
	}
	return svc.Spec.Canary.Weight
}

func (r *GryviaInferenceServiceReconciler) routeSpec(svc *gryviav1.GryviaInferenceService) map[string]interface{} {
	parent := map[string]interface{}{"group": "gateway.networking.k8s.io", "kind": "Gateway", "name": svc.Annotations[AnnotationGatewayParent], "namespace": svc.Namespace}
	if section := svc.Annotations[AnnotationGatewaySection]; section != "" {
		parent["sectionName"] = section
	}
	w := routeWeight(svc)
	refs := []interface{}{map[string]interface{}{"group": "", "kind": "Service", "name": inferTrackServiceName(svc, trackStable), "port": int64(r.port(svc)), "weight": int64(100 - w)}}
	if w > 0 {
		refs = append(refs, map[string]interface{}{"group": "", "kind": "Service", "name": inferTrackServiceName(svc, trackCanary), "port": int64(r.port(svc)), "weight": int64(w)})
	}
	spec := map[string]interface{}{"parentRefs": []interface{}{parent}, "rules": []interface{}{map[string]interface{}{"matches": []interface{}{map[string]interface{}{"path": map[string]interface{}{"type": "PathPrefix", "value": "/"}}}, "backendRefs": refs}}}
	if host := svc.Annotations[AnnotationGatewayHostname]; host != "" {
		spec["hostnames"] = []interface{}{host}
	}
	return spec
}

func (r *GryviaInferenceServiceReconciler) ensureTrackService(ctx context.Context, svc *gryviav1.GryviaInferenceService, track string) error {
	name := inferTrackServiceName(svc, track)
	live := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: name}, live)
	wanted := corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Selector: map[string]string{"gryvia.io/inference": svc.Name, "gryvia.io/component": "inference-server", labelTrack: track}, Ports: []corev1.ServicePort{{Name: "http", Port: r.port(svc), TargetPort: intstr.FromString("http"), Protocol: corev1.ProtocolTCP}}}
	if errors.IsNotFound(err) {
		live = &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: svc.Namespace}, Spec: wanted}
		if err := controllerutil.SetControllerReference(svc, live, r.Scheme); err != nil {
			return err
		}
		return r.Create(ctx, live)
	}
	if err != nil {
		return err
	}
	if !metav1.IsControlledBy(live, svc) {
		return fmt.Errorf("routing Service %s is not owned by this inference service", name)
	}
	base := live.DeepCopy()
	live.Spec.Selector = wanted.Selector
	live.Spec.Ports = wanted.Ports
	return r.Patch(ctx, live, client.MergeFrom(base))
}

func (r *GryviaInferenceServiceReconciler) reconcileRouting(ctx context.Context, svc *gryviav1.GryviaInferenceService) error {
	if !r.GatewayRouting {
		if routingRequested(svc) {
			setCondition(&svc.Status.Conditions, svc.Generation, ConditionGatewayRouting, metav1.ConditionFalse, "Disabled", "Gateway routing requires --inference-gateway-routing")
		}
		return nil
	}
	if !routingRequested(svc) && meta.FindStatusCondition(svc.Status.Conditions, ConditionGatewayRouting) == nil {
		return nil
	}
	live := newInferenceRoute()
	key := types.NamespacedName{Namespace: svc.Namespace, Name: inferRouteName(svc)}
	err := r.Get(ctx, key, live)
	if meta.IsNoMatchError(err) {
		if routingRequested(svc) {
			setCondition(&svc.Status.Conditions, svc.Generation, ConditionGatewayRouting, metav1.ConditionFalse, "APIMissing", "Install Gateway API v1 CRDs and a Gateway controller")
		}
		return nil
	}
	if err != nil && !errors.IsNotFound(err) {
		return err
	}
	exists := err == nil
	if exists && !metav1.IsControlledBy(live, svc) {
		setCondition(&svc.Status.Conditions, svc.Generation, ConditionGatewayRouting, metav1.ConditionFalse, "NameCollision", "HTTPRoute name is owned by another resource; it was left untouched")
		return nil
	}
	if !routingRequested(svc) {
		// Remove the route first. Track Services remain until a later pass observes
		// it gone, so an asynchronously deleting route never references missing backends.
		if exists {
			return r.Delete(ctx, live)
		}
		for _, track := range []string{trackStable, trackCanary} {
			backend := &corev1.Service{}
			e := r.Get(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: inferTrackServiceName(svc, track)}, backend)
			if e != nil && !errors.IsNotFound(e) {
				return e
			}
			if e == nil && metav1.IsControlledBy(backend, svc) {
				if e = r.Delete(ctx, backend); e != nil && !errors.IsNotFound(e) {
					return e
				}
			}
		}
		removeCondition(&svc.Status.Conditions, ConditionGatewayRouting)
		return nil
	}
	for _, track := range []string{trackStable, trackCanary} {
		if err := r.ensureTrackService(ctx, svc, track); err != nil {
			return err
		}
	}
	desired := r.routeSpec(svc)
	if !exists {
		live.SetName(key.Name)
		live.SetNamespace(key.Namespace)
		live.Object["spec"] = desired
		if err := controllerutil.SetControllerReference(svc, live, r.Scheme); err != nil {
			return err
		}
		if err := r.Create(ctx, live); err != nil {
			return err
		}
	} else if specHash(live.Object["spec"]) != specHash(desired) {
		base := live.DeepCopy()
		live.Object["spec"] = desired
		if err := r.Patch(ctx, live, client.MergeFrom(base)); err != nil {
			return err
		}
		// Gateway conditions still describe the previous spec, even if a fake client
		// does not increment generation. Never report the new weights ready yet.
		setCondition(&svc.Status.Conditions, svc.Generation, ConditionGatewayRouting, metav1.ConditionUnknown, "Reconciling", "Waiting for the Gateway controller to accept updated route weights")
		return nil
	}
	ready, msg := routeAccepted(live, svc)
	status := metav1.ConditionFalse
	reason := "NotAccepted"
	if ready {
		status, reason = metav1.ConditionTrue, "Accepted"
	}
	setCondition(&svc.Status.Conditions, svc.Generation, ConditionGatewayRouting, status, reason, msg)
	return nil
}

func routeAccepted(route *unstructured.Unstructured, svc *gryviav1.GryviaInferenceService) (bool, string) {
	parents, _, _ := unstructured.NestedSlice(route.Object, "status", "parents")
	for _, raw := range parents {
		p, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		ref, ok := p["parentRef"].(map[string]interface{})
		if !ok {
			continue
		}
		if group, ok := ref["group"].(string); ok && group != "gateway.networking.k8s.io" {
			continue
		}
		if kind, ok := ref["kind"].(string); ok && kind != "Gateway" {
			continue
		}
		if ref["name"] != svc.Annotations[AnnotationGatewayParent] {
			continue
		}
		if ns, ok := ref["namespace"].(string); ok && ns != svc.Namespace {
			continue
		}
		if section, ok := ref["sectionName"].(string); ok && section != svc.Annotations[AnnotationGatewaySection] {
			continue
		}
		if svc.Annotations[AnnotationGatewaySection] != "" && ref["sectionName"] != svc.Annotations[AnnotationGatewaySection] {
			continue
		}
		accepted, resolved := false, false
		conds, _ := p["conditions"].([]interface{})
		for _, raw := range conds {
			c, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			gen, ok := c["observedGeneration"].(int64)
			if !ok || gen < route.GetGeneration() {
				continue
			}
			if c["type"] == "Accepted" {
				accepted = c["status"] == "True"
			}
			if c["type"] == "ResolvedRefs" {
				resolved = c["status"] == "True"
			}
		}
		if accepted && resolved {
			return true, "Gateway accepted the current route and resolved its stable/canary Service references"
		}
	}
	return false, "Waiting for current-generation Accepted=True and ResolvedRefs=True on the configured Gateway parent"
}

func (r *GryviaInferenceServiceReconciler) canPromoteWithRouting(ctx context.Context, svc *gryviav1.GryviaInferenceService) bool {
	if !routingRequested(svc) {
		return true
	}
	if !r.GatewayRouting || routeWeight(svc) == 0 {
		return false
	}
	route := newInferenceRoute()
	if err := r.Get(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: inferRouteName(svc)}, route); err != nil {
		return false
	}
	if !metav1.IsControlledBy(route, svc) {
		return false
	}
	// Check spec as well as status: stale accepted conditions for a different weight
	// or parent must not authorize promotion before the new route is programmed.
	if specHash(route.Object["spec"]) != specHash(r.routeSpec(svc)) {
		return false
	}
	ready, _ := routeAccepted(route, svc)
	return ready
}

// Promotion waits for consecutive observed readiness and accepted routing, not
// merely for a deployment's age. A changed route/version resets the hold window.
func (r *GryviaInferenceServiceReconciler) routePromotionReady(ctx context.Context, svc *gryviav1.GryviaInferenceService, canary *appsv1.Deployment, now time.Time) (bool, error) {
	const sinceKey = "gryvia.io/route-healthy-since"
	const hashKey = "gryvia.io/route-healthy-hash"
	ready := svc.Spec.Canary.AutoPromote && r.canPromoteWithRouting(ctx, svc)
	hash := specHash(r.routeSpec(svc)) + ":" + specHash(svc.Spec.Canary) + ":" + canary.Annotations[annotationSpecHash]
	stamp, err := time.Parse(time.RFC3339Nano, canary.Annotations[sinceKey])
	if ready && err == nil && canary.Annotations[hashKey] == hash && !stamp.After(now) {
		return now.Sub(stamp) >= time.Duration(svc.Spec.Canary.PromoteAfterSeconds)*time.Second, nil
	}
	if !ready && canary.Annotations[sinceKey] == "" && canary.Annotations[hashKey] == "" {
		return false, nil
	}
	base := canary.DeepCopy()
	if canary.Annotations == nil {
		canary.Annotations = map[string]string{}
	}
	if ready {
		canary.Annotations[sinceKey] = now.Format(time.RFC3339Nano)
		canary.Annotations[hashKey] = hash
	} else {
		delete(canary.Annotations, sinceKey)
		delete(canary.Annotations, hashKey)
	}
	if err := r.Patch(ctx, canary, client.MergeFrom(base)); err != nil {
		return false, err
	}
	return ready && svc.Spec.Canary.PromoteAfterSeconds == 0, nil
}
