// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type fakeRunner struct {
	calls [][]int
	err   error
}

func (f *fakeRunner) Reset(_ context.Context, gpus []int) (string, error) {
	f.calls = append(f.calls, gpus)
	return "GPU reset ok", f.err
}

func node(ann map[string]string, cordoned bool) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1", Annotations: ann}, Spec: corev1.NodeSpec{Unschedulable: cordoned}}
}

func gpuPod(name, nodeName string, owner string, phase corev1.PodPhase) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec: corev1.PodSpec{NodeName: nodeName, Containers: []corev1.Container{{Name: "c", Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{"nvidia.com/gpu": *resource.NewQuantity(1, resource.DecimalSI)}}}}},
		Status: corev1.PodStatus{Phase: phase},
	}
	if owner != "" {
		yes := true
		p.OwnerReferences = []metav1.OwnerReference{{Kind: owner, Name: "o", Controller: &yes}}
	}
	return p
}

func newClient(objs ...client.Object) client.Client {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

func result(t *testing.T, c client.Client) (Result, map[string]string) {
	t.Helper()
	n := &corev1.Node{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "n1"}, n); err != nil {
		t.Fatal(err)
	}
	var r Result
	_ = json.Unmarshal([]byte(n.Annotations[AnnResult]), &r)
	return r, n.Annotations
}

var fixed = func() time.Time { return time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC) }

func TestNoRequestDoesNothing(t *testing.T) {
	c := newClient(node(nil, true))
	a := &Agent{Client: c, Node: "n1", Runner: &fakeRunner{}, Now: fixed}
	if s, err := a.Reconcile(context.Background()); err != nil || s != "" {
		t.Errorf("state=%q err=%v", s, err)
	}
}

func TestDryRunIsTheDefaultAndRunsNothing(t *testing.T) {
	c := newClient(node(map[string]string{AnnRequest: "r1", AnnGPUs: "1, 0"}, true))
	run := &fakeRunner{}
	a := &Agent{Client: c, Node: "n1", Runner: run, Now: fixed}
	if s, err := a.Reconcile(context.Background()); err != nil || s != StateDryRun {
		t.Fatalf("state=%q err=%v", s, err)
	}
	if len(run.calls) != 0 {
		t.Error("dry-run executed the reset")
	}
	r, ann := result(t, c)
	if r.ID != "r1" || !strings.Contains(r.Message, "GPU 0,1") || ann[AnnObserved] != "r1" {
		t.Errorf("result=%+v annotations=%v", r, ann)
	}
	// the same request id is not handled twice
	if s, _ := a.Reconcile(context.Background()); s != "" {
		t.Errorf("second reconcile handled it again: %q", s)
	}
}

func TestExecuteResetsSelectedGPUsOnACordonedIdleNode(t *testing.T) {
	// a DaemonSet GPU pod, a finished GPU pod and a GPU pod on ANOTHER node do not block
	c := newClient(node(map[string]string{AnnRequest: "r2", AnnGPUs: "0,2"}, true),
		gpuPod("ds", "n1", "DaemonSet", corev1.PodRunning), gpuPod("done", "n1", "Job", corev1.PodSucceeded), gpuPod("other", "n2", "Job", corev1.PodRunning))
	run := &fakeRunner{}
	a := &Agent{Client: c, Node: "n1", Execute: true, Runner: run, Now: fixed}
	if s, err := a.Reconcile(context.Background()); err != nil || s != StateDone {
		t.Fatalf("state=%q err=%v", s, err)
	}
	if !reflect.DeepEqual(run.calls, [][]int{{0, 2}}) {
		t.Errorf("calls = %v", run.calls)
	}
	if r, ann := result(t, c); r.State != StateDone || ann[AnnObserved] != "r2" {
		t.Errorf("result=%+v ann=%v", r, ann)
	}
}

func TestAllGPUsWhenNoIndicesGiven(t *testing.T) {
	c := newClient(node(map[string]string{AnnRequest: "r3"}, true))
	run := &fakeRunner{}
	(&Agent{Client: c, Node: "n1", Execute: true, Runner: run, Now: fixed}).Reconcile(context.Background())
	if len(run.calls) != 1 || run.calls[0] != nil {
		t.Errorf("calls = %v, want one call with no indices", run.calls)
	}
}

func TestBlockedWhileNotCordonedOrGPUPodsRemainThenProceeds(t *testing.T) {
	c := newClient(node(map[string]string{AnnRequest: "r4"}, false), gpuPod("train", "n1", "Job", corev1.PodRunning))
	run := &fakeRunner{}
	a := &Agent{Client: c, Node: "n1", Execute: true, Runner: run, Now: fixed}
	if s, _ := a.Reconcile(context.Background()); s != StateBlocked {
		t.Fatalf("uncordoned node: state %q", s)
	}
	if r, ann := result(t, c); !strings.Contains(r.Message, "not cordoned") || ann[AnnObserved] != "" {
		t.Errorf("result=%+v observed=%q", r, ann[AnnObserved])
	}
	n := &corev1.Node{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "n1"}, n)
	n.Spec.Unschedulable = true
	_ = c.Update(context.Background(), n)
	if s, _ := a.Reconcile(context.Background()); s != StateBlocked {
		t.Fatalf("GPU pod still there: state %q", s)
	}
	if r, _ := result(t, c); !strings.Contains(r.Message, "ns/train") {
		t.Errorf("message should name the pod: %q", r.Message)
	}
	if s, _ := a.Reconcile(context.Background()); s != "" { // same block, no rewrite
		t.Errorf("identical blocked state rewritten: %q", s)
	}
	_ = c.Delete(context.Background(), gpuPod("train", "n1", "Job", corev1.PodRunning))
	if s, _ := a.Reconcile(context.Background()); s != StateDone {
		t.Fatalf("after the pod is gone: state %q", s)
	}
	if len(run.calls) != 1 {
		t.Errorf("reset ran %d times, want 1", len(run.calls))
	}
}

func TestFailureIsRecordedAndNotRetriedForTheSameID(t *testing.T) {
	c := newClient(node(map[string]string{AnnRequest: "r5"}, true))
	run := &fakeRunner{err: errors.New("exit status 4")}
	a := &Agent{Client: c, Node: "n1", Execute: true, Runner: run, Now: fixed}
	if s, _ := a.Reconcile(context.Background()); s != StateFailed {
		t.Fatalf("state %q", s)
	}
	if r, ann := result(t, c); !strings.Contains(r.Message, "exit status 4") || ann[AnnObserved] != "r5" {
		t.Errorf("result=%+v ann=%v", r, ann)
	}
	a.Reconcile(context.Background())
	if len(run.calls) != 1 {
		t.Errorf("a failed request was retried automatically (%d calls); a new id is a new request", len(run.calls))
	}
}

func TestInvalidInputIsRejectedWithoutRunning(t *testing.T) {
	for name, ann := range map[string]map[string]string{
		"bad id":      {AnnRequest: "has space"},
		"bad index":   {AnnRequest: "ok", AnnGPUs: "0;rm -rf /"},
		"too big":     {AnnRequest: "ok", AnnGPUs: "64"},
		"negative":    {AnnRequest: "ok", AnnGPUs: "-1"},
		"id too long": {AnnRequest: strings.Repeat("a", 64)},
	} {
		c := newClient(node(ann, true))
		run := &fakeRunner{}
		a := &Agent{Client: c, Node: "n1", Execute: true, Runner: run, Now: fixed}
		if s, _ := a.Reconcile(context.Background()); s != StateInvalid || len(run.calls) != 0 {
			t.Errorf("%s: state=%q calls=%v", name, s, run.calls)
		}
	}
}

func TestParseGPUs(t *testing.T) {
	if got, err := ParseGPUs("3, 1,1"); err != nil || !reflect.DeepEqual(got, []int{1, 3}) {
		t.Errorf("got %v err %v", got, err)
	}
	if got, err := ParseGPUs(""); err != nil || got != nil {
		t.Errorf("empty = %v err %v", got, err)
	}
}

func TestAgentActsOnlyOnItsOwnNode(t *testing.T) {
	other := node(map[string]string{AnnRequest: "x"}, true)
	other.Name = "n2"
	c := newClient(node(nil, true), other)
	run := &fakeRunner{}
	a := &Agent{Client: c, Node: "n1", Execute: true, Runner: run, Now: fixed}
	a.Reconcile(context.Background())
	if len(run.calls) != 0 {
		t.Error("the agent on n1 acted on n2's request")
	}
}
