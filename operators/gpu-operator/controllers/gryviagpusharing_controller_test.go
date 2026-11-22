// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	gryviav1 "github.com/zyvorai/gryvia/operators/gpu-operator/api/v1"
)

func sharingGpuNode(name, nodeName, gpuType string, gpus int) *gryviav1.GryviaGpuNode {
	return &gryviav1.GryviaGpuNode{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       gryviav1.GryviaGpuNodeSpec{NodeName: nodeName, GpuType: gpuType, GpuCount: gpus},
	}
}

// reconcileSharing runs one reconcile of policy against a fake cluster and returns the nodes.
func reconcileSharing(t *testing.T, policy *gryviav1.GryviaGPUSharingPolicy, objs ...client.Object) (client.Client, *gryviav1.GryviaGPUSharingPolicy) {
	t.Helper()
	scheme := newGpuNodeTestScheme()
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(append(objs, policy)...).
		WithStatusSubresource(&gryviav1.GryviaGPUSharingPolicy{}).
		Build()
	r := &GryviaGPUSharingPolicyReconciler{Client: c, Scheme: scheme, Log: ctrl.Log.WithName("test")}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: policy.Name}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := &gryviav1.GryviaGPUSharingPolicy{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: policy.Name}, got); err != nil {
		t.Fatal(err)
	}
	return c, got
}

func nodeLabels(t *testing.T, c client.Client, name string) map[string]string {
	t.Helper()
	n := &corev1.Node{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, n); err != nil {
		t.Fatal(err)
	}
	return n.Labels
}

func wantLabel(t *testing.T, labels map[string]string, key, want string) {
	t.Helper()
	if got, ok := labels[key]; !ok || got != want {
		t.Errorf("label %s = %q (present=%v), want %q; labels: %v", key, got, ok, want, labels)
	}
}

// A time-slicing policy puts the label the NVIDIA GPU Operator's device plugin reads on the matching nodes.
func TestSharingTimeSlicingWritesNVIDIADevicePluginLabel(t *testing.T) {
	policy := &gryviav1.GryviaGPUSharingPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "share-inference"},
		Spec: gryviav1.GryviaGPUSharingPolicySpec{
			Strategy:    "time-slicing",
			TimeSlicing: &gryviav1.GPUTimeSlicingConfig{Enabled: true, MaxPodsPerGPU: 4},
		},
	}
	c, got := reconcileSharing(t, policy,
		sharingGpuNode("gpu-1", "node-1", "A100", 2),
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
	)
	l := nodeLabels(t, c, "node-1")
	wantLabel(t, l, "nvidia.com/device-plugin.config", "gryvia-time-slicing")
	wantLabel(t, l, "gryvia.io/gpu-sharing", "time-slicing")
	wantLabel(t, l, "gryvia.io/max-pods-per-gpu", "4")
	if len(got.Status.AffectedNodes) != 1 || got.Status.AffectedNodes[0] != "node-1" || got.Status.TotalGPUs != 2 {
		t.Errorf("status: affected=%v total=%d, want [node-1] and 2", got.Status.AffectedNodes, got.Status.TotalGPUs)
	}
}

// A MIG policy sets nvidia.com/mig.config to its first profile, which must be a real mig-parted name.
func TestSharingMIGWritesNVIDIAMigConfigLabel(t *testing.T) {
	policy := &gryviav1.GryviaGPUSharingPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "mig-small"},
		Spec: gryviav1.GryviaGPUSharingPolicySpec{
			Strategy: "mig",
			MIG:      &gryviav1.GPUMIGConfig{Enabled: true, Profiles: []gryviav1.GPUMIGProfile{{Name: "all-1g.10gb", Count: 7}}},
		},
	}
	c, _ := reconcileSharing(t, policy,
		sharingGpuNode("gpu-1", "node-1", "A100", 1),
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
	)
	l := nodeLabels(t, c, "node-1")
	wantLabel(t, l, "nvidia.com/mig.config", "all-1g.10gb")
	wantLabel(t, l, "gryvia.io/gpu-sharing", "mig")
	wantLabel(t, l, "gryvia.io/mig-all-1g.10gb", "7")
}

// Only nodes the policy's nodeSelector matches are labelled; others are left alone.
func TestSharingLeavesNonMatchingNodesAlone(t *testing.T) {
	policy := &gryviav1.GryviaGPUSharingPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "a100-only"},
		Spec: gryviav1.GryviaGPUSharingPolicySpec{
			NodeSelector: map[string]string{"gryvia.io/gpu-type": "A100"},
			Strategy:     "time-slicing",
			TimeSlicing:  &gryviav1.GPUTimeSlicingConfig{Enabled: true, MaxPodsPerGPU: 2},
		},
	}
	c, _ := reconcileSharing(t, policy,
		sharingGpuNode("gpu-a", "node-a", "A100", 1),
		sharingGpuNode("gpu-h", "node-h", "H100", 1),
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-h"}},
	)
	wantLabel(t, nodeLabels(t, c, "node-a"), "nvidia.com/device-plugin.config", "gryvia-time-slicing")
	if _, ok := nodeLabels(t, c, "node-h")["nvidia.com/device-plugin.config"]; ok {
		t.Error("the H100 node does not match the selector but was labelled")
	}
}

// A strategy whose own block is disabled writes nothing.
func TestSharingDisabledBlockWritesNoLabels(t *testing.T) {
	policy := &gryviav1.GryviaGPUSharingPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "off"},
		Spec: gryviav1.GryviaGPUSharingPolicySpec{
			Strategy:    "time-slicing",
			TimeSlicing: &gryviav1.GPUTimeSlicingConfig{Enabled: false, MaxPodsPerGPU: 4},
		},
	}
	c, _ := reconcileSharing(t, policy,
		sharingGpuNode("gpu-1", "node-1", "A100", 1),
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
	)
	if l := nodeLabels(t, c, "node-1"); len(l) != 0 {
		t.Errorf("a disabled policy labelled the node: %v", l)
	}
}

func readyCondition(t *testing.T, p *gryviav1.GryviaGPUSharingPolicy) metav1.Condition {
	t.Helper()
	for _, c := range p.Status.Conditions {
		if c.Type == "Ready" {
			return c
		}
	}
	t.Fatalf("no Ready condition: %v", p.Status.Conditions)
	return metav1.Condition{}
}

func timeSlicingPolicy(name string, enabled bool) *gryviav1.GryviaGPUSharingPolicy {
	return &gryviav1.GryviaGPUSharingPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: gryviav1.GryviaGPUSharingPolicySpec{
			Strategy:    "time-slicing",
			TimeSlicing: &gryviav1.GPUTimeSlicingConfig{Enabled: enabled, MaxPodsPerGPU: 4},
		},
	}
}

// Ready=True only after the labels were written, and the message says the GPU figure is an estimate.
func TestSharingReadyTrueWhenLabelsWritten(t *testing.T) {
	_, got := reconcileSharing(t, timeSlicingPolicy("ok", true),
		sharingGpuNode("gpu-1", "node-1", "A100", 2),
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
	)
	c := readyCondition(t, got)
	if c.Status != metav1.ConditionTrue || c.Reason != "LabelsApplied" {
		t.Errorf("Ready = %s/%s, want True/LabelsApplied (%s)", c.Status, c.Reason, c.Message)
	}
	if !strings.Contains(c.Message, "estimated") {
		t.Errorf("message should call the GPU figure an estimate: %q", c.Message)
	}
}

// A disabled strategy block must not report Ready.
func TestSharingNotReadyWhenStrategyDisabled(t *testing.T) {
	_, got := reconcileSharing(t, timeSlicingPolicy("off", false),
		sharingGpuNode("gpu-1", "node-1", "A100", 1),
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
	)
	if c := readyCondition(t, got); c.Status != metav1.ConditionFalse || c.Reason != "StrategyNotEnabled" {
		t.Errorf("Ready = %s/%s, want False/StrategyNotEnabled", c.Status, c.Reason)
	}
}

// No Kubernetes Node behind the GryviaGpuNode means nothing was labelled, so not Ready.
func TestSharingNotReadyWhenNodeMissing(t *testing.T) {
	_, got := reconcileSharing(t, timeSlicingPolicy("missing", true),
		sharingGpuNode("gpu-1", "ghost", "A100", 1),
	)
	if c := readyCondition(t, got); c.Status != metav1.ConditionFalse || c.Reason != "NodesNotFound" {
		t.Errorf("Ready = %s/%s, want False/NodesNotFound", c.Status, c.Reason)
	}
}

// A failed label write must surface as Ready=False, not be swallowed.
func TestSharingNotReadyWhenLabelWriteFails(t *testing.T) {
	policy := timeSlicingPolicy("fail", true)
	scheme := newGpuNodeTestScheme()
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy, sharingGpuNode("gpu-1", "node-1", "A100", 1),
			&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}).
		WithStatusSubresource(&gryviav1.GryviaGPUSharingPolicy{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*corev1.Node); ok {
					return fmt.Errorf("forbidden: nodes is forbidden")
				}
				return cl.Update(ctx, obj, opts...)
			},
		}).
		Build()
	r := &GryviaGPUSharingPolicyReconciler{Client: c, Scheme: scheme, Log: ctrl.Log.WithName("test")}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "fail"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := &gryviav1.GryviaGPUSharingPolicy{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "fail"}, got); err != nil {
		t.Fatal(err)
	}
	if cond := readyCondition(t, got); cond.Status != metav1.ConditionFalse || cond.Reason != "LabelWriteFailed" {
		t.Errorf("Ready = %s/%s, want False/LabelWriteFailed (%s)", cond.Status, cond.Reason, cond.Message)
	}
}

func reconcileWith(t *testing.T, r *GryviaGPUSharingPolicyReconciler, c client.Client, name string) *gryviav1.GryviaGPUSharingPolicy {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := &gryviav1.GryviaGPUSharingPolicy{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, got); err != nil {
		t.Fatal(err)
	}
	return got
}

// With a device-plugin ConfigMap configured, the policy's maxPodsPerGPU is what the plugin is told,
// and the node label points at that key. The chart's own key is left alone.
func TestSharingWritesDevicePluginConfigForReplicas(t *testing.T) {
	policy := timeSlicingPolicy("ts8", true)
	policy.Spec.TimeSlicing.MaxPodsPerGPU = 8
	scheme := newGpuNodeTestScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(policy, sharingGpuNode("gpu-1", "node-1", "A100", 1),
			&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "gpu", Name: "plugin-cfg"},
				Data: map[string]string{"gryvia-time-slicing": "chart-owned"}}).
		WithStatusSubresource(&gryviav1.GryviaGPUSharingPolicy{}).Build()
	r := &GryviaGPUSharingPolicyReconciler{Client: c, Scheme: scheme, Log: ctrl.Log.WithName("test"),
		DevicePluginConfigName: "plugin-cfg", DevicePluginConfigNamespace: "gpu"}
	got := reconcileWith(t, r, c, "ts8")

	cm := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "gpu", Name: "plugin-cfg"}, cm); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cm.Data["gryvia-ts-8"], "replicas: 8") {
		t.Errorf("key gryvia-ts-8 missing or wrong: %q", cm.Data["gryvia-ts-8"])
	}
	if cm.Data["gryvia-time-slicing"] != "chart-owned" {
		t.Errorf("the chart's key was modified: %q", cm.Data["gryvia-time-slicing"])
	}
	wantLabel(t, nodeLabels(t, c, "node-1"), "nvidia.com/device-plugin.config", "gryvia-ts-8")
	if cond := readyCondition(t, got); cond.Status != metav1.ConditionTrue {
		t.Errorf("Ready = %s/%s (%s)", cond.Status, cond.Reason, cond.Message)
	}
}

// The ConfigMap is created when missing; a failed write is Ready=False, not swallowed.
func TestSharingCreatesConfigMapAndReportsWriteFailure(t *testing.T) {
	policy := timeSlicingPolicy("ts4", true)
	scheme := newGpuNodeTestScheme()
	build := func(fail bool) (*GryviaGPUSharingPolicyReconciler, client.Client) {
		b := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(policy.DeepCopy(), sharingGpuNode("gpu-1", "node-1", "A100", 1),
				&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}).
			WithStatusSubresource(&gryviav1.GryviaGPUSharingPolicy{})
		if fail {
			b = b.WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*corev1.ConfigMap); ok {
					return fmt.Errorf("forbidden")
				}
				return cl.Create(ctx, obj, opts...)
			}})
		}
		c := b.Build()
		return &GryviaGPUSharingPolicyReconciler{Client: c, Scheme: scheme, Log: ctrl.Log.WithName("test"),
			DevicePluginConfigName: "plugin-cfg", DevicePluginConfigNamespace: "gpu"}, c
	}
	r, c := build(false)
	reconcileWith(t, r, c, "ts4")
	cm := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "gpu", Name: "plugin-cfg"}, cm); err != nil || !strings.Contains(cm.Data["gryvia-ts-4"], "replicas: 4") {
		t.Fatalf("ConfigMap not created: %v %v", err, cm.Data)
	}
	r, c = build(true)
	got := reconcileWith(t, r, c, "ts4")
	if cond := readyCondition(t, got); cond.Status != metav1.ConditionFalse || cond.Reason != "ConfigWriteFailed" {
		t.Errorf("Ready = %s/%s, want False/ConfigWriteFailed", cond.Status, cond.Reason)
	}
	if l := nodeLabels(t, c, "node-1"); l["nvidia.com/device-plugin.config"] != "" {
		t.Errorf("nodes were labelled although the ConfigMap write failed: %v", l)
	}
}
