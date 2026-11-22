// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/ai-operator/pkg/cron"
	"github.com/zyvorai/gryvia/operators/ai-operator/pkg/llmgateway"
)

const (
	// DefaultQdrantImage and DefaultRAGIngestImage are the --rag-qdrant-image and --rag-ingest-image defaults.
	DefaultQdrantImage    = "qdrant/qdrant:v1.12.6-unprivileged"
	DefaultRAGIngestImage = "ghcr.io/zyvorai/gryvia-rag-ingest:latest"

	vectorIndexFinalizer = "gryvia.io/vector-index"
	// annotationReingest: any new value re-runs the ingestion (also after a failure).
	annotationReingest       = "gryvia.io/reingest"
	annotationDatasetVersion = "gryvia.io/dataset-version"
	annotationIndexGen       = "gryvia.io/index-generation"
	labelVectorIndex         = "gryvia.io/vector-index"
	conditionStoreReady      = "StoreReady"
	conditionIngested        = "Ingested"
	qdrantPort               = 6333
	defaultChunkSize         = 1000
	defaultChunkOverlap      = 200
	defaultEmbedBatch        = 32
	ingestJobTTL             = int32(24 * 3600)
)

var datasetGVK = schema.GroupVersionKind{Group: "gryvia.io", Version: "v1alpha1", Kind: "GryviaDataset"}

// GryviaVectorIndexReconciler builds retrieval indexes: a store (managed Qdrant or external), a gateway key, and
// an ingestion Job per dataset version, spec change, schedule tick or reingest annotation.
type GryviaVectorIndexReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	Log          logr.Logger
	QdrantImage  string
	IngestImage  string
	GatewayURL   string
	KeyNamespace string
	Clock        func() time.Time
}

//+kubebuilder:rbac:groups=gryvia.io,resources=gryviavectorindexes,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviavectorindexes/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviavectorindexes/finalizers,verbs=update
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviadatasets,verbs=get;list;watch
//+kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;delete
//+kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
//+kubebuilder:rbac:groups="",resources=services;secrets,verbs=get;list;watch;create;update;delete
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

func (r *GryviaVectorIndexReconciler) keyNamespace() string {
	if r.KeyNamespace != "" {
		return r.KeyNamespace
	}
	return "gryvia-llm-keys"
}

// Reconcile drives one index towards a served, up-to-date collection.
func (r *GryviaVectorIndexReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	idx := &gryviav1.GryviaVectorIndex{}
	if err := r.Get(ctx, req.NamespacedName, idx); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !idx.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(idx, vectorIndexFinalizer) {
			if err := llmgateway.DeleteOwnedKey(ctx, r.Client, r.keyNamespace(), idx.Namespace, "index", idx.Name); err != nil {
				return ctrl.Result{}, err
			}
			controllerutil.RemoveFinalizer(idx, vectorIndexFinalizer)
			return ctrl.Result{}, r.Update(ctx, idx)
		}
		return ctrl.Result{}, nil
	}
	if !controllerutil.ContainsFinalizer(idx, vectorIndexFinalizer) {
		controllerutil.AddFinalizer(idx, vectorIndexFinalizer)
		if err := r.Update(ctx, idx); err != nil {
			return ctrl.Result{}, err
		}
	}
	orig := idx.DeepCopy()
	res, err := r.reconcileIndex(ctx, idx)
	idx.Status.ObservedGeneration = idx.Generation
	if perr := patchStatus(ctx, r.Client, idx, orig); perr != nil && err == nil {
		err = perr
	}
	return res, err
}

func validateVectorIndex(idx *gryviav1.GryviaVectorIndex) error {
	s := idx.Spec
	switch s.Store.Type {
	case gryviav1.VectorStoreManaged:
		if s.Store.External != nil {
			return fmt.Errorf("store.external is set but store.type is managed")
		}
		if m := s.Store.Managed; m != nil && m.StorageSize != "" {
			if _, err := resource.ParseQuantity(m.StorageSize); err != nil {
				return fmt.Errorf("store.managed.storageSize %q: %v", m.StorageSize, err)
			}
		}
	case gryviav1.VectorStoreExternal:
		if s.Store.External == nil || s.Store.External.URL == "" {
			return fmt.Errorf("store.type external needs store.external.url")
		}
	default:
		return fmt.Errorf("store.type must be managed or external")
	}
	size, overlap := chunking(idx)
	if overlap >= size {
		return fmt.Errorf("chunking.overlap (%d) must be less than chunking.size (%d)", overlap, size)
	}
	if s.Schedule != "" {
		if _, err := cron.Parse(s.Schedule); err != nil {
			return fmt.Errorf("schedule: %v", err)
		}
	}
	return nil
}

func chunking(idx *gryviav1.GryviaVectorIndex) (int32, int32) {
	c := idx.Spec.Chunking
	if c == nil {
		return defaultChunkSize, defaultChunkOverlap
	}
	size := c.Size
	if size <= 0 {
		size = defaultChunkSize
	}
	return size, c.Overlap
}

func collectionOf(idx *gryviav1.GryviaVectorIndex) string {
	if idx.Spec.Collection != "" {
		return idx.Spec.Collection
	}
	return idx.Name
}

func (r *GryviaVectorIndexReconciler) setPhase(idx *gryviav1.GryviaVectorIndex, phase, msg string) {
	idx.Status.Phase = phase
	idx.Status.Message = msg
}

func (r *GryviaVectorIndexReconciler) reconcileIndex(ctx context.Context, idx *gryviav1.GryviaVectorIndex) (ctrl.Result, error) {
	now := clock(r.Clock)
	if err := validateVectorIndex(idx); err != nil {
		r.setPhase(idx, gryviav1.VectorIndexFailed, "Invalid index: "+err.Error())
		return ctrl.Result{}, nil
	}
	idx.Status.Collection = collectionOf(idx)

	url, ready, msg, err := r.ensureStore(ctx, idx)
	if err != nil {
		return ctrl.Result{}, err
	}
	idx.Status.StoreURL = url
	if !ready {
		setCondition(&idx.Status.Conditions, idx.Generation, conditionStoreReady, metav1.ConditionFalse, "NotReady", msg)
		if idx.Status.LastIngested == nil {
			r.setPhase(idx, gryviav1.VectorIndexProvisioning, msg)
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	setCondition(&idx.Status.Conditions, idx.Generation, conditionStoreReady, metav1.ConditionTrue, "Ready", "The vector store answers")

	keySecret := childName(idx.Name, "llm-key")
	if err := llmgateway.EnsureOwnedKey(ctx, r.Client, r.Scheme, r.keyNamespace(), idx, "index", keySecret); err != nil {
		return ctrl.Result{}, fmt.Errorf("gateway key: %w", err)
	}
	idx.Status.KeySecret = keySecret

	ds, wait, err := r.dataset(ctx, idx)
	if err != nil {
		return ctrl.Result{}, err
	}
	if wait != "" {
		phase := gryviav1.VectorIndexPending
		if idx.Status.LastIngested != nil {
			phase = idx.Status.Phase
		}
		r.setPhase(idx, phase, wait)
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}
	if ds.namespace != idx.Namespace {
		r.setPhase(idx, gryviav1.VectorIndexFailed,
			fmt.Sprintf("Dataset %s is materialized in namespace %s, not %s (set its spec.namespace)", idx.Spec.DatasetRef, ds.namespace, idx.Namespace))
		return ctrl.Result{}, nil
	}

	// What to ingest: a new dataset version or spec, a reingest annotation, or a due schedule tick.
	tick, requeue := scheduleTick(idx, now)
	reingest := idx.Annotations[annotationReingest]
	upToDate := idx.Status.LastIngested != nil && idx.Status.DatasetVersion == ds.version &&
		idx.Status.IngestedGeneration == idx.Generation && tick.IsZero() &&
		idx.Status.IngestJob != "" && idx.Status.IngestJob == r.jobName(idx, ds.version, reingest, time.Time{}, true)
	if upToDate {
		r.setPhase(idx, gryviav1.VectorIndexReady, fmt.Sprintf("Serving %d chunks of %d documents (dataset version %s)",
			idx.Status.Chunks, idx.Status.Documents, idx.Status.DatasetVersion))
		return ctrl.Result{RequeueAfter: requeue}, nil
	}
	if idx.Spec.Suspend {
		if idx.Status.LastIngested == nil {
			r.setPhase(idx, gryviav1.VectorIndexPending, "Suspended before the first ingestion")
		} else {
			idx.Status.Message = "Suspended; serving the last ingestion"
		}
		return ctrl.Result{}, nil
	}

	name := r.jobName(idx, ds.version, reingest, tick, false)
	job := &batchv1.Job{}
	err = r.Get(ctx, types.NamespacedName{Namespace: idx.Namespace, Name: name}, job)
	if errors.IsNotFound(err) {
		job, err = r.buildIngestJob(idx, ds, name)
		if err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, job); err != nil && !errors.IsAlreadyExists(err) {
			return ctrl.Result{}, err
		}
		idx.Status.IngestJob = name
		idx.Status.NextScheduleTime = nil
		r.setPhase(idx, gryviav1.VectorIndexIngesting, fmt.Sprintf("Ingesting dataset %s version %s", idx.Spec.DatasetRef, ds.version))
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	idx.Status.IngestJob = name
	switch {
	case jobCondition(job, batchv1.JobComplete):
		out, err := r.jobOutputs(ctx, job)
		if err != nil {
			return ctrl.Result{}, err
		}
		idx.Status.Documents = parseInt(out["documents"])
		idx.Status.Chunks = parseInt(out["chunks"])
		idx.Status.Dimensions = parseInt(out["dimensions"])
		idx.Status.DatasetVersion = ds.version
		idx.Status.IngestedGeneration = idx.Generation
		t := metav1.NewTime(now)
		if job.Status.CompletionTime != nil {
			t = *job.Status.CompletionTime
		}
		idx.Status.LastIngested = &t
		// Remember the job under its non-scheduled name, so a finished scheduled run counts as up to date.
		idx.Status.IngestJob = r.jobName(idx, ds.version, reingest, time.Time{}, true)
		_, requeue = scheduleTick(idx, now)
		setCondition(&idx.Status.Conditions, idx.Generation, conditionIngested, metav1.ConditionTrue, "Ingested",
			fmt.Sprintf("%d chunks from %d documents", idx.Status.Chunks, idx.Status.Documents))
		r.setPhase(idx, gryviav1.VectorIndexReady, fmt.Sprintf("Serving %d chunks of %d documents (dataset version %s)",
			idx.Status.Chunks, idx.Status.Documents, ds.version))
		return ctrl.Result{RequeueAfter: requeue}, nil
	case jobCondition(job, batchv1.JobFailed):
		msg := r.jobFailure(ctx, job)
		setCondition(&idx.Status.Conditions, idx.Generation, conditionIngested, metav1.ConditionFalse, "IngestFailed", msg)
		r.setPhase(idx, gryviav1.VectorIndexFailed, fmt.Sprintf("Ingestion %s failed: %s (set annotation %s to retry)", name, msg, annotationReingest))
		return ctrl.Result{RequeueAfter: requeue}, nil
	default:
		r.setPhase(idx, gryviav1.VectorIndexIngesting, fmt.Sprintf("Ingesting dataset %s version %s", idx.Spec.DatasetRef, ds.version))
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
}

// scheduleTick returns the due schedule tick (zero when none is due, or before the first ingestion) and how long
// until the next one; it sets status.nextScheduleTime.
func scheduleTick(idx *gryviav1.GryviaVectorIndex, now time.Time) (time.Time, time.Duration) {
	idx.Status.NextScheduleTime = nil
	if idx.Spec.Schedule == "" || idx.Status.LastIngested == nil {
		return time.Time{}, 0
	}
	sched, err := cron.Parse(idx.Spec.Schedule)
	if err != nil {
		return time.Time{}, 0
	}
	next, ok := sched.Next(idx.Status.LastIngested.Time)
	if !ok {
		return time.Time{}, 0
	}
	if !now.Before(next) {
		return next, 0
	}
	t := metav1.NewTime(next)
	idx.Status.NextScheduleTime = &t
	return time.Time{}, next.Sub(now) + time.Second
}

// jobName is unique per (dataset version, generation, reingest annotation, schedule tick). With stable set the
// tick is left out: the name recorded once an ingestion succeeded.
func (r *GryviaVectorIndexReconciler) jobName(idx *gryviav1.GryviaVectorIndex, version, reingest string, tick time.Time, stable bool) string {
	t := int64(0)
	if !stable && !tick.IsZero() {
		t = tick.Unix()
	}
	return childName(idx.Name, "ingest", specHash([]interface{}{version, idx.Generation, reingest, t})[:10])
}

func jobCondition(job *batchv1.Job, t batchv1.JobConditionType) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == t && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func parseInt(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

type datasetInfo struct {
	namespace, version, pvc, subPath string
}

// dataset reads the referenced GryviaDataset; wait is why the index cannot ingest yet.
func (r *GryviaVectorIndexReconciler) dataset(ctx context.Context, idx *gryviav1.GryviaVectorIndex) (datasetInfo, string, error) {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(datasetGVK)
	if err := r.Get(ctx, types.NamespacedName{Name: idx.Spec.DatasetRef}, u); err != nil {
		if errors.IsNotFound(err) {
			return datasetInfo{}, fmt.Sprintf("Waiting for dataset %s (not found)", idx.Spec.DatasetRef), nil
		}
		return datasetInfo{}, "", err
	}
	str := func(path ...string) string { v, _, _ := unstructured.NestedString(u.Object, path...); return v }
	if state := str("status", "state"); state != "ready" {
		return datasetInfo{}, fmt.Sprintf("Waiting for dataset %s (state %q)", idx.Spec.DatasetRef, state), nil
	}
	d := datasetInfo{namespace: str("status", "namespace"), version: str("status", "currentVersion"),
		pvc: str("status", "pvcName"), subPath: str("status", "subPath")}
	if d.namespace == "" {
		d.namespace = str("spec", "namespace")
	}
	if d.pvc == "" {
		return datasetInfo{}, fmt.Sprintf("Waiting for dataset %s (no PVC yet)", idx.Spec.DatasetRef), nil
	}
	return d, "", nil
}

func (r *GryviaVectorIndexReconciler) qdrantImage(idx *gryviav1.GryviaVectorIndex) string {
	if m := idx.Spec.Store.Managed; m != nil && m.Image != "" {
		return m.Image
	}
	if r.QdrantImage != "" {
		return r.QdrantImage
	}
	return DefaultQdrantImage
}

// ensureStore creates the managed Qdrant (or mirrors the external store's key) and reports whether it is ready.
func (r *GryviaVectorIndexReconciler) ensureStore(ctx context.Context, idx *gryviav1.GryviaVectorIndex) (string, bool, string, error) {
	if idx.Spec.Store.Type == gryviav1.VectorStoreExternal {
		ext := idx.Spec.Store.External
		apiKey := ""
		if ref := ext.APIKeySecretRef; ref != nil {
			s := &corev1.Secret{}
			if err := r.Get(ctx, types.NamespacedName{Namespace: idx.Namespace, Name: ref.Name}, s); err != nil {
				if errors.IsNotFound(err) {
					return ext.URL, false, fmt.Sprintf("Secret %s with the store API key not found", ref.Name), nil
				}
				return "", false, "", err
			}
			apiKey = string(s.Data[secretKey(ref)])
		}
		if err := llmgateway.MirrorStoreKey(ctx, r.Client, r.keyNamespace(), idx.Namespace, idx.Name, apiKey); err != nil {
			return "", false, "", err
		}
		return ext.URL, true, "", nil
	}

	name := childName(idx.Name, "qdrant")
	labels := map[string]string{labelVectorIndex: idx.Name, "gryvia.io/component": "vector-store"}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: idx.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = labels
		svc.Spec.Selector = labels
		svc.Spec.Ports = []corev1.ServicePort{{Name: "http", Port: qdrantPort, TargetPort: intstr.FromInt32(qdrantPort), Protocol: corev1.ProtocolTCP}}
		return controllerutil.SetControllerReference(idx, svc, r.Scheme)
	}); err != nil {
		return "", false, "", err
	}
	url := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", name, idx.Namespace, qdrantPort)

	sts := &appsv1.StatefulSet{}
	err := r.Get(ctx, types.NamespacedName{Namespace: idx.Namespace, Name: name}, sts)
	if errors.IsNotFound(err) {
		sts = r.buildQdrant(idx, name, labels)
		if err := controllerutil.SetControllerReference(idx, sts, r.Scheme); err != nil {
			return "", false, "", err
		}
		if err := r.Create(ctx, sts); err != nil {
			return "", false, "", err
		}
		return url, false, "Starting the managed Qdrant", nil
	}
	if err != nil {
		return "", false, "", err
	}
	if want := r.qdrantImage(idx); sts.Spec.Template.Spec.Containers[0].Image != want {
		sts.Spec.Template.Spec.Containers[0].Image = want
		if err := r.Update(ctx, sts); err != nil {
			return "", false, "", err
		}
	}
	if sts.Status.ReadyReplicas < 1 {
		return url, false, "Waiting for the managed Qdrant to be ready", nil
	}
	return url, true, "", nil
}

func secretKey(ref *gryviav1.SecretKeyRef) string {
	if ref.Key != "" {
		return ref.Key
	}
	return "token"
}

func (r *GryviaVectorIndexReconciler) buildQdrant(idx *gryviav1.GryviaVectorIndex, name string, labels map[string]string) *appsv1.StatefulSet {
	size := resource.MustParse("10Gi")
	var class *string
	if m := idx.Spec.Store.Managed; m != nil {
		if m.StorageSize != "" {
			size = resource.MustParse(m.StorageSize)
		}
		if m.StorageClass != "" {
			class = &m.StorageClass
		}
	}
	probe := func(path string, period int32) *corev1.Probe {
		return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: path, Port: intstr.FromInt32(qdrantPort)}}, PeriodSeconds: period}
	}
	one := int32(1)
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: idx.Namespace, Labels: labels},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    &one,
			ServiceName: name,
			Selector:    &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					// The -unprivileged Qdrant images run as uid 1000; fsGroup makes the volume writable.
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: boolPtr(true), RunAsUser: int64Ptr(1000), RunAsGroup: int64Ptr(1000), FSGroup: int64Ptr(1000),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:  "qdrant",
						Image: r.qdrantImage(idx),
						Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: qdrantPort, Protocol: corev1.ProtocolTCP}},
						Env:   []corev1.EnvVar{{Name: "QDRANT__TELEMETRY_DISABLED", Value: "true"}},
						Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("256Mi")}},
						ReadinessProbe: probe("/readyz", 5),
						LivenessProbe:  probe("/livez", 20),
						VolumeMounts:   []corev1.VolumeMount{{Name: "storage", MountPath: "/qdrant/storage"}},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: boolPtr(false),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
					}},
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: "storage", Labels: labels},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					StorageClassName: class,
					Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: size}},
				},
			}},
		},
	}
}

func (r *GryviaVectorIndexReconciler) buildIngestJob(idx *gryviav1.GryviaVectorIndex, ds datasetInfo, name string) (*batchv1.Job, error) {
	image := idx.Spec.IngestImage
	if image == "" {
		image = r.IngestImage
	}
	if image == "" {
		image = DefaultRAGIngestImage
	}
	size, overlap := chunking(idx)
	batch := idx.Spec.Embedding.BatchSize
	if batch <= 0 {
		batch = defaultEmbedBatch
	}
	env := []corev1.EnvVar{
		{Name: "GATEWAY_URL", Value: r.GatewayURL},
		{Name: "GRYVIA_LLM_KEY", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: idx.Status.KeySecret}, Key: llmgateway.OwnedKeyField}}},
		{Name: "EMBED_MODEL", Value: idx.Spec.Embedding.Model},
		{Name: "EMBED_BATCH", Value: strconv.Itoa(int(batch))},
		{Name: "STORE_URL", Value: idx.Status.StoreURL},
		{Name: "COLLECTION", Value: collectionOf(idx)},
		{Name: "RUN_ID", Value: name},
		{Name: "CHUNK_SIZE", Value: strconv.Itoa(int(size))},
		{Name: "CHUNK_OVERLAP", Value: strconv.Itoa(int(overlap))},
		{Name: "DATA_DIR", Value: "/data"},
		{Name: "DATASET_VERSION", Value: ds.version},
	}
	if ext := idx.Spec.Store.External; idx.Spec.Store.Type == gryviav1.VectorStoreExternal && ext.APIKeySecretRef != nil {
		env = append(env, corev1.EnvVar{Name: "STORE_API_KEY", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: ext.APIKeySecretRef.Name}, Key: secretKey(ext.APIKeySecretRef)}}})
	}
	backoff, ttl := int32(1), ingestJobTTL
	labels := map[string]string{labelVectorIndex: idx.Name}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: idx.Namespace, Labels: labels,
			Annotations: map[string]string{annotationDatasetVersion: ds.version, annotationIndexGen: strconv.FormatInt(idx.Generation, 10)},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: boolPtr(true), RunAsUser: int64Ptr(65534),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:    "ingest",
						Image:   image,
						Command: []string{"python3", "-u", "/app/ingest.py"},
						Env:     env,
						Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("256Mi")}},
						VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/data", SubPath: ds.subPath, ReadOnly: true}},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: boolPtr(false),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
					}},
					Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: ds.pvc, ReadOnly: true}}}},
				},
			},
		},
	}
	if err := controllerutil.SetControllerReference(idx, job, r.Scheme); err != nil {
		return nil, err
	}
	return job, nil
}

func int64Ptr(v int64) *int64 { return &v }

func (r *GryviaVectorIndexReconciler) jobPods(ctx context.Context, job *batchv1.Job) ([]corev1.Pod, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(job.Namespace), client.MatchingLabels{"job-name": job.Name}); err != nil {
		return nil, err
	}
	return pods.Items, nil
}

func (r *GryviaVectorIndexReconciler) jobOutputs(ctx context.Context, job *batchv1.Job) (map[string]string, error) {
	pods, err := r.jobPods(ctx, job)
	if err != nil {
		return nil, err
	}
	if p := outputPod(pods); p != nil {
		return parseTerminationOutputs(p), nil
	}
	return map[string]string{}, nil
}

func (r *GryviaVectorIndexReconciler) jobFailure(ctx context.Context, job *batchv1.Job) string {
	pods, err := r.jobPods(ctx, job)
	if err == nil {
		for i := range pods {
			p := &pods[i]
			if p.Status.Phase != corev1.PodFailed {
				continue
			}
			for _, cs := range p.Status.ContainerStatuses {
				if t := cs.State.Terminated; t != nil && t.Message != "" {
					return truncate(t.Message, 300)
				}
			}
			return podFailureMessage(p)
		}
	}
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Message != "" {
			return c.Message
		}
	}
	return "the ingestion Job failed"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// SetupWithManager watches indexes, their Jobs and StatefulSets, and the datasets they read.
func (r *GryviaVectorIndexReconciler) SetupWithManager(mgr ctrl.Manager) error {
	ds := &unstructured.Unstructured{}
	ds.SetGroupVersionKind(datasetGVK)
	return ctrl.NewControllerManagedBy(mgr).
		For(&gryviav1.GryviaVectorIndex{}).
		Owns(&batchv1.Job{}).
		Owns(&appsv1.StatefulSet{}).
		Watches(ds, handler.EnqueueRequestsFromMapFunc(r.indexesOfDataset)).
		Complete(r)
}

func (r *GryviaVectorIndexReconciler) indexesOfDataset(ctx context.Context, obj client.Object) []reconcile.Request {
	var list gryviav1.GryviaVectorIndexList
	if err := r.List(ctx, &list); err != nil {
		return nil
	}
	var out []reconcile.Request
	for _, idx := range list.Items {
		if idx.Spec.DatasetRef == obj.GetName() {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: idx.Namespace, Name: idx.Name}})
		}
	}
	return out
}
