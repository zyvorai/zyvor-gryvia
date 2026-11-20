// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gryviav1 "github.com/zyvorai/gryvia/operators/network-intelligence/api/v1"
	"github.com/zyvorai/gryvia/operators/network-intelligence/pkg/sources"
)

// GryviaTraceSessionReconciler reconciles a GryviaTraceSession object
//
// Flows come from Netra's history API over the session window [startTime, endTime], filtered to the
// pods behind spec.service in spec.namespace (and spec.filters port/protocol/dstIP; srcIP and the
// l3/l4/l7 level are NOT applied - Netra records carry no such split). They are written to the result
// ConfigMap (replaced on every poll, at most 2000 flows) and status.flowsCaptured is that count. Without
// Netra (GRYVIA_NETRA_URL) nothing is captured and the SourceAvailable condition says so. Header capture
// is not supported.
type GryviaTraceSessionReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Netra is the flow source; nil or unconfigured means no capture.
	Netra sources.FlowHistory
}

const (
	maxTraceFlows = 2000
	// tracePollInterval is how often an active session re-reads Netra.
	tracePollInterval = 30 * time.Second
)

//+kubebuilder:rbac:groups=gryvia.io,resources=gryviatracesessions,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviatracesessions/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviatracesessions/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *GryviaTraceSessionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the GryviaTraceSession instance
	session := &gryviav1.GryviaTraceSession{}
	if err := r.Get(ctx, req.NamespacedName, session); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("GryviaTraceSession resource not found, ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get GryviaTraceSession")
		return ctrl.Result{}, err
	}

	logger.Info("Reconciling GryviaTraceSession",
		"name", session.Name,
		"service", session.Spec.Service,
		"level", session.Spec.Level,
	)

	// Handle session based on current phase
	switch session.Status.Phase {
	case "completed", "expired":
		logger.Info("Trace session already finished", "phase", session.Status.Phase)
		return ctrl.Result{}, nil
	case "active":
		return r.reconcileActiveSession(ctx, req.NamespacedName, session)
	default:
		return r.startSession(ctx, req.NamespacedName, session)
	}
}

// startSession initializes a new trace session
func (r *GryviaTraceSessionReconciler) startSession(ctx context.Context, namespacedName types.NamespacedName, session *gryviav1.GryviaTraceSession) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Starting new trace session")

	// Parse session duration
	sessionDuration := 5 * time.Minute
	if session.Spec.Duration != "" {
		if parsed, err := time.ParseDuration(session.Spec.Duration); err == nil && parsed > 0 {
			sessionDuration = parsed
		}
	}

	// Create the results ConfigMap
	resultCMName := fmt.Sprintf("trace-%s", session.Name)
	resultCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resultCMName,
			Namespace: session.Namespace,
			Labels: map[string]string{
				"gryvia.io/managed-by": "netpredator",
				"gryvia.io/trace":      session.Name,
			},
		},
		Data: map[string]string{
			"session": session.Name,
			"service": session.Spec.Service,
			"level":   session.Spec.Level,
			"status":  "active",
			"flows":   "[]",
		},
	}

	existingCM := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Name: resultCMName, Namespace: session.Namespace}, existingCM)
	if errors.IsNotFound(err) {
		if createErr := r.Create(ctx, resultCM); createErr != nil {
			return ctrl.Result{}, fmt.Errorf("failed to create trace results ConfigMap: %w", createErr)
		}
	} else if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to check trace results ConfigMap: %w", err)
	}

	// Update status to active
	now := metav1.Now()
	endTime := metav1.NewTime(now.Add(sessionDuration))

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &gryviav1.GryviaTraceSession{}
		if err := r.Get(ctx, namespacedName, current); err != nil {
			return err
		}
		current.Status.Phase = "active"
		current.Status.FlowsCaptured = 0
		current.Status.StartTime = now
		current.Status.EndTime = endTime
		current.Status.ResultRef = &gryviav1.TraceResultRef{
			Kind:      "ConfigMap",
			Name:      resultCMName,
			Namespace: session.Namespace,
		}
		var startErr error
		if r.Netra == nil || !r.Netra.Configured() {
			startErr = &sources.SourceError{Source: "netra", Reason: sources.ReasonNotConfigured,
				Message: "no flows will be captured: trace sessions read Netra's flow history and GRYVIA_NETRA_URL is not set on the operator"}
		}
		r.setCaptureCondition(current, startErr)
		return r.Status().Update(ctx, current)
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update trace session status: %w", err)
	}

	logger.Info("Trace session started",
		"duration", sessionDuration,
		"resultConfigMap", resultCMName,
	)

	// Requeue to check for expiration
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

// reconcileActiveSession checks if the session has expired and captures flow data
func (r *GryviaTraceSessionReconciler) reconcileActiveSession(ctx context.Context, namespacedName types.NamespacedName, session *gryviav1.GryviaTraceSession) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Check if session has expired
	if !session.Status.EndTime.IsZero() && time.Now().After(session.Status.EndTime.Time) {
		logger.Info("Trace session expired, completing")
		return r.completeSession(ctx, namespacedName, session)
	}

	flows, cerr := r.captureFlows(ctx, session)
	if session.Status.ResultRef != nil && cerr == nil {
		r.updateTraceResults(ctx, session, flows)
	}
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &gryviav1.GryviaTraceSession{}
		if err := r.Get(ctx, namespacedName, current); err != nil {
			return err
		}
		if cerr == nil {
			current.Status.FlowsCaptured = int64(len(flows))
		}
		r.setCaptureCondition(current, cerr)
		return r.Status().Update(ctx, current)
	}); err != nil {
		logger.Error(err, "Failed to update flow count")
	}

	requeueAfter := tracePollInterval
	if !session.Status.EndTime.IsZero() {
		if remaining := time.Until(session.Status.EndTime.Time); remaining < requeueAfter {
			requeueAfter = remaining
		}
	}
	if requeueAfter < time.Second {
		requeueAfter = time.Second
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

func (r *GryviaTraceSessionReconciler) setCaptureCondition(s *gryviav1.GryviaTraceSession, err error) {
	setSource(&s.Status.Conditions, s.Generation, err, sources.Stats{},
		"flows are Netra history records of the service's pods; spec.level, spec.captureHeaders and filters.srcIP are not applied")
}

// completeSession finalizes the trace session
func (r *GryviaTraceSessionReconciler) completeSession(ctx context.Context, namespacedName types.NamespacedName, session *gryviav1.GryviaTraceSession) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Final capture over the whole window.
	flows, cerr := r.captureFlows(ctx, session)
	if session.Status.ResultRef != nil && cerr == nil {
		r.updateTraceResults(ctx, session, flows)
		session.Status.FlowsCaptured = int64(len(flows))
	}

	// Update the results ConfigMap to mark as completed
	if session.Status.ResultRef != nil {
		cm := &corev1.ConfigMap{}
		err := r.Get(ctx, types.NamespacedName{
			Name:      session.Status.ResultRef.Name,
			Namespace: session.Status.ResultRef.Namespace,
		}, cm)
		if err == nil {
			if cm.Data == nil {
				cm.Data = make(map[string]string)
			}
			cm.Data["status"] = "completed"
			cm.Data["flowsCaptured"] = fmt.Sprintf("%d", session.Status.FlowsCaptured)
			if updateErr := r.Update(ctx, cm); updateErr != nil {
				logger.Error(updateErr, "Failed to update trace results ConfigMap")
			}
		}
	}

	// Parse session duration to determine if expired vs completed
	phase := "completed"
	if session.Spec.Duration != "" {
		if parsed, err := time.ParseDuration(session.Spec.Duration); err == nil {
			if !session.Status.StartTime.IsZero() && time.Since(session.Status.StartTime.Time) > parsed {
				phase = "expired"
			}
		}
	}

	// Update status to completed/expired
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &gryviav1.GryviaTraceSession{}
		if err := r.Get(ctx, namespacedName, current); err != nil {
			return err
		}
		current.Status.Phase = phase
		current.Status.EndTime = metav1.Now()
		if cerr == nil {
			current.Status.FlowsCaptured = int64(len(flows))
		}
		r.setCaptureCondition(current, cerr)
		return r.Status().Update(ctx, current)
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update completed session status: %w", err)
	}

	logger.Info("Trace session completed",
		"phase", phase,
		"flowsCaptured", session.Status.FlowsCaptured,
	)

	return ctrl.Result{}, nil
}

// capturedFlow represents a single captured network flow
type capturedFlow struct {
	Timestamp   string `json:"timestamp"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Port        int    `json:"port"`
	Protocol    string `json:"protocol"`
	Verdict     string `json:"verdict"`
	Bytes       int64  `json:"bytes"`
}

// captureFlows reads the Netra flows of the session window that belong to the traced service.
func (r *GryviaTraceSessionReconciler) captureFlows(ctx context.Context, session *gryviav1.GryviaTraceSession) ([]capturedFlow, error) {
	if r.Netra == nil || !r.Netra.Configured() {
		return nil, &sources.SourceError{Source: "netra", Reason: sources.ReasonNotConfigured,
			Message: "no flows captured: trace sessions read Netra's flow history and GRYVIA_NETRA_URL is not set on the operator"}
	}
	start, end := session.Status.StartTime.Time, session.Status.EndTime.Time
	if start.IsZero() {
		start = time.Now()
	}
	if end.IsZero() || end.After(time.Now()) {
		end = time.Now()
	}
	ns := session.Spec.Namespace
	if ns == "" {
		ns = session.Namespace
	}
	pods, havePods, err := r.servicePods(ctx, ns, session.Spec.Service)
	if err != nil {
		return nil, err
	}
	recs, err := r.Netra.History(ctx, time.Since(start)+5*time.Second, 5000)
	if err != nil {
		return nil, err
	}
	var out []capturedFlow
	for _, rec := range recs {
		if rec.Namespace != ns || rec.ObservedAt.Before(start) || rec.ObservedAt.After(end.Add(5*time.Second)) {
			continue
		}
		if havePods {
			if !pods[rec.Pod] {
				continue
			}
		} else if rec.WorkloadName != session.Spec.Service && !strings.HasPrefix(rec.Pod, session.Spec.Service+"-") {
			continue
		}
		if f := session.Spec.Filters; f != nil {
			if f.Port > 0 && rec.Port != f.Port {
				continue
			}
			if f.Protocol != "" && f.Protocol != "any" && rec.Protocol != "" && !strings.EqualFold(rec.Protocol, f.Protocol) {
				continue
			}
			if f.DstIP != "" && rec.Peer != f.DstIP {
				continue
			}
		}
		verdict := "FORWARDED"
		if rec.Blocked {
			verdict = "DROP"
		}
		out = append(out, capturedFlow{Timestamp: rec.ObservedAt.UTC().Format(time.RFC3339), Source: netraEndpoint(rec),
			Destination: rec.Peer, Port: rec.Port, Protocol: strings.ToUpper(rec.Protocol), Verdict: verdict, Bytes: rec.Bytes})
	}
	if len(out) > maxTraceFlows {
		out = out[len(out)-maxTraceFlows:]
	}
	return out, nil
}

func netraEndpoint(rec sources.FlowRecord) string {
	switch {
	case rec.Namespace != "" && rec.Pod != "":
		return rec.Namespace + "/" + rec.Pod
	case rec.Pod != "":
		return rec.Pod
	case rec.WorkloadName != "":
		return rec.WorkloadName
	case rec.Comm != "" && rec.Node != "":
		return rec.Comm + "@" + rec.Node
	}
	return rec.Comm + rec.Node
}

// servicePods returns the pods selected by the Service; ok=false when the Service does not exist or has no selector.
func (r *GryviaTraceSessionReconciler) servicePods(ctx context.Context, ns, service string) (map[string]bool, bool, error) {
	svc := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{Name: service, Namespace: ns}, svc); err != nil {
		if errors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if len(svc.Spec.Selector) == 0 {
		return nil, false, nil
	}
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(ns), client.MatchingLabels(svc.Spec.Selector)); err != nil {
		return nil, false, err
	}
	out := map[string]bool{}
	for _, p := range pods.Items {
		out[p.Name] = true
	}
	return out, true, nil
}

// updateTraceResults replaces the flows in the results ConfigMap.
func (r *GryviaTraceSessionReconciler) updateTraceResults(ctx context.Context, session *gryviav1.GryviaTraceSession, flows []capturedFlow) {
	if session.Status.ResultRef == nil {
		return
	}
	logger := log.FromContext(ctx)
	cm := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{Name: session.Status.ResultRef.Name, Namespace: session.Status.ResultRef.Namespace}, cm); err != nil {
		logger.Error(err, "Failed to get trace results ConfigMap")
		return
	}
	if flows == nil {
		flows = []capturedFlow{}
	}
	flowsJSON, err := json.Marshal(flows)
	if err != nil {
		logger.Error(err, "Failed to marshal flows")
		return
	}
	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}
	cm.Data["flows"] = string(flowsJSON)
	cm.Data["source"] = "netra"
	cm.Data["flowsCaptured"] = fmt.Sprintf("%d", len(flows))
	if updateErr := r.Update(ctx, cm); updateErr != nil {
		logger.Error(updateErr, "Failed to update trace results ConfigMap")
	}
}

// SetupWithManager sets up the controller with the Manager
func (r *GryviaTraceSessionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gryviav1.GryviaTraceSession{}).
		Complete(r)
}
