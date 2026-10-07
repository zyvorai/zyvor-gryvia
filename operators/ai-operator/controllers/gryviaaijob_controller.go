package controllers

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/ai-operator/pkg/scheduler"
)

const (
	// Status phases
	PhasePending    = "Pending"
	PhaseScheduling = "Scheduling"
	PhaseRunning    = "Running"
	PhaseSucceeded  = "Succeeded"
	PhaseFailed     = "Failed"
	PhaseUnknown    = "Unknown"

	// Phases set by other controllers (quota operator, priority controller) or by
	// the cancel annotation; see docs/aijob-lifecycle.md.
	PhaseQueued    = "Queued"
	PhaseRejected  = "Rejected"
	PhasePreempted = "Preempted"
	PhaseCancelled = "Cancelled"

	// Condition types
	ConditionScheduled = "Scheduled"
	ConditionReady     = "Ready"
)

// GryviaAIJobReconciler reconciles a GryviaAIJob object
type GryviaAIJobReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Log    logr.Logger

	// FabricAware turns fabric-aware node ranking on for every job that does not
	// carry gryvia.io/fabric-aware: "false" (operator flag -fabric-aware-scheduling,
	// default false). A job with the annotation "true" is ranked fabric-aware even
	// when this is false.
	FabricAware bool
	// FabricMaxPenalty lowers the per-node penalty cap (points); 0 = the default 25.
	FabricMaxPenalty float64
	// ClusterDomain is the cluster DNS suffix used in MASTER_ADDR (default "cluster.local").
	ClusterDomain string
	// Recorder emits the placement Event (optional).
	Recorder record.EventRecorder

	// KueueIntegration (flag --kueue-integration, default false) creates the batch Job suspended with a
	// Kueue queue label and maps Kueue's admission state to the job phase. See gryviaaijob_kueue.go.
	KueueIntegration bool
	// KueueStrictAdmission keeps tenant jobs suspended even when their default queue is absent.
	KueueStrictAdmission bool
	// KueueDefaultQueue is the LocalQueue used in tenant-* namespaces (flag --kueue-default-queue).
	KueueDefaultQueue string
	// priorityClasses caches the WorkloadPriorityClasses already ensured.
	priorityClasses sync.Map
	// AdmissionGate enables the quota and budget gate before a job's workload is created
	// (operator flag --admission-gate, default false). See gryviaaijob_admission.go.
	AdmissionGate bool
	// PreflightEnforce rejects a Pending job whose preflight annotations fit no node pool
	// (operator flag --preflight-enforce, default false). See pkg/preflight.
	PreflightEnforce bool
	// AdmissionDefaultHours is the forecast duration of a job without spec.timeout (0 = 1h).
	AdmissionDefaultHours float64
	// PlacementHolds (flag --placement-holds, default false) holds the GPUs of a job's chosen
	// nodes until its pods are up, so two jobs placed in the same window cannot pick the same free GPUs.
	// A single job is already placed all-or-nothing (a placement that finds too few nodes fails and the job
	// stays Pending); this closes the race between jobs. See gryviaaijob_holds.go.
	PlacementHolds bool
	gpuHolds       *scheduler.GPUHolds
	gpuHoldsOnce   sync.Once
}

//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaaijobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaaijobs/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaaijobs/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=batch,resources=jobs/status,verbs=get
//+kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gryvia.io,resources=gryvianodefabrics,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile is part of the main kubernetes reconciliation loop
func (r *GryviaAIJobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("gryviaaijob", req.NamespacedName)

	// Fetch the GryviaAIJob instance
	job := &gryviav1.GryviaAIJob{}
	err := r.Get(ctx, req.NamespacedName, job)
	if err != nil {
		if errors.IsNotFound(err) {
			log.Info("GryviaAIJob resource not found. Ignoring since object must be deleted")
			r.releasePlacement(req.NamespacedName)
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get GryviaAIJob")
		return ctrl.Result{}, err
	}

	// Handle deletion - owned resources (StatefulSet, Service, PVC) are
	// garbage-collected via controller references, so no finalizer is needed.
	if !job.ObjectMeta.DeletionTimestamp.IsZero() {
		r.releasePlacement(req.NamespacedName)
		return ctrl.Result{}, nil
	}

	// Initialize status if needed
	if job.Status.Phase == "" {
		job.Status.Phase = PhasePending
		if err := r.Status().Update(ctx, job); err != nil {
			log.Error(err, "Failed to update GryviaAIJob status")
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Reconcile the AI job
	result, err := r.reconcileAIJob(ctx, job)
	r.releaseIfSettled(job)
	if err != nil {
		log.Error(err, "Failed to reconcile AI job")
		return result, err
	}

	return result, nil
}

// reconcileAIJob runs these steps in order; each one only ever short-circuits, so no later step
// can undo an earlier decision:
//
//  1. cancel annotation (until terminal), then the phase gate: Succeeded/Failed are sticky,
//     Cancelled/Preempted tear the workload down, Rejected is sticky and removes anything created
//     before the rejection landed.
//  2. Queued: a job held by the quota operator (not a Kueue wait) creates nothing; a job waiting
//     for Kueue (condition KueueAdmitted=False) falls through so its suspended Job keeps reconciling.
//  3. spec validation (invalid timeout -> Failed).
//  4. admission gate (--admission-gate): runs only in phase Pending, so it can never touch a job
//     that is Queued for Kueue or already running; on rejection it sets the sticky Rejected phase
//     and creates nothing.
//  5. scheduling advice (Pending/Scheduling), PVC, headless Service.
//  6. workload: buildPodTemplate applies the reservation toleration/selector, the batch Job is
//     created with the Kueue queue label and suspended (--kueue-integration), then status
//     (including the Queued phase from Kueue's admission state) is derived from it.
func (r *GryviaAIJobReconciler) reconcileAIJob(ctx context.Context, job *gryviav1.GryviaAIJob) (ctrl.Result, error) {
	log := r.Log.WithValues("gryviaaijob", job.Name)

	// Cancel request (annotation gryvia.io/cancel=true): honoured until the job is terminal.
	if job.Annotations[AnnotationCancel] == "true" && !isTerminalPhase(job.Status.Phase) {
		return r.cancelJob(ctx, job)
	}

	// Phase gate: phases owned by other controllers or terminal states never create anything.
	switch job.Status.Phase {
	case PhaseSucceeded, PhaseFailed:
		return ctrl.Result{}, nil // sticky
	case PhaseCancelled, PhasePreempted:
		// The workload goes, the PVC stays. Usage metering needs a completion time.
		if err := r.teardown(ctx, job, false); err != nil {
			return ctrl.Result{}, err
		}
		if job.Status.CompletionTime == nil {
			now := metav1.Now()
			job.Status.CompletionTime = &now
			return ctrl.Result{}, r.Status().Update(ctx, job)
		}
		return ctrl.Result{}, nil
	case PhaseRejected:
		// Rejected means "never ran": also remove anything created before the rejection landed.
		return ctrl.Result{}, r.teardown(ctx, job, true)
	case PhaseQueued:
		if !isKueueQueued(job) {
			return ctrl.Result{}, nil // held by the quota operator: no PVC, Service or workload
		}
		// waiting for Kueue: the workload exists (suspended), keep reconciling it
	}

	if _, err := job.Spec.TimeoutSeconds(); err != nil {
		return ctrl.Result{}, r.failJob(ctx, job, "InvalidSpec", err.Error())
	}

	if done, res, err := r.admissionGate(ctx, job); done { // opt-in quota/budget gate: creates nothing when it rejects
		return res, err
	}

	if job.Status.Phase == PhasePending || (job.Status.Phase == PhaseScheduling && !conditionTrue(job, ConditionScheduled)) {
		if err := validateRecoveryOptions(job); err != nil {
			return ctrl.Result{}, r.failJob(ctx, job, "InvalidRecoveryOptions", err.Error())
		}
	}
	kind, err := r.resolveWorkloadKind(ctx, job)
	if err != nil {
		return ctrl.Result{}, err
	}
	if job.Status.Phase == PhasePending || (job.Status.Phase == PhaseScheduling && !conditionTrue(job, ConditionScheduled)) {
		if err := validateElastic(job, kind); err != nil {
			return ctrl.Result{}, r.failJob(ctx, job, "InvalidElasticConfig", err.Error())
		}
	}
	var queue string
	if job.Status.Phase == PhasePending || (job.Status.Phase == PhaseScheduling && !conditionTrue(job, ConditionScheduled)) {
		queue, _, err = r.resolveKueueQueue(ctx, job)
		if err != nil {
			return ctrl.Result{}, err
		}
	}
	// Strict admission cannot protect a StatefulSet; do not silently run it outside Kueue.
	if r.KueueStrictAdmission && queue != "" && kind != gryviav1.WorkloadKindJob &&
		(job.Status.Phase == PhasePending || job.Status.Phase == PhaseScheduling) {
		return ctrl.Result{}, r.failJob(ctx, job, "UnsupportedQueueWorkload", "strict Kueue admission requires workloadKind: job")
	}
	queueManaged := queue != "" && kind == gryviav1.WorkloadKindJob
	gpusPerPod := r.getGPUsPerPod(job)

	// Phase 1: Scheduling - find suitable GPU nodes (advisory; kept in status.nodesAllocated).
	if job.Status.Phase == PhasePending || (job.Status.Phase == PhaseScheduling && !conditionTrue(job, ConditionScheduled)) {
		log.Info("Scheduling AI job")
		job.Status.Phase = PhaseScheduling
		dsBind, dsLocal, dsNamespace := r.datasetLocality(ctx, job)
		var dsSelectors []map[string]string
		if dsBind != nil {
			dsSelectors = dsBind.NodeSelectors
		}

		if queueManaged {
			// Free capacity is Kueue's decision. Checking it here prevents occupied GPUs
			// from ever producing a Workload, hiding demand and blocking preemption.
			job.Status.NodesAllocated = nil
			job.Status.PlacementExplanation = nil
			job.Status.GpusAllocated = 0
			r.bindDataset(ctx, job, dsBind, dsLocal, dsNamespace, nil)
			r.updateCondition(job, ConditionScheduled, metav1.ConditionTrue, "QueueManaged", "Placement delegated to Kueue and kube-scheduler")
			if err := r.Status().Update(ctx, job); err != nil {
				return ctrl.Result{}, err
			}
		} else if gpusPerPod == 0 {
			// CPU-only job: there is no GPU placement to advise on.
			job.Status.GpusAllocated = 0
			r.bindDataset(ctx, job, dsBind, dsLocal, dsNamespace, nil)
			r.updateCondition(job, ConditionScheduled, metav1.ConditionTrue, "CPUOnly", "CPU-only job: no GPU placement needed")
			if err := r.Status().Update(ctx, job); err != nil {
				return ctrl.Result{}, err
			}
		} else {
			// Use GPU-aware scheduler to find optimal nodes
			var nodes []string
			var err error
			held := r.placementHeld(job) // GPUs other jobs hold and have not bound yet (nil when off)
			schedCtx := scheduler.WithPreferredPools(ctx, dsSelectors)
			if scheduler.FabricEnabled(job, r.FabricAware) {
				var p scheduler.Placement
				p, err = scheduler.FindOptimalNodesFabricHeld(schedCtx, r.Client, job, scheduler.FabricOptions{
					MaxPenalty: r.FabricMaxPenalty,
					OnError: func(e error) {
						log.Info("fabric-aware scheduling: node signals unavailable, ranking unchanged", "reason", e.Error())
					},
				}, held)
				nodes = p.Nodes
				if err == nil {
					job.Status.PlacementExplanation = p.Explanation
					r.recordFabricPlacement(job, p)
				}
			} else {
				nodes, err = scheduler.FindOptimalNodesHeld(schedCtx, r.Client, job, held)
			}
			if err != nil {
				log.Error(err, "Failed to schedule job")
				r.updateCondition(job, ConditionScheduled, metav1.ConditionFalse, "SchedulingFailed", err.Error())
				job.Status.Phase = PhasePending
				if updateErr := r.Status().Update(ctx, job); updateErr != nil {
					log.Error(updateErr, "Failed to update status after scheduling failure")
				}
				return ctrl.Result{RequeueAfter: 30 * time.Second}, err
			}

			job.Status.NodesAllocated = nodes
			job.Status.GpusAllocated = job.Spec.GPUs
			r.bindDataset(ctx, job, dsBind, dsLocal, dsNamespace, nodes)
			r.holdPlacement(job, nodes)
			r.updateCondition(job, ConditionScheduled, metav1.ConditionTrue, "Scheduled", "Job scheduled successfully")

			if err := r.Status().Update(ctx, job); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	// Phase 2: Create PVC if storage is specified
	if job.Spec.Storage != "" {
		if err := r.ensurePVC(ctx, job); err != nil {
			log.Error(err, "Failed to create PVC")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, err
		}
	}

	// Phase 3: Create headless service (pod DNS for both workload kinds)
	if err := r.ensureHeadlessService(ctx, job); err != nil {
		log.Error(err, "Failed to create headless service")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, err
	}

	// Phase 4+5: create the workload and derive status from it.
	if kind == gryviav1.WorkloadKindJob {
		if err := r.reconcileBatchJob(ctx, job); err != nil {
			log.Error(err, "Failed to reconcile batch Job")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, err
		}
	} else {
		if err := r.reconcileStatefulSetWorkload(ctx, job); err != nil {
			log.Error(err, "Failed to reconcile StatefulSet")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, err
		}
	}

	// Set CompletionTime when job transitions to terminal state
	if (job.Status.Phase == PhaseSucceeded || job.Status.Phase == PhaseFailed) && job.Status.CompletionTime == nil {
		now := metav1.Now()
		job.Status.CompletionTime = &now
	}

	if err := r.Status().Update(ctx, job); err != nil {
		log.Error(err, "Failed to update status")
		return ctrl.Result{}, err
	}

	// Only requeue if job is still active (not completed or failed)
	if job.Status.Phase == PhaseSucceeded || job.Status.Phase == PhaseFailed {
		return ctrl.Result{}, nil
	}

	if job.Status.Phase == PhaseQueued {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil // Workload changes do not trigger this controller
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// reconcileStatefulSetWorkload is the StatefulSet path: pods restart forever (restartPolicy
// Always), so completion is only detected if the pods themselves reach Succeeded/Failed.
func (r *GryviaAIJobReconciler) reconcileStatefulSetWorkload(ctx context.Context, job *gryviav1.GryviaAIJob) error {
	if err := r.ensureStatefulSet(ctx, job); err != nil {
		return err
	}
	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: job.Namespace, Name: r.getStatefulSetName(job)}, sts); err != nil {
		return err
	}

	job.Status.ReplicasReady = sts.Status.ReadyReplicas

	// Update phase based on replicas
	desiredReplicas := r.getReplicaCount(job)
	if sts.Status.ReadyReplicas > 0 {
		if job.Status.Phase != PhaseRunning {
			if job.Status.StartTime == nil {
				now := metav1.Now()
				job.Status.StartTime = &now
			}
			job.Status.Phase = PhaseRunning
		}
		if sts.Status.ReadyReplicas == desiredReplicas {
			r.updateCondition(job, ConditionReady, metav1.ConditionTrue, "Ready", "All replicas are ready")
		} else {
			r.updateCondition(job, ConditionReady, metav1.ConditionFalse, "PartiallyReady",
				fmt.Sprintf("%d/%d replicas are ready", sts.Status.ReadyReplicas, desiredReplicas))
		}
	}

	// Check for job completion by examining pod status
	if job.Status.Phase == PhaseRunning {
		pods := &corev1.PodList{}
		if err := r.List(ctx, pods, client.InNamespace(job.Namespace), client.MatchingLabels{"gryvia.io/job": job.Name}); err == nil {
			completedPods := 0
			failedPods := 0
			for _, pod := range pods.Items {
				if pod.Status.Phase == corev1.PodSucceeded {
					completedPods++
				} else if pod.Status.Phase == corev1.PodFailed {
					failedPods++
				}
			}
			replicas := int(r.getReplicaCount(job))
			if completedPods >= replicas {
				job.Status.Phase = PhaseSucceeded
			} else if failedPods > 0 && (completedPods+failedPods) >= replicas {
				// Only mark Failed when all pods have terminated and at least one failed
				job.Status.Phase = PhaseFailed
				job.Status.Message = fmt.Sprintf("%d/%d pod(s) failed", failedPods, replicas)
			}
		}
	}
	return nil
}

func (r *GryviaAIJobReconciler) ensurePVC(ctx context.Context, job *gryviav1.GryviaAIJob) error {
	pvcName := fmt.Sprintf("%s-data", job.Name)

	pvc := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, types.NamespacedName{
		Namespace: job.Namespace,
		Name:      pvcName,
	}, pvc)

	if err != nil && errors.IsNotFound(err) {
		// Parse storage size safely
		storageQuantity, parseErr := resource.ParseQuantity(job.Spec.StorageSize())
		if parseErr != nil {
			return fmt.Errorf("invalid storage size %q: %w", job.Spec.StorageSize(), parseErr)
		}

		// Create PVC
		pvc = &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pvcName,
				Namespace: job.Namespace,
				Labels: map[string]string{
					"gryvia.io/job": job.Name,
				},
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{
					corev1.ReadWriteMany,
				},
				StorageClassName: &job.Spec.Storage,
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceStorage: storageQuantity,
					},
				},
			},
		}

		if err := controllerutil.SetControllerReference(job, pvc, r.Scheme); err != nil {
			return err
		}

		return r.Create(ctx, pvc)
	}

	return err
}

func (r *GryviaAIJobReconciler) ensureHeadlessService(ctx context.Context, job *gryviav1.GryviaAIJob) error {
	svcName := headlessServiceName(job)

	svc := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{
		Namespace: job.Namespace,
		Name:      svcName,
	}, svc)

	if err != nil && errors.IsNotFound(err) {
		// Create headless service
		svc = &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      svcName,
				Namespace: job.Namespace,
				Labels: map[string]string{
					"gryvia.io/job": job.Name,
				},
			},
			Spec: corev1.ServiceSpec{
				ClusterIP: "None",
				// Peers must resolve each other before they are Ready (rendezvous).
				PublishNotReadyAddresses: true,
				Selector: map[string]string{
					"gryvia.io/job": job.Name,
				},
				Ports: []corev1.ServicePort{
					{
						Name: "torch-dist",
						Port: 29500,
					},
				},
			},
		}

		if err := controllerutil.SetControllerReference(job, svc, r.Scheme); err != nil {
			return err
		}

		return r.Create(ctx, svc)
	}

	return err
}

func (r *GryviaAIJobReconciler) ensureStatefulSet(ctx context.Context, job *gryviav1.GryviaAIJob) error {
	stsName := r.getStatefulSetName(job)

	sts := &appsv1.StatefulSet{}
	err := r.Get(ctx, types.NamespacedName{
		Namespace: job.Namespace,
		Name:      stsName,
	}, sts)

	if err != nil && errors.IsNotFound(err) {
		// Create StatefulSet
		sts = r.buildStatefulSet(job)

		if err := controllerutil.SetControllerReference(job, sts, r.Scheme); err != nil {
			return err
		}

		return r.Create(ctx, sts)
	}
	if err != nil {
		return err
	}

	// Update StatefulSet if the job spec has changed. A StatefulSet created before the
	// v2 environment (RANK, qualified MASTER_ADDR) keeps its v1 environment so that an
	// operator upgrade never restarts running pods.
	desired := r.buildStatefulSetVersion(job, statefulSetEnvVersion(sts))
	needsUpdate := false

	// Check replica count
	if *sts.Spec.Replicas != *desired.Spec.Replicas {
		sts.Spec.Replicas = desired.Spec.Replicas
		needsUpdate = true
	}

	// Check container image
	if len(sts.Spec.Template.Spec.Containers) > 0 && len(desired.Spec.Template.Spec.Containers) > 0 {
		if sts.Spec.Template.Spec.Containers[0].Image != desired.Spec.Template.Spec.Containers[0].Image {
			sts.Spec.Template.Spec.Containers[0].Image = desired.Spec.Template.Spec.Containers[0].Image
			needsUpdate = true
		}

		// Check command
		if !stringSlicesEqual(sts.Spec.Template.Spec.Containers[0].Command, desired.Spec.Template.Spec.Containers[0].Command) {
			sts.Spec.Template.Spec.Containers[0].Command = desired.Spec.Template.Spec.Containers[0].Command
			needsUpdate = true
		}

		// Check args
		if !stringSlicesEqual(sts.Spec.Template.Spec.Containers[0].Args, desired.Spec.Template.Spec.Containers[0].Args) {
			sts.Spec.Template.Spec.Containers[0].Args = desired.Spec.Template.Spec.Containers[0].Args
			needsUpdate = true
		}

		// Check env vars
		if !envVarsEqual(sts.Spec.Template.Spec.Containers[0].Env, desired.Spec.Template.Spec.Containers[0].Env) {
			sts.Spec.Template.Spec.Containers[0].Env = desired.Spec.Template.Spec.Containers[0].Env
			needsUpdate = true
		}

		// Check resource limits (GPU count changes)
		desiredLimits := desired.Spec.Template.Spec.Containers[0].Resources.Limits
		currentLimits := sts.Spec.Template.Spec.Containers[0].Resources.Limits
		gpuResource := corev1.ResourceName("nvidia.com/gpu")
		desiredGPU, desiredHas := desiredLimits[gpuResource]
		currentGPU, currentHas := currentLimits[gpuResource]
		if desiredHas != currentHas || (desiredHas && !desiredGPU.Equal(currentGPU)) {
			sts.Spec.Template.Spec.Containers[0].Resources = desired.Spec.Template.Spec.Containers[0].Resources
			needsUpdate = true
		}
	}

	// Check annotations (network mode changes)
	if !mapsEqual(sts.Spec.Template.Annotations, desired.Spec.Template.Annotations) {
		sts.Spec.Template.Annotations = desired.Spec.Template.Annotations
		needsUpdate = true
	}

	// Check node selector
	if !mapsEqual(sts.Spec.Template.Spec.NodeSelector, desired.Spec.Template.Spec.NodeSelector) {
		sts.Spec.Template.Spec.NodeSelector = desired.Spec.Template.Spec.NodeSelector
		needsUpdate = true
	}

	if needsUpdate {
		return r.Update(ctx, sts)
	}

	return nil
}

func (r *GryviaAIJobReconciler) buildStatefulSet(job *gryviav1.GryviaAIJob) *appsv1.StatefulSet {
	return r.buildStatefulSetVersion(job, envVersionCurrent)
}

func (r *GryviaAIJobReconciler) buildStatefulSetVersion(job *gryviav1.GryviaAIJob, envVersion int) *appsv1.StatefulSet {
	labels := map[string]string{
		"gryvia.io/job":  job.Name,
		"gryvia.io/type": job.Spec.Type,
	}

	replicas := r.getReplicaCount(job)
	gpusPerPod := r.getGPUsPerPod(job)

	// Build pod template
	podTemplate := r.buildPodTemplate(job, labels, gpusPerPod, gryviav1.WorkloadKindStatefulSet, envVersion)

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      r.getStatefulSetName(job),
			Namespace: job.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    &replicas,
			ServiceName: headlessServiceName(job),
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: podTemplate,
		},
	}

	return sts
}

func (r *GryviaAIJobReconciler) buildPodTemplate(job *gryviav1.GryviaAIJob, labels map[string]string, gpusPerPod int32, kind string, envVersion int) corev1.PodTemplateSpec {
	annotations := make(map[string]string)
	if kind == gryviav1.WorkloadKindStatefulSet && envVersion >= envVersionCurrent {
		annotations[AnnotationEnvVersion] = fmt.Sprintf("%d", envVersion)
	}

	// Add RDMA annotation if network mode is RDMA
	if job.Spec.Network == "rdma" {
		annotations["gryvia.io/rdma"] = "true"
		annotations["k8s.v1.cni.cncf.io/networks"] = "rdma-network"
	}

	// Add SR-IOV annotation if network mode is SR-IOV
	if job.Spec.Network == "sriov" {
		annotations["gryvia.io/sriov"] = "true"
		annotations["k8s.v1.cni.cncf.io/networks"] = "sriov-network"
	}

	// Build container
	container := corev1.Container{
		Name:            "trainer",
		Image:           job.Spec.Image,
		ImagePullPolicy: job.Spec.ImagePullPolicy,
		Command:         job.Spec.Command,
		Args:            job.Spec.Args,
		Env:             r.buildEnvVarsFor(job, kind, envVersion),
		WorkingDir:      job.Spec.WorkingDir,
		VolumeMounts:    r.buildVolumeMounts(job),
		Resources:       r.buildResources(job, gpusPerPod),
	}

	// Build pod spec
	podSpec := corev1.PodSpec{
		Containers:   []corev1.Container{container},
		Volumes:      r.buildVolumes(job),
		NodeSelector: r.buildNodeSelector(job),
		Tolerations:  job.Spec.Tolerations,
		Affinity:     r.buildAffinity(job),
		// StatefulSets only support RestartPolicyAlways, so they never complete;
		// run-to-completion jobs use a batch/v1 Job (see buildJob).
		RestartPolicy: corev1.RestartPolicyAlways,
	}

	applyRecoveryOptions(job, &podSpec)
	datasetPodSpec(job, &podSpec)
	r.applyReservation(job, &podSpec) // gryvia.io/reservation: toleration + nodeSelector (gryviaaijob_reservation.go)

	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: podSpec,
	}
}

// buildAffinity returns job.Spec.Affinity, plus (only when a fabric-aware
// decision penalised a node and the job has not opted out) SOFT preferred node
// affinity toward the nodes the operator selected. Required terms and the user's
// own preferences are never touched; the spec object is never mutated.
func (r *GryviaAIJobReconciler) buildAffinity(job *gryviav1.GryviaAIJob) *corev1.Affinity {
	if job.Annotations[scheduler.AnnotationPinPlacement] == "true" && len(job.Status.NodesAllocated) > 0 {
		// Opt-in: required, not preferred. If a chosen node goes away the pods stay Pending until
		// the job is rescheduled; that is the price of making placement binding.
		return scheduler.PinToNodes(job.Spec.Affinity, job.Status.NodesAllocated)
	}
	if job.Annotations[scheduler.AnnotationFabricAware] == "false" {
		return job.Spec.Affinity
	}
	terms := scheduler.PreferredNodeTerms(job.Status.NodesAllocated, job.Status.PlacementExplanation)
	if len(terms) == 0 {
		return job.Spec.Affinity
	}
	aff := job.Spec.Affinity.DeepCopy()
	if aff == nil {
		aff = &corev1.Affinity{}
	}
	if aff.NodeAffinity == nil {
		aff.NodeAffinity = &corev1.NodeAffinity{}
	}
	aff.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution = append(
		aff.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution, terms...)
	return aff
}

// recordFabricPlacement emits one Event when fabric health changed the ranking.
func (r *GryviaAIJobReconciler) recordFabricPlacement(job *gryviav1.GryviaAIJob, p scheduler.Placement) {
	if r.Recorder == nil {
		return
	}
	var parts []string
	for _, e := range p.Explanation {
		if e.FabricPenalty > 0 {
			parts = append(parts, fmt.Sprintf("%s -%d (%d->%d)", e.Node, e.FabricPenalty, e.BaseScore, e.FinalScore))
		}
	}
	if len(parts) == 0 {
		return
	}
	r.Recorder.Eventf(job, corev1.EventTypeNormal, "FabricAwarePlacement",
		"fabric health lowered node scores: %s; selected %v", strings.Join(parts, ", "), p.Nodes)
}

// buildEnvVars returns the environment of a current-version batch Job pod.
func (r *GryviaAIJobReconciler) buildEnvVars(job *gryviav1.GryviaAIJob) []corev1.EnvVar {
	return r.buildEnvVarsFor(job, gryviav1.WorkloadKindJob, envVersionCurrent)
}

// buildEnvVarsFor returns the container environment. The exact variables per kind and
// framework are documented in docs/aijob-lifecycle.md.
func (r *GryviaAIJobReconciler) buildEnvVarsFor(job *gryviav1.GryviaAIJob, kind string, envVersion int) []corev1.EnvVar {
	// Copy to avoid mutating the spec
	envVars := make([]corev1.EnvVar, len(job.Spec.Env))
	copy(envVars, job.Spec.Env)

	if job.Spec.Distributed == nil || !job.Spec.Distributed.Enabled {
		return envVars
	}
	dist := job.Spec.Distributed
	nodes := r.getReplicaCount(job)
	gpusPerNode := r.getGPUsPerPod(job)
	procsPerNode := gpusPerNode
	if procsPerNode == 0 {
		procsPerNode = 1 // CPU-only: one process per node
	}
	worldSize := nodes * procsPerNode
	nccl := dist.Backend != "gloo" // gloo is the CPU/TCP backend: NCCL tuning does not apply

	if envVersion < envVersionCurrent {
		// v1 (StatefulSets created by earlier operator versions): unchanged.
		envVars = append(envVars,
			corev1.EnvVar{Name: "MASTER_ADDR", Value: fmt.Sprintf("%s-training-0.%s-headless", job.Name, job.Name)},
			corev1.EnvVar{Name: "MASTER_PORT", Value: "29500"},
			corev1.EnvVar{Name: "WORLD_SIZE", Value: fmt.Sprintf("%d", worldSize)},
			corev1.EnvVar{Name: "NCCL_DEBUG", Value: "INFO"},
		)
		if job.Spec.Network == "rdma" {
			envVars = append(envVars,
				corev1.EnvVar{Name: "NCCL_IB_DISABLE", Value: "0"},
				corev1.EnvVar{Name: "NCCL_NET_GDR_LEVEL", Value: "5"},
			)
		}
		return envVars
	}

	domain := r.ClusterDomain
	if domain == "" {
		domain = "cluster.local"
	}
	svc := headlessServiceName(job)
	// Pod 0 of the workload is the rendezvous host. Job pods are named <job>-<index>
	// (Indexed Job with spec.subdomain), StatefulSet pods <job>-training-<ordinal>.
	master := fmt.Sprintf("%s-0", job.Name)
	rankFrom := "metadata.annotations['batch.kubernetes.io/job-completion-index']"
	if kind == gryviav1.WorkloadKindStatefulSet {
		master = fmt.Sprintf("%s-0", r.getStatefulSetName(job))
		// Label set by the StatefulSet controller (beta in Kubernetes 1.28, GA in 1.32);
		// on older clusters it is missing and RANK is empty.
		rankFrom = "metadata.labels['apps.kubernetes.io/pod-index']"
	}
	rank := corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: rankFrom}}
	envVars = append(envVars,
		corev1.EnvVar{Name: "MASTER_ADDR", Value: fmt.Sprintf("%s.%s.%s.svc.%s", master, svc, job.Namespace, domain)},
		corev1.EnvVar{Name: "MASTER_PORT", Value: "29500"},
		corev1.EnvVar{Name: "WORLD_SIZE", Value: fmt.Sprintf("%d", worldSize)},
		// RANK and NODE_RANK are the node (pod) index. With gpusPerNode > 1 the launcher
		// (torchrun, deepspeed) derives the per-process RANK from NODE_RANK itself.
		corev1.EnvVar{Name: "RANK", ValueFrom: &rank},
		corev1.EnvVar{Name: "NODE_RANK", ValueFrom: &rank},
		corev1.EnvVar{Name: "NNODES", Value: fmt.Sprintf("%d", nodes)},
		corev1.EnvVar{Name: "NPROC_PER_NODE", Value: fmt.Sprintf("%d", procsPerNode)},
	)
	if min, max, ok := dist.ElasticBounds(); ok {
		// torchrun --nnodes accepts "min:max". WORLD_SIZE above is the upper bound; the launcher recomputes it
		// after each rendezvous.
		envVars = replaceEnv(envVars, "NNODES", fmt.Sprintf("%d:%d", min, max))
		envVars = append(envVars,
			corev1.EnvVar{Name: "GRYVIA_ELASTIC", Value: "true"},
			corev1.EnvVar{Name: "GRYVIA_ELASTIC_MIN_NODES", Value: fmt.Sprintf("%d", min)},
			corev1.EnvVar{Name: "GRYVIA_ELASTIC_MAX_NODES", Value: fmt.Sprintf("%d", max)},
		)
		// torch >= 2.4 caches the workers' store address from the first rendezvous round; when another node
		// becomes rank 0 after a loss, the agent fails an assertion in _restart_workers (seen with torch 2.8).
		if !hasEnv(job.Spec.Env, "TORCH_DISABLE_SHARE_RDZV_TCP_STORE") {
			envVars = append(envVars, corev1.EnvVar{Name: "TORCH_DISABLE_SHARE_RDZV_TCP_STORE", Value: "1"})
		}
	}
	if dist.Framework != "" {
		envVars = append(envVars, corev1.EnvVar{Name: "GRYVIA_DIST_FRAMEWORK", Value: dist.Framework})
	}
	if dist.Backend != "" {
		envVars = append(envVars, corev1.EnvVar{Name: "GRYVIA_DIST_BACKEND", Value: dist.Backend})
	}
	if nccl {
		envVars = append(envVars, corev1.EnvVar{Name: "NCCL_DEBUG", Value: "INFO"})
		if job.Spec.Network == "rdma" {
			envVars = append(envVars,
				corev1.EnvVar{Name: "NCCL_IB_DISABLE", Value: "0"},
				corev1.EnvVar{Name: "NCCL_NET_GDR_LEVEL", Value: "5"},
			)
		}
	}
	return envVars
}

func (r *GryviaAIJobReconciler) buildVolumeMounts(job *gryviav1.GryviaAIJob) []corev1.VolumeMount {
	// Copy to avoid mutating the spec
	volumeMounts := make([]corev1.VolumeMount, len(job.Spec.VolumeMounts))
	copy(volumeMounts, job.Spec.VolumeMounts)

	// Add data volume if storage is specified
	if job.Spec.Storage != "" {
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      "data",
			MountPath: "/data",
		})
	}

	// Add shared memory for distributed training
	if job.Spec.Distributed != nil && job.Spec.Distributed.Enabled {
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      "shm",
			MountPath: "/dev/shm",
		})
	}

	return volumeMounts
}

func (r *GryviaAIJobReconciler) buildVolumes(job *gryviav1.GryviaAIJob) []corev1.Volume {
	// Copy to avoid mutating the spec
	volumes := make([]corev1.Volume, len(job.Spec.Volumes))
	copy(volumes, job.Spec.Volumes)

	// Add PVC volume if storage is specified
	if job.Spec.Storage != "" {
		volumes = append(volumes, corev1.Volume{
			Name: "data",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: fmt.Sprintf("%s-data", job.Name),
				},
			},
		})
	}

	// Add shared memory for distributed training
	if job.Spec.Distributed != nil && job.Spec.Distributed.Enabled {
		volumes = append(volumes, corev1.Volume{
			Name: "shm",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{
					Medium: corev1.StorageMediumMemory,
				},
			},
		})
	}

	return volumes
}

func (r *GryviaAIJobReconciler) buildResources(job *gryviav1.GryviaAIJob, gpusPerPod int32) corev1.ResourceRequirements {
	resources := *job.Spec.Resources.DeepCopy() // never mutate the spec's maps

	// Add GPU resource limits (none for CPU-only jobs)
	if gpusPerPod > 0 {
		if resources.Limits == nil {
			resources.Limits = corev1.ResourceList{}
		}
		resources.Limits["nvidia.com/gpu"] = *resource.NewQuantity(int64(gpusPerPod), resource.DecimalSI)
	}

	return resources
}

func (r *GryviaAIJobReconciler) buildNodeSelector(job *gryviav1.GryviaAIJob) map[string]string {
	// Copy to avoid mutating the spec
	nodeSelector := make(map[string]string)
	for k, v := range job.Spec.NodeSelector {
		nodeSelector[k] = v
	}

	// Add GPU type selector if specified (not for CPU-only jobs)
	if r.getGPUsPerPod(job) > 0 && job.Spec.GpuType != "" && job.Spec.GpuType != "any" {
		nodeSelector["gryvia.io/gpu"] = job.Spec.GpuType
	}

	// Add RDMA selector if network mode is RDMA
	if job.Spec.Network == "rdma" {
		nodeSelector["gryvia.io/rdma"] = "true"
	}

	return nodeSelector
}

func (r *GryviaAIJobReconciler) getStatefulSetName(job *gryviav1.GryviaAIJob) string {
	return fmt.Sprintf("%s-training", job.Name)
}

func (r *GryviaAIJobReconciler) getReplicaCount(job *gryviav1.GryviaAIJob) int32 {
	if job.Spec.Distributed != nil && job.Spec.Distributed.Enabled {
		if _, max, ok := job.Spec.Distributed.ElasticBounds(); ok && max > 0 {
			return max
		}
		if job.Spec.Distributed.Nodes > 0 {
			return job.Spec.Distributed.Nodes
		}
		return 1
	}
	return 1
}

func (r *GryviaAIJobReconciler) getGPUsPerPod(job *gryviav1.GryviaAIJob) int32 {
	if job.Spec.Distributed != nil && job.Spec.Distributed.Enabled {
		if job.Spec.Distributed.GpusPerNode > 0 {
			return job.Spec.Distributed.GpusPerNode
		}
		if job.Spec.GPUs == 0 {
			return 0 // CPU-only
		}
		return 1
	}
	if job.Spec.GPUs < 0 {
		return 1
	}
	return job.Spec.GPUs // 0 = CPU-only
}

func (r *GryviaAIJobReconciler) updateCondition(job *gryviav1.GryviaAIJob, condType string, status metav1.ConditionStatus, reason, message string) {
	condition := metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: job.Generation,
		LastTransitionTime: metav1.Now(),
	}

	// Only update LastTransitionTime when status actually changes
	for _, cond := range job.Status.Conditions {
		if cond.Type == condType && cond.Status == status {
			condition.LastTransitionTime = cond.LastTransitionTime
			break
		}
	}

	// Find and update existing condition or append new one
	found := false
	for i, cond := range job.Status.Conditions {
		if cond.Type == condType {
			job.Status.Conditions[i] = condition
			found = true
			break
		}
	}
	if !found {
		job.Status.Conditions = append(job.Status.Conditions, condition)
	}
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func envVarsEqual(a, b []corev1.EnvVar) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].Value != b[i].Value {
			return false
		}
		// Compare ValueFrom references
		if (a[i].ValueFrom == nil) != (b[i].ValueFrom == nil) {
			return false
		}
	}
	return true
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// SetupWithManager sets up the controller with the Manager.
func (r *GryviaAIJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gryviav1.GryviaAIJob{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&batchv1.Job{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Complete(r)
}
