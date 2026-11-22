// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gryviav1 "github.com/zyvorai/gryvia/operators/storage-operator/api/v1"
)

func datasetScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = gryviav1.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	_ = batchv1.AddToScheme(s)
	return s
}

func newDatasetReconciler(objs ...client.Object) (*GryviaDatasetReconciler, client.Client) {
	s := datasetScheme()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).WithStatusSubresource(&gryviav1.GryviaDataset{}).Build()
	return &GryviaDatasetReconciler{Client: c, Scheme: s, Log: ctrl.Log.WithName("test"), DefaultNamespace: "data",
		Image: "busybox:1.36", S3Image: "amazon/aws-cli:2.17.0", DefaultSize: "10Gi"}, c
}

func httpDataset(name string, mutate ...func(*gryviav1.GryviaDataset)) *gryviav1.GryviaDataset {
	ds := &gryviav1.GryviaDataset{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(name + "-uid"), Generation: 1},
		Spec: gryviav1.GryviaDatasetSpec{
			Type:   "text",
			Source: gryviav1.DatasetSource{Type: "http", HTTP: &gryviav1.HTTPSource{URL: "https://example.com/corpus.jsonl", ChecksumURL: "https://example.com/corpus.jsonl.sha256"}},
		},
	}
	for _, f := range mutate {
		f(ds)
	}
	return ds
}

func reconcileDataset(t *testing.T, r *GryviaDatasetReconciler, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return res
}

func getDataset(t *testing.T, c client.Client, name string) *gryviav1.GryviaDataset {
	t.Helper()
	ds := &gryviav1.GryviaDataset{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, ds); err != nil {
		t.Fatal(err)
	}
	return ds
}

func onlyJob(t *testing.T, c client.Client) *batchv1.Job {
	t.Helper()
	jobs := &batchv1.JobList{}
	if err := c.List(context.Background(), jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("jobs = %d, want 1", len(jobs.Items))
	}
	return &jobs.Items[0]
}

func envOf(job *batchv1.Job) map[string]string {
	out := map[string]string{}
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		out[e.Name] = e.Value
	}
	return out
}

// finishJob marks the Job complete (or failed) and gives it a pod whose container wrote msg.
func finishJob(t *testing.T, c client.Client, job *batchv1.Job, cond batchv1.JobConditionType, msg string) {
	t.Helper()
	job.Status.Conditions = append(job.Status.Conditions, batchv1.JobCondition{Type: cond, Status: corev1.ConditionTrue})
	if err := c.Status().Update(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-abcde", Namespace: job.Namespace, Labels: map[string]string{"job-name": job.Name}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "download",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: msg}}}}},
	}
	if err := c.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
}

func TestDataset_HTTPMaterializedIntoPVC(t *testing.T) {
	r, c := newDatasetReconciler(httpDataset("corpus"))
	if res := reconcileDataset(t, r, "corpus"); res.RequeueAfter == 0 {
		t.Error("a running download must requeue")
	}

	pvc := &corev1.PersistentVolumeClaim{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "data", Name: "dataset-corpus"}, pvc); err != nil {
		t.Fatalf("pvc: %v", err)
	}
	if q := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; q.String() != "10Gi" || !metav1.IsControlledBy(pvc, getDataset(t, c, "corpus")) {
		t.Errorf("pvc size %s, owners %v", q.String(), pvc.OwnerReferences)
	}

	job := onlyJob(t, c)
	env := envOf(job)
	if env["SOURCE"] != "http" || env["URL"] != "https://example.com/corpus.jsonl" || env["CHECKSUM_URL"] == "" || env["VERSION"] != "latest" || env["KEEP"] != "latest" {
		t.Errorf("env: %v", env)
	}
	if env["HASH"] != job.Labels["gryvia.io/dataset-source-hash"] || env["HASH"] == "" || env["CURRENT"] != "" {
		t.Errorf("resume env: HASH %q CURRENT %q", env["HASH"], env["CURRENT"])
	}
	pod := job.Spec.Template.Spec
	if pod.Containers[0].Image != "busybox:1.36" || pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot {
		t.Errorf("pod: image %s, security %+v", pod.Containers[0].Image, pod.SecurityContext)
	}
	if pod.Volumes[0].PersistentVolumeClaim.ClaimName != "dataset-corpus" {
		t.Errorf("volumes: %+v", pod.Volumes)
	}
	ds := getDataset(t, c, "corpus")
	if ds.Status.State != DatasetStateSyncing || ds.Status.PVCName != "dataset-corpus" || ds.Status.Namespace != "data" {
		t.Errorf("status: %+v", ds.Status)
	}

	finishJob(t, c, job, batchv1.JobComplete, `{"files":1,"bytes":2048,"sha256":"abc"}`)
	reconcileDataset(t, r, "corpus")
	ds = getDataset(t, c, "corpus")
	st := ds.Status
	if st.State != DatasetStateReady || st.CurrentVersion != "latest" || st.SubPath != "latest" || st.FileCount != 1 || st.TotalSizeBytes != 2048 {
		t.Fatalf("status: %+v", st)
	}
	if len(st.Versions) != 1 || st.Versions[0].Checksum != "abc" || st.Versions[0].Size != 2048 {
		t.Errorf("versions: %+v", st.Versions)
	}

	// Ready and unchanged: no new Job.
	reconcileDataset(t, r, "corpus")
	onlyJob(t, c)
}

func TestDataset_NewVersionKeepsRetention(t *testing.T) {
	r, c := newDatasetReconciler(httpDataset("corpus", func(ds *gryviav1.GryviaDataset) {
		ds.Spec.Version = "v1"
		ds.Spec.Versioning = &gryviav1.DatasetVersioning{Enabled: true, RetentionPolicy: &gryviav1.RetentionPolicy{KeepLast: 2}}
	}))
	for i, v := range []string{"v1", "v2", "v3"} {
		if i > 0 {
			ds := getDataset(t, c, "corpus")
			ds.Spec.Version = v
			if err := c.Update(context.Background(), ds); err != nil {
				t.Fatal(err)
			}
		}
		reconcileDataset(t, r, "corpus")
		jobs := &batchv1.JobList{}
		if err := c.List(context.Background(), jobs, client.InNamespace("data")); err != nil {
			t.Fatal(err)
		}
		var job *batchv1.Job
		for i := range jobs.Items {
			if envOf(&jobs.Items[i])["VERSION"] == v {
				job = &jobs.Items[i]
			}
		}
		if job == nil {
			t.Fatalf("no job for %s", v)
		}
		if v == "v3" && envOf(job)["KEEP"] != "v2 v3" {
			t.Errorf("KEEP for v3 = %q, want the newest two", envOf(job)["KEEP"])
		}
		if v == "v3" && envOf(job)["CURRENT"] != "v2" {
			t.Errorf("CURRENT for v3 = %q, want v2 (the version a new s3 directory starts from)", envOf(job)["CURRENT"])
		}
		finishJob(t, c, job, batchv1.JobComplete, `{"files":1,"bytes":1,"sha256":"`+v+`"}`)
		reconcileDataset(t, r, "corpus")
	}
	st := getDataset(t, c, "corpus").Status
	if st.CurrentVersion != "v3" || len(st.Versions) != 2 || st.Versions[0].Version != "v2" || st.Versions[1].Version != "v3" {
		t.Errorf("status: current %s, versions %+v", st.CurrentVersion, st.Versions)
	}
}

func TestDataset_SourceChangeDownloadsAgain(t *testing.T) {
	r, c := newDatasetReconciler(httpDataset("corpus"))
	reconcileDataset(t, r, "corpus")
	finishJob(t, c, onlyJob(t, c), batchv1.JobComplete, `{"files":1,"bytes":1,"sha256":"a"}`)
	reconcileDataset(t, r, "corpus")
	ds := getDataset(t, c, "corpus")
	ds.Spec.Source.HTTP.URL = "https://example.com/corpus-2.jsonl"
	if err := c.Update(context.Background(), ds); err != nil {
		t.Fatal(err)
	}
	reconcileDataset(t, r, "corpus")
	jobs := &batchv1.JobList{}
	if err := c.List(context.Background(), jobs); err != nil || len(jobs.Items) != 2 {
		t.Fatalf("jobs = %d, err %v", len(jobs.Items), err)
	}
	if getDataset(t, c, "corpus").Status.State != DatasetStateSyncing {
		t.Error("a changed source must sync again")
	}
}

func TestDataset_FailureIsReported(t *testing.T) {
	r, c := newDatasetReconciler(httpDataset("corpus"))
	reconcileDataset(t, r, "corpus")
	finishJob(t, c, onlyJob(t, c), batchv1.JobFailed, `{"error":"sha256 mismatch: want a, got b"}`)
	reconcileDataset(t, r, "corpus")
	st := getDataset(t, c, "corpus").Status
	if st.State != DatasetStateError || !strings.Contains(st.Message, "sha256 mismatch") {
		t.Errorf("status: %+v", st)
	}
}

func TestDataset_S3AndNFSJobs(t *testing.T) {
	r, c := newDatasetReconciler(
		httpDataset("s3data", func(ds *gryviav1.GryviaDataset) {
			ds.Spec.Namespace = "team-a"
			ds.Spec.Source = gryviav1.DatasetSource{Type: "s3", S3: &gryviav1.S3Source{Bucket: "b", Prefix: "p/", Region: "eu-west-1", CredentialsSecret: "aws",
				Endpoint: "http://minio.storage.svc:9000"}}
			ds.Spec.Cache = &gryviav1.DatasetCache{StorageClass: "fast", Size: "50Gi"}
		}),
		httpDataset("nfsdata", func(ds *gryviav1.GryviaDataset) {
			ds.Spec.Source = gryviav1.DatasetSource{Type: "nfs", NFS: &gryviav1.NFSSource{Server: "10.0.0.1", Path: "/export/data"}}
		}))
	reconcileDataset(t, r, "s3data")
	reconcileDataset(t, r, "nfsdata")

	jobs := &batchv1.JobList{}
	if err := c.List(context.Background(), jobs, client.InNamespace("team-a")); err != nil || len(jobs.Items) != 1 {
		t.Fatalf("s3 jobs = %d, err %v", len(jobs.Items), err)
	}
	s3 := jobs.Items[0].Spec.Template.Spec.Containers[0]
	if s3.Image != "amazon/aws-cli:2.17.0" || envOf(&jobs.Items[0])["BUCKET"] != "b" || envOf(&jobs.Items[0])["AWS_DEFAULT_REGION"] != "eu-west-1" ||
		envOf(&jobs.Items[0])["AWS_ENDPOINT_URL"] != "http://minio.storage.svc:9000" || len(s3.EnvFrom) != 1 || s3.EnvFrom[0].SecretRef.Name != "aws" {
		t.Errorf("s3 container: %+v", s3)
	}
	pvc := &corev1.PersistentVolumeClaim{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "team-a", Name: "dataset-s3data"}, pvc); err != nil {
		t.Fatal(err)
	}
	if q := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; q.String() != "50Gi" || pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "fast" {
		t.Errorf("pvc: %+v", pvc.Spec)
	}

	if err := c.List(context.Background(), jobs, client.InNamespace("data")); err != nil || len(jobs.Items) != 1 {
		t.Fatalf("nfs jobs = %d, err %v", len(jobs.Items), err)
	}
	var nfs *corev1.NFSVolumeSource
	for _, v := range jobs.Items[0].Spec.Template.Spec.Volumes {
		if v.NFS != nil {
			nfs = v.NFS
		}
	}
	if nfs == nil || nfs.Server != "10.0.0.1" || nfs.Path != "/export/data" || !nfs.ReadOnly {
		t.Errorf("nfs volume: %+v", nfs)
	}
}

func TestDataset_InvalidSpecs(t *testing.T) {
	cases := map[string]func(*gryviav1.GryviaDataset){
		"gcs":         func(ds *gryviav1.GryviaDataset) { ds.Spec.Source = gryviav1.DatasetSource{Type: "gcs"} },
		"no-url":      func(ds *gryviav1.GryviaDataset) { ds.Spec.Source.HTTP.URL = "ftp://x" },
		"bad-version": func(ds *gryviav1.GryviaDataset) { ds.Spec.Version = "../etc" },
		"s3-bucket": func(ds *gryviav1.GryviaDataset) {
			ds.Spec.Source = gryviav1.DatasetSource{Type: "s3", S3: &gryviav1.S3Source{}}
		},
		"s3-endpoint": func(ds *gryviav1.GryviaDataset) {
			ds.Spec.Source = gryviav1.DatasetSource{Type: "s3", S3: &gryviav1.S3Source{Bucket: "b", Endpoint: "minio:9000"}}
		},
		"nfs-path": func(ds *gryviav1.GryviaDataset) {
			ds.Spec.Source = gryviav1.DatasetSource{Type: "nfs", NFS: &gryviav1.NFSSource{Server: "s", Path: "rel"}}
		},
		"size": func(ds *gryviav1.GryviaDataset) { ds.Spec.Cache = &gryviav1.DatasetCache{Size: "lots"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r, c := newDatasetReconciler(httpDataset("d", mutate))
			reconcileDataset(t, r, "d")
			if st := getDataset(t, c, "d").Status; st.State != DatasetStateError || st.Message == "" {
				t.Errorf("status: %+v", st)
			}
			jobs := &batchv1.JobList{}
			if err := c.List(context.Background(), jobs); err != nil || len(jobs.Items) != 0 {
				t.Errorf("an invalid dataset created %d jobs", len(jobs.Items))
			}
		})
	}
}

func TestDataset_ForeignPVCIsAConfigError(t *testing.T) {
	foreign := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "dataset-corpus", Namespace: "data"}}
	r, c := newDatasetReconciler(httpDataset("corpus"), foreign)
	reconcileDataset(t, r, "corpus")
	if st := getDataset(t, c, "corpus").Status; st.State != DatasetStateError || !strings.Contains(st.Message, "not owned") {
		t.Errorf("status: %+v", st)
	}
}
