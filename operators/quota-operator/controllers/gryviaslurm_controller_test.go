// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gryviav1 "github.com/zyvorai/gryvia/operators/quota-operator/api/v1"
)

const slurmNS = "slurm"

func slurmClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(newQuotaTestScheme()).WithObjects(objs...).Build()
}

func getNodeSet(t *testing.T, c client.Client, tenant string) *unstructured.Unstructured {
	t.Helper()
	ns := &unstructured.Unstructured{}
	ns.SetGroupVersionKind(NodeSetGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: slurmNS, Name: SlurmPartitionName(tenant)}, ns); err != nil {
		t.Fatalf("NodeSet for %s: %v", tenant, err)
	}
	return ns
}

func reconcileSlurm(t *testing.T, r *GryviaSlurmReconciler, tenant string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: tenant}}); err != nil {
		t.Fatal(err)
	}
}

func TestSlurmNodeSetPerTenant(t *testing.T) {
	c := slurmClient(kueueTenant("research", 0))
	r := &GryviaSlurmReconciler{Client: c, Namespace: slurmNS, Controller: "slurm", NodesPerTenant: 2, Oversubscribe: true}
	reconcileSlurm(t, r, "research")

	ns := getNodeSet(t, c, "research")
	spec := ns.Object["spec"].(map[string]interface{})
	if spec["controllerRef"].(map[string]interface{})["name"] != "slurm" {
		t.Fatalf("controllerRef = %v", spec["controllerRef"])
	}
	if fmt.Sprint(spec["replicas"]) != "2" || spec["oversubscribeNode"] != true {
		t.Fatalf("replicas=%v oversubscribe=%v", spec["replicas"], spec["oversubscribeNode"])
	}
	part := spec["partition"].(map[string]interface{})
	if part["enabled"] != true || part["config"] != "MaxNodes=2" {
		t.Fatalf("partition = %v", part)
	}
	if img := spec["slurmd"].(map[string]interface{})["image"]; img != DefaultSlurmdImage {
		t.Fatalf("slurmd image = %v", img)
	}
	if ns.GetLabels()[kueueTenantLabel] != "research" || ns.GetLabels()[kueueManagedByLabel] != kueueManagedBy {
		t.Fatalf("labels = %v", ns.GetLabels())
	}
	tenant := &gryviav1.GryviaTenant{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "research"}, tenant)
	if !containsString(tenant.Finalizers, SlurmFinalizer) {
		t.Fatalf("finalizers = %v", tenant.Finalizers)
	}
}

func TestSlurmNodesFollowTheGPUQuota(t *testing.T) {
	quota := &gryviav1.GryviaQuota{ObjectMeta: metav1.ObjectMeta{Name: "q"}}
	quota.Spec.Namespaces = []string{"tenant-b"}
	quota.Spec.GPUQuota.MaxGPUs = 4
	c := slurmClient(kueueTenant("a", 10), kueueTenant("b", 0), kueueTenant("c", 0), quota)
	r := &GryviaSlurmReconciler{Client: c, Namespace: slurmNS, Controller: "slurm", GPUsPerNode: 8, NodesPerTenant: 1}
	for _, tc := range []struct {
		tenant, replicas, source string
	}{
		{"a", "2", "tenant"}, // 10 GPUs over 8 per node, rounded up
		{"b", "1", "quota"},
		{"c", "1", "default"},
	} {
		reconcileSlurm(t, r, tc.tenant)
		ns := getNodeSet(t, c, tc.tenant)
		spec := ns.Object["spec"].(map[string]interface{})
		if fmt.Sprint(spec["replicas"]) != tc.replicas || ns.GetAnnotations()[kueueQuotaSourceAnnotation] != tc.source {
			t.Errorf("%s: replicas=%v source=%s, want %s %s", tc.tenant, spec["replicas"], ns.GetAnnotations()[kueueQuotaSourceAnnotation], tc.replicas, tc.source)
		}
		limits := spec["slurmd"].(map[string]interface{})["resources"].(map[string]interface{})["limits"].(map[string]interface{})
		if limits["nvidia.com/gpu"] != "8" {
			t.Errorf("%s: gpu limit = %v", tc.tenant, limits)
		}
	}
}

func TestSlurmNodeSetUpdatedAndDeletedWithTheTenant(t *testing.T) {
	c := slurmClient(kueueTenant("t", 0))
	r := &GryviaSlurmReconciler{Client: c, Namespace: slurmNS, Controller: "slurm", NodesPerTenant: 1}
	reconcileSlurm(t, r, "t")
	r.NodesPerTenant = 3
	reconcileSlurm(t, r, "t")
	if got := fmt.Sprint(getNodeSet(t, c, "t").Object["spec"].(map[string]interface{})["replicas"]); got != "3" {
		t.Fatalf("replicas after the change = %s", got)
	}

	tenant := &gryviav1.GryviaTenant{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "t"}, tenant)
	if err := c.Delete(context.Background(), tenant); err != nil {
		t.Fatal(err)
	}
	reconcileSlurm(t, r, "t")
	ns := &unstructured.Unstructured{}
	ns.SetGroupVersionKind(NodeSetGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: slurmNS, Name: "gryvia-t"}, ns); err == nil {
		t.Fatal("NodeSet left after the tenant was deleted")
	}
}

func TestSlurmLeavesUnmanagedNodeSets(t *testing.T) {
	foreign := &unstructured.Unstructured{}
	foreign.SetGroupVersionKind(NodeSetGVK)
	foreign.SetNamespace(slurmNS)
	foreign.SetName("gryvia-x")
	_ = unstructured.SetNestedField(foreign.Object, int64(7), "spec", "replicas")
	c := slurmClient(kueueTenant("x", 0), foreign)
	r := &GryviaSlurmReconciler{Client: c, Namespace: slurmNS, Controller: "slurm"}
	reconcileSlurm(t, r, "x")
	if got := fmt.Sprint(getNodeSet(t, c, "x").Object["spec"].(map[string]interface{})["replicas"]); got != "7" {
		t.Fatalf("an unmanaged NodeSet was changed: replicas = %s", got)
	}
}

// slurmrestd stand-in: serves /slurm/<version>/jobs for the given version and checks the token.
type fakeRestd struct {
	mu      sync.Mutex
	version string
	jobs    string
	calls   []string
}

func (f *fakeRestd) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls = append(f.calls, r.URL.Path)
		f.mu.Unlock()
		if r.Header.Get("X-SLURM-USER-TOKEN") != "jwt-1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/slurm/"+f.version+"/jobs" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(w, f.jobs)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func slurmJobJSON(id int, partition string, states string, start, end int64, tres string) string {
	return fmt.Sprintf(`{"job_id":%d,"name":"train","cluster":"Slurm_1","partition":%q,"user_name":"root","account":"",`+
		`"job_state":[%s],"start_time":{"set":true,"infinite":false,"number":%d},"end_time":{"set":true,"infinite":false,"number":%d},`+
		`"node_count":{"set":true,"number":1},"tres_alloc_str":%q}`, id, partition, states, start, end, tres)
}

func managedNodeSet(tenant string) *unstructured.Unstructured {
	ns := &unstructured.Unstructured{}
	ns.SetGroupVersionKind(NodeSetGVK)
	ns.SetNamespace(slurmNS)
	ns.SetName(SlurmPartitionName(tenant))
	ns.SetLabels(map[string]string{kueueManagedByLabel: kueueManagedBy, kueueTenantLabel: tenant})
	return ns
}

func TestSlurmAccountingRecordsFinishedJobsOnce(t *testing.T) {
	start := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC).Unix()
	f := &fakeRestd{version: "v0.0.43"}
	f.jobs = `{"jobs":[` +
		slurmJobJSON(11, "gryvia-research", `"COMPLETED"`, start, start+1800, "cpu=2,mem=1G,node=1,billing=2,gres/gpu=2,gres/gpu:a100=2") + "," +
		slurmJobJSON(12, "gryvia-research", `"RUNNING"`, start, 0, "cpu=2") + "," +
		slurmJobJSON(13, "gryvia-research", `"COMPLETED","COMPLETING"`, start, start+60, "cpu=2") + "," +
		slurmJobJSON(14, "other", `"COMPLETED"`, start, start+60, "cpu=2") + "," +
		slurmJobJSON(15, "gryvia-research", `"FAILED"`, start, start+60, "cpu=2,node=1") +
		`]}`
	srv := f.server(t)
	sku := kueueSku("a100", "a100", true)
	sku.Spec.HourlyRate = 3
	c := slurmClient(
		kueueTenant("research", 0), managedNodeSet("research"), sku,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-research"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: slurmNS, Name: "gryvia-slurm-token"}, Data: map[string][]byte{"auth-token": []byte("jwt-1\n")}},
	)
	a := &SlurmAccounting{Client: c, Namespace: slurmNS, URL: srv.URL, TokenSecret: "gryvia-slurm-token", TokenKey: "auth-token"}
	n, err := a.Sync(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("first sync = %d, %v (calls %v)", n, err, f.calls)
	}
	if a.version != "v0.0.43" {
		t.Fatalf("version = %s", a.version)
	}

	rec := &gryviav1.GryviaUsageRecord{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "tenant-research", Name: "slurm-slurm-1-11"}, rec); err != nil {
		t.Fatal(err)
	}
	s := rec.Spec
	if s.Kind != KindSlurm || s.Tenant != "research" || !s.Final || s.Gpus != 2 || s.GpuType != "a100" || s.Sku != "a100" {
		t.Fatalf("record = %+v", s)
	}
	if s.GpuHours != 1 || s.Rate != 3 || s.Cost != 3 || s.End.Sub(s.Start.Time) != 30*time.Minute {
		t.Fatalf("hours=%v rate=%v cost=%v span=%v", s.GpuHours, s.Rate, s.Cost, s.End.Sub(s.Start.Time))
	}
	if rec.Labels[labelTenant] != "research" || rec.Labels[labelUsageKind] != KindSlurm || rec.Annotations["gryvia.io/slurm-job-id"] != "11" {
		t.Fatalf("labels=%v annotations=%v", rec.Labels, rec.Annotations)
	}
	failed := &gryviav1.GryviaUsageRecord{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "tenant-research", Name: "slurm-slurm-1-15"}, failed); err != nil {
		t.Fatal(err)
	}
	if failed.Spec.Gpus != 0 || failed.Spec.Cost != 0 || failed.Annotations["gryvia.io/slurm-state"] != "FAILED" {
		t.Fatalf("CPU-only failed job = %+v %v", failed.Spec, failed.Annotations)
	}

	if n, err := a.Sync(context.Background()); err != nil || n != 0 {
		t.Fatalf("second sync created %d (%v), want 0", n, err)
	}
	list := &gryviav1.GryviaUsageRecordList{}
	_ = c.List(context.Background(), list)
	if len(list.Items) != 2 {
		t.Fatalf("records = %d, want 2 (running, completing and other-partition jobs skipped)", len(list.Items))
	}
}

func TestSlurmAccountingErrors(t *testing.T) {
	f := &fakeRestd{version: "v0.0.44", jobs: `{"jobs":[]}`}
	srv := f.server(t)
	c := slurmClient(managedNodeSet("r"),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: slurmNS, Name: "tok"}, Data: map[string][]byte{"auth-token": []byte("wrong")}})
	a := &SlurmAccounting{Client: c, Namespace: slurmNS, URL: srv.URL, TokenSecret: "tok", TokenKey: "auth-token"}
	if _, err := a.Sync(context.Background()); err == nil {
		t.Fatal("a rejected token is not an error")
	}
	a.TokenSecret = "missing"
	if _, err := a.Sync(context.Background()); err == nil {
		t.Fatal("a missing token secret is not an error")
	}
	// Without tenant NodeSets nothing is fetched.
	empty := &SlurmAccounting{Client: slurmClient(), Namespace: slurmNS, URL: srv.URL, TokenSecret: "tok", TokenKey: "auth-token"}
	f.calls = nil
	if n, err := empty.Sync(context.Background()); err != nil || n != 0 || len(f.calls) != 0 {
		t.Fatalf("no partitions: n=%d err=%v calls=%v", n, err, f.calls)
	}
}

func TestSlurmNumberAndTres(t *testing.T) {
	var j SlurmJob
	if err := json.Unmarshal([]byte(`{"job_id":1,"job_state":["TIMEOUT"],"start_time":5,"end_time":{"set":true,"infinite":false,"number":9},"tres_alloc_str":"cpu=1,gres/gpu=3"}`), &j); err != nil {
		t.Fatal(err)
	}
	if !j.Finished() || j.StartTime.Value != 5 || j.EndTime.Value != 9 {
		t.Fatalf("job = %+v", j)
	}
	if n, typ := j.GPUs(); n != 3 || typ != "" {
		t.Fatalf("gpus = %d %q", n, typ)
	}
	var inf SlurmJob
	_ = json.Unmarshal([]byte(`{"job_state":["COMPLETED"],"end_time":{"set":true,"infinite":true,"number":0}}`), &inf)
	if inf.Finished() {
		t.Fatal("an infinite end time counts as finished")
	}
}
