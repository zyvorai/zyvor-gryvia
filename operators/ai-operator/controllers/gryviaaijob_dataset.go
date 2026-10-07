package controllers

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/ai-operator/pkg/scheduler"
)

const (
	AnnotationDataset   = "gryvia.io/dataset"
	datasetVolumeName   = "gryvia-dataset"
	datasetMountRoot    = "/datasets/"
	datasetReplicaRWO   = "ReadWriteOnce"
	datasetMaxNameChars = 253
)

type datasetReplica struct {
	pool, pvc, version, accessMode string
	selector                       map[string]string
}

// datasetLocality reads the job's dataset (annotation gryvia.io/dataset) and returns its pools holding a ready,
// verified replica of the current version. A missing dataset or a read error yields an explanatory binding and
// no preference: locality never blocks scheduling.
func (r *GryviaAIJobReconciler) datasetLocality(ctx context.Context, job *gryviav1.GryviaAIJob) (*gryviav1.AIJobDataset, []datasetReplica, string) {
	name := strings.TrimSpace(job.Annotations[AnnotationDataset])
	if name == "" {
		return nil, nil, ""
	}
	bind := &gryviav1.AIJobDataset{Name: name}
	if len(name) > datasetMaxNameChars {
		bind.Message = "annotation gryvia.io/dataset is not a valid name"
		return bind, nil, ""
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(datasetGVK)
	if err := r.Get(ctx, types.NamespacedName{Name: name}, u); err != nil {
		if errors.IsNotFound(err) {
			bind.Message = fmt.Sprintf("dataset %s not found; no locality preference", name)
		} else {
			bind.Message = fmt.Sprintf("dataset %s could not be read (%v); no locality preference", name, err)
		}
		return bind, nil, ""
	}
	ns, _, _ := unstructured.NestedString(u.Object, "status", "namespace")
	current, _, _ := unstructured.NestedString(u.Object, "status", "currentVersion")
	items, _, _ := unstructured.NestedSlice(u.Object, "status", "replicas")
	var local []datasetReplica
	for _, it := range items {
		m, ok := it.(map[string]interface{})
		if !ok {
			continue
		}
		ready, _, _ := unstructured.NestedBool(m, "ready")
		verified, _, _ := unstructured.NestedBool(m, "verified")
		rep := datasetReplica{}
		rep.pool, _, _ = unstructured.NestedString(m, "pool")
		rep.pvc, _, _ = unstructured.NestedString(m, "pvcName")
		rep.version, _, _ = unstructured.NestedString(m, "version")
		rep.accessMode, _, _ = unstructured.NestedString(m, "accessMode")
		rep.selector, _, _ = unstructured.NestedStringMap(m, "nodeSelector")
		if !ready || !verified || rep.pool == "" || len(rep.selector) == 0 || (current != "" && rep.version != current) {
			continue
		}
		local = append(local, rep)
	}
	sort.SliceStable(local, func(i, j int) bool { return local[i].pool < local[j].pool })
	for _, rep := range local {
		bind.LocalPools = append(bind.LocalPools, rep.pool)
		bind.NodeSelectors = append(bind.NodeSelectors, rep.selector)
	}
	if len(local) == 0 {
		bind.Message = fmt.Sprintf("dataset %s has no ready, verified replica; no locality preference", name)
	}
	return bind, local, ns
}

// bindDataset decides, once at scheduling, which replica the pods mount. It prefers the pool of the first node the
// operator placed the job on. A ReadWriteOnce replica is only mounted by single-node jobs (other nodes could not
// attach it), and a replica in another namespace cannot be mounted at all.
func (r *GryviaAIJobReconciler) bindDataset(ctx context.Context, job *gryviav1.GryviaAIJob, bind *gryviav1.AIJobDataset, local []datasetReplica, dsNamespace string, nodes []string) {
	if bind == nil || len(local) == 0 {
		job.Status.Dataset = bind
		return
	}
	chosen := local[0]
	if len(nodes) > 0 {
		node := &corev1.Node{}
		if err := r.Get(ctx, types.NamespacedName{Name: nodes[0]}, node); err == nil {
			for _, rep := range local {
				if scheduler.InAnyPool(*node, []map[string]string{rep.selector}) {
					chosen = rep
					break
				}
			}
		}
	}
	replicas := r.getReplicaCount(job)
	switch {
	case dsNamespace != job.Namespace:
		bind.Message = fmt.Sprintf("replica not mounted: the dataset is materialized in namespace %s, the job runs in %s; nodes in %s are preferred",
			dsNamespace, job.Namespace, strings.Join(bind.LocalPools, ", "))
	case (chosen.accessMode == "" || chosen.accessMode == datasetReplicaRWO) && replicas > 1:
		bind.Message = fmt.Sprintf("replica not mounted: %s is ReadWriteOnce and the job runs on %d nodes; nodes in %s are preferred",
			chosen.pvc, replicas, strings.Join(bind.LocalPools, ", "))
	default:
		bind.Pool, bind.PVCName, bind.SubPath = chosen.pool, chosen.pvc, chosen.version
		bind.Message = fmt.Sprintf("replica %s (pool %s) mounted read-only at %s%s", chosen.pvc, chosen.pool, datasetMountRoot, bind.Name)
	}
	job.Status.Dataset = bind
}

// datasetPodSpec adds the bound replica's read-only volume, mount and GRYVIA_DATASET_PATH, and soft node
// affinity toward the dataset's local pools. Nothing changes for jobs without a dataset binding.
func datasetPodSpec(job *gryviav1.GryviaAIJob, spec *corev1.PodSpec) {
	b := job.Status.Dataset
	if b == nil {
		return
	}
	if len(b.NodeSelectors) > 0 {
		if spec.Affinity == nil {
			spec.Affinity = &corev1.Affinity{}
		} else {
			spec.Affinity = spec.Affinity.DeepCopy()
		}
		if spec.Affinity.NodeAffinity == nil {
			spec.Affinity.NodeAffinity = &corev1.NodeAffinity{}
		}
		for _, sel := range b.NodeSelectors {
			keys := make([]string, 0, len(sel))
			for k := range sel {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			var exprs []corev1.NodeSelectorRequirement
			for _, k := range keys {
				exprs = append(exprs, corev1.NodeSelectorRequirement{Key: k, Operator: corev1.NodeSelectorOpIn, Values: []string{sel[k]}})
			}
			spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution = append(spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution,
				corev1.PreferredSchedulingTerm{Weight: 50, Preference: corev1.NodeSelectorTerm{MatchExpressions: exprs}})
		}
	}
	if b.PVCName == "" {
		return
	}
	spec.Volumes = append(spec.Volumes, corev1.Volume{Name: datasetVolumeName, VolumeSource: corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: b.PVCName, ReadOnly: true}}})
	path := datasetMountRoot + b.Name
	for i := range spec.Containers {
		c := &spec.Containers[i]
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: datasetVolumeName, MountPath: path, SubPath: b.SubPath, ReadOnly: true})
		c.Env = append(c.Env, corev1.EnvVar{Name: "GRYVIA_DATASET_PATH", Value: path})
	}
}
