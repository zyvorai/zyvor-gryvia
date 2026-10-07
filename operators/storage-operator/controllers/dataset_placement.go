package controllers

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gryviav1 "github.com/zyvorai/gryvia/operators/storage-operator/api/v1"
)

const conditionPlaced = "Placed"

var datasetPoolRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,22}[a-z0-9])?$`)

func validatePlacement(p *gryviav1.DatasetPlacement) string {
	if p == nil {
		return ""
	}
	if len(p.Pools) == 0 || len(p.Pools) > 16 {
		return "spec.placement.pools needs 1 to 16 pools"
	}
	switch p.AccessMode {
	case "", string(corev1.ReadWriteOnce), string(corev1.ReadOnlyMany), string(corev1.ReadWriteMany):
	default:
		return fmt.Sprintf("spec.placement.accessMode %q must be ReadWriteOnce, ReadOnlyMany or ReadWriteMany", p.AccessMode)
	}
	seen := map[string]bool{}
	for _, pool := range p.Pools {
		if !datasetPoolRE.MatchString(pool.Name) {
			return fmt.Sprintf("spec.placement pool name %q must be a DNS label of at most 24 characters", pool.Name)
		}
		if seen[pool.Name] {
			return fmt.Sprintf("spec.placement pool %q is listed twice", pool.Name)
		}
		seen[pool.Name] = true
		if len(pool.NodeSelector) == 0 {
			return fmt.Sprintf("spec.placement pool %q needs a nodeSelector", pool.Name)
		}
	}
	return ""
}

func replicaPVCName(ds *gryviav1.GryviaDataset, pool string) string {
	return truncName("dataset-"+ds.Name, "-"+pool)
}

func replicaJobName(ds *gryviav1.GryviaDataset, pool, hash string) string {
	return truncName("dataset-"+ds.Name, "-"+pool+"-"+hash)
}

// primaryDigest is the checksum of the primary copy of version, when that copy is ready.
func primaryDigest(ds *gryviav1.GryviaDataset, hash string) string {
	if ds.Status.State != DatasetStateReady || ds.Status.SourceHash != hash {
		return ""
	}
	for _, v := range ds.Status.Versions {
		if v.Version == ds.Status.CurrentVersion {
			return v.Checksum
		}
	}
	return ""
}

// reconcileReplicas keeps one copy of the current version per spec.placement pool. Replica downloads start once
// the primary copy is ready (or right away with cache.warmup); a replica is verified when its digest equals the
// primary's. Replica PVCs of pools no longer listed are deleted. It returns a requeue delay while work is pending.
func (r *GryviaDatasetReconciler) reconcileReplicas(ctx context.Context, ds *gryviav1.GryviaDataset, ns, version, hash string) (time.Duration, error) {
	if err := r.pruneReplicas(ctx, ds, ns); err != nil {
		return 0, err
	}
	p := ds.Spec.Placement
	if p == nil {
		ds.Status.Replicas = nil
		meta.RemoveStatusCondition(&ds.Status.Conditions, conditionPlaced)
		return 0, nil
	}
	if msg := validatePlacement(p); msg != "" {
		ds.Status.Replicas = nil
		meta.SetStatusCondition(&ds.Status.Conditions, metav1.Condition{Type: conditionPlaced, Status: metav1.ConditionFalse,
			Reason: "InvalidSpec", Message: msg, ObservedGeneration: ds.Generation})
		return 0, nil
	}
	mode := corev1.ReadWriteOnce
	if p.AccessMode != "" {
		mode = corev1.PersistentVolumeAccessMode(p.AccessMode)
	}
	class := p.StorageClass
	if class == "" && ds.Spec.Cache != nil {
		class = ds.Spec.Cache.StorageClass
	}
	digest := primaryDigest(ds, hash)
	start := digest != "" || (ds.Spec.Cache != nil && ds.Spec.Cache.Warmup && ds.Status.State != DatasetStateError)

	prev := map[string]gryviav1.DatasetReplica{}
	for _, rep := range ds.Status.Replicas {
		prev[rep.Pool] = rep
	}
	var out []gryviav1.DatasetReplica
	pending := false
	for _, pool := range p.Pools {
		rep := prev[pool.Name]
		rep.Pool, rep.NodeSelector, rep.AccessMode = pool.Name, pool.NodeSelector, string(mode)
		rep.PVCName = replicaPVCName(ds, pool.Name)
		if err := r.ensureClaim(ctx, ds, ns, rep.PVCName, class, mode, map[string]string{datasetPoolLabel: pool.Name}); err != nil {
			if errors.IsInvalid(err) || isConfigErr(err) {
				rep.Ready, rep.Verified, rep.Message = false, false, err.Error()
				out = append(out, rep)
				continue
			}
			return 0, err
		}
		if rep.SourceHash != hash || !rep.Ready {
			if !start {
				rep.Ready, rep.Verified = false, false
				rep.Message = "Waiting for the primary copy"
				pending = true
				out = append(out, rep)
				continue
			}
			done, err := r.syncReplica(ctx, ds, &rep, pool, ns, version, hash)
			if err != nil {
				return 0, err
			}
			pending = pending || !done
		}
		rep.Verified = rep.Ready && digest != "" && rep.Digest == digest
		switch {
		case rep.Ready && digest == "":
			rep.Message = "Downloaded; waiting for the primary copy's digest"
			pending = true
		case rep.Ready && !rep.Verified:
			rep.Message = fmt.Sprintf("digest %s differs from the primary copy's %s; the source changed during the downloads", short(rep.Digest), short(digest))
		case rep.Verified:
			rep.Message = fmt.Sprintf("Version %s verified: %d files, %d bytes", rep.Version, rep.Files, rep.Bytes)
		}
		out = append(out, rep)
	}
	ds.Status.Replicas = out
	setPlacedCondition(ds)
	if pending {
		return datasetRequeue, nil
	}
	return 0, nil
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func isConfigErr(err error) bool {
	_, ok := err.(datasetConfigError)
	return ok
}

// syncReplica creates or follows the pool's download Job; done reports a finished (complete or failed) Job.
func (r *GryviaDatasetReconciler) syncReplica(ctx context.Context, ds *gryviav1.GryviaDataset, rep *gryviav1.DatasetReplica,
	pool gryviav1.DatasetPool, ns, version, hash string) (bool, error) {
	name := replicaJobName(ds, pool.Name, hash)
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, job)
	if errors.IsNotFound(err) {
		job = r.buildCopyJob(ds, copyTarget{ns: ns, name: name, version: version, hash: hash, pvc: rep.PVCName,
			current: rep.Version, keep: []string{version}, pool: pool.Name, nodeSelector: pool.NodeSelector})
		if err := controllerutil.SetControllerReference(ds, job, r.Scheme); err != nil {
			return false, err
		}
		if err := r.Create(ctx, job); err != nil {
			return false, err
		}
		rep.Ready, rep.Verified, rep.Message = false, false, fmt.Sprintf("Downloading version %s into pool %s", version, pool.Name)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	switch {
	case jobFinished(job, batchv1.JobComplete):
		out, err := r.jobResult(ctx, job)
		if err != nil {
			return false, err
		}
		now := metav1.Now()
		rep.Ready, rep.Version, rep.SourceHash, rep.Digest = true, version, hash, out.SHA256
		rep.Files, rep.Bytes, rep.LastSynced = out.Files, out.Bytes, &now
		return true, nil
	case jobFinished(job, batchv1.JobFailed):
		out, _ := r.jobResult(ctx, job)
		rep.Ready, rep.Verified = false, false
		rep.Message = "download job " + job.Name + " failed"
		if out.Error != "" {
			rep.Message += ": " + out.Error
		}
		return true, nil
	default:
		rep.Ready, rep.Verified, rep.Message = false, false, fmt.Sprintf("Downloading version %s into pool %s", version, pool.Name)
		return false, nil
	}
}

func setPlacedCondition(ds *gryviav1.GryviaDataset) {
	var notReady []string
	for _, rep := range ds.Status.Replicas {
		if !rep.Verified {
			notReady = append(notReady, rep.Pool)
		}
	}
	c := metav1.Condition{Type: conditionPlaced, Status: metav1.ConditionTrue, Reason: "Verified",
		Message: fmt.Sprintf("%d replicas verified", len(ds.Status.Replicas)), ObservedGeneration: ds.Generation}
	if len(notReady) > 0 {
		c.Status, c.Reason = metav1.ConditionFalse, "Syncing"
		c.Message = fmt.Sprintf("%d of %d replicas not verified yet: %s", len(notReady), len(ds.Status.Replicas), strings.Join(notReady, ", "))
	}
	meta.SetStatusCondition(&ds.Status.Conditions, c)
}

// pruneReplicas deletes the replica PVCs (owned by this dataset) of pools no longer in spec.placement.
func (r *GryviaDatasetReconciler) pruneReplicas(ctx context.Context, ds *gryviav1.GryviaDataset, ns string) error {
	want := map[string]bool{}
	if ds.Spec.Placement != nil {
		for _, p := range ds.Spec.Placement.Pools {
			want[replicaPVCName(ds, p.Name)] = true
		}
	}
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := r.List(ctx, pvcs, client.InNamespace(ns), client.MatchingLabels{datasetLabel: ds.Name}, client.HasLabels{datasetPoolLabel}); err != nil {
		return err
	}
	var gone []string
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		if want[pvc.Name] || !metav1.IsControlledBy(pvc, ds) || !pvc.DeletionTimestamp.IsZero() {
			continue
		}
		if err := r.Delete(ctx, pvc); err != nil && !errors.IsNotFound(err) {
			return err
		}
		gone = append(gone, pvc.Name)
	}
	sort.Strings(gone)
	if len(gone) > 0 {
		r.Log.Info("deleted dataset replicas of removed pools", "dataset", ds.Name, "pvcs", gone)
	}
	return nil
}
