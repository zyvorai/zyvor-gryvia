// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"crypto/sha1" //nolint:gosec // a name suffix, not a security use
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gryviav1 "github.com/zyvorai/gryvia/operators/network-intelligence/api/v1"
	"github.com/zyvorai/gryvia/operators/network-intelligence/pkg/sources"
)

const (
	// LabelAutoPolicy names the GryviaAutoPolicy that produced a suggested GryviaFlowPolicy.
	LabelAutoPolicy = "gryvia.io/auto-policy"

	autoPolicyInterval = 1 * time.Minute
	maxLearnedEdges    = 1500

	// ConditionEnforceNotAutomatic is set when spec.mode=enforce: suggestions are never auto-applied.
	ConditionEnforceNotAutomatic = "EnforceNotAutomatic"
)

// GryviaAutoPolicyReconciler reconciles a GryviaAutoPolicy object.
//
// It learns service-to-service edges from the merged collector graph (a Service -> Service edge whose
// both ends are Services of spec.targetNamespaces, minus spec.excludeServices), keeps them in the
// ConfigMap autopolicy-<name>-learned (collector edges expire after ten minutes, a learning window is
// longer), and, in mode suggest, once the learning window has elapsed since creation, writes one
// GryviaFlowPolicy per learned edge: deterministic name, label gryvia.io/suggested=true, owner reference to
// this object, action allow, never applied. The FlowPolicy controller ignores suggested policies until a
// human removes the label (gryvia network policy apply). Suggestions are only made for sources in this
// object's own namespace (a CiliumNetworkPolicy only selects pods of its namespace). Mode enforce does NOT
// enforce anything: it behaves as suggest and sets the EnforceNotAutomatic condition; appliedPolicies counts
// the suggestions a human has approved (label removed). The confidence is a heuristic from the number of
// observed flows (>=100: 0.9, >=10: 0.75, else 0.5), not a statistical measure.
type GryviaAutoPolicyReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Collector sources.Collector
	now       func() time.Time
}

//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaautopolicies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaautopolicies/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaautopolicies/finalizers,verbs=update
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaflowpolicies,verbs=get;list;watch;create
//+kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *GryviaAutoPolicyReconciler) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

type learnedEdge struct {
	Source      string    `json:"source"`
	Destination string    `json:"destination"`
	SourceNS    string    `json:"sourceNamespace"`
	DestNS      string    `json:"destinationNamespace"`
	Protocol    string    `json:"protocol"`
	Port        int       `json:"port"`
	Bytes       int64     `json:"bytes"`
	Flows       int64     `json:"flows"`
	FirstSeen   time.Time `json:"firstSeen"`
	LastSeen    time.Time `json:"lastSeen"`
}

func (e learnedEdge) key() string {
	return fmt.Sprintf("%s/%s|%s/%s|%s|%d", e.SourceNS, e.Source, e.DestNS, e.Destination, e.Protocol, e.Port)
}

func (r *GryviaAutoPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	ap := &gryviav1.GryviaAutoPolicy{}
	if err := r.Get(ctx, req.NamespacedName, ap); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	now := r.clock()

	learned, cmErr := r.loadLearned(ctx, ap)
	if cmErr != nil {
		return ctrl.Result{}, cmErr
	}

	var stats sources.Stats
	var gerr error
	if r.Collector == nil {
		gerr = errNoCollector
	} else {
		var g sources.Graph
		g, stats, gerr = r.Collector.Graph(ctx)
		if gerr == nil {
			svcs, err := r.targetServices(ctx, ap)
			if err != nil {
				return ctrl.Result{}, err
			}
			learn(learned, g, svcs, ap.Spec.ExcludeServices, now)
			if err := r.saveLearned(ctx, ap, learned); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	window := time.Duration(0)
	if ap.Spec.LearningWindow != "" {
		if d, err := time.ParseDuration(ap.Spec.LearningWindow); err == nil && d > 0 {
			window = d
		}
	}
	windowElapsed := !ap.CreationTimestamp.IsZero() && now.Sub(ap.CreationTimestamp.Time) >= window
	edges := sortedEdges(learned)

	phase := "learning"
	var suggestions []gryviav1.SuggestedPolicy
	skipped := 0
	if ap.Spec.Mode != "learn" && windowElapsed {
		phase = "suggesting"
		var err error
		suggestions, skipped, err = r.ensureSuggestions(ctx, ap, edges)
		if err != nil {
			return ctrl.Result{}, err
		}
	}
	applied, aerr := r.countApproved(ctx, ap)
	if aerr != nil {
		return ctrl.Result{}, aerr
	}

	uerr := updateStatus(ctx, r.Client, req.NamespacedName, func() *gryviav1.GryviaAutoPolicy { return &gryviav1.GryviaAutoPolicy{} },
		func(a *gryviav1.GryviaAutoPolicy) {
			a.Status.Phase = phase
			a.Status.LearnedPolicies = len(edges)
			a.Status.SuggestedPolicies = suggestions
			a.Status.AppliedPolicies = applied
			if gerr == nil {
				a.Status.LastLearned = metav1.NewTime(now)
			}
			note := "learned edges are kept in ConfigMap " + learnedName(ap.Name)
			if skipped > 0 {
				note += fmt.Sprintf("; %d learned edges have a source outside namespace %s and were not suggested (create an AutoPolicy there)", skipped, ap.Namespace)
			}
			setSource(&a.Status.Conditions, a.Generation, gerr, stats, note)
			if ap.Spec.Mode == "enforce" {
				setCondition(&a.Status.Conditions, a.Generation, ConditionEnforceNotAutomatic, metav1.ConditionTrue, "NeverAutoApplied",
					"mode enforce behaves as suggest: suggestions are GryviaFlowPolicy objects labelled "+LabelSuggested+"=true and are applied only when a human removes the label (gryvia network policy apply)")
			} else {
				removeCondition(&a.Status.Conditions, ConditionEnforceNotAutomatic)
			}
		})
	if uerr != nil && !errors.IsNotFound(uerr) {
		return ctrl.Result{}, uerr
	}
	logger.Info("GryviaAutoPolicy reconciled", "phase", phase, "learned", len(edges), "suggested", len(suggestions), "applied", applied)
	return ctrl.Result{RequeueAfter: autoPolicyInterval}, nil
}

func learnedName(ap string) string { return "autopolicy-" + ap + "-learned" }

// targetServices maps a Service name to its namespace over the target namespaces; a name present in
// several of them is ambiguous and omitted.
func (r *GryviaAutoPolicyReconciler) targetServices(ctx context.Context, ap *gryviav1.GryviaAutoPolicy) (map[string]string, error) {
	nss := ap.Spec.TargetNamespaces
	if len(nss) == 0 {
		nss = []string{ap.Namespace}
	}
	out := map[string]string{}
	ambiguous := map[string]bool{}
	for _, ns := range nss {
		list := &corev1.ServiceList{}
		if err := r.List(ctx, list, client.InNamespace(ns)); err != nil {
			return nil, err
		}
		for _, s := range list.Items {
			if _, dup := out[s.Name]; dup && out[s.Name] != ns {
				ambiguous[s.Name] = true
			}
			out[s.Name] = ns
		}
	}
	for n := range ambiguous {
		delete(out, n)
	}
	return out, nil
}

// learn merges the current graph into the learned edges. Collector counters are cumulative, so the
// kept value is the larger of the stored and the current one.
func learn(store map[string]learnedEdge, g sources.Graph, svcs map[string]string, exclude []string, now time.Time) {
	excluded := map[string]bool{}
	for _, e := range exclude {
		excluded[e] = true
	}
	for _, e := range g.Edges {
		proto := strings.ToLower(e.Protocol)
		if (proto != "tcp" && proto != "udp") || e.Port == 0 || e.Source == e.Target {
			continue
		}
		srcNS, ok1 := svcs[e.Source]
		dstNS, ok2 := svcs[e.Target]
		if !ok1 || !ok2 || excluded[e.Source] || excluded[e.Target] {
			continue
		}
		le := learnedEdge{Source: e.Source, Destination: e.Target, SourceNS: srcNS, DestNS: dstNS, Protocol: proto,
			Port: int(e.Port), Bytes: satInt64(e.BytesTotal), Flows: satInt64(e.FlowCount), FirstSeen: now, LastSeen: now}
		k := le.key()
		if old, ok := store[k]; ok {
			le.FirstSeen = old.FirstSeen
			if old.Bytes > le.Bytes {
				le.Bytes = old.Bytes
			}
			if old.Flows > le.Flows {
				le.Flows = old.Flows
			}
		}
		store[k] = le
	}
	if len(store) > maxLearnedEdges {
		edges := sortedEdges(store)
		sort.SliceStable(edges, func(i, j int) bool { return edges[i].LastSeen.After(edges[j].LastSeen) })
		for _, e := range edges[maxLearnedEdges:] {
			delete(store, e.key())
		}
	}
}

func sortedEdges(store map[string]learnedEdge) []learnedEdge {
	out := make([]learnedEdge, 0, len(store))
	for _, e := range store {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	return out
}

func (r *GryviaAutoPolicyReconciler) loadLearned(ctx context.Context, ap *gryviav1.GryviaAutoPolicy) (map[string]learnedEdge, error) {
	store := map[string]learnedEdge{}
	cm := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Name: learnedName(ap.Name), Namespace: ap.Namespace}, cm)
	if errors.IsNotFound(err) {
		return store, nil
	}
	if err != nil {
		return nil, err
	}
	var list []learnedEdge
	if raw := cm.Data["edges"]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &list); err != nil {
			log.FromContext(ctx).Error(err, "learned-edge ConfigMap is corrupt; starting over")
		}
	}
	for _, e := range list {
		store[e.key()] = e
	}
	return store, nil
}

func (r *GryviaAutoPolicyReconciler) saveLearned(ctx context.Context, ap *gryviav1.GryviaAutoPolicy, store map[string]learnedEdge) error {
	raw, err := json.Marshal(sortedEdges(store))
	if err != nil {
		return err
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: learnedName(ap.Name), Namespace: ap.Namespace,
		Labels: map[string]string{LabelAutoPolicy: ap.Name, "gryvia.io/managed-by": "netpredator"}}}
	err = r.Get(ctx, client.ObjectKeyFromObject(cm), cm)
	switch {
	case errors.IsNotFound(err):
		cm.Data = map[string]string{"edges": string(raw)}
		if err := controllerutil.SetControllerReference(ap, cm, r.Scheme); err != nil {
			return err
		}
		return r.Create(ctx, cm)
	case err != nil:
		return err
	}
	if cm.Data["edges"] == string(raw) {
		return nil
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data["edges"] = string(raw)
	return r.Update(ctx, cm)
}

var nameClean = regexp.MustCompile(`[^a-z0-9]+`)

func nameFrag(s string, n int) string {
	s = strings.Trim(nameClean.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > n {
		s = strings.TrimRight(s[:n], "-")
	}
	return s
}

// suggestionName is deterministic: same AutoPolicy + edge, same name.
func suggestionName(ap *gryviav1.GryviaAutoPolicy, e learnedEdge) string {
	sum := sha1.Sum([]byte(ap.Namespace + "/" + ap.Name + "|" + e.key())) //nolint:gosec
	return fmt.Sprintf("auto-%s-to-%s-%d-%s", nameFrag(e.Source, 16), nameFrag(e.Destination, 16), e.Port, hex.EncodeToString(sum[:4]))
}

func confidenceOf(flows int64) float64 {
	switch {
	case flows >= 100:
		return 0.9
	case flows >= 10:
		return 0.75
	default:
		return 0.5
	}
}

// ensureSuggestions creates the missing suggested FlowPolicies (never modifies or recreates an existing one,
// approved or not) and returns the status suggestions plus the number of edges skipped for a foreign source.
func (r *GryviaAutoPolicyReconciler) ensureSuggestions(ctx context.Context, ap *gryviav1.GryviaAutoPolicy, edges []learnedEdge) ([]gryviav1.SuggestedPolicy, int, error) {
	var out []gryviav1.SuggestedPolicy
	skipped := 0
	for _, e := range edges {
		if e.SourceNS != ap.Namespace {
			skipped++
			continue
		}
		name := suggestionName(ap, e)
		conf := confidenceOf(e.Flows)
		existing := &gryviav1.GryviaFlowPolicy{}
		err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ap.Namespace}, existing)
		if errors.IsNotFound(err) {
			fp := &gryviav1.GryviaFlowPolicy{
				ObjectMeta: metav1.ObjectMeta{
					Name: name, Namespace: ap.Namespace,
					Labels: map[string]string{LabelSuggested: "true", LabelAutoPolicy: ap.Name},
					Annotations: map[string]string{
						"gryvia.io/confidence":     fmt.Sprintf("%.2f", conf),
						"gryvia.io/observed-flows": fmt.Sprintf("%d", e.Flows),
						"gryvia.io/observed-bytes": fmt.Sprintf("%d", e.Bytes),
						"gryvia.io/first-seen":     e.FirstSeen.UTC().Format(time.RFC3339),
					},
				},
				Spec: gryviav1.GryviaFlowPolicySpec{
					Source:      &gryviav1.FlowEndpoint{Service: e.Source, Namespace: e.SourceNS},
					Destination: &gryviav1.FlowEndpoint{Service: e.Destination, Namespace: e.DestNS, Port: e.Port},
					Protocol:    e.Protocol,
					Action:      "allow",
					Intent:      "auto-learned from observed traffic",
				},
			}
			if err := controllerutil.SetControllerReference(ap, fp, r.Scheme); err != nil {
				return nil, skipped, err
			}
			if err := r.Create(ctx, fp); err != nil && !errors.IsAlreadyExists(err) {
				return nil, skipped, err
			}
		} else if err != nil {
			return nil, skipped, err
		}
		out = append(out, gryviav1.SuggestedPolicy{Source: e.SourceNS + "/" + e.Source, Destination: e.DestNS + "/" + e.Destination,
			Port: e.Port, Confidence: conf})
	}
	return out, skipped, nil
}

// countApproved counts this object's FlowPolicies whose suggested label a human has removed.
func (r *GryviaAutoPolicyReconciler) countApproved(ctx context.Context, ap *gryviav1.GryviaAutoPolicy) (int, error) {
	list := &gryviav1.GryviaFlowPolicyList{}
	if err := r.List(ctx, list, client.InNamespace(ap.Namespace), client.MatchingLabels{LabelAutoPolicy: ap.Name}); err != nil {
		return 0, err
	}
	n := 0
	for _, p := range list.Items {
		if p.Labels[LabelSuggested] != "true" {
			n++
		}
	}
	return n, nil
}

// SetupWithManager sets up the controller with the Manager
func (r *GryviaAutoPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gryviav1.GryviaAutoPolicy{}).
		Complete(r)
}
