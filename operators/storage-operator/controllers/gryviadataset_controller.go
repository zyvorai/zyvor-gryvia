package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gryviav1 "github.com/zyvorai/gryvia/operators/storage-operator/api/v1"
)

const (
	DatasetStateSyncing = "syncing"
	DatasetStateReady   = "ready"
	DatasetStateError   = "error"

	datasetLabel        = "gryvia.io/dataset"
	datasetPoolLabel    = "gryvia.io/dataset-pool"
	datasetDefaultVer   = "latest"
	datasetMountPath    = "/data"
	datasetBackoffLimit = int32(2)
	datasetJobTTL       = int32(24 * 3600)
	datasetRequeue      = 10 * time.Second
	datasetUID          = int64(65534)
)

var datasetVersionRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// GryviaDatasetReconciler materializes a dataset's source into a PVC: one directory per version, written by a
// download Job (http with an optional sha256 checksum, s3 through the AWS CLI, or a copy from an NFS export).
type GryviaDatasetReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Log    logr.Logger

	// DefaultNamespace is used when spec.namespace is empty.
	DefaultNamespace string
	// Image runs the http and nfs downloads (needs sh, wget, sha256sum, find, stat); S3Image runs s3 (the AWS CLI).
	Image   string
	S3Image string
	// DefaultSize is the PVC size when spec.cache.size is empty.
	DefaultSize string
}

// datasetConfigError is a problem only a spec change fixes; it is reported in status, not retried.
type datasetConfigError struct{ error }

type datasetResult struct {
	Files  int64  `json:"files"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
	Error  string `json:"error"`
}

func (r *GryviaDatasetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ds := &gryviav1.GryviaDataset{}
	if err := r.Get(ctx, req.NamespacedName, ds); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !ds.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	base := ds.DeepCopy()
	res, err := r.reconcileDataset(ctx, ds)
	if !equalStatus(base.Status, ds.Status) {
		if perr := r.Status().Patch(ctx, ds, client.MergeFrom(base)); perr != nil && err == nil {
			err = perr
		}
	}
	return res, err
}

func equalStatus(a, b gryviav1.GryviaDatasetStatus) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

func (r *GryviaDatasetReconciler) reconcileDataset(ctx context.Context, ds *gryviav1.GryviaDataset) (ctrl.Result, error) {
	ns := ds.Spec.Namespace
	if ns == "" {
		ns = r.DefaultNamespace
	}
	version := ds.Spec.Version
	if version == "" {
		version = datasetDefaultVer
	}
	if msg := validateDataset(ds, version); msg != "" {
		r.setState(ds, DatasetStateError, "InvalidSpec", msg)
		return ctrl.Result{}, nil
	}
	ds.Status.Namespace = ns
	ds.Status.PVCName = datasetPVCName(ds)

	if err := r.ensurePVC(ctx, ds, ns); err != nil {
		var cfg datasetConfigError
		if stderrors.As(err, &cfg) || errors.IsInvalid(err) {
			r.setState(ds, DatasetStateError, "InvalidSpec", err.Error())
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	hash := datasetHash(ds, version)
	res, err := r.reconcilePrimary(ctx, ds, ns, version, hash)
	if err != nil {
		return res, err
	}
	wait, err := r.reconcileReplicas(ctx, ds, ns, version, hash)
	if err != nil {
		return ctrl.Result{}, err
	}
	if wait > 0 && (res.RequeueAfter == 0 || wait < res.RequeueAfter) {
		res.RequeueAfter = wait
	}
	return res, nil
}

// reconcilePrimary materializes the version into the dataset's own PVC.
func (r *GryviaDatasetReconciler) reconcilePrimary(ctx context.Context, ds *gryviav1.GryviaDataset, ns, version, hash string) (ctrl.Result, error) {
	if ds.Status.SourceHash == hash && ds.Status.State == DatasetStateReady {
		return ctrl.Result{}, nil
	}

	job := &batchv1.Job{}
	name := datasetJobName(ds, hash)
	err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, job)
	if errors.IsNotFound(err) {
		job = r.buildJob(ds, ns, name, version, hash)
		if err := controllerutil.SetControllerReference(ds, job, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, job); err != nil {
			return ctrl.Result{}, err
		}
		r.setState(ds, DatasetStateSyncing, "Downloading", fmt.Sprintf("Downloading version %s from %s", version, ds.Spec.Source.Type))
		return ctrl.Result{RequeueAfter: datasetRequeue}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	switch {
	case jobFinished(job, batchv1.JobComplete):
		out, err := r.jobResult(ctx, job)
		if err != nil {
			return ctrl.Result{}, err
		}
		now := metav1.Now()
		r.recordVersion(ds, gryviav1.DatasetVersionInfo{Version: version, CreatedAt: &now, Size: out.Bytes, Checksum: out.SHA256})
		ds.Status.CurrentVersion = version
		ds.Status.SubPath = version
		ds.Status.FileCount = out.Files
		ds.Status.TotalSizeBytes = out.Bytes
		ds.Status.SourceHash = hash
		r.setState(ds, DatasetStateReady, "Materialized", fmt.Sprintf("Version %s: %d files, %d bytes in PVC %s/%s", version, out.Files, out.Bytes, ns, ds.Status.PVCName))
		return ctrl.Result{}, nil
	case jobFinished(job, batchv1.JobFailed):
		out, _ := r.jobResult(ctx, job)
		msg := "download job " + job.Name + " failed"
		if out.Error != "" {
			msg += ": " + out.Error
		}
		r.setState(ds, DatasetStateError, "DownloadFailed", msg)
		return ctrl.Result{}, nil
	default:
		r.setState(ds, DatasetStateSyncing, "Downloading", fmt.Sprintf("Downloading version %s from %s", version, ds.Spec.Source.Type))
		return ctrl.Result{RequeueAfter: datasetRequeue}, nil
	}
}

func validateDataset(ds *gryviav1.GryviaDataset, version string) string {
	if !datasetVersionRE.MatchString(version) {
		return fmt.Sprintf("spec.version %q must be 1-63 letters, digits, '.', '_' or '-'", version)
	}
	s := ds.Spec.Source
	switch s.Type {
	case "http":
		if s.HTTP == nil || !(strings.HasPrefix(s.HTTP.URL, "http://") || strings.HasPrefix(s.HTTP.URL, "https://")) {
			return "source.type http needs source.http.url (http:// or https://)"
		}
		if c := s.HTTP.ChecksumURL; c != "" && !(strings.HasPrefix(c, "http://") || strings.HasPrefix(c, "https://")) {
			return "source.http.checksumURL must be http:// or https://"
		}
	case "s3":
		if s.S3 == nil || s.S3.Bucket == "" {
			return "source.type s3 needs source.s3.bucket"
		}
		if e := s.S3.Endpoint; e != "" && !(strings.HasPrefix(e, "http://") || strings.HasPrefix(e, "https://")) {
			return "source.s3.endpoint must be http:// or https://"
		}
	case "nfs":
		if s.NFS == nil || s.NFS.Server == "" || !strings.HasPrefix(s.NFS.Path, "/") {
			return "source.type nfs needs source.nfs.server and an absolute source.nfs.path"
		}
	default:
		return fmt.Sprintf("source.type %q is not supported (http, s3 or nfs)", s.Type)
	}
	if c := ds.Spec.Cache; c != nil && c.Size != "" {
		if _, err := resource.ParseQuantity(c.Size); err != nil {
			return fmt.Sprintf("spec.cache.size %q is not a quantity", c.Size)
		}
	}
	return ""
}

func (r *GryviaDatasetReconciler) setState(ds *gryviav1.GryviaDataset, state, reason, msg string) {
	ds.Status.State = state
	ds.Status.Message = msg
	status := metav1.ConditionFalse
	if state == DatasetStateReady {
		status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&ds.Status.Conditions, metav1.Condition{Type: "Ready", Status: status, Reason: reason, Message: msg, ObservedGeneration: ds.Generation})
}

// recordVersion adds or replaces a version and keeps the newest versioning.retentionPolicy.keepLast of them.
func (r *GryviaDatasetReconciler) recordVersion(ds *gryviav1.GryviaDataset, v gryviav1.DatasetVersionInfo) {
	out := []gryviav1.DatasetVersionInfo{}
	for _, old := range ds.Status.Versions {
		if old.Version != v.Version {
			out = append(out, old)
		}
	}
	out = append(out, v)
	if keep := keepLast(ds); keep > 0 && len(out) > keep {
		out = out[len(out)-keep:]
	}
	ds.Status.Versions = out
}

func keepLast(ds *gryviav1.GryviaDataset) int {
	if v := ds.Spec.Versioning; v != nil && v.Enabled && v.RetentionPolicy != nil && v.RetentionPolicy.KeepLast > 0 {
		return int(v.RetentionPolicy.KeepLast)
	}
	if v := ds.Spec.Versioning; v != nil && v.Enabled {
		return 0 // versioning without a limit keeps every version
	}
	return 1
}

// keptVersions are the version directories the download Job leaves in place: the new one plus the newest
// keepLast-1 recorded ones (all of them when keepLast is 0).
func keptVersions(ds *gryviav1.GryviaDataset, version string) []string {
	keep := keepLast(ds)
	var prior []string
	for _, v := range ds.Status.Versions {
		if v.Version != version {
			prior = append(prior, v.Version)
		}
	}
	if keep > 0 {
		if n := keep - 1; len(prior) > n {
			prior = prior[len(prior)-n:]
		}
	}
	out := append(prior, version)
	sort.Strings(out)
	return out
}

func datasetPVCName(ds *gryviav1.GryviaDataset) string {
	return truncName("dataset-" + ds.Name)
}

func datasetJobName(ds *gryviav1.GryviaDataset, hash string) string {
	return truncName("dataset-"+ds.Name, "-"+hash)
}

func truncName(name string, suffix ...string) string {
	s := strings.Join(suffix, "")
	if max := 63 - len(s); len(name) > max {
		name = strings.TrimRight(name[:max], "-.")
	}
	return name + s
}

func datasetHash(ds *gryviav1.GryviaDataset, version string) string {
	b, _ := json.Marshal(struct {
		Source  gryviav1.DatasetSource
		Version string
	}{ds.Spec.Source, version})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:10]
}

func (r *GryviaDatasetReconciler) ensurePVC(ctx context.Context, ds *gryviav1.GryviaDataset, ns string) error {
	var class string
	if c := ds.Spec.Cache; c != nil {
		class = c.StorageClass
	}
	return r.ensureClaim(ctx, ds, ns, datasetPVCName(ds), class, corev1.ReadWriteOnce, nil)
}

func (r *GryviaDatasetReconciler) ensureClaim(ctx context.Context, ds *gryviav1.GryviaDataset, ns, name, storageClass string,
	mode corev1.PersistentVolumeAccessMode, extraLabels map[string]string) error {
	pvc := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, pvc)
	if err == nil {
		if !metav1.IsControlledBy(pvc, ds) {
			return datasetConfigError{fmt.Errorf("PVC %s/%s already exists and is not owned by this dataset", ns, pvc.Name)}
		}
		return nil
	}
	if !errors.IsNotFound(err) {
		return err
	}
	size := r.DefaultSize
	if c := ds.Spec.Cache; c != nil && c.Size != "" {
		size = c.Size
	}
	var class *string
	if storageClass != "" {
		class = &storageClass
	}
	labels := map[string]string{datasetLabel: ds.Name}
	for k, v := range extraLabels {
		labels[k] = v
	}
	pvc = &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{mode},
			StorageClassName: class,
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)}},
		},
	}
	if err := controllerutil.SetControllerReference(ds, pvc, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, pvc)
}

// datasetScript downloads into a temporary directory, swaps it into /data/$VERSION, removes version directories
// not in $KEEP and writes {"files","bytes","sha256"} (or {"error"}) to the termination message.
//
// The temporary directory is kept when the Job fails, so a retry of the same source ($HASH, recorded next to it)
// resumes: http continues the partial file, s3 sync fetches only what is missing. A new s3 directory starts as hard
// links to the current version's files, so only changed objects are downloaded; the AWS CLI writes each download to
// a temporary file and renames it, which leaves the linked files of the old version untouched. nfs copies everything,
// because cp would overwrite linked files in place.
const datasetScript = `set -eu
fail() { printf '{"error":"%s"}' "$1" > /dev/termination-log; echo "$1" >&2; exit 1; }
dest="/data/$VERSION"; tmp="/data/.tmp-$VERSION"; mark="/data/.tmp-$VERSION.source"
[ "$(cat "$mark" 2>/dev/null || true)" = "$HASH" ] || rm -rf "$tmp"
if [ ! -d "$tmp" ]; then
  mkdir -p "$tmp"
  seed=""
  if [ -d "$dest" ]; then seed="$dest"; elif [ -n "${CURRENT:-}" ] && [ -d "/data/$CURRENT" ]; then seed="/data/$CURRENT"; fi
  if [ "$SOURCE" = s3 ] && [ -n "$seed" ]; then
    cp -al "$seed/." "$tmp/" || { rm -rf "$tmp"; mkdir -p "$tmp"; }
  fi
fi
echo "$HASH" > "$mark"
case "$SOURCE" in
http)
  f="${URL##*/}"; f="${f%%\?*}"; [ -n "$f" ] || f=data
  wget -q -c -O "$tmp/$f" "$URL" || { rm -f "$tmp/$f"; wget -q -O "$tmp/$f" "$URL"; } || fail "download of $URL failed"
  if [ -n "${CHECKSUM_URL:-}" ]; then
    want=$(wget -q -O - "$CHECKSUM_URL" | head -n1 | cut -d' ' -f1) || fail "download of $CHECKSUM_URL failed"
    got=$(sha256sum "$tmp/$f" | cut -d' ' -f1)
    [ "$want" = "$got" ] || { rm -f "$tmp/$f"; fail "sha256 mismatch: want $want, got $got"; }
  fi ;;
s3) aws s3 sync "s3://$BUCKET/${PREFIX:-}" "$tmp" --delete --only-show-errors || fail "aws s3 sync of s3://$BUCKET/${PREFIX:-} failed" ;;
nfs) rm -rf "$tmp"; mkdir -p "$tmp"; cp -R /src/. "$tmp"/ || fail "copy from the NFS export failed" ;;
esac
rm -rf "$dest"; mv "$tmp" "$dest"; rm -f "$mark"
for d in /data/*; do
  [ -d "$d" ] || continue
  v="${d##*/}"
  case " $KEEP " in *" $v "*) ;; *) rm -rf "$d" ;; esac
done
files=0; bytes=0
for s in $(find "$dest" -type f -exec stat -c %s {} +); do files=$((files+1)); bytes=$((bytes+s)); done
sum=$(cd "$dest" && find . -type f -exec sha256sum {} + | sort -k2 | sha256sum | cut -d' ' -f1)
printf '{"files":%d,"bytes":%d,"sha256":"%s"}' "$files" "$bytes" "$sum" > /dev/termination-log
`

func (r *GryviaDatasetReconciler) buildJob(ds *gryviav1.GryviaDataset, ns, name, version, hash string) *batchv1.Job {
	return r.buildCopyJob(ds, copyTarget{ns: ns, name: name, version: version, hash: hash, pvc: datasetPVCName(ds),
		current: ds.Status.CurrentVersion, keep: keptVersions(ds, version)})
}

// copyTarget is where one download Job writes: the primary PVC, or a pool replica pinned by nodeSelector.
type copyTarget struct {
	ns, name, version, hash, pvc, current, pool string
	keep                                        []string
	nodeSelector                                map[string]string
}

func (r *GryviaDatasetReconciler) buildCopyJob(ds *gryviav1.GryviaDataset, t copyTarget) *batchv1.Job {
	ns, name, hash := t.ns, t.name, t.hash
	s := ds.Spec.Source
	env := []corev1.EnvVar{
		{Name: "SOURCE", Value: s.Type},
		{Name: "VERSION", Value: t.version},
		{Name: "KEEP", Value: strings.Join(t.keep, " ")},
		{Name: "HASH", Value: hash},
		{Name: "CURRENT", Value: t.current},
	}
	image := r.Image
	var envFrom []corev1.EnvFromSource
	mounts := []corev1.VolumeMount{{Name: "data", MountPath: datasetMountPath}}
	volumes := []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: t.pvc}}}}
	switch s.Type {
	case "http":
		env = append(env, corev1.EnvVar{Name: "URL", Value: s.HTTP.URL}, corev1.EnvVar{Name: "CHECKSUM_URL", Value: s.HTTP.ChecksumURL})
	case "s3":
		image = r.S3Image
		env = append(env, corev1.EnvVar{Name: "BUCKET", Value: s.S3.Bucket}, corev1.EnvVar{Name: "PREFIX", Value: s.S3.Prefix},
			corev1.EnvVar{Name: "HOME", Value: "/tmp"})
		if s.S3.Region != "" {
			env = append(env, corev1.EnvVar{Name: "AWS_DEFAULT_REGION", Value: s.S3.Region})
		}
		if s.S3.Endpoint != "" {
			env = append(env, corev1.EnvVar{Name: "AWS_ENDPOINT_URL", Value: s.S3.Endpoint})
		}
		if s.S3.CredentialsSecret != "" {
			// The Secret (in the dataset namespace) holds AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY.
			envFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: s.S3.CredentialsSecret}}}}
		}
	case "nfs":
		mounts = append(mounts, corev1.VolumeMount{Name: "src", MountPath: "/src", ReadOnly: true})
		volumes = append(volumes, corev1.Volume{Name: "src", VolumeSource: corev1.VolumeSource{NFS: &corev1.NFSVolumeSource{Server: s.NFS.Server, Path: s.NFS.Path, ReadOnly: true}}})
	}
	uid := datasetUID
	nonRoot := true
	noEscalation := false
	backoff, ttl := datasetBackoffLimit, datasetJobTTL
	labels := map[string]string{datasetLabel: ds.Name, "gryvia.io/dataset-source-hash": hash}
	if t.pool != "" {
		labels[datasetPoolLabel] = t.pool
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:   corev1.RestartPolicyNever,
					NodeSelector:    t.nodeSelector,
					SecurityContext: &corev1.PodSecurityContext{RunAsUser: &uid, RunAsGroup: &uid, FSGroup: &uid, RunAsNonRoot: &nonRoot},
					Containers: []corev1.Container{{
						Name:            "download",
						Image:           image,
						Command:         []string{"sh", "-c", datasetScript},
						Env:             env,
						EnvFrom:         envFrom,
						VolumeMounts:    mounts,
						SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &noEscalation, Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
							Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("512Mi")},
						},
					}},
					Volumes: volumes,
				},
			},
		},
	}
}

func jobFinished(job *batchv1.Job, t batchv1.JobConditionType) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == t && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// jobResult reads the JSON the download container wrote to its termination message (the newest pod's).
func (r *GryviaDatasetReconciler) jobResult(ctx context.Context, job *batchv1.Job) (datasetResult, error) {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(job.Namespace), client.MatchingLabels{"job-name": job.Name}); err != nil {
		return datasetResult{}, err
	}
	sort.Slice(pods.Items, func(i, j int) bool {
		return pods.Items[j].CreationTimestamp.Before(&pods.Items[i].CreationTimestamp)
	})
	for _, p := range pods.Items {
		for _, cs := range p.Status.ContainerStatuses {
			if cs.State.Terminated != nil && cs.State.Terminated.Message != "" {
				var out datasetResult
				if err := json.Unmarshal([]byte(cs.State.Terminated.Message), &out); err == nil {
					return out, nil
				}
			}
		}
	}
	return datasetResult{}, nil
}

func (r *GryviaDatasetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gryviav1.GryviaDataset{}).
		Owns(&batchv1.Job{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Complete(r)
}
