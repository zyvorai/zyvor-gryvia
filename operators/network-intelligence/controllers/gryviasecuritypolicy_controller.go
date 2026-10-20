package controllers

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gryviav1 "github.com/zyvorai/gryvia/operators/network-intelligence/api/v1"
	"github.com/zyvorai/gryvia/operators/network-intelligence/pkg/sources"
)

const (
	// securityCheckInterval is the default requeue interval for security policy checks
	securityCheckInterval = 30 * time.Second

	// securityWebhookTimeout is the timeout for security alert webhook calls
	securityWebhookTimeout = 10 * time.Second

	// ConditionAutoBlockIgnored is set when the deprecated spec.autoBlock is true.
	ConditionAutoBlockIgnored = "AutoBlockIgnored"
)

// ruleEventTypes maps a GryviaSecurityPolicy detection rule type to the collector event_type names
// (collector/pkg/decoder SecurityEventTypeName) it covers. A rule type that is itself an event_type
// name matches that type.
var ruleEventTypes = map[string][]string{
	"escape":       {"container_escape", "namespace_breach"},
	"mining":       {"crypto_mining"},
	"exfiltration": {"data_exfiltration"},
	"privesc":      {"privilege_escalation"},
	"driver_fim":   {"driver_tampering"},
}

// GryviaSecurityPolicyReconciler reconciles a GryviaSecurityPolicy object.
//
// Alerts come from the merged collector /api/v1/security/alerts. The collector keeps a bounded ring of
// recent alerts per node; only alerts newer than status.lastAlert are counted, so counters do not grow
// on every poll. Alerts carry no namespace, so spec.targetNamespaces cannot filter them. Nothing is ever
// blocked: spec.autoBlock is deprecated and ignored.
type GryviaSecurityPolicyReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Collector sources.Collector
}

//+kubebuilder:rbac:groups=gryvia.io,resources=gryviasecuritypolicies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviasecuritypolicies/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviasecuritypolicies/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *GryviaSecurityPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	policy := &gryviav1.GryviaSecurityPolicy{}
	if err := r.Get(ctx, req.NamespacedName, policy); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	active := 0
	for _, rule := range policy.Spec.DetectionRules {
		if rule.Enabled {
			active++
		}
	}

	var raw []sources.SecurityAlert
	var stats sources.Stats
	var err error
	if r.Collector == nil {
		err = errNoCollector
	} else {
		raw, stats, err = r.Collector.SecurityAlerts(ctx)
	}

	var fresh []sources.SecurityAlert
	counts := map[string]int{}
	var newest time.Time
	if err == nil {
		fresh, counts = newAlerts(policy, raw, policy.Status.LastAlert.Time)
		for _, a := range fresh {
			if a.Timestamp.After(newest) {
				newest = a.Timestamp
			}
		}
		if policy.Spec.AlertWebhook != "" && len(fresh) > 0 {
			r.sendSecurityWebhook(ctx, policy, fresh)
		}
	}

	uerr := updateStatus(ctx, r.Client, req.NamespacedName, func() *gryviav1.GryviaSecurityPolicy { return &gryviav1.GryviaSecurityPolicy{} },
		func(p *gryviav1.GryviaSecurityPolicy) {
			p.Status.ActiveDetections = active
			if err != nil {
				p.Status.Phase = "Degraded"
			} else {
				p.Status.Phase = "Active"
				if p.Status.DetectionCounts == nil {
					p.Status.DetectionCounts = map[string]int{}
				}
				for k, v := range counts {
					p.Status.DetectionCounts[k] += v
					p.Status.AlertsTriggered += v
				}
				if !newest.IsZero() {
					p.Status.LastAlert = metav1.NewTime(newest)
				}
			}
			setSource(&p.Status.Conditions, p.Generation, err, stats,
				"alerts are node-level (no namespace), so targetNamespaces does not filter them")
			if p.Spec.AutoBlock {
				setCondition(&p.Status.Conditions, p.Generation, ConditionAutoBlockIgnored, metav1.ConditionTrue, "Deprecated",
					"spec.autoBlock is deprecated and ignored: the operator never blocks traffic on a security alert")
			} else {
				removeCondition(&p.Status.Conditions, ConditionAutoBlockIgnored)
			}
		})
	if uerr != nil && !errors.IsNotFound(uerr) {
		return ctrl.Result{}, uerr
	}
	logger.Info("GryviaSecurityPolicy check complete", "activeDetections", active, "newAlerts", len(fresh), "collectorError", err != nil)
	return ctrl.Result{RequeueAfter: securityCheckInterval}, nil
}

func removeCondition(conds *[]metav1.Condition, typ string) {
	out := (*conds)[:0]
	for _, c := range *conds {
		if c.Type != typ {
			out = append(out, c)
		}
	}
	*conds = out
}

// newAlerts returns the alerts newer than since that match an enabled rule (a policy with no enabled rule
// counts nothing) and the per-rule-type counts of those.
func newAlerts(policy *gryviav1.GryviaSecurityPolicy, raw []sources.SecurityAlert, since time.Time) ([]sources.SecurityAlert, map[string]int) {
	byEvent := map[string]string{} // event_type -> rule type
	for _, rule := range policy.Spec.DetectionRules {
		if !rule.Enabled {
			continue
		}
		if evs, ok := ruleEventTypes[rule.Type]; ok {
			for _, e := range evs {
				byEvent[e] = rule.Type
			}
		} else if rule.Type != "" {
			byEvent[rule.Type] = rule.Type
		}
	}
	counts := map[string]int{}
	var out []sources.SecurityAlert
	for _, a := range raw {
		if !a.Timestamp.After(since) {
			continue
		}
		ruleType, ok := byEvent[a.EventType]
		if !ok {
			continue
		}
		counts[ruleType]++
		out = append(out, a)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Timestamp.Before(out[j].Timestamp) })
	return out, counts
}

// securityAlert is the alert element of the operator's OWN webhook payload (unchanged contract); it is
// filled from the collector's event_type / process_name / src_ip / details fields.
type securityAlert struct {
	Type     string `json:"type"`
	Severity string `json:"severity"`
	Process  string `json:"process"`
	Path     string `json:"path"`
	SourceIP string `json:"sourceIP"`
	Message  string `json:"message"`
}

// securityWebhookPayload is the JSON payload sent to security alert webhooks
type securityWebhookPayload struct {
	PolicyName string          `json:"policyName"`
	Namespace  string          `json:"namespace"`
	Alerts     []securityAlert `json:"alerts"`
	Timestamp  string          `json:"timestamp"`
}

func toWebhookAlerts(alerts []sources.SecurityAlert) []securityAlert {
	out := make([]securityAlert, 0, len(alerts))
	for _, a := range alerts {
		out = append(out, securityAlert{Type: a.EventType, Severity: a.Severity, Process: a.ProcessName,
			Path: a.Path, SourceIP: a.SrcIP, Message: a.Details})
	}
	return out
}

// sendSecurityWebhook sends new security alerts to the configured webhook URL
func (r *GryviaSecurityPolicyReconciler) sendSecurityWebhook(ctx context.Context, policy *gryviav1.GryviaSecurityPolicy, alerts []sources.SecurityAlert) {
	logger := log.FromContext(ctx)

	payload := securityWebhookPayload{
		PolicyName: policy.Name,
		Namespace:  policy.Namespace,
		Alerts:     toWebhookAlerts(alerts),
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		logger.Error(err, "Failed to marshal security webhook payload")
		return
	}
	if err := validateWebhookURL(policy.Spec.AlertWebhook); err != nil {
		logger.Error(err, "Refusing to send security webhook alert")
		return
	}
	httpClient := newWebhookClient(securityWebhookTimeout)
	resp, err := httpClient.Post(policy.Spec.AlertWebhook, "application/json", bytes.NewReader(body))
	if err != nil {
		logger.Error(err, "Failed to send security webhook alert", "url", policy.Spec.AlertWebhook)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		logger.Info("Security webhook returned non-success status", "url", policy.Spec.AlertWebhook, "statusCode", resp.StatusCode)
	}
}

// SetupWithManager sets up the controller with the Manager
func (r *GryviaSecurityPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gryviav1.GryviaSecurityPolicy{}).
		Complete(r)
}
