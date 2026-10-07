package controllers

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gryviav1 "github.com/zyvorai/gryvia/operators/gpu-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/gpu-operator/pkg/agent"
)

func ladderHC(rc gryviav1.RemediationConfig) *gryviav1.GryviaHealthCheck {
	return &gryviav1.GryviaHealthCheck{ObjectMeta: metav1.ObjectMeta{Name: "health", UID: "h"}, Spec: gryviav1.GryviaHealthCheckSpec{
		OnFailure: &gryviav1.FailureActions{Cordon: true, AutoRemediate: true}, Remediation: &rc}}
}

func quarantinedNode(owner string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu", Annotations: map[string]string{quarantineOwner: owner}},
		Spec: corev1.NodeSpec{Unschedulable: true, Taints: []corev1.Taint{{Key: unhealthyTaint, Value: "true", Effect: corev1.TaintEffectNoSchedule}, {Key: "keep", Effect: corev1.TaintEffectNoSchedule}}}}
}

func getNode(t *testing.T, c client.Client) *corev1.Node {
	t.Helper()
	n := &corev1.Node{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "gpu"}, n); err != nil {
		t.Fatal(err)
	}
	return n
}

// agentAnswers plays the reset agent: it writes a result for the current request.
func agentAnswers(t *testing.T, c client.Client, state string, at time.Time) {
	t.Helper()
	n := getNode(t, c)
	b, _ := json.Marshal(agent.Result{ID: n.Annotations[agent.AnnRequest], State: state, Message: "agent says " + state, At: at.UTC().Format(time.RFC3339)})
	n.Annotations[agent.AnnResult] = string(b)
	if err := c.Update(context.Background(), n); err != nil {
		t.Fatal(err)
	}
}

func step(t *testing.T, r *GryviaHealthCheckReconciler, hc *gryviav1.GryviaHealthCheck, bad bool, now time.Time) recoverySummary {
	t.Helper()
	nodes := &corev1.NodeList{}
	_ = r.List(context.Background(), nodes)
	sum, err := r.reconcileRecovery(context.Background(), hc, nodes.Items, map[string]bool{"gpu": bad}, now)
	if err != nil {
		t.Fatal(err)
	}
	return sum
}

func TestRecoveryLadderEscalatesThenLiftsQuarantine(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newGpuNodeTestScheme()).WithObjects(quarantinedNode("h")).Build()
	r := &GryviaHealthCheckReconciler{Client: c, EnableRemediation: true}
	hc := ladderHC(gryviav1.RemediationConfig{GpuReset: true, DriverReload: true, NodeReboot: true})
	t0 := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

	step(t, r, hc, true, t0)
	n := getNode(t, c)
	first := n.Annotations[agent.AnnRequest]
	if n.Annotations[agent.AnnAction] != agent.ActionReset || first == "" || n.Annotations[annRecoveryAttempts] != "1" {
		t.Fatalf("first request %v", n.Annotations)
	}
	if sum := step(t, r, hc, true, t0.Add(time.Minute)); len(sum.recovering) != 1 || getNode(t, c).Annotations[agent.AnnRequest] != first {
		t.Fatal("must wait for the agent, not re-request")
	}

	agentAnswers(t, c, agent.StateDone, t0.Add(2*time.Minute))
	if sum := step(t, r, hc, true, t0.Add(2*time.Minute)); getNode(t, c).Annotations[agent.AnnAction] != agent.ActionReset || len(sum.recovering) != 1 {
		t.Fatal("must not judge the reset before a check runs after it")
	}
	step(t, r, hc, true, t0.Add(3*time.Minute))
	n = getNode(t, c)
	if n.Annotations[agent.AnnAction] != agent.ActionDriverReload || n.Annotations[agent.AnnRequest] == first {
		t.Fatalf("reset done but still failing must escalate to driver-reload: %v", n.Annotations)
	}

	agentAnswers(t, c, agent.StateFailed, t0.Add(4*time.Minute))
	step(t, r, hc, true, t0.Add(5*time.Minute))
	if getNode(t, c).Annotations[agent.AnnAction] != agent.ActionReboot {
		t.Fatal("failed driver reload must escalate to reboot")
	}

	agentAnswers(t, c, agent.StateDone, t0.Add(6*time.Minute))
	sum := step(t, r, hc, false, t0.Add(7*time.Minute))
	n = getNode(t, c)
	if len(sum.recovered) != 1 || n.Spec.Unschedulable || len(n.Spec.Taints) != 1 || n.Spec.Taints[0].Key != "keep" {
		t.Fatalf("passing check after reboot must lift quarantine: %+v", n.Spec)
	}
	for _, k := range []string{quarantineOwner, agent.AnnRequest, agent.AnnAction, agent.AnnResult, annRecoveryAttempts, annRecoveryRecorded} {
		if _, ok := n.Annotations[k]; ok {
			t.Errorf("annotation %s left behind", k)
		}
	}
	var actions []string
	for _, e := range hc.Status.RemediationHistory {
		actions = append(actions, e.Action)
	}
	if want := "reset,driver-reload,reboot,uncordon"; strings.Join(actions, ",") != want {
		t.Fatalf("history %v, want %s", actions, want)
	}
}

func TestRecoveryExhaustedAtMaxAttempts(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newGpuNodeTestScheme()).WithObjects(quarantinedNode("h")).Build()
	r := &GryviaHealthCheckReconciler{Client: c, EnableRemediation: true}
	hc := ladderHC(gryviav1.RemediationConfig{GpuReset: true, NodeReboot: true, MaxAttempts: 1})
	t0 := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	step(t, r, hc, true, t0)
	agentAnswers(t, c, agent.StateFailed, t0.Add(time.Minute))
	sum := step(t, r, hc, true, t0.Add(2*time.Minute))
	n := getNode(t, c)
	if len(sum.exhausted) != 1 || n.Annotations[agent.AnnAction] != agent.ActionReset || !n.Spec.Unschedulable {
		t.Fatalf("maxAttempts=1 must stop after the reset and keep the quarantine: %+v %v", sum, n.Annotations)
	}
	if len(hc.Status.RemediationHistory) != 1 {
		t.Fatal("each result is recorded once")
	}
	step(t, r, hc, true, t0.Add(3*time.Minute))
	if len(hc.Status.RemediationHistory) != 1 {
		t.Fatal("result recorded twice")
	}
}

func TestRecoveryDryRunAgentDoesNotEscalate(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newGpuNodeTestScheme()).WithObjects(quarantinedNode("h")).Build()
	r := &GryviaHealthCheckReconciler{Client: c, EnableRemediation: true}
	hc := ladderHC(gryviav1.RemediationConfig{GpuReset: true, NodeReboot: true})
	t0 := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	step(t, r, hc, true, t0)
	agentAnswers(t, c, agent.StateDryRun, t0.Add(time.Minute))
	sum := step(t, r, hc, true, t0.Add(2*time.Minute))
	if len(sum.dryRun) != 1 || getNode(t, c).Annotations[agent.AnnAction] != agent.ActionReset {
		t.Fatalf("dry-run must stop the ladder: %+v", sum)
	}
}

func TestRecoveryLeavesOtherNodesAndDisabledLadderAlone(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newGpuNodeTestScheme()).WithObjects(quarantinedNode("someone-else")).Build()
	r := &GryviaHealthCheckReconciler{Client: c, EnableRemediation: true}
	t0 := time.Now()
	step(t, r, ladderHC(gryviav1.RemediationConfig{GpuReset: true}), true, t0)
	if _, ok := getNode(t, c).Annotations[agent.AnnRequest]; ok {
		t.Fatal("acted on a node another health check quarantined")
	}

	c = fake.NewClientBuilder().WithScheme(newGpuNodeTestScheme()).WithObjects(quarantinedNode("h")).Build()
	for _, r := range []*GryviaHealthCheckReconciler{{Client: c}, {Client: c, EnableRemediation: true}} {
		hc := ladderHC(gryviav1.RemediationConfig{GpuReset: true})
		if r.EnableRemediation {
			hc.Spec.OnFailure.AutoRemediate = false
		}
		step(t, r, hc, true, t0)
		if _, ok := getNode(t, c).Annotations[agent.AnnRequest]; ok {
			t.Fatal("ladder ran without remediation enabled and autoRemediate")
		}
	}
}
