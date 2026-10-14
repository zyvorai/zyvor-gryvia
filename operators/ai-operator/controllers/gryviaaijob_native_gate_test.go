package controllers

import (
	"context"
	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"testing"
	"time"
)

func gatePolicy(text string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: NativeConfigMapName, Namespace: NativeConfigNamespace}, Data: map[string]string{"policy.yaml": text}}
}
func gateNode() *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu"}, Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{gpuResource: resource.MustParse("8")}}}
}
func gateJob(name string, priority int32) *gryviav1.GryviaAIJob {
	j := newTestAIJob(name, "default")
	j.Spec.GPUs = 1
	j.Spec.Priority = priority
	j.Status.Phase = PhasePending
	j.CreationTimestamp = metav1.NewTime(time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC))
	return j
}

const validGatePolicy = "queues:\n- name: default\n  maxGPU: 8000\n  borrow: true\n"

func callGate(t *testing.T, r *GryviaAIJobReconciler, c client.Client, name string) (bool, *gryviav1.GryviaAIJob) {
	t.Helper()
	j := &gryviav1.GryviaAIJob{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, j); err != nil {
		t.Fatal(err)
	}
	done, _, err := r.nativeQueueGate(context.Background(), j)
	if err != nil {
		t.Fatal(err)
	}
	return done, j
}
func TestNativeGateFailsClosedAndRecovers(t *testing.T) {
	for _, policy := range []string{"", "queues: [broken", "queues: []", "queues:\n- name: other\n  borrow: true\n"} {
		t.Run(policy, func(t *testing.T) {
			j := gateJob("job", 1)
			objs := []client.Object{j, gateNode()}
			if policy != "" {
				objs = append(objs, gatePolicy(policy))
			}
			r, c := newAIJobReconciler(objs...)
			r.NativeQueueScheduler = true
			done, current := callGate(t, r, c, j.Name)
			if !done || current.Status.Phase != PhaseQueued || !isNativeQueued(current) || meta.IsStatusConditionTrue(current.Status.Conditions, ConditionNativeAdmitted) {
				t.Fatalf("admitted invalid policy: %+v", current.Status)
			}
			if policy == "" {
				if err := c.Create(context.Background(), gatePolicy(validGatePolicy)); err != nil {
					t.Fatal(err)
				}
			} else {
				cm := &corev1.ConfigMap{}
				if err := c.Get(context.Background(), types.NamespacedName{Name: NativeConfigMapName, Namespace: NativeConfigNamespace}, cm); err != nil {
					t.Fatal(err)
				}
				cm.Data["policy.yaml"] = validGatePolicy
				if err := c.Update(context.Background(), cm); err != nil {
					t.Fatal(err)
				}
			}
			_, current = callGate(t, r, c, j.Name)
			if current.Status.Phase != PhasePending || !meta.IsStatusConditionTrue(current.Status.Conditions, ConditionNativeAdmitted) {
				t.Fatalf("did not recover: %+v", current.Status)
			}
		})
	}
}
func TestNativeGatePriorityDoesNotDependOnReconcileOrder(t *testing.T) {
	r, c := newAIJobReconciler(gateJob("low", 1), gateJob("high", 100), gateNode(), gatePolicy(validGatePolicy))
	r.NativeQueueScheduler = true
	done, low := callGate(t, r, c, "low")
	if !done || low.Status.Phase != PhaseQueued {
		t.Fatal("low job bypassed waiting high job")
	}
	done, high := callGate(t, r, c, "high")
	if done || !meta.IsStatusConditionTrue(high.Status.Conditions, ConditionNativeAdmitted) {
		t.Fatal("high job was not admitted")
	}
}
func TestNativeGateSkipsOversizedHead(t *testing.T) {
	big := gateJob("big", 100)
	big.Spec.GPUs = 9
	r, c := newAIJobReconciler(big, gateJob("small", 1), gateNode(), gatePolicy(validGatePolicy))
	r.NativeQueueScheduler = true
	done, small := callGate(t, r, c, "small")
	if done || !meta.IsStatusConditionTrue(small.Status.Conditions, ConditionNativeAdmitted) {
		t.Fatal("blocked head starved fitting job")
	}
}
func TestNativeGateCPUBypassAndMalformedPendingIsolation(t *testing.T) {
	cpu := gateJob("cpu", 100)
	cpu.Spec.GPUs = 0
	bad := gateJob("bad", 99)
	bad.Annotations = map[string]string{AnnotationGPUShare: "invalid"}
	r, c := newAIJobReconciler(cpu, bad, gateJob("valid", 1), gateNode(), gatePolicy(validGatePolicy))
	r.NativeQueueScheduler = true
	done, j := callGate(t, r, c, "cpu")
	if done || j.Status.Phase != PhasePending {
		t.Fatal("CPU job queued")
	}
	done, j = callGate(t, r, c, "valid")
	if done || !meta.IsStatusConditionTrue(j.Status.Conditions, ConditionNativeAdmitted) {
		t.Fatal("invalid/CPU pending job poisoned queue")
	}
}
func TestNativeGateKeepsAlreadyAdmittedReservation(t *testing.T) {
	j := gateJob("admitted", 1)
	meta.SetStatusCondition(&j.Status.Conditions, metav1.Condition{Type: ConditionNativeAdmitted, Status: metav1.ConditionTrue, Reason: "Admitted"})
	r, c := newAIJobReconciler(j, gateJob("new-high", 100), gateNode(), gatePolicy(validGatePolicy))
	r.NativeQueueScheduler = true
	done, _ := callGate(t, r, c, j.Name)
	if done {
		t.Fatal("reordered existing admitted job")
	}
}

func TestNativeGateOrdersUnderShareQueueFirst(t *testing.T) {
	a := gateJob("waiting-a", 10)
	a.Annotations = map[string]string{AnnotationNativeQueue: "a"}
	b := gateJob("waiting-b", 10)
	b.Annotations = map[string]string{AnnotationNativeQueue: "b"}
	active := gateJob("active-a", 1)
	active.Annotations = map[string]string{AnnotationNativeQueue: "a"}
	active.Status.Phase = PhaseRunning
	active.Spec.GPUs = 2
	policy := "queues:\n- name: a\n  guaranteeGPU: 2000\n  maxGPU: 8000\n  borrow: true\n- name: b\n  guaranteeGPU: 2000\n  maxGPU: 8000\n  borrow: true\n"
	r, c := newAIJobReconciler(a, b, active, gateNode(), gatePolicy(policy))
	r.NativeQueueScheduler = true
	done, _ := callGate(t, r, c, a.Name)
	if !done {
		t.Fatal("over-share queue bypassed under-share queue")
	}
	done, current := callGate(t, r, c, b.Name)
	if done || !meta.IsStatusConditionTrue(current.Status.Conditions, ConditionNativeAdmitted) {
		t.Fatal("under-share queue not admitted")
	}
}
func TestNativeGateDisabledPreservesExistingBehavior(t *testing.T) {
	r, c := newAIJobReconciler(gateJob("j", 1))
	done, j := callGate(t, r, c, "j")
	if done || j.Status.Phase != PhasePending {
		t.Fatal("disabled gate changed behavior")
	}
}

func TestNativeGateDoesNotLaunchFullElasticShapeFromPartialPlan(t *testing.T) {
	j := gateJob("elastic", 1)
	j.Spec.Distributed = &gryviav1.DistributedConfig{Enabled: true, Nodes: 4, GpusPerNode: 1, Elastic: &gryviav1.ElasticConfig{MinNodes: 1}}
	r, c := newAIJobReconciler(j, gateNode(), gatePolicy("queues:\n- name: default\n  maxGPU: 2000\n  borrow: true\n"))
	r.NativeQueueScheduler = true
	done, current := callGate(t, r, c, j.Name)
	if !done || current.Status.Phase != PhaseQueued || meta.IsStatusConditionTrue(current.Status.Conditions, ConditionNativeAdmitted) {
		t.Fatalf("partial reservation could launch full shape: %+v", current.Status)
	}
}
