package controllers

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

func placedDataset(namespace string, replicas ...map[string]interface{}) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": "corpus"},
		"status": map[string]interface{}{
			"state": "ready", "namespace": namespace, "currentVersion": "v1", "replicas": func() []interface{} {
				out := []interface{}{}
				for _, r := range replicas {
					out = append(out, r)
				}
				return out
			}(),
		},
	}}
	u.SetGroupVersionKind(datasetGVK)
	return u
}

func replica(pool, zone, access string, ready, verified bool) map[string]interface{} {
	return map[string]interface{}{"pool": pool, "pvcName": "dataset-corpus-" + pool, "version": "v1", "accessMode": access,
		"ready": ready, "verified": verified, "nodeSelector": map[string]interface{}{"topology.kubernetes.io/zone": zone}}
}

func zoneNode(name, zone string) *corev1.Node {
	n := gpuNode(name)
	n.Labels["topology.kubernetes.io/zone"] = zone
	return n
}

func datasetReconciler(objs ...client.Object) (*GryviaAIJobReconciler, client.Client) {
	s := newAIJobTestScheme()
	s.AddKnownTypeWithName(datasetGVK, &unstructured.Unstructured{})
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithStatusSubresource(&gryviav1.GryviaAIJob{}, &appsv1.StatefulSet{}, &batchv1.Job{}).Build()
	return &GryviaAIJobReconciler{Client: c, Scheme: s, Log: ctrl.Log.WithName("test")}, c
}

func datasetJob(name string, nodes int32) *gryviav1.GryviaAIJob {
	j := newTestAIJob(name, ns)
	j.Annotations = map[string]string{AnnotationDataset: "corpus"}
	j.Spec.WorkloadKind = gryviav1.WorkloadKindJob
	if nodes > 1 {
		j.Spec.Distributed = &gryviav1.DistributedConfig{Enabled: true, Nodes: nodes, GpusPerNode: 4}
	}
	return j
}

func TestAIJobDataset_PrefersAndMountsLocalReplica(t *testing.T) {
	ds := placedDataset(ns, replica("zone-a", "a", "ReadWriteOnce", true, false), replica("zone-b", "b", "ReadWriteOnce", true, true))
	r, c := datasetReconciler(datasetJob("train", 1), zoneNode("n1", "a"), zoneNode("n2", "b"), ds)
	reconcileN(t, r, "train", 3)

	job := getAIJob(t, c, "train")
	if len(job.Status.NodesAllocated) != 1 || job.Status.NodesAllocated[0] != "n2" {
		t.Fatalf("the node in the pool with a verified replica must win the tie, got %v", job.Status.NodesAllocated)
	}
	b := job.Status.Dataset
	if b == nil || b.Pool != "zone-b" || b.PVCName != "dataset-corpus-zone-b" || b.SubPath != "v1" || strings.Join(b.LocalPools, ",") != "zone-b" {
		t.Fatalf("binding %+v", b)
	}
	bj := &batchv1.Job{}
	if !exists(t, c, bj, "train") {
		t.Fatal("workload not created")
	}
	pod := bj.Spec.Template.Spec
	var vol *corev1.Volume
	for i := range pod.Volumes {
		if pod.Volumes[i].Name == datasetVolumeName {
			vol = &pod.Volumes[i]
		}
	}
	if vol == nil || vol.PersistentVolumeClaim.ClaimName != "dataset-corpus-zone-b" || !vol.PersistentVolumeClaim.ReadOnly {
		t.Fatalf("dataset volume %+v", vol)
	}
	var mount *corev1.VolumeMount
	for i, m := range pod.Containers[0].VolumeMounts {
		if m.Name == datasetVolumeName {
			mount = &pod.Containers[0].VolumeMounts[i]
		}
	}
	if mount == nil || mount.MountPath != "/datasets/corpus" || mount.SubPath != "v1" || !mount.ReadOnly {
		t.Fatalf("dataset mount %+v", mount)
	}
	if envMap(pod.Containers[0].Env)["GRYVIA_DATASET_PATH"].Value != "/datasets/corpus" {
		t.Error("GRYVIA_DATASET_PATH missing")
	}
	pref := pod.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution
	if len(pref) != 1 || pref[0].Preference.MatchExpressions[0].Values[0] != "b" {
		t.Fatalf("preferred affinity %+v", pref)
	}
}

func TestAIJobDataset_NotMountedCases(t *testing.T) {
	for _, tc := range []struct {
		name, dsNamespace, want string
		nodes                   int32
		replicas                []map[string]interface{}
	}{
		{"multi-node with a ReadWriteOnce replica", ns, "ReadWriteOnce", 2, []map[string]interface{}{replica("zone-b", "b", "ReadWriteOnce", true, true)}},
		{"dataset in another namespace", "data", "namespace data", 1, []map[string]interface{}{replica("zone-b", "b", "ReadOnlyMany", true, true)}},
		{"no verified replica", ns, "no ready, verified replica", 1, []map[string]interface{}{replica("zone-b", "b", "ReadOnlyMany", true, false)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, c := datasetReconciler(datasetJob("train", tc.nodes), zoneNode("n1", "a"), zoneNode("n2", "b"), zoneNode("n3", "b"),
				placedDataset(tc.dsNamespace, tc.replicas...))
			reconcileN(t, r, "train", 3)
			b := getAIJob(t, c, "train").Status.Dataset
			if b == nil || b.PVCName != "" || !strings.Contains(b.Message, tc.want) {
				t.Fatalf("binding %+v", b)
			}
			bj := &batchv1.Job{}
			if !exists(t, c, bj, "train") {
				t.Fatal("locality must never block the workload")
			}
			for _, v := range bj.Spec.Template.Spec.Volumes {
				if v.Name == datasetVolumeName {
					t.Fatal("replica must not be mounted")
				}
			}
		})
	}

	r, c := datasetReconciler(datasetJob("train", 1), zoneNode("n1", "a"))
	reconcileN(t, r, "train", 3)
	if b := getAIJob(t, c, "train").Status.Dataset; b == nil || !strings.Contains(b.Message, "not found") {
		t.Fatalf("missing dataset: %+v", b)
	}
	if !exists(t, c, &batchv1.Job{}, "train") {
		t.Fatal("a missing dataset must not block the workload")
	}
}
