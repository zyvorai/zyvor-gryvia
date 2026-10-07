package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

// Federation failover (--federation-failover and spec.failover.enabled+automatic): when a member fails
// failureThreshold health checks in a row, its workloads are fenced (deleted on the member when its API server
// still answers, otherwise assumed stopped once spec.failover.leaseTimeout has passed since the first failure),
// and only then are the manager-side Kueue Workloads that MultiKueue dispatched to it evicted (spec.active=false)
// and, once Kueue reports them evicted, reactivated so MultiKueue dispatches them to a healthy member.
const (
	FenceRemoteDelete = "RemoteDelete"
	FenceLeaseExpired = "LeaseExpired"

	// AnnotationFailoverFrom marks a Workload deactivated by failover and not yet reactivated.
	AnnotationFailoverFrom = "gryvia.io/failover-from"
	// AnnotationFailedOverFrom records the member a reactivated Workload was moved off.
	AnnotationFailedOverFrom = "gryvia.io/failed-over-from"

	defaultFailureThreshold = 3
	defaultLeaseTimeout     = 5 * time.Minute
	remoteFenceTimeout      = 5 * time.Second
	maxFailoverRecords      = 20
)

var kueueWorkloadGVK = schema.GroupVersionKind{Group: "kueue.x-k8s.io", Version: "v1beta1", Kind: "Workload"}

//+kubebuilder:rbac:groups=kueue.x-k8s.io,resources=workloads,verbs=get;list;watch;patch

func failureThreshold(f *gryviav1.FederationFailover) int {
	if f != nil && f.HealthCheck != nil && f.HealthCheck.FailureThreshold > 0 {
		return f.HealthCheck.FailureThreshold
	}
	return defaultFailureThreshold
}

func leaseTimeout(f *gryviav1.FederationFailover) time.Duration {
	if f != nil && f.LeaseTimeout != "" {
		if d, err := time.ParseDuration(f.LeaseTimeout); err == nil && d > 0 {
			return d
		}
	}
	return defaultLeaseTimeout
}

func multiKueueName(c gryviav1.FederationCluster) string {
	if c.MultiKueueCluster != "" {
		return c.MultiKueueCluster
	}
	return c.Name
}

// trackHealth carries the failure streak and fence state over from the previous status.
func trackHealth(prev []gryviav1.FederationClusterStatus, cur []gryviav1.FederationClusterStatus, now metav1.Time) {
	old := map[string]gryviav1.FederationClusterStatus{}
	for _, p := range prev {
		old[p.Name] = p
	}
	for i := range cur {
		s, p := &cur[i], old[cur[i].Name]
		if s.State == "healthy" {
			continue // streak and fence reset
		}
		s.ConsecutiveFailures = p.ConsecutiveFailures + 1
		s.UnhealthySince = p.UnhealthySince
		if s.UnhealthySince == nil {
			t := now
			s.UnhealthySince = &t
		}
		s.Fenced, s.FenceMethod, s.FencedAt = p.Fenced, p.FenceMethod, p.FencedAt
	}
}

func newWorkloadList() *unstructured.UnstructuredList {
	l := &unstructured.UnstructuredList{}
	l.SetGroupVersionKind(kueueWorkloadGVK.GroupVersion().WithKind("WorkloadList"))
	return l
}

func workloadCondition(wl *unstructured.Unstructured, condType string) bool {
	conds, _, _ := unstructured.NestedSlice(wl.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]interface{})
		if ok && m["type"] == condType {
			return m["status"] == "True"
		}
	}
	return false
}

func workloadActive(wl *unstructured.Unstructured) bool {
	active, found, _ := unstructured.NestedBool(wl.Object, "spec", "active")
	return !found || active
}

// assignedTo reports whether MultiKueue placed the Workload on the named MultiKueueCluster: status.clusterName,
// or the admission check message `... reservation on "<name>"` of Kueue versions without that field.
func assignedTo(wl *unstructured.Unstructured, mk string) bool {
	if name, _, _ := unstructured.NestedString(wl.Object, "status", "clusterName"); name != "" {
		return name == mk
	}
	checks, _, _ := unstructured.NestedSlice(wl.Object, "status", "admissionChecks")
	for _, c := range checks {
		if m, ok := c.(map[string]interface{}); ok {
			if msg, _ := m["message"].(string); strings.Contains(msg, `"`+mk+`"`) {
				return true
			}
		}
	}
	return false
}

func ownerJob(wl *unstructured.Unstructured) string {
	for _, o := range wl.GetOwnerReferences() {
		if o.Kind == "Job" && strings.HasPrefix(o.APIVersion, "batch/") {
			return o.Name
		}
	}
	return ""
}

// remoteClient connects to a member through the same validation as the probe.
func (r *GryviaFederationReconciler) remoteClient(ctx context.Context, c gryviav1.FederationCluster) (client.Client, error) {
	if r.RemoteClient != nil {
		return r.RemoteClient(ctx, c)
	}
	cfg, err := r.restConfigFor(ctx, c)
	if err != nil {
		return nil, err
	}
	cfg.Timeout = remoteFenceTimeout
	return client.New(cfg, client.Options{Scheme: r.Scheme})
}

// fenceRemote deletes the member's copies (same namespace and names as on the manager) of the given Workloads and
// their Jobs. Any error means the member could not be fenced this way.
func (r *GryviaFederationReconciler) fenceRemote(ctx context.Context, c gryviav1.FederationCluster, wls []unstructured.Unstructured) error {
	rc, err := r.remoteClient(ctx, c)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, remoteFenceTimeout*time.Duration(1+len(wls)))
	defer cancel()
	background := client.PropagationPolicy(metav1.DeletePropagationBackground)
	for i := range wls {
		wl := &wls[i]
		if job := ownerJob(wl); job != "" {
			bj := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: wl.GetNamespace(), Name: job}}
			if err := rc.Delete(ctx, bj, background); err != nil && !errors.IsNotFound(err) {
				return err
			}
		}
		remote := &unstructured.Unstructured{}
		remote.SetGroupVersionKind(kueueWorkloadGVK)
		remote.SetNamespace(wl.GetNamespace())
		remote.SetName(wl.GetName())
		if err := rc.Delete(ctx, remote, background); err != nil && !errors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (r *GryviaFederationReconciler) setActive(ctx context.Context, wl *unstructured.Unstructured, active bool, annotations map[string]interface{}) error {
	patch := map[string]interface{}{"spec": map[string]interface{}{"active": active}}
	if annotations != nil {
		patch["metadata"] = map[string]interface{}{"annotations": annotations}
	}
	raw, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	return r.Patch(ctx, wl, client.RawPatch(types.MergePatchType, raw))
}

// reconcileFailover fences failed members and moves their Workloads. It returns how soon to look again (0: the
// normal health-check interval).
func (r *GryviaFederationReconciler) reconcileFailover(ctx context.Context, fed *gryviav1.GryviaFederation, now metav1.Time) (time.Duration, error) {
	f := fed.Spec.Failover
	if !r.Failover || f == nil || !f.Enabled || !f.Automatic {
		return 0, nil
	}
	list := newWorkloadList()
	if err := r.List(ctx, list); err != nil {
		return 0, fmt.Errorf("list Kueue workloads: %w", err)
	}
	var wait time.Duration
	// Second phase first: reactivate Workloads that Kueue has evicted.
	for i := range list.Items {
		wl := &list.Items[i]
		from := wl.GetAnnotations()[AnnotationFailoverFrom]
		if from == "" || workloadActive(wl) {
			continue
		}
		if !workloadCondition(wl, "Evicted") && workloadCondition(wl, "QuotaReserved") {
			wait = 5 * time.Second // Kueue has not processed the deactivation yet
			continue
		}
		if err := r.setActive(ctx, wl, true, map[string]interface{}{AnnotationFailoverFrom: nil, AnnotationFailedOverFrom: from}); err != nil {
			return 0, err
		}
		markRequeued(fed, wl, now)
		r.event(fed, "Normal", "FailoverRequeued", fmt.Sprintf("Workload %s/%s requeued for dispatch off %s", wl.GetNamespace(), wl.GetName(), from))
	}

	threshold, lease := failureThreshold(f), leaseTimeout(f)
	clusters := map[string]gryviav1.FederationCluster{}
	for _, c := range fed.Spec.Clusters {
		clusters[c.Name] = c
	}
	for i := range fed.Status.ClusterStatus {
		s := &fed.Status.ClusterStatus[i]
		c, ok := clusters[s.Name]
		if !ok || s.State == "healthy" || s.ConsecutiveFailures < threshold {
			continue
		}
		mk := multiKueueName(c)
		var mine []unstructured.Unstructured
		for j := range list.Items {
			wl := &list.Items[j]
			if workloadActive(wl) && !workloadCondition(wl, "Finished") && assignedTo(wl, mk) {
				mine = append(mine, *wl)
			}
		}
		if len(mine) == 0 {
			continue
		}
		if !s.Fenced {
			if err := r.fenceRemote(ctx, c, mine); err == nil {
				s.Fenced, s.FenceMethod = true, FenceRemoteDelete
			} else if s.UnhealthySince != nil && now.Sub(s.UnhealthySince.Time) >= lease {
				s.Fenced, s.FenceMethod = true, FenceLeaseExpired
			} else {
				remaining := lease
				if s.UnhealthySince != nil {
					remaining = lease - now.Sub(s.UnhealthySince.Time)
				}
				if wait == 0 || remaining < wait {
					wait = remaining
				}
				r.updateFederationCondition(fed, "FailoverFenced", metav1.ConditionFalse, "AwaitingLease",
					fmt.Sprintf("%s is unreachable (%v); its %d workloads move after the lease timeout", s.Name, err, len(mine)))
				continue
			}
			t := now
			s.FencedAt = &t
			r.updateFederationCondition(fed, "FailoverFenced", metav1.ConditionTrue, s.FenceMethod,
				fmt.Sprintf("%s fenced (%s)", s.Name, s.FenceMethod))
			r.event(fed, "Warning", "MemberFenced", fmt.Sprintf("%s fenced by %s after %d failed health checks", s.Name, s.FenceMethod, s.ConsecutiveFailures))
		}
		for i := range mine {
			wl := &mine[i]
			if err := r.setActive(ctx, wl, false, map[string]interface{}{AnnotationFailoverFrom: s.Name}); err != nil {
				return 0, err
			}
			fed.Status.Failovers = append(fed.Status.Failovers, gryviav1.FederationFailoverRecord{
				Cluster: s.Name, Namespace: wl.GetNamespace(), Workload: wl.GetName(), FenceMethod: s.FenceMethod, EvictedAt: now,
			})
			r.event(fed, "Warning", "FailoverEvicted", fmt.Sprintf("Workload %s/%s evicted off %s", wl.GetNamespace(), wl.GetName(), s.Name))
		}
		if n := len(fed.Status.Failovers); n > maxFailoverRecords {
			fed.Status.Failovers = fed.Status.Failovers[n-maxFailoverRecords:]
		}
		if wait == 0 || wait > 5*time.Second {
			wait = 5 * time.Second // come back soon to reactivate
		}
	}
	return wait, nil
}

func markRequeued(fed *gryviav1.GryviaFederation, wl *unstructured.Unstructured, now metav1.Time) {
	for i := len(fed.Status.Failovers) - 1; i >= 0; i-- {
		rec := &fed.Status.Failovers[i]
		if rec.Namespace == wl.GetNamespace() && rec.Workload == wl.GetName() && rec.RequeuedAt == nil {
			t := now
			rec.RequeuedAt = &t
			return
		}
	}
}

func (r *GryviaFederationReconciler) event(fed *gryviav1.GryviaFederation, kind, reason, msg string) {
	if r.Recorder != nil {
		r.Recorder.Event(fed, kind, reason, msg)
	}
}
