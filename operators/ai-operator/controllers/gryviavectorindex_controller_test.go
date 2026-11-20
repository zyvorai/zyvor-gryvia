// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controllers

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/ai-operator/pkg/llmgateway"
)

const ragKeyNS = "gryvia-llm-keys"

func ragScheme() *runtime.Scheme {
	s := mlScheme()
	_ = batchv1.AddToScheme(s)
	s.AddKnownTypeWithName(datasetGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(datasetGVK.GroupVersion().WithKind("GryviaDatasetList"), &unstructured.UnstructuredList{})
	return s
}

func ragClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(ragScheme()).WithObjects(objs...).
		WithStatusSubresource(&gryviav1.GryviaVectorIndex{}, &batchv1.Job{}, &appsv1.StatefulSet{}).Build()
}

func newRAGReconciler(c client.Client, clk *fakeClock) *GryviaVectorIndexReconciler {
	return &GryviaVectorIndexReconciler{Client: c, Scheme: ragScheme(), Log: ctrl.Log.WithName("test"),
		GatewayURL: "http://gw:8080", KeyNamespace: ragKeyNS, IngestImage: "ingest:1", Clock: clk.Now}
}

func newIndex(name string, mutate ...func(*gryviav1.GryviaVectorIndex)) *gryviav1.GryviaVectorIndex {
	idx := &gryviav1.GryviaVectorIndex{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: name, Generation: 1,
			CreationTimestamp: metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))},
		Spec: gryviav1.GryviaVectorIndexSpec{
			DatasetRef: "docs",
			Embedding:  gryviav1.VectorEmbedding{Model: "embed"},
			Store:      gryviav1.VectorStore{Type: gryviav1.VectorStoreManaged},
		},
	}
	for _, f := range mutate {
		f(idx)
	}
	return idx
}

func readyDataset(version string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": "docs"},
		"spec":     map[string]interface{}{"namespace": "tenant-a"},
		"status": map[string]interface{}{"state": "ready", "currentVersion": version, "namespace": "tenant-a",
			"pvcName": "docs-data", "subPath": "versions/" + version},
	}}
	u.SetGroupVersionKind(datasetGVK)
	return u
}

func getIndex(t *testing.T, c client.Client, name string) *gryviav1.GryviaVectorIndex {
	t.Helper()
	idx := &gryviav1.GryviaVectorIndex{}
	mustGet(t, c, "tenant-a", name, idx)
	return idx
}

func markStoreReady(t *testing.T, c client.Client, name string) {
	t.Helper()
	sts := &appsv1.StatefulSet{}
	mustGet(t, c, "tenant-a", name+"-qdrant", sts)
	sts.Status.ReadyReplicas = 1
	if err := c.Status().Update(context.Background(), sts); err != nil {
		t.Fatal(err)
	}
}

// finishJob marks the Job complete (or failed) and gives it a pod with the given termination message.
func finishJob(t *testing.T, c client.Client, name string, ok bool, message string) {
	t.Helper()
	ctx := context.Background()
	job := &batchv1.Job{}
	mustGet(t, c, "tenant-a", name, job)
	cond, phase := batchv1.JobComplete, corev1.PodSucceeded
	if !ok {
		cond, phase = batchv1.JobFailed, corev1.PodFailed
	}
	job.Status.Conditions = append(job.Status.Conditions, batchv1.JobCondition{Type: cond, Status: corev1.ConditionTrue})
	if err := c.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: name + "-pod", Labels: map[string]string{"job-name": name}}}
	if err := c.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.Phase = phase
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "ingest", State: corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{ExitCode: map[bool]int32{true: 0, false: 1}[ok], Message: message}}}}
	if err := c.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
}

func ingestJobs(t *testing.T, c client.Client) []batchv1.Job {
	t.Helper()
	var jobs batchv1.JobList
	if err := c.List(context.Background(), &jobs, client.InNamespace("tenant-a")); err != nil {
		t.Fatal(err)
	}
	return jobs.Items
}

func containerEnv(c corev1.Container) map[string]corev1.EnvVar {
	out := map[string]corev1.EnvVar{}
	for _, e := range c.Env {
		out[e.Name] = e
	}
	return out
}

func TestVectorIndexManagedLifecycle(t *testing.T) {
	clk := newClock()
	c := ragClient(newIndex("kb"))
	r := newRAGReconciler(c, clk)

	reconcileOnce(t, r, "tenant-a", "kb")
	idx := getIndex(t, c, "kb")
	if idx.Status.Phase != gryviav1.VectorIndexProvisioning {
		t.Fatalf("phase = %q (%s), want Provisioning", idx.Status.Phase, idx.Status.Message)
	}
	if len(idx.Finalizers) != 1 || idx.Finalizers[0] != vectorIndexFinalizer {
		t.Fatalf("finalizers = %v", idx.Finalizers)
	}
	if idx.Status.StoreURL != "http://kb-qdrant.tenant-a.svc.cluster.local:6333" {
		t.Fatalf("storeURL = %q", idx.Status.StoreURL)
	}
	sts := &appsv1.StatefulSet{}
	mustGet(t, c, "tenant-a", "kb-qdrant", sts)
	if img := sts.Spec.Template.Spec.Containers[0].Image; img != DefaultQdrantImage {
		t.Fatalf("qdrant image = %q", img)
	}
	if got := sts.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests.Storage().String(); got != "10Gi" {
		t.Fatalf("storage = %s", got)
	}
	if !mlObjectExists(c, "tenant-a", "kb-qdrant", &corev1.Service{}) {
		t.Fatal("qdrant Service missing")
	}

	// Store ready, dataset missing: the key is issued and the index waits.
	markStoreReady(t, c, "kb")
	reconcileOnce(t, r, "tenant-a", "kb")
	idx = getIndex(t, c, "kb")
	if idx.Status.Phase != gryviav1.VectorIndexPending || !strings.Contains(idx.Status.Message, "not found") {
		t.Fatalf("phase = %q (%s), want Pending on a missing dataset", idx.Status.Phase, idx.Status.Message)
	}
	raw := &corev1.Secret{}
	mustGet(t, c, "tenant-a", "kb-llm-key", raw)
	key := string(raw.Data[llmgateway.OwnedKeyField])
	if !strings.HasPrefix(key, "gk-") || len(raw.OwnerReferences) != 1 {
		t.Fatalf("raw key secret = %v owners %v", raw.Data, raw.OwnerReferences)
	}
	hashed := &corev1.Secret{}
	mustGet(t, c, ragKeyNS, "tenant-a.index-kb", hashed)
	if string(hashed.Data["hash"]) != llmgateway.HashKey(key) || hashed.Labels[llmgateway.LabelKey] != "true" ||
		hashed.Labels[llmgateway.LabelTenant] != "a" {
		t.Fatalf("hashed key secret = %v labels %v", hashed.Data, hashed.Labels)
	}

	// Dataset ready: an ingestion Job starts.
	if err := c.Create(context.Background(), readyDataset("v1")); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "tenant-a", "kb")
	idx = getIndex(t, c, "kb")
	if idx.Status.Phase != gryviav1.VectorIndexIngesting {
		t.Fatalf("phase = %q (%s), want Ingesting", idx.Status.Phase, idx.Status.Message)
	}
	jobs := ingestJobs(t, c)
	if len(jobs) != 1 || jobs[0].Name != idx.Status.IngestJob {
		t.Fatalf("jobs = %d, status.ingestJob %q", len(jobs), idx.Status.IngestJob)
	}
	ctr := jobs[0].Spec.Template.Spec.Containers[0]
	env := containerEnv(ctr)
	for k, want := range map[string]string{"GATEWAY_URL": "http://gw:8080", "EMBED_MODEL": "embed", "COLLECTION": "kb",
		"CHUNK_SIZE": "1000", "CHUNK_OVERLAP": "200", "EMBED_BATCH": "32", "DATASET_VERSION": "v1",
		"STORE_URL": idx.Status.StoreURL, "RUN_ID": jobs[0].Name} {
		if env[k].Value != want {
			t.Errorf("env %s = %q, want %q", k, env[k].Value, want)
		}
	}
	if ref := env["GRYVIA_LLM_KEY"].ValueFrom; ref == nil || ref.SecretKeyRef.Name != "kb-llm-key" {
		t.Errorf("GRYVIA_LLM_KEY = %+v", env["GRYVIA_LLM_KEY"])
	}
	if _, ok := env["STORE_API_KEY"]; ok {
		t.Error("managed store must not get STORE_API_KEY")
	}
	if ctr.Image != "ingest:1" || ctr.VolumeMounts[0].SubPath != "versions/v1" || !ctr.VolumeMounts[0].ReadOnly {
		t.Errorf("container = %s mounts %+v", ctr.Image, ctr.VolumeMounts)
	}
	if v := jobs[0].Spec.Template.Spec.Volumes[0].PersistentVolumeClaim; v == nil || v.ClaimName != "docs-data" {
		t.Errorf("volume = %+v", jobs[0].Spec.Template.Spec.Volumes)
	}

	// Still running: same Job.
	reconcileOnce(t, r, "tenant-a", "kb")
	if n := len(ingestJobs(t, c)); n != 1 {
		t.Fatalf("jobs = %d while running", n)
	}

	finishJob(t, c, jobs[0].Name, true, `{"documents": 3, "chunks": 12, "dimensions": 384}`)
	reconcileOnce(t, r, "tenant-a", "kb")
	idx = getIndex(t, c, "kb")
	if idx.Status.Phase != gryviav1.VectorIndexReady || idx.Status.Documents != 3 || idx.Status.Chunks != 12 ||
		idx.Status.Dimensions != 384 || idx.Status.DatasetVersion != "v1" || idx.Status.LastIngested == nil {
		t.Fatalf("status = %+v", idx.Status)
	}

	// Up to date: nothing new.
	reconcileOnce(t, r, "tenant-a", "kb")
	if n := len(ingestJobs(t, c)); n != 1 {
		t.Fatalf("jobs = %d after Ready", n)
	}
	if getIndex(t, c, "kb").Status.Phase != gryviav1.VectorIndexReady {
		t.Fatal("index left Ready without a change")
	}

	// A new dataset version re-ingests, and the index keeps serving meanwhile.
	ds := readyDataset("v2")
	cur := &unstructured.Unstructured{}
	cur.SetGroupVersionKind(datasetGVK)
	mustGet(t, c, "", "docs", cur)
	ds.SetResourceVersion(cur.GetResourceVersion())
	if err := c.Update(context.Background(), ds); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "tenant-a", "kb")
	if n := len(ingestJobs(t, c)); n != 2 {
		t.Fatalf("jobs = %d after a new dataset version", n)
	}
	idx = getIndex(t, c, "kb")
	if idx.Status.Phase != gryviav1.VectorIndexIngesting || idx.Status.LastIngested == nil {
		t.Fatalf("status = %+v", idx.Status)
	}
}

func TestVectorIndexFailedIngestAndReingest(t *testing.T) {
	clk := newClock()
	c := ragClient(newIndex("kb"), readyDataset("v1"))
	r := newRAGReconciler(c, clk)
	reconcileOnce(t, r, "tenant-a", "kb")
	markStoreReady(t, c, "kb")
	reconcileOnce(t, r, "tenant-a", "kb")
	first := getIndex(t, c, "kb").Status.IngestJob
	finishJob(t, c, first, false, "embedding model embed is not served")
	reconcileOnce(t, r, "tenant-a", "kb")
	idx := getIndex(t, c, "kb")
	if idx.Status.Phase != gryviav1.VectorIndexFailed || !strings.Contains(idx.Status.Message, "not served") ||
		!strings.Contains(idx.Status.Message, annotationReingest) {
		t.Fatalf("status = %q %q", idx.Status.Phase, idx.Status.Message)
	}
	// Failed stays failed without a change.
	reconcileOnce(t, r, "tenant-a", "kb")
	if n := len(ingestJobs(t, c)); n != 1 {
		t.Fatalf("jobs = %d after a failure", n)
	}

	idx.Annotations = map[string]string{annotationReingest: "1"}
	if err := c.Update(context.Background(), idx); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "tenant-a", "kb")
	idx = getIndex(t, c, "kb")
	if idx.Status.IngestJob == first || idx.Status.Phase != gryviav1.VectorIndexIngesting {
		t.Fatalf("reingest did not start a new Job: %+v", idx.Status)
	}
}

func TestVectorIndexSchedule(t *testing.T) {
	clk := newClock()
	c := ragClient(newIndex("kb", func(i *gryviav1.GryviaVectorIndex) { i.Spec.Schedule = "0 * * * *" }), readyDataset("v1"))
	r := newRAGReconciler(c, clk)
	reconcileOnce(t, r, "tenant-a", "kb")
	markStoreReady(t, c, "kb")
	reconcileOnce(t, r, "tenant-a", "kb")
	finishJob(t, c, getIndex(t, c, "kb").Status.IngestJob, true, `{"documents": 1, "chunks": 1, "dimensions": 8}`)
	res := reconcileOnce(t, r, "tenant-a", "kb")
	idx := getIndex(t, c, "kb")
	if idx.Status.Phase != gryviav1.VectorIndexReady || idx.Status.NextScheduleTime == nil || res.RequeueAfter <= 0 {
		t.Fatalf("status = %+v requeue %v", idx.Status, res.RequeueAfter)
	}
	// LastIngested is the fake clock time (12:00); the next tick is 13:00.
	clk.Add(30 * time.Minute)
	reconcileOnce(t, r, "tenant-a", "kb")
	if n := len(ingestJobs(t, c)); n != 1 {
		t.Fatalf("jobs = %d before the tick", n)
	}
	clk.Add(31 * time.Minute)
	reconcileOnce(t, r, "tenant-a", "kb")
	jobs := ingestJobs(t, c)
	if len(jobs) != 2 {
		t.Fatalf("jobs = %d after the tick", len(jobs))
	}
	scheduled := getIndex(t, c, "kb").Status.IngestJob
	finishJob(t, c, scheduled, true, `{"documents": 2, "chunks": 2, "dimensions": 8}`)
	reconcileOnce(t, r, "tenant-a", "kb")
	reconcileOnce(t, r, "tenant-a", "kb")
	idx = getIndex(t, c, "kb")
	if idx.Status.Phase != gryviav1.VectorIndexReady || idx.Status.Documents != 2 || len(ingestJobs(t, c)) != 2 {
		t.Fatalf("after the scheduled run: %+v (%d jobs)", idx.Status, len(ingestJobs(t, c)))
	}
}

func TestVectorIndexSuspend(t *testing.T) {
	c := ragClient(newIndex("kb", func(i *gryviav1.GryviaVectorIndex) { i.Spec.Suspend = true }), readyDataset("v1"))
	r := newRAGReconciler(c, newClock())
	reconcileOnce(t, r, "tenant-a", "kb")
	markStoreReady(t, c, "kb")
	reconcileOnce(t, r, "tenant-a", "kb")
	if n := len(ingestJobs(t, c)); n != 0 {
		t.Fatalf("suspended index started %d jobs", n)
	}
	if p := getIndex(t, c, "kb").Status.Phase; p != gryviav1.VectorIndexPending {
		t.Fatalf("phase = %q", p)
	}
}

func TestVectorIndexExternalStore(t *testing.T) {
	cred := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: "qdrant-cred"}, Data: map[string][]byte{"apiKey": []byte("s3cret")}}
	c := ragClient(cred, readyDataset("v1"), newIndex("kb", func(i *gryviav1.GryviaVectorIndex) {
		i.Spec.Store = gryviav1.VectorStore{Type: gryviav1.VectorStoreExternal, External: &gryviav1.ExternalVectorStore{
			URL: "https://qdrant.example.com", APIKeySecretRef: &gryviav1.SecretKeyRef{Name: "qdrant-cred", Key: "apiKey"}}}
		i.Spec.Collection = "handbook"
		i.Spec.Chunking = &gryviav1.VectorChunking{Size: 500}
	}))
	r := newRAGReconciler(c, newClock())
	reconcileOnce(t, r, "tenant-a", "kb")
	if mlObjectExists(c, "tenant-a", "kb-qdrant", &appsv1.StatefulSet{}) {
		t.Fatal("external store must not create a Qdrant")
	}
	mirror := &corev1.Secret{}
	mustGet(t, c, ragKeyNS, llmgateway.StoreSecretName("tenant-a", "kb"), mirror)
	if string(mirror.Data["apiKey"]) != "s3cret" || mirror.Labels[llmgateway.LabelKey] != llmgateway.KeyStore {
		t.Fatalf("mirror = %v %v", mirror.Data, mirror.Labels)
	}
	idx := getIndex(t, c, "kb")
	if idx.Status.StoreURL != "https://qdrant.example.com" || idx.Status.Collection != "handbook" || idx.Status.Phase != gryviav1.VectorIndexIngesting {
		t.Fatalf("status = %+v", idx.Status)
	}
	env := containerEnv(ingestJobs(t, c)[0].Spec.Template.Spec.Containers[0])
	if env["STORE_API_KEY"].ValueFrom == nil || env["STORE_API_KEY"].ValueFrom.SecretKeyRef.Key != "apiKey" {
		t.Errorf("STORE_API_KEY = %+v", env["STORE_API_KEY"])
	}
	if env["CHUNK_SIZE"].Value != "500" || env["CHUNK_OVERLAP"].Value != "0" || env["COLLECTION"].Value != "handbook" {
		t.Errorf("chunking/collection env = %s/%s/%s", env["CHUNK_SIZE"].Value, env["CHUNK_OVERLAP"].Value, env["COLLECTION"].Value)
	}
}

func TestVectorIndexValidation(t *testing.T) {
	cases := map[string]func(*gryviav1.GryviaVectorIndex){
		"external needs url": func(i *gryviav1.GryviaVectorIndex) {
			i.Spec.Store = gryviav1.VectorStore{Type: gryviav1.VectorStoreExternal}
		},
		"overlap": func(i *gryviav1.GryviaVectorIndex) {
			i.Spec.Chunking = &gryviav1.VectorChunking{Size: 100, Overlap: 100}
		},
		"schedule": func(i *gryviav1.GryviaVectorIndex) { i.Spec.Schedule = "every day" },
		"storage size": func(i *gryviav1.GryviaVectorIndex) {
			i.Spec.Store.Managed = &gryviav1.ManagedVectorStore{StorageSize: "lots"}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := ragClient(newIndex("kb", mutate))
			reconcileOnce(t, newRAGReconciler(c, newClock()), "tenant-a", "kb")
			idx := getIndex(t, c, "kb")
			if idx.Status.Phase != gryviav1.VectorIndexFailed || !strings.HasPrefix(idx.Status.Message, "Invalid index") {
				t.Fatalf("status = %q %q", idx.Status.Phase, idx.Status.Message)
			}
		})
	}
}

func TestVectorIndexDatasetInOtherNamespace(t *testing.T) {
	ds := readyDataset("v1")
	_ = unstructured.SetNestedField(ds.Object, "shared", "status", "namespace")
	c := ragClient(newIndex("kb"), ds)
	r := newRAGReconciler(c, newClock())
	reconcileOnce(t, r, "tenant-a", "kb")
	markStoreReady(t, c, "kb")
	reconcileOnce(t, r, "tenant-a", "kb")
	idx := getIndex(t, c, "kb")
	if idx.Status.Phase != gryviav1.VectorIndexFailed || !strings.Contains(idx.Status.Message, "namespace shared") {
		t.Fatalf("status = %q %q", idx.Status.Phase, idx.Status.Message)
	}
}

func TestVectorIndexDeletionRemovesKeys(t *testing.T) {
	c := ragClient(newIndex("kb"), readyDataset("v1"))
	r := newRAGReconciler(c, newClock())
	reconcileOnce(t, r, "tenant-a", "kb")
	markStoreReady(t, c, "kb")
	reconcileOnce(t, r, "tenant-a", "kb")
	if !mlObjectExists(c, ragKeyNS, "tenant-a.index-kb", &corev1.Secret{}) {
		t.Fatal("hashed key missing")
	}
	if err := c.Delete(context.Background(), getIndex(t, c, "kb")); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "tenant-a", "kb")
	if mlObjectExists(c, ragKeyNS, "tenant-a.index-kb", &corev1.Secret{}) {
		t.Fatal("hashed key survived the index")
	}
	if mlObjectExists(c, "tenant-a", "kb", &gryviav1.GryviaVectorIndex{}) {
		t.Fatal("finalizer not removed")
	}
}

func TestVectorIndexRevokedKeyIsReissued(t *testing.T) {
	c := ragClient(newIndex("kb"), readyDataset("v1"))
	r := newRAGReconciler(c, newClock())
	reconcileOnce(t, r, "tenant-a", "kb")
	markStoreReady(t, c, "kb")
	reconcileOnce(t, r, "tenant-a", "kb")
	h := &corev1.Secret{}
	mustGet(t, c, ragKeyNS, "tenant-a.index-kb", h)
	if err := c.Delete(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "tenant-a", "kb")
	if !mlObjectExists(c, ragKeyNS, "tenant-a.index-kb", &corev1.Secret{}) {
		t.Fatal("hashed key not recreated")
	}
}

func TestIndexesOfDataset(t *testing.T) {
	c := ragClient(newIndex("a"), newIndex("b", func(i *gryviav1.GryviaVectorIndex) { i.Spec.DatasetRef = "other" }))
	r := newRAGReconciler(c, newClock())
	reqs := r.indexesOfDataset(context.Background(), readyDataset("v1"))
	if len(reqs) != 1 || reqs[0].Name != "a" {
		t.Fatalf("requests = %v", reqs)
	}
}
