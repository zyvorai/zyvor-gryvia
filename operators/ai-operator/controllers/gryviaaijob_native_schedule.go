package controllers

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/ai-operator/pkg/scheduler"
)

const (
	AnnotationNativeQueue       = "gryvia.io/native-queue"
	AnnotationGPUShare          = scheduler.AnnotationGPUShare
	AnnotationGPUIsolation      = scheduler.AnnotationGPUIsolation
	AnnotationNativePreemptible = "gryvia.io/native-preemptible"
)

// NativeWorkloadFromAIJob converts the existing GryviaAIJob into the native
// fair-share/gang scheduler model without creating another workload CRD.
func NativeWorkloadFromAIJob(job *gryviav1.GryviaAIJob) (scheduler.NativeWorkload, error) {
	queue := job.Annotations[AnnotationNativeQueue]
	if queue == "" {
		queue = job.Spec.QueueName
	}
	if queue == "" {
		queue = job.Namespace
	}
	replicas := 1
	minAvailable := 1
	if job.Spec.Distributed != nil && job.Spec.Distributed.Enabled {
		replicas = int(job.Spec.Distributed.Nodes)
		minAvailable = replicas
		if min, _, ok := job.Spec.Distributed.ElasticBounds(); ok {
			minAvailable = int(min)
		}
	}
	share, err := scheduler.ParseGPUShare(job.Annotations, scheduler.GPUsPerPlacedNode(job))
	if err != nil {
		return scheduler.NativeWorkload{}, err
	}
	if share.MilliGPU == 0 {
		share.MilliGPU = int64(scheduler.GPUsPerPlacedNode(job)) * scheduler.MilliGPU
	}
	return scheduler.NativeWorkload{
		ID:           job.Namespace + "/" + job.Name,
		Queue:        queue,
		Priority:     job.Spec.Priority,
		CreatedAt:    job.CreationTimestamp.Time,
		Replicas:     replicas,
		MinAvailable: minAvailable,
		GPUPerPod:    share.MilliGPU,
		Preemptible:  job.Annotations[AnnotationNativePreemptible] == "true",
	}, nil
}

// NativeAllocationFromAIJob converts active Gryvia jobs into scheduler accounting.
// Allocated GPUs prefer status when available and otherwise use the declared shape.
func NativeAllocationFromAIJob(job *gryviav1.GryviaAIJob) (scheduler.NativeAllocation, bool, error) {
	switch job.Status.Phase {
	case "Scheduling", "Running":
	default:
		return scheduler.NativeAllocation{}, false, nil
	}
	w, err := NativeWorkloadFromAIJob(job)
	if err != nil {
		return scheduler.NativeAllocation{}, false, err
	}
	gpu := int64(job.Status.GpusAllocated) * scheduler.MilliGPU
	if gpu == 0 {
		gpu = int64(w.Replicas) * w.GPUPerPod
	}
	return scheduler.NativeAllocation{
		WorkloadID: w.ID, Queue: w.Queue, Priority: w.Priority, GPU: gpu, Preemptible: w.Preemptible,
	}, true, nil
}

const (
	// ConditionNativeQueued marks a job held by the native queue scheduler (phase Queued).
	ConditionNativeQueued = "NativeQueued"
	// NativeConfigMapName / NativeConfigNamespace locate the queue policy (deploy/native-scheduler-config.yaml).
	NativeConfigMapName   = "gryvia-native-scheduler"
	NativeConfigNamespace = "gryvia-system"
	nativeRequeue         = 15 * time.Second
	gpuResource           = corev1.ResourceName("nvidia.com/gpu")
)

type nativePolicyFile struct {
	Queues []struct {
		Name         string  `json:"name"`
		GuaranteeGPU int64   `json:"guaranteeGPU"`
		MaxGPU       int64   `json:"maxGPU"`
		Weight       float64 `json:"weight"`
		Borrow       bool    `json:"borrow"`
		Preemptible  bool    `json:"preemptible"`
	} `json:"queues"`
}

func isNativeQueued(job *gryviav1.GryviaAIJob) bool {
	return meta.IsStatusConditionTrue(job.Status.Conditions, ConditionNativeQueued)
}

func (r *GryviaAIJobReconciler) nativeEngine(ctx context.Context) (*scheduler.NativeQueueEngine, error) {
	var cm corev1.ConfigMap
	if err := r.Get(ctx, types.NamespacedName{Namespace: NativeConfigNamespace, Name: NativeConfigMapName}, &cm); err != nil {
		return nil, err
	}
	var f nativePolicyFile
	if err := yaml.Unmarshal([]byte(cm.Data["policy.yaml"]), &f); err != nil {
		return nil, fmt.Errorf("policy.yaml: %w", err)
	}
	var pols []scheduler.NativeQueuePolicy
	for _, q := range f.Queues {
		pols = append(pols, scheduler.NativeQueuePolicy{Name: q.Name, GuaranteeGPU: q.GuaranteeGPU,
			MaxGPU: q.MaxGPU, Weight: q.Weight, Borrow: q.Borrow, Preemptible: q.Preemptible})
	}
	return scheduler.NewNativeQueueEngine(pols)
}

// nativeQueueGate runs the native DRF/gang/preemption admission for a Pending (or native-Queued) job.
// It fails open (job proceeds, Warning condition) when policy or capacity cannot be read, like admissionGate.
// done=true means the gate handled the reconcile (job queued, or released back to Pending).
func (r *GryviaAIJobReconciler) nativeQueueGate(ctx context.Context, job *gryviav1.GryviaAIJob) (bool, ctrl.Result, error) {
	if !r.NativeQueueScheduler || (job.Status.Phase != PhasePending && !(job.Status.Phase == PhaseQueued && isNativeQueued(job))) {
		return false, ctrl.Result{}, nil
	}
	fail := func(reason string, err error) (bool, ctrl.Result, error) {
		r.setGateCondition(ctx, job, ConditionAdmissionUnchecked, metav1.ConditionTrue, reason,
			"native queue gate skipped: "+err.Error(), corev1.EventTypeWarning)
		return false, ctrl.Result{}, nil
	}
	engine, err := r.nativeEngine(ctx)
	if err != nil {
		return fail("NativePolicyUnavailable", err)
	}
	w, err := NativeWorkloadFromAIJob(job)
	if err != nil {
		return fail("NativeWorkloadInvalid", err)
	}
	var jobs gryviav1.GryviaAIJobList
	if err := r.List(ctx, &jobs); err != nil {
		return fail("NativeListFailed", err)
	}
	var allocs []scheduler.NativeAllocation
	for i := range jobs.Items {
		j := &jobs.Items[i]
		if j.Namespace == job.Namespace && j.Name == job.Name {
			continue
		}
		if a, ok, err := NativeAllocationFromAIJob(j); err == nil && ok {
			allocs = append(allocs, a)
		}
	}
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return fail("NativeListFailed", err)
	}
	total := int64(0)
	for _, n := range nodes.Items {
		if q, ok := n.Status.Allocatable[gpuResource]; ok {
			total += q.Value() * scheduler.MilliGPU
		}
	}
	used := int64(0)
	for _, a := range allocs {
		used += a.GPU
	}
	free := total - used
	if free < 0 {
		free = 0
	}

	plan, err := engine.PlanAdmission(w, allocs, free)
	if err == nil && len(plan.Victims) > 0 && r.CheckpointGuard {
		// Victims cannot be checkpointed by this gate; never evict through an active checkpoint guard.
		err, plan.Reason = scheduler.ErrNoPreemptionPlan, "preemption deferred: checkpoint guard active"
	}
	switch {
	case err == nil:
		for _, v := range plan.Victims {
			if perr := r.nativePreempt(ctx, v, job); perr != nil {
				return true, ctrl.Result{}, perr
			}
		}
		if job.Status.Phase == PhaseQueued {
			base := job.DeepCopy()
			job.Status.Phase = PhasePending
			job.Status.Message = ""
			meta.RemoveStatusCondition(&job.Status.Conditions, ConditionNativeQueued)
			if perr := r.Status().Patch(ctx, job, client.MergeFrom(base)); perr != nil {
				return true, ctrl.Result{}, perr
			}
			return true, ctrl.Result{Requeue: true}, nil
		}
		return false, ctrl.Result{}, nil
	case errors.Is(err, scheduler.ErrQuotaExceeded), errors.Is(err, scheduler.ErrGangUnsatisfied), errors.Is(err, scheduler.ErrNoPreemptionPlan):
		msg := fmt.Sprintf("queued by native scheduler: %s (%v)", plan.Reason, err)
		if !isNativeQueued(job) || job.Status.Message != msg {
			base := job.DeepCopy()
			job.Status.Phase = PhaseQueued
			job.Status.Message = msg
			meta.SetStatusCondition(&job.Status.Conditions, metav1.Condition{Type: ConditionNativeQueued,
				Status: metav1.ConditionTrue, Reason: "WaitingForCapacity", Message: msg, LastTransitionTime: metav1.Now()})
			if perr := r.Status().Patch(ctx, job, client.MergeFrom(base)); perr != nil {
				return true, ctrl.Result{}, perr
			}
			if r.Recorder != nil {
				r.Recorder.Event(job, corev1.EventTypeNormal, "NativeQueued", msg)
			}
		}
		return true, ctrl.Result{RequeueAfter: nativeRequeue}, nil
	default:
		return fail("NativePlanFailed", err)
	}
}

// nativePreempt marks one victim Preempted; the existing PhasePreempted path tears its workload down.
func (r *GryviaAIJobReconciler) nativePreempt(ctx context.Context, v scheduler.PreemptionVictim, by *gryviav1.GryviaAIJob) error {
	var ns, name string
	for i := 0; i < len(v.WorkloadID); i++ {
		if v.WorkloadID[i] == '/' {
			ns, name = v.WorkloadID[:i], v.WorkloadID[i+1:]
			break
		}
	}
	var victim gryviav1.GryviaAIJob
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &victim); err != nil {
		return client.IgnoreNotFound(err)
	}
	base := victim.DeepCopy()
	victim.Status.Phase = PhasePreempted
	victim.Status.Message = fmt.Sprintf("preempted by %s/%s (native scheduler)", by.Namespace, by.Name)
	if err := r.Status().Patch(ctx, &victim, client.MergeFrom(base)); err != nil {
		return err
	}
	if r.Recorder != nil {
		r.Recorder.Event(&victim, corev1.EventTypeWarning, "NativePreempted", victim.Status.Message)
	}
	return nil
}

// applyNativeGPUShare applies the fractional-GPU runtime plan (gryvia.io/gpu-share) to the trainer container.
func applyNativeGPUShare(job *gryviav1.GryviaAIJob, annotations map[string]string, spec *corev1.PodSpec) {
	if job.Annotations[AnnotationGPUShare] == "" {
		return
	}
	req, err := scheduler.ParseGPUShare(job.Annotations, scheduler.GPUsPerPlacedNode(job))
	if err != nil {
		return
	}
	plan, err := scheduler.PlanGPURuntime(req, nil)
	if err != nil {
		return
	}
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: annotations}, Spec: *spec}
	if err := scheduler.ApplyRuntimePlan(&pod, 0, plan); err != nil {
		return
	}
	*spec = pod.Spec
}
