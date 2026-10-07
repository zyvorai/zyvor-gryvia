package scheduler

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	LabelIBBlock = "gryvia.io/ib-block"
	LabelRack    = "gryvia.io/rack"
	// AnnotationTopologyPlacement set to "false" on a job opts it out of group placement.
	AnnotationTopologyPlacement = "gryvia.io/topology-placement"
)

// TopologyLevels are tried in order: the tightest interconnect domain first.
var TopologyLevels = []string{LabelIBBlock, LabelRack}

type topologyKey struct{}

// WithTopologyPlacement turns group placement on for a placement: a multi-node job is kept inside one
// InfiniBand block, else one rack, when such a group has enough eligible nodes.
func WithTopologyPlacement(ctx context.Context) context.Context {
	return context.WithValue(ctx, topologyKey{}, true)
}

func topologyPlacement(ctx context.Context) bool {
	on, _ := ctx.Value(topologyKey{}).(bool)
	return on
}

// groupPlacement picks n nodes from one topology group. Per level it takes the smallest group with at least n
// eligible nodes (keeping larger groups free for larger jobs); ties go to the higher sum of the n best scores,
// then to the group name. Within the group the existing ranking order is kept. ok is false when no group at
// any level fits, and the caller keeps the plain ranking.
func groupPlacement(scored []NodeScore, nodes []corev1.Node, n int) (picked []NodeScore, key, value string, ok bool) {
	if n < 2 {
		return nil, "", "", false
	}
	labels := map[string]map[string]string{}
	for _, node := range nodes {
		labels[node.Name] = node.Labels
	}
	for _, key := range TopologyLevels {
		groups := map[string][]NodeScore{}
		for _, s := range scored {
			if v := labels[s.NodeName][key]; v != "" {
				groups[v] = append(groups[v], s)
			}
		}
		best, bestSum := "", 0
		for v, g := range groups {
			if len(g) < n {
				continue
			}
			sum := 0
			for _, s := range g[:n] {
				sum += s.Score
			}
			if best == "" || len(g) < len(groups[best]) ||
				(len(g) == len(groups[best]) && (sum > bestSum || (sum == bestSum && v < best))) {
				best, bestSum = v, sum
			}
		}
		if best != "" {
			return groups[best][:n], key, best, true
		}
	}
	return nil, "", "", false
}

// SharedTopology returns the tightest topology level all the named nodes share ("gryvia.io/ib-block=b1"), or ""
// when they share none or a node cannot be read.
func SharedTopology(ctx context.Context, c client.Client, names []string) string {
	if len(names) < 2 {
		return ""
	}
	var nodes []corev1.Node
	for _, name := range names {
		n := corev1.Node{}
		if err := c.Get(ctx, types.NamespacedName{Name: name}, &n); err != nil {
			return ""
		}
		nodes = append(nodes, n)
	}
	for _, key := range TopologyLevels {
		v := nodes[0].Labels[key]
		same := v != ""
		for _, n := range nodes[1:] {
			if n.Labels[key] != v {
				same = false
				break
			}
		}
		if same {
			return fmt.Sprintf("%s=%s", key, v)
		}
	}
	return ""
}
