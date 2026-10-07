package controllers

import (
	"context"
	"errors"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

func failoverScheme() *runtime.Scheme {
	s := mlScheme()
	_ = batchv1.AddToScheme(s)
	s.AddKnownTypeWithName(kueueWorkloadGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(kueueWorkloadGVK.GroupVersion().WithKind("WorkloadList"), &unstructured.UnstructuredList{})
	return s
}

// workload builds a manager-side Kueue Workload owned by Job <name>, admitted on cluster (empty: not dispatched).
func kueueWl(name, cluster string, conds ...string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(kueueWorkloadGVK)
	u.SetNamespace("tenant-q1")
	u.SetName("job-" + name)
	u.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: name, UID: types.UID("uid-" + name)}})
	_ = unstructured.SetNestedField(u.Object, true, "spec", "active")
	if cluster != "" {
		_ = unstructured.SetNestedField(u.Object, cluster, "status", "clusterName")
	}
	var cs []interface{}
	for _, c := range conds {
		cs = append(cs, map[string]interface{}{"type": c, "status": "True"})
	}
	if cs != nil {
		_ = unstructured.SetNestedSlice(u.Object, cs, "status", "conditions")
	}
	return u
}

func federation(threshold int, lease string) *gryviav1.GryviaFederation {
	return &gryviav1.GryviaFederation{
		ObjectMeta: metav1.ObjectMeta{Name: "fed"},
		Spec: gryviav1.GryviaFederationSpec{
			Clusters: []gryviav1.FederationCluster{
				{Name: "w1", Enabled: true, APIServer: "https://w1.example:6443", MultiKueueCluster: "worker1"},
				{Name: "w2", Enabled: true, APIServer: "https://w2.example:6443"},
			},
			Failover: &gryviav1.FederationFailover{Enabled: true, Automatic: true, LeaseTimeout: lease,
				HealthCheck: &gryviav1.FederationHealthCheck{Interval: "5s", FailureThreshold: threshold}},
		},
	}
}

type failoverRig struct {
	r      *GryviaFederationReconciler
	c      client.Client
	remote client.Client
	clock  time.Time
}

// rig: both members fail the probe (no allowed servers); remote nil means w1's API server cannot be reached.
func newRig(t *testing.T, fed *gryviav1.GryviaFederation, remote client.Client, objs ...client.Object) *failoverRig {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(failoverScheme()).WithObjects(append(objs, fed)...).
		WithStatusSubresource(fed).Build()
	rig := &failoverRig{c: c, remote: remote, clock: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)}
	rig.r = &GryviaFederationReconciler{
		Client: c, Scheme: failoverScheme(), Log: ctrl.Log.WithName("test"), Failover: true,
		Recorder: record.NewFakeRecorder(50), Now: func() time.Time { return rig.clock },
		RemoteClient: func(_ context.Context, cl gryviav1.FederationCluster) (client.Client, error) {
			if rig.remote == nil || cl.Name != "w1" {
				return nil, errors.New("dial tcp: connection refused")
			}
			return rig.remote, nil
		},
	}
	return rig
}

func (g *failoverRig) reconcile(t *testing.T) ctrl.Result {
	t.Helper()
	res, err := g.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "fed"}})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (g *failoverRig) fed(t *testing.T) *gryviav1.GryviaFederation {
	t.Helper()
	f := &gryviav1.GryviaFederation{}
	if err := g.c.Get(context.Background(), types.NamespacedName{Name: "fed"}, f); err != nil {
		t.Fatal(err)
	}
	return f
}

func (g *failoverRig) workload(t *testing.T, name string) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(kueueWorkloadGVK)
	if err := g.c.Get(context.Background(), types.NamespacedName{Namespace: "tenant-q1", Name: "job-" + name}, u); err != nil {
		t.Fatal(err)
	}
	return u
}

func clusterStatus(f *gryviav1.GryviaFederation, name string) gryviav1.FederationClusterStatus {
	for _, s := range f.Status.ClusterStatus {
		if s.Name == name {
			return s
		}
	}
	return gryviav1.FederationClusterStatus{}
}

func TestFailoverWaitsForThresholdThenFencesRemotelyAndEvicts(t *testing.T) {
	remoteJob := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-q1", Name: "train"}}
	remoteWl := kueueWl("train", "")
	remote := fake.NewClientBuilder().WithScheme(failoverScheme()).WithObjects(remoteJob, remoteWl).Build()
	rig := newRig(t, federation(2, "1m"), remote,
		kueueWl("train", "worker1", "QuotaReserved", "Admitted"),
		kueueWl("other", "w2", "QuotaReserved", "Admitted"),
		kueueWl("done", "worker1", "QuotaReserved", "Finished"))

	rig.reconcile(t)
	if s := clusterStatus(rig.fed(t), "w1"); s.ConsecutiveFailures != 1 || s.Fenced || s.UnhealthySince == nil {
		t.Fatalf("after one failure: %+v", s)
	}
	if !workloadActive(rig.workload(t, "train")) {
		t.Fatal("evicted before the failure threshold")
	}

	rig.clock = rig.clock.Add(5 * time.Second)
	res := rig.reconcile(t)
	f := rig.fed(t)
	s := clusterStatus(f, "w1")
	if !s.Fenced || s.FenceMethod != FenceRemoteDelete || s.ConsecutiveFailures != 2 {
		t.Fatalf("w1 status = %+v, want fenced by remote delete", s)
	}
	if err := remote.Get(context.Background(), client.ObjectKeyFromObject(remoteJob), &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Errorf("remote Job still present: %v", err)
	}
	rw := &unstructured.Unstructured{}
	rw.SetGroupVersionKind(kueueWorkloadGVK)
	if err := remote.Get(context.Background(), client.ObjectKeyFromObject(remoteWl), rw); !apierrors.IsNotFound(err) {
		t.Errorf("remote Workload still present: %v", err)
	}
	wl := rig.workload(t, "train")
	if workloadActive(wl) || wl.GetAnnotations()[AnnotationFailoverFrom] != "w1" {
		t.Fatalf("train workload not deactivated: %v", wl.Object)
	}
	if !workloadActive(rig.workload(t, "other")) || !workloadActive(rig.workload(t, "done")) {
		t.Error("a workload on another member, or a finished one, was evicted")
	}
	if len(f.Status.Failovers) != 1 || f.Status.Failovers[0].Workload != "job-train" || f.Status.Failovers[0].FenceMethod != FenceRemoteDelete {
		t.Errorf("failovers = %+v", f.Status.Failovers)
	}
	if res.RequeueAfter != 5*time.Second {
		t.Errorf("requeue = %v, want 5s to reactivate", res.RequeueAfter)
	}

	// Kueue has not evicted yet: stays inactive.
	rig.reconcile(t)
	if workloadActive(rig.workload(t, "train")) {
		t.Fatal("reactivated before Kueue evicted it")
	}
	// Kueue evicts (Evicted, quota released): reactivated for redispatch.
	wl = rig.workload(t, "train")
	_ = unstructured.SetNestedSlice(wl.Object, []interface{}{map[string]interface{}{"type": "Evicted", "status": "True"}}, "status", "conditions")
	unstructured.RemoveNestedField(wl.Object, "status", "clusterName")
	if err := rig.c.Update(context.Background(), wl); err != nil {
		t.Fatal(err)
	}
	rig.reconcile(t)
	wl = rig.workload(t, "train")
	if !workloadActive(wl) || wl.GetAnnotations()[AnnotationFailoverFrom] != "" || wl.GetAnnotations()[AnnotationFailedOverFrom] != "w1" {
		t.Fatalf("not reactivated: %v", wl.Object)
	}
	if rec := rig.fed(t).Status.Failovers[0]; rec.RequeuedAt == nil {
		t.Errorf("record not marked requeued: %+v", rec)
	}
}

func TestFailoverUnreachableMemberWaitsForLease(t *testing.T) {
	rig := newRig(t, federation(1, "30s"), nil, kueueWl("train", "worker1", "QuotaReserved", "Admitted"))
	res := rig.reconcile(t)
	f := rig.fed(t)
	if s := clusterStatus(f, "w1"); s.Fenced {
		t.Fatalf("fenced before the lease timeout: %+v", s)
	}
	if !workloadActive(rig.workload(t, "train")) {
		t.Fatal("evicted an unfenced member's workload")
	}
	if res.RequeueAfter != 5*time.Second && res.RequeueAfter != 30*time.Second {
		t.Errorf("requeue = %v", res.RequeueAfter)
	}
	if c := meta.FindStatusCondition(f.Status.Conditions, "FailoverFenced"); c == nil || c.Reason != "AwaitingLease" {
		t.Errorf("FailoverFenced = %+v", c)
	}

	rig.clock = rig.clock.Add(31 * time.Second)
	rig.reconcile(t)
	if s := clusterStatus(rig.fed(t), "w1"); !s.Fenced || s.FenceMethod != FenceLeaseExpired {
		t.Fatalf("w1 = %+v, want fenced by lease", s)
	}
	if workloadActive(rig.workload(t, "train")) {
		t.Fatal("workload not evicted after the lease")
	}
}

func TestFailoverOffUnlessFlagAndAutomatic(t *testing.T) {
	for name, mutate := range map[string]func(*GryviaFederationReconciler, *gryviav1.GryviaFederation){
		"flag off":      func(r *GryviaFederationReconciler, _ *gryviav1.GryviaFederation) { r.Failover = false },
		"not automatic": func(_ *GryviaFederationReconciler, f *gryviav1.GryviaFederation) { f.Spec.Failover.Automatic = false },
		"not enabled":   func(_ *GryviaFederationReconciler, f *gryviav1.GryviaFederation) { f.Spec.Failover.Enabled = false },
	} {
		t.Run(name, func(t *testing.T) {
			fed := federation(1, "1s")
			remote := fake.NewClientBuilder().WithScheme(failoverScheme()).Build()
			rig := newRig(t, fed, remote, kueueWl("train", "worker1", "QuotaReserved", "Admitted"))
			mutate(rig.r, fed)
			if err := rig.c.Update(context.Background(), fed); err != nil {
				t.Fatal(err)
			}
			rig.reconcile(t)
			rig.clock = rig.clock.Add(time.Minute)
			rig.reconcile(t)
			if !workloadActive(rig.workload(t, "train")) {
				t.Fatal("failover acted while disabled")
			}
			if s := clusterStatus(rig.fed(t), "w1"); s.Fenced || s.ConsecutiveFailures != 2 {
				t.Errorf("status = %+v (streak is tracked, fence is not)", s)
			}
		})
	}
}

func TestTrackHealthResetsOnRecovery(t *testing.T) {
	now := metav1.NewTime(time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC))
	since := metav1.NewTime(now.Add(-time.Minute))
	prev := []gryviav1.FederationClusterStatus{{Name: "w1", State: "unhealthy", ConsecutiveFailures: 4, UnhealthySince: &since, Fenced: true, FenceMethod: FenceLeaseExpired}}
	cur := []gryviav1.FederationClusterStatus{{Name: "w1", State: "unhealthy"}}
	trackHealth(prev, cur, now)
	if cur[0].ConsecutiveFailures != 5 || !cur[0].UnhealthySince.Equal(&since) || !cur[0].Fenced {
		t.Errorf("carried = %+v", cur[0])
	}
	cur = []gryviav1.FederationClusterStatus{{Name: "w1", State: "healthy"}}
	trackHealth(prev, cur, now)
	if cur[0].ConsecutiveFailures != 0 || cur[0].UnhealthySince != nil || cur[0].Fenced {
		t.Errorf("not reset: %+v", cur[0])
	}
}

func TestAssignedToFallsBackToAdmissionCheckMessage(t *testing.T) {
	wl := kueueWl("x", "")
	_ = unstructured.SetNestedSlice(wl.Object, []interface{}{map[string]interface{}{
		"name": "multikueue", "state": "Ready", "message": `The workload got reservation on "worker1"`}}, "status", "admissionChecks")
	if !assignedTo(wl, "worker1") || assignedTo(wl, "worker") {
		t.Error("admission check message not matched exactly")
	}
	if !assignedTo(kueueWl("y", "worker2"), "worker2") || assignedTo(kueueWl("y", "worker2"), "worker1") {
		t.Error("status.clusterName not used")
	}
}
