package controllers

import (
	"context"
	"sort"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

const maxQueueBorrowers = 8

// queueCategory classifies a queued job from the Workload state and the message Kueue gave.
func queueCategory(st workloadState, reason, msg string) string {
	switch {
	case !st.Found:
		return "WaitingForWorkload"
	case st.Evicted || reason == "Evicted":
		return "Evicted"
	case st.QuotaReserved && !st.Admitted:
		return "AdmissionChecksPending"
	case strings.Contains(strings.ToLower(msg), "insufficient"):
		return "QuotaExhausted"
	}
	return "Pending"
}

// clusterQueueUsage returns the nominal quota, current use and borrowed amount of the
// ClusterQueue's first resource group, for the busiest flavor (flavors repeat the same nominal,
// and a job lands in one of them).
func clusterQueueUsage(cq *unstructured.Unstructured) (nominal, inUse, borrowed int64) {
	groups, _, _ := unstructured.NestedSlice(cq.Object, "spec", "resourceGroups")
	covered := map[string]bool{}
	if len(groups) > 0 {
		g, _ := groups[0].(map[string]interface{})
		names, _ := g["coveredResources"].([]interface{})
		for _, n := range names {
			if s, ok := n.(string); ok {
				covered[s] = true
			}
		}
		flavors, _ := g["flavors"].([]interface{})
		for _, f := range flavors {
			fm, _ := f.(map[string]interface{})
			res, _ := fm["resources"].([]interface{})
			for _, r := range res {
				rm, _ := r.(map[string]interface{})
				if q, err := resource.ParseQuantity(str(rm["nominalQuota"])); err == nil && q.Value() > nominal {
					nominal = q.Value()
				}
			}
		}
	}
	usage, _, _ := unstructured.NestedSlice(cq.Object, "status", "flavorsUsage")
	for _, f := range usage {
		fm, _ := f.(map[string]interface{})
		res, _ := fm["resources"].([]interface{})
		for _, r := range res {
			rm, _ := r.(map[string]interface{})
			if len(covered) > 0 && !covered[str(rm["name"])] {
				continue
			}
			if q, err := resource.ParseQuantity(str(rm["total"])); err == nil && q.Value() > inUse {
				inUse = q.Value()
			}
			if q, err := resource.ParseQuantity(str(rm["borrowed"])); err == nil && q.Value() > borrowed {
				borrowed = q.Value()
			}
		}
	}
	return nominal, inUse, borrowed
}

func str(v interface{}) string {
	s, _ := v.(string)
	return s
}

// buildQueueExplanation assembles the explanation from the Workload state, the job's ClusterQueue
// (may be nil) and its cohort peers (may be empty). It does no I/O.
func buildQueueExplanation(st workloadState, reason, msg string, cq *unstructured.Unstructured, peers []*unstructured.Unstructured) *gryviav1.QueueExplanation {
	out := &gryviav1.QueueExplanation{Category: queueCategory(st, reason, msg), Message: msg}
	if cq == nil {
		return out
	}
	out.ClusterQueue = cq.GetName()
	out.Cohort, _, _ = unstructured.NestedString(cq.Object, "spec", "cohort")
	out.Nominal, out.InUse, out.Borrowed = clusterQueueUsage(cq)
	if out.Cohort == "" {
		return out
	}
	for _, p := range peers {
		if p.GetName() == cq.GetName() {
			continue
		}
		if c, _, _ := unstructured.NestedString(p.Object, "spec", "cohort"); c != out.Cohort {
			continue
		}
		if _, _, b := clusterQueueUsage(p); b > 0 {
			out.Borrowers = append(out.Borrowers, gryviav1.QueueBorrower{ClusterQueue: p.GetName(), Borrowed: b})
		}
	}
	sort.Slice(out.Borrowers, func(i, j int) bool {
		if out.Borrowers[i].Borrowed != out.Borrowers[j].Borrowed {
			return out.Borrowers[i].Borrowed > out.Borrowers[j].Borrowed
		}
		return out.Borrowers[i].ClusterQueue < out.Borrowers[j].ClusterQueue
	})
	if len(out.Borrowers) > maxQueueBorrowers {
		out.Borrowers = out.Borrowers[:maxQueueBorrowers]
	}
	return out
}

// explainQueue reads the job's LocalQueue, ClusterQueue and cohort peers best-effort: any lookup
// error only leaves the explanation with less detail.
func (r *GryviaAIJobReconciler) explainQueue(ctx context.Context, job *gryviav1.GryviaAIJob, bj *batchv1.Job, st workloadState, reason, msg string) *gryviav1.QueueExplanation {
	var cq *unstructured.Unstructured
	var peers []*unstructured.Unstructured

	if lqName := bj.Labels[LabelKueueQueue]; lqName != "" {
		lq := &unstructured.Unstructured{}
		lq.SetGroupVersionKind(KueueGVK("LocalQueue"))
		if err := r.Get(ctx, types.NamespacedName{Namespace: job.Namespace, Name: lqName}, lq); err == nil {
			if name, _, _ := unstructured.NestedString(lq.Object, "spec", "clusterQueue"); name != "" {
				c := &unstructured.Unstructured{}
				c.SetGroupVersionKind(KueueGVK("ClusterQueue"))
				if err := r.Get(ctx, client.ObjectKey{Name: name}, c); err == nil {
					cq = c
				}
			}
		}
	}
	if cq != nil {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(KueueGVK("ClusterQueueList"))
		if err := r.List(ctx, list); err == nil {
			for i := range list.Items {
				peers = append(peers, &list.Items[i])
			}
		}
	}
	return buildQueueExplanation(st, reason, msg, cq, peers)
}
