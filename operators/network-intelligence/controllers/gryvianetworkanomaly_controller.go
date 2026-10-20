package controllers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gryviav1 "github.com/zyvorai/gryvia/operators/network-intelligence/api/v1"
	"github.com/zyvorai/gryvia/operators/network-intelligence/pkg/sources"
)

const (
	// maxAnomalyHistory is the maximum number of anomalies to retain in status
	maxAnomalyHistory = 100

	// webhookTimeout is the timeout for alert webhook calls
	webhookTimeout = 10 * time.Second
)

// ruleAnomalyType maps a detection rule metric to the collector anomaly type it corresponds to.
// The metrics "errors" and "drops" have no collector anomaly (see docs/network-intelligence-sources.md).
var ruleAnomalyType = map[string]string{
	"latency":     "latency_spike",
	"throughput":  "traffic_burst",
	"connections": "new_connection",
}

// GryviaNetworkAnomalyReconciler reconciles a GryviaNetworkAnomaly object.
//
// The anomalies are the collector's own (/api/v1/anomalies, merged over all nodes): a statistical
// baseline per service that the collector maintains. spec.detectionRules only select WHICH collector
// anomaly types to keep (latency -> latency_spike, throughput -> traffic_burst, connections ->
// new_connection; no rules keeps every type); their thresholds and operators are not evaluated here.
type GryviaNetworkAnomalyReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Collector sources.Collector
}

//+kubebuilder:rbac:groups=gryvia.io,resources=gryvianetworkanomalies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gryvia.io,resources=gryvianetworkanomalies/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryvianetworkanomalies/finalizers,verbs=update
//+kubebuilder:rbac:groups=cilium.io,resources=ciliumnetworkpolicies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *GryviaNetworkAnomalyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	detector := &gryviav1.GryviaNetworkAnomaly{}
	if err := r.Get(ctx, req.NamespacedName, detector); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	checkInterval := r.getCheckInterval(detector)

	var raw []sources.Anomaly
	var stats sources.Stats
	var err error
	if r.Collector == nil {
		err = errNoCollector
	} else {
		raw, stats, err = r.Collector.Anomalies(ctx)
	}

	var fresh []gryviav1.NetworkAnomalyEvent
	all := detector.Status.Anomalies
	if err == nil {
		fresh = newAnomalies(detector, raw, detector.Status.Anomalies)
		if detector.Spec.AutoMitigate {
			r.autoMitigate(ctx, detector, fresh)
		}
		if detector.Spec.AlertWebhook != "" && len(fresh) > 0 {
			r.sendWebhookAlert(ctx, detector, fresh)
		}
		all = r.mergeAnomalies(detector.Status.Anomalies, fresh)
	}

	uerr := updateStatus(ctx, r.Client, req.NamespacedName, func() *gryviav1.GryviaNetworkAnomaly { return &gryviav1.GryviaNetworkAnomaly{} },
		func(d *gryviav1.GryviaNetworkAnomaly) {
			if err == nil {
				d.Status.Anomalies = all
				d.Status.LastCheck = metav1.Now()
			}
			setSource(&d.Status.Conditions, d.Generation, err, stats,
				"anomalies are the collector's statistical detections; detectionRules only select types")
		})
	if uerr != nil && !errors.IsNotFound(uerr) {
		return ctrl.Result{}, uerr
	}
	logger.Info("GryviaNetworkAnomaly check complete", "new", len(fresh), "total", len(all), "collectorError", err != nil)
	return ctrl.Result{RequeueAfter: checkInterval}, nil
}

// getCheckInterval determines the reconciliation interval based on the shortest rule window
func (r *GryviaNetworkAnomalyReconciler) getCheckInterval(detector *gryviav1.GryviaNetworkAnomaly) time.Duration {
	shortest := 1 * time.Minute
	for _, rule := range detector.Spec.DetectionRules {
		if rule.Window != "" {
			if parsed, err := time.ParseDuration(rule.Window); err == nil && parsed > 0 && parsed < shortest {
				shortest = parsed
			}
		}
	}
	return shortest
}

// newAnomalies converts the collector anomalies of the target service (all when empty) that pass the
// rule type filter and are not already in existing (same type and detection time).
func newAnomalies(detector *gryviav1.GryviaNetworkAnomaly, raw []sources.Anomaly, existing []gryviav1.NetworkAnomalyEvent) []gryviav1.NetworkAnomalyEvent {
	types := map[string]bool{}
	for _, rule := range detector.Spec.DetectionRules {
		if t, ok := ruleAnomalyType[rule.Metric]; ok {
			types[t] = true
		}
	}
	type key struct {
		t  string
		at int64
	}
	seen := map[key]bool{}
	for _, e := range existing {
		seen[key{e.Type, e.Detected.Unix()}] = true
	}
	var out []gryviav1.NetworkAnomalyEvent
	for _, a := range raw {
		if detector.Spec.TargetService != "" && a.Service != detector.Spec.TargetService {
			continue
		}
		if len(types) > 0 && !types[a.Type] {
			continue
		}
		k := key{a.Type, a.DetectedAt.Unix()}
		if seen[k] {
			continue
		}
		seen[k] = true
		desc := a.Message
		if desc == "" {
			desc = fmt.Sprintf("%s on %s: value %.2f, baseline %.2f", a.Type, a.Service, a.Value, a.Baseline)
		}
		out = append(out, gryviav1.NetworkAnomalyEvent{Type: a.Type, Severity: anomalySeverity(a),
			Detected: metav1.NewTime(a.DetectedAt), Description: desc})
	}
	return out
}

// mergeAnomalies combines existing and new anomalies, keeping only recent entries
func (r *GryviaNetworkAnomalyReconciler) mergeAnomalies(existing, newAnomalies []gryviav1.NetworkAnomalyEvent) []gryviav1.NetworkAnomalyEvent {
	var recent []gryviav1.NetworkAnomalyEvent
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, a := range existing {
		if !a.Detected.IsZero() && a.Detected.Time.After(cutoff) {
			recent = append(recent, a)
		}
	}
	recent = append(recent, newAnomalies...)
	if len(recent) > maxAnomalyHistory {
		recent = recent[len(recent)-maxAnomalyHistory:]
	}
	return recent
}

// autoMitigate applies temporary deny policies for suspicious traffic
func (r *GryviaNetworkAnomalyReconciler) autoMitigate(ctx context.Context, detector *gryviav1.GryviaNetworkAnomaly, anomalies []gryviav1.NetworkAnomalyEvent) {
	logger := log.FromContext(ctx)

	for i, anomaly := range anomalies {
		if anomaly.Severity != "critical" && anomaly.Severity != "high" {
			continue
		}

		// Create a temporary deny policy for the target service
		policyName := fmt.Sprintf("fna-mitigate-%s-%d", detector.Name, anomaly.Detected.Unix())

		mitigationPolicy := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "cilium.io/v2",
				"kind":       "CiliumNetworkPolicy",
				"metadata": map[string]interface{}{
					"name":      policyName,
					"namespace": detector.Namespace,
					"annotations": map[string]interface{}{
						"gryvia.io/managed-by":   "netpredator-anomaly",
						"gryvia.io/anomaly-type": anomaly.Type,
						"gryvia.io/temporary":    "true",
						"gryvia.io/expires":      time.Now().Add(15 * time.Minute).Format(time.RFC3339),
					},
					"labels": map[string]interface{}{
						"gryvia.io/mitigation": "auto",
					},
				},
				"spec": map[string]interface{}{
					"endpointSelector": map[string]interface{}{
						"matchLabels": map[string]interface{}{
							"app": detector.Spec.TargetService,
						},
					},
					"ingressDeny": []interface{}{
						map[string]interface{}{},
					},
				},
			},
		}

		existingPolicy := &unstructured.Unstructured{}
		existingPolicy.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "cilium.io",
			Version: "v2",
			Kind:    "CiliumNetworkPolicy",
		})

		err := r.Get(ctx, types.NamespacedName{
			Name:      policyName,
			Namespace: detector.Namespace,
		}, existingPolicy)

		if errors.IsNotFound(err) {
			if createErr := r.Create(ctx, mitigationPolicy); createErr != nil {
				logger.Error(createErr, "Failed to create mitigation policy", "name", policyName)
				continue
			}
			logger.Info("Created auto-mitigation policy",
				"name", policyName,
				"anomalyType", anomaly.Type,
				"severity", anomaly.Severity,
			)
			anomalies[i].Mitigated = true
		} else if err != nil {
			logger.Error(err, "Failed to check mitigation policy", "name", policyName)
		}
	}

	// Clean up expired mitigation policies
	r.cleanupExpiredMitigations(ctx, detector)
}

// cleanupExpiredMitigations removes temporary mitigation policies that have expired
func (r *GryviaNetworkAnomalyReconciler) cleanupExpiredMitigations(ctx context.Context, detector *gryviav1.GryviaNetworkAnomaly) {
	logger := log.FromContext(ctx)

	// List CiliumNetworkPolicies with mitigation label in the detector's namespace
	policyList := &unstructured.UnstructuredList{}
	policyList.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "cilium.io",
		Version: "v2",
		Kind:    "CiliumNetworkPolicyList",
	})

	if err := r.List(ctx, policyList,
		client.InNamespace(detector.Namespace),
		client.MatchingLabels{"gryvia.io/mitigation": "auto"},
	); err != nil {
		logger.V(1).Info("Failed to list mitigation policies for cleanup", "error", err)
		return
	}

	for _, policy := range policyList.Items {
		annotations := policy.GetAnnotations()
		if annotations == nil {
			continue
		}

		expiresStr, ok := annotations["gryvia.io/expires"]
		if !ok {
			continue
		}

		expires, err := time.Parse(time.RFC3339, expiresStr)
		if err != nil {
			continue
		}

		if time.Now().After(expires) {
			if deleteErr := r.Delete(ctx, &policy); deleteErr != nil {
				logger.Error(deleteErr, "Failed to delete expired mitigation policy", "name", policy.GetName())
			} else {
				logger.Info("Cleaned up expired mitigation policy", "name", policy.GetName())
			}
		}
	}
}

// webhookPayload is the JSON payload sent to alert webhooks
type webhookPayload struct {
	Service   string                         `json:"service"`
	Anomalies []gryviav1.NetworkAnomalyEvent `json:"anomalies"`
	Timestamp string                         `json:"timestamp"`
}

// sendWebhookAlert sends anomaly alerts to the configured webhook URL
func (r *GryviaNetworkAnomalyReconciler) sendWebhookAlert(ctx context.Context, detector *gryviav1.GryviaNetworkAnomaly, anomalies []gryviav1.NetworkAnomalyEvent) {
	logger := log.FromContext(ctx)

	payload := webhookPayload{
		Service:   detector.Spec.TargetService,
		Anomalies: anomalies,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	body, err := json.Marshal(payload)
	if err != nil {
		logger.Error(err, "Failed to marshal webhook payload")
		return
	}

	if err := validateWebhookURL(detector.Spec.AlertWebhook); err != nil {
		logger.Error(err, "Refusing to send webhook alert")
		return
	}
	httpClient := newWebhookClient(webhookTimeout)
	resp, err := httpClient.Post(detector.Spec.AlertWebhook, "application/json", bytes.NewReader(body))
	if err != nil {
		logger.Error(err, "Failed to send webhook alert", "url", detector.Spec.AlertWebhook)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		logger.Info("Webhook alert returned non-success status",
			"url", detector.Spec.AlertWebhook,
			"statusCode", resp.StatusCode,
		)
	} else {
		logger.Info("Webhook alert sent successfully",
			"url", detector.Spec.AlertWebhook,
			"anomalies", len(anomalies),
		)
	}
}

// SetupWithManager sets up the controller with the Manager
func (r *GryviaNetworkAnomalyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gryviav1.GryviaNetworkAnomaly{}).
		Complete(r)
}
