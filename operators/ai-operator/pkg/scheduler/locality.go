package scheduler

import (
	"context"
	"sort"

	corev1 "k8s.io/api/core/v1"
)

type preferredPoolsKey struct{}

// WithPreferredPools asks the placement to rank nodes matching any of the selectors (pools holding a local copy
// of the job's dataset) ahead of the others. Within each group the score order is kept, so locality only breaks
// ties between eligible nodes and never makes an ineligible node eligible.
func WithPreferredPools(ctx context.Context, selectors []map[string]string) context.Context {
	if len(selectors) == 0 {
		return ctx
	}
	return context.WithValue(ctx, preferredPoolsKey{}, selectors)
}

func preferredPools(ctx context.Context) []map[string]string {
	s, _ := ctx.Value(preferredPoolsKey{}).([]map[string]string)
	return s
}

// InAnyPool reports whether the node matches one of the selectors.
func InAnyPool(node corev1.Node, selectors []map[string]string) bool {
	for _, sel := range selectors {
		if len(sel) > 0 && matchesNodeSelector(node, sel) {
			return true
		}
	}
	return false
}

// preferLocal moves nodes in a preferred pool to the front, keeping the existing order within both groups.
func preferLocal(scored []NodeScore, nodes []corev1.Node, selectors []map[string]string) []NodeScore {
	if len(selectors) == 0 {
		return scored
	}
	local := map[string]bool{}
	for _, n := range nodes {
		local[n.Name] = InAnyPool(n, selectors)
	}
	out := append([]NodeScore(nil), scored...)
	sort.SliceStable(out, func(i, j int) bool { return local[out[i].NodeName] && !local[out[j].NodeName] })
	return out
}
