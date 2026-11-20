// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controllers

import (
	"context"
	"net"
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
	"sigs.k8s.io/controller-runtime/pkg/log"

	gryviav1 "github.com/zyvorai/gryvia/operators/network-intelligence/api/v1"
	"github.com/zyvorai/gryvia/operators/network-intelligence/pkg/sources"
)

// maxGraphEdges bounds status.edges (the busiest edges by bytes are kept).
const maxGraphEdges = 500

// GryviaServiceGraphReconciler reconciles a GryviaServiceGraph object.
//
// Nodes come from the Services of the target namespaces; edges from the merged collector graph
// (/api/v1/graph of every collector node). The collector graph is cumulative counters of live edges
// (an edge expires 10 minutes after its last flow), not a time-windowed rate.
type GryviaServiceGraphReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Collector sources.Collector
}

//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaservicegraphs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaservicegraphs/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaservicegraphs/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *GryviaServiceGraphReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	graph := &gryviav1.GryviaServiceGraph{}
	if err := r.Get(ctx, req.NamespacedName, graph); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	refreshInterval := 1 * time.Minute
	if graph.Spec.RefreshInterval != "" {
		if parsed, err := time.ParseDuration(graph.Spec.RefreshInterval); err == nil && parsed > 0 {
			refreshInterval = parsed
		}
	}

	services := r.discoverServiceNodes(ctx, graph)

	var cg sources.Graph
	var stats sources.Stats
	var err error
	if r.Collector == nil {
		err = errNoCollector
	} else {
		cg, stats, err = r.Collector.Graph(ctx)
	}

	var nodes []gryviav1.ServiceGraphNode
	var edges []gryviav1.ServiceGraphEdge
	if err == nil {
		nodes, edges = buildGraph(graph, services, cg)
		evaluateNodeHealth(nodes, edges)
	} else {
		logger.Info("collector graph unavailable; keeping the last known edges", "reason", err.Error())
		nodes, edges = services, graph.Status.Edges
	}

	key := req.NamespacedName
	uerr := updateStatus(ctx, r.Client, key, func() *gryviav1.GryviaServiceGraph { return &gryviav1.GryviaServiceGraph{} },
		func(g *gryviav1.GryviaServiceGraph) {
			g.Status.Nodes = nodes
			g.Status.Edges = edges
			if err == nil {
				g.Status.LastUpdated = metav1.Now()
			}
			setSource(&g.Status.Conditions, g.Generation, err, stats,
				"edges are collector counters of live connections (no verdicts, no p99); spec.depth is not used")
		})
	if uerr != nil && !errors.IsNotFound(uerr) {
		return ctrl.Result{}, uerr
	}
	logger.Info("GryviaServiceGraph refreshed", "nodes", len(nodes), "edges", len(edges), "collectorError", err != nil)
	return ctrl.Result{RequeueAfter: refreshInterval}, nil
}

// discoverServiceNodes enumerates services across target namespaces to build graph nodes.
func (r *GryviaServiceGraphReconciler) discoverServiceNodes(ctx context.Context, graph *gryviav1.GryviaServiceGraph) []gryviav1.ServiceGraphNode {
	logger := log.FromContext(ctx)
	var nodes []gryviav1.ServiceGraphNode

	namespaces := graph.Spec.Namespaces
	if len(namespaces) == 0 {
		namespaces = []string{graph.Namespace}
	}
	for _, ns := range namespaces {
		namespace := &corev1.Namespace{}
		if err := r.Get(ctx, types.NamespacedName{Name: ns}, namespace); err != nil {
			logger.V(1).Info("Namespace not found, skipping", "namespace", ns)
			continue
		}
		svcList := &corev1.ServiceList{}
		if err := r.List(ctx, svcList, client.InNamespace(ns)); err != nil {
			logger.Error(err, "Failed to list services", "namespace", ns)
			continue
		}
		for _, svc := range svcList.Items {
			nodeType := "service"
			if svc.Spec.Type == corev1.ServiceTypeExternalName {
				if !graph.Spec.IncludeExternal {
					continue
				}
				nodeType = "external"
			}
			nodes = append(nodes, gryviav1.ServiceGraphNode{Name: svc.Name, Namespace: svc.Namespace, Type: nodeType, Health: "unknown"})
		}
	}
	return nodes
}

func isIPEndpoint(id string) bool {
	host := id
	if h, _, err := net.SplitHostPort(id); err == nil {
		host = h
	}
	return net.ParseIP(host) != nil
}

// buildGraph turns the merged collector graph into status nodes and edges. An edge is kept when at
// least one endpoint is a known Service of the target namespaces (or a collector node whose namespace
// is one of them); endpoints that are bare IPs are external and kept only with spec.includeExternal.
func buildGraph(graph *gryviav1.GryviaServiceGraph, services []gryviav1.ServiceGraphNode, cg sources.Graph) ([]gryviav1.ServiceGraphNode, []gryviav1.ServiceGraphEdge) {
	targets := map[string]bool{}
	for _, ns := range graph.Spec.Namespaces {
		targets[ns] = true
	}
	if len(targets) == 0 {
		targets[graph.Namespace] = true
	}
	byName := map[string][]gryviav1.ServiceGraphNode{}
	for _, n := range services {
		byName[n.Name] = append(byName[n.Name], n)
	}
	cgNS := map[string]string{}
	for _, n := range cg.Nodes {
		cgNS[n.ID] = n.Namespace
	}
	inScope := func(id string) bool {
		if len(byName[id]) > 0 {
			return true
		}
		if i := strings.IndexByte(id, '/'); i > 0 {
			if targets[id[:i]] {
				return true
			}
		}
		return targets[cgNS[id]] && !isIPEndpoint(id)
	}

	nodeSet := map[string]gryviav1.ServiceGraphNode{}
	for _, n := range services {
		nodeSet[n.Name] = n
	}
	var kept []sources.Edge
	for _, e := range cg.Edges {
		if !inScope(e.Source) && !inScope(e.Target) {
			continue
		}
		if !graph.Spec.IncludeExternal && (isIPEndpoint(e.Source) && !inScope(e.Source) || isIPEndpoint(e.Target) && !inScope(e.Target)) {
			continue
		}
		kept = append(kept, e)
	}
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].BytesTotal > kept[j].BytesTotal })
	if len(kept) > maxGraphEdges {
		kept = kept[:maxGraphEdges]
	}
	edges := make([]gryviav1.ServiceGraphEdge, 0, len(kept))
	for _, e := range kept {
		for _, id := range []string{e.Source, e.Target} {
			if _, ok := nodeSet[id]; !ok {
				typ := "workload"
				if isIPEndpoint(id) {
					typ = "external"
				}
				ns := ""
				if !isIPEndpoint(id) {
					ns = cgNS[id]
				}
				nodeSet[id] = gryviav1.ServiceGraphNode{Name: id, Namespace: ns, Type: typ, Health: "unknown"}
			}
		}
		edges = append(edges, gryviav1.ServiceGraphEdge{
			Source: e.Source, Destination: e.Target, Protocol: strings.ToLower(e.Protocol), Port: int(e.Port),
			LatencyP50: formatMs(e.LatencyMs), BytesTotal: satInt64(e.BytesTotal), FlowCount: satInt64(e.FlowCount),
			Throughput: formatBytes(satInt64(e.BytesTotal)),
		})
	}
	sort.Slice(edges, func(i, j int) bool {
		a, b := edges[i], edges[j]
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		if a.Destination != b.Destination {
			return a.Destination < b.Destination
		}
		return a.Port < b.Port
	})
	nodes := make([]gryviav1.ServiceGraphNode, 0, len(nodeSet))
	for _, n := range nodeSet {
		nodes = append(nodes, n)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	return nodes, edges
}

// evaluateNodeHealth derives health from edge verdicts. The collector graph carries no verdicts, so
// nodes stay "unknown" unless some other source fills them; never "healthy" by absence of data.
func evaluateNodeHealth(nodes []gryviav1.ServiceGraphNode, edges []gryviav1.ServiceGraphEdge) {
	errs, total := map[string]int{}, map[string]int{}
	for _, e := range edges {
		if e.Verdict == "" {
			continue
		}
		total[e.Destination]++
		if e.Verdict == "dropped" || e.Verdict == "error" {
			errs[e.Destination]++
		}
	}
	for i := range nodes {
		t := total[nodes[i].Name]
		if t == 0 {
			nodes[i].Health = "unknown"
			continue
		}
		switch rate := float64(errs[nodes[i].Name]) / float64(t); {
		case rate == 0:
			nodes[i].Health = "healthy"
		case rate < 0.05:
			nodes[i].Health = "degraded"
		default:
			nodes[i].Health = "unhealthy"
		}
	}
}

// SetupWithManager sets up the controller with the Manager
func (r *GryviaServiceGraphReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gryviav1.GryviaServiceGraph{}).
		Complete(r)
}
