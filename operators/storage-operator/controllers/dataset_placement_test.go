package controllers

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gryviav1 "github.com/zyvorai/gryvia/operators/storage-operator/api/v1"
)

func placed(mutate ...func(*gryviav1.GryviaDataset)) *gryviav1.GryviaDataset {
	return httpDataset("corpus", append([]func(*gryviav1.GryviaDataset){func(ds *gryviav1.GryviaDataset) {
		ds.Spec.Placement = &gryviav1.DatasetPlacement{StorageClass: "local-path", Pools: []gryviav1.DatasetPool{
			{Name: "zone-a", NodeSelector: map[string]string{"topology.kubernetes.io/zone": "a"}},
			{Name: "zone-b", NodeSelector: map[string]string{"topology.kubernetes.io/zone": "b"}},
		}}
	}}, mutate...)...)
}

func jobsByPool(t *testing.T, c client.Client) map[string]*batchv1.Job {
	t.Helper()
	jobs := &batchv1.JobList{}
	if err := c.List(context.Background(), jobs); err != nil {
		t.Fatal(err)
	}
	out := map[string]*batchv1.Job{}
	for i := range jobs.Items {
		out[jobs.Items[i].Labels[datasetPoolLabel]] = &jobs.Items[i]
	}
	return out
}

func TestPlacement_ReplicasAfterPrimaryAndVerified(t *testing.T) {
	r, c := newDatasetReconciler(placed())
	reconcileDataset(t, r, "corpus")
	if jobs := jobsByPool(t, c); len(jobs) != 1 || jobs[""] == nil {
		t.Fatalf("only the primary download may start before the primary is ready, got %v", jobs)
	}
	ds := getDataset(t, c, "corpus")
	if len(ds.Status.Replicas) != 2 || ds.Status.Replicas[0].Message != "Waiting for the primary copy" {
		t.Fatalf("replicas: %+v", ds.Status.Replicas)
	}
	pvc := &corev1.PersistentVolumeClaim{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "data", Name: "dataset-corpus-zone-a"}, pvc); err != nil {
		t.Fatalf("replica pvc: %v", err)
	}
	if *pvc.Spec.StorageClassName != "local-path" || pvc.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Errorf("replica pvc spec %+v", pvc.Spec)
	}

	finishJob(t, c, jobsByPool(t, c)[""], batchv1.JobComplete, `{"files":1,"bytes":2048,"sha256":"abc"}`)
	reconcileDataset(t, r, "corpus")
	jobs := jobsByPool(t, c)
	if len(jobs) != 3 {
		t.Fatalf("one download per pool expected, got %d jobs", len(jobs))
	}
	a := jobs["zone-a"]
	if a.Spec.Template.Spec.NodeSelector["topology.kubernetes.io/zone"] != "a" || a.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != "dataset-corpus-zone-a" {
		t.Fatalf("zone-a job not pinned to its pool: %+v", a.Spec.Template.Spec)
	}
	if env := envOf(a); env["KEEP"] != "latest" || env["URL"] == "" {
		t.Errorf("replica env %v", env)
	}

	finishJob(t, c, a, batchv1.JobComplete, `{"files":1,"bytes":2048,"sha256":"abc"}`)
	finishJob(t, c, jobs["zone-b"], batchv1.JobComplete, `{"files":1,"bytes":2048,"sha256":"changed"}`)
	reconcileDataset(t, r, "corpus")
	ds = getDataset(t, c, "corpus")
	got := map[string]gryviav1.DatasetReplica{}
	for _, rep := range ds.Status.Replicas {
		got[rep.Pool] = rep
	}
	if !got["zone-a"].Ready || !got["zone-a"].Verified || got["zone-a"].Version != "latest" || got["zone-a"].Bytes != 2048 {
		t.Errorf("zone-a: %+v", got["zone-a"])
	}
	if !got["zone-b"].Ready || got["zone-b"].Verified || !strings.Contains(got["zone-b"].Message, "differs") {
		t.Errorf("zone-b digest mismatch must not verify: %+v", got["zone-b"])
	}
	if cond := meta.FindStatusCondition(ds.Status.Conditions, conditionPlaced); cond == nil || cond.Status != "False" || !strings.Contains(cond.Message, "zone-b") {
		t.Errorf("Placed condition %+v", cond)
	}
}

func TestPlacement_WarmupStartsWithPrimary(t *testing.T) {
	r, c := newDatasetReconciler(placed(func(ds *gryviav1.GryviaDataset) {
		ds.Spec.Cache = &gryviav1.DatasetCache{Warmup: true}
	}))
	reconcileDataset(t, r, "corpus")
	jobs := jobsByPool(t, c)
	if len(jobs) != 3 {
		t.Fatalf("warmup starts every download at once, got %d", len(jobs))
	}
	finishJob(t, c, jobs["zone-a"], batchv1.JobComplete, `{"files":1,"bytes":1,"sha256":"abc"}`)
	reconcileDataset(t, r, "corpus")
	rep := getDataset(t, c, "corpus").Status.Replicas[0]
	if !rep.Ready || rep.Verified {
		t.Fatalf("a replica finished before the primary is ready but not verified: %+v", rep)
	}
	finishJob(t, c, jobs[""], batchv1.JobComplete, `{"files":1,"bytes":1,"sha256":"abc"}`)
	reconcileDataset(t, r, "corpus")
	if rep := getDataset(t, c, "corpus").Status.Replicas[0]; !rep.Verified {
		t.Fatalf("verified once the primary digest is known: %+v", rep)
	}
}

func TestPlacement_RemovedPoolIsPrunedAndInvalidReported(t *testing.T) {
	r, c := newDatasetReconciler(placed())
	reconcileDataset(t, r, "corpus")
	ds := getDataset(t, c, "corpus")
	ds.Spec.Placement.Pools = ds.Spec.Placement.Pools[:1]
	if err := c.Update(context.Background(), ds); err != nil {
		t.Fatal(err)
	}
	reconcileDataset(t, r, "corpus")
	pvc := &corev1.PersistentVolumeClaim{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "data", Name: "dataset-corpus-zone-b"}, pvc); err == nil {
		t.Fatal("the removed pool's replica PVC must be deleted")
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "data", Name: "dataset-corpus"}, pvc); err != nil {
		t.Fatal("the primary PVC must stay")
	}

	ds = getDataset(t, c, "corpus")
	ds.Spec.Placement.Pools = append(ds.Spec.Placement.Pools, ds.Spec.Placement.Pools[0])
	if err := c.Update(context.Background(), ds); err != nil {
		t.Fatal(err)
	}
	reconcileDataset(t, r, "corpus")
	cond := meta.FindStatusCondition(getDataset(t, c, "corpus").Status.Conditions, conditionPlaced)
	if cond == nil || cond.Reason != "InvalidSpec" || !strings.Contains(cond.Message, "twice") {
		t.Fatalf("duplicate pool: %+v", cond)
	}
}
