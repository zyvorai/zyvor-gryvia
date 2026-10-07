package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type fakeRebooter struct {
	methods []string
	err     error
}

func (f *fakeRebooter) Reboot(_ context.Context, method string) error {
	f.methods = append(f.methods, method)
	return f.err
}

func leaseClient(objs ...client.Object) client.Client {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = coordinationv1.AddToScheme(s)
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

func driverPod(name, app string, created time.Time, ready bool) *corev1.Pod {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "gpu-operator", Labels: map[string]string{"app": app},
		CreationTimestamp: metav1.NewTime(created)}, Spec: corev1.PodSpec{NodeName: "n1"}}
	if ready {
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	}
	return p
}

func recoveryNode(action string) *corev1.Node {
	n := node(map[string]string{AnnRequest: "r1", AnnAction: action}, true)
	n.Status.NodeInfo.BootID = "boot-1"
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	return n
}

func setBoot(t *testing.T, c client.Client, boot string) {
	t.Helper()
	n := &corev1.Node{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "n1"}, n); err != nil {
		t.Fatal(err)
	}
	n.Status.NodeInfo.BootID = boot
	if err := c.Status().Update(context.Background(), n); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryActionsNeedPermission(t *testing.T) {
	for _, action := range []string{ActionDriverReload, ActionReboot} {
		c := leaseClient(recoveryNode(action))
		a := &Agent{Client: c, Node: "n1", Execute: true, Rebooter: &fakeRebooter{}, Now: fixed}
		if s, _ := a.Reconcile(context.Background()); s != StateInvalid {
			t.Errorf("%s without permission: %s", action, s)
		}
	}
	c := leaseClient(recoveryNode("format-disk"))
	a := &Agent{Client: c, Node: "n1", Execute: true, Now: fixed}
	if s, _ := a.Reconcile(context.Background()); s != StateInvalid {
		t.Errorf("unknown action: %s", s)
	}
}

func TestRecoveryDryRunActsOnNothing(t *testing.T) {
	c := leaseClient(recoveryNode(ActionDriverReload), driverPod("drv", "nvidia-driver-daemonset", fixed().Add(-time.Hour), true))
	reb := &fakeRebooter{}
	a := &Agent{Client: c, Node: "n1", AllowDriverReload: true, AllowReboot: true, Rebooter: reb, LeaseNamespace: "gryvia", Now: fixed}
	if s, _ := a.Reconcile(context.Background()); s != StateDryRun {
		t.Fatalf("dry-run: %s", s)
	}
	pods := &corev1.PodList{}
	_ = c.List(context.Background(), pods)
	lease := &coordinationv1.Lease{}
	if len(pods.Items) != 1 || len(reb.methods) != 0 || c.Get(context.Background(), types.NamespacedName{Namespace: "gryvia", Name: LeaseName}, lease) == nil {
		t.Fatal("dry-run deleted pods, rebooted or took the lease")
	}
}

func TestDriverReloadLifecycle(t *testing.T) {
	now := fixed()
	clock := func() time.Time { return now }
	c := leaseClient(recoveryNode(ActionDriverReload),
		driverPod("drv-old", "nvidia-driver-daemonset", now.Add(-time.Hour), true),
		driverPod("dp-old", "nvidia-device-plugin-daemonset", now.Add(-time.Hour), true),
		driverPod("other", "my-app", now.Add(-time.Hour), true))
	a := &Agent{Client: c, Node: "n1", Execute: true, AllowDriverReload: true, LeaseNamespace: "gryvia", Now: clock}

	if s, err := a.Reconcile(context.Background()); err != nil || s != StateInProgress {
		t.Fatalf("start: %s %v", s, err)
	}
	r, ann := result(t, c)
	if strings.Join(r.Apps, ",") != "nvidia-device-plugin-daemonset,nvidia-driver-daemonset" || ann[AnnObserved] != "" {
		t.Fatalf("in-progress record %+v observed %q", r, ann[AnnObserved])
	}
	pods := &corev1.PodList{}
	_ = c.List(context.Background(), pods)
	if len(pods.Items) != 1 || pods.Items[0].Name != "other" {
		t.Fatalf("only GPU Operator pods are deleted, left %d", len(pods.Items))
	}
	lease := &coordinationv1.Lease{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "gryvia", Name: LeaseName}, lease); err != nil || *lease.Spec.HolderIdentity != "n1" {
		t.Fatalf("lease not taken: %v", err)
	}

	now = now.Add(time.Minute)
	_ = c.Create(context.Background(), driverPod("drv-new", "nvidia-driver-daemonset", now, true))
	if s, _ := a.Reconcile(context.Background()); s != "" {
		t.Fatalf("still waiting for the device plugin, got %q", s)
	}
	_ = c.Create(context.Background(), driverPod("dp-new", "nvidia-device-plugin-daemonset", now, true))
	if s, _ := a.Reconcile(context.Background()); s != StateDone {
		t.Fatalf("done: %q", s)
	}
	if _, ann := result(t, c); ann[AnnObserved] != "r1" {
		t.Fatal("a finished reload must mark the request observed")
	}
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "gryvia", Name: LeaseName}, lease)
	if lease.Spec.HolderIdentity != nil {
		t.Fatal("lease not released")
	}
}

func TestDriverReloadTimesOut(t *testing.T) {
	now := fixed()
	c := leaseClient(recoveryNode(ActionDriverReload), driverPod("drv", "nvidia-driver-daemonset", now.Add(-time.Hour), true))
	a := &Agent{Client: c, Node: "n1", Execute: true, AllowDriverReload: true, Now: func() time.Time { return now }}
	_, _ = a.Reconcile(context.Background())
	now = now.Add(DriverReloadTimeout + time.Minute)
	if s, _ := a.Reconcile(context.Background()); s != StateFailed {
		t.Fatalf("timeout: %q", s)
	}
}

func TestRebootLifecycleAndLease(t *testing.T) {
	now := fixed()
	other := "n2"
	dur := int32(3600)
	renew := metav1.NewMicroTime(now.Add(-time.Minute))
	held := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: LeaseName, Namespace: "gryvia"},
		Spec: coordinationv1.LeaseSpec{HolderIdentity: &other, LeaseDurationSeconds: &dur, RenewTime: &renew}}
	c := leaseClient(recoveryNode(ActionReboot), held)
	reb := &fakeRebooter{}
	a := &Agent{Client: c, Node: "n1", Execute: true, AllowReboot: true, Rebooter: reb, LeaseNamespace: "gryvia", Now: func() time.Time { return now }}

	if s, _ := a.Reconcile(context.Background()); s != StateBlocked || len(reb.methods) != 0 {
		t.Fatalf("another node holds the lease: %q, reboots %v", s, reb.methods)
	}
	if r, _ := result(t, c); !strings.Contains(r.Message, "n2") {
		t.Fatalf("blocked message %q", r.Message)
	}

	now = now.Add(2 * time.Hour) // n2's lease expired
	if s, _ := a.Reconcile(context.Background()); s != StateInProgress || len(reb.methods) != 1 || reb.methods[0] != RebootKured {
		t.Fatalf("reboot start: %q %v", s, reb.methods)
	}
	if r, _ := result(t, c); r.BootID != "boot-1" {
		t.Fatalf("boot id not recorded: %+v", r)
	}
	if s, _ := a.Reconcile(context.Background()); s != "" || len(reb.methods) != 1 {
		t.Fatal("an in-progress reboot must not be started again")
	}
	setBoot(t, c, "boot-2")
	if s, _ := a.Reconcile(context.Background()); s != StateDone {
		t.Fatalf("after the boot id changed: %q", s)
	}
}

func TestRebootStartFailureReleasesLease(t *testing.T) {
	c := leaseClient(recoveryNode(ActionReboot))
	a := &Agent{Client: c, Node: "n1", Execute: true, AllowReboot: true, Rebooter: &fakeRebooter{err: errors.New("no kured")}, LeaseNamespace: "gryvia", Now: fixed}
	if s, _ := a.Reconcile(context.Background()); s != StateFailed {
		t.Fatalf("failed start: %q", s)
	}
	lease := &coordinationv1.Lease{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "gryvia", Name: LeaseName}, lease); err != nil || lease.Spec.HolderIdentity != nil {
		t.Fatal("lease must be released after a failed start")
	}
}

func TestKuredSentinel(t *testing.T) {
	path := t.TempDir() + "/run/reboot-required"
	if err := (HostRebooter{Sentinel: path}).Reboot(context.Background(), RebootKured); err != nil {
		t.Fatal(err)
	}
	if err := (HostRebooter{}).Reboot(context.Background(), RebootKured); err == nil {
		t.Fatal("no sentinel path must fail")
	}
	if err := (HostRebooter{Sentinel: path}).Reboot(context.Background(), "magic"); err == nil {
		t.Fatal("unknown method must fail")
	}
}
