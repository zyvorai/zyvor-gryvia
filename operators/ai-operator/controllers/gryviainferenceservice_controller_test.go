// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

func newInferReconciler(c client.Client, clk *fakeClock) *GryviaInferenceServiceReconciler {
	return &GryviaInferenceServiceReconciler{
		Client: c, Scheme: mlScheme(), Log: ctrl.Log.WithName("test"), Clock: clk.Now,
		Images: map[gryviav1.InferenceBackend]string{gryviav1.BackendVLLM: "example/vllm:test"},
	}
}

func newInfer(name string, mutate ...func(*gryviav1.GryviaInferenceService)) *gryviav1.GryviaInferenceService {
	s := &gryviav1.GryviaInferenceService{
		ObjectMeta: objMeta("ns", name),
		Spec:       gryviav1.GryviaInferenceServiceSpec{ModelRef: "llama", Backend: gryviav1.BackendVLLM, Replicas: 2},
	}
	for _, m := range mutate {
		m(s)
	}
	return s
}

func getInfer(t *testing.T, c client.Client, name string) *gryviav1.GryviaInferenceService {
	t.Helper()
	s := &gryviav1.GryviaInferenceService{}
	mustGet(t, c, "ns", name, s)
	return s
}

func setReady(t *testing.T, c client.Client, name string, ready int32) {
	t.Helper()
	d := &appsv1.Deployment{}
	mustGet(t, c, "ns", name, d)
	d.Status.ReadyReplicas = ready
	if err := c.Status().Update(context.Background(), d); err != nil {
		t.Fatal(err)
	}
}

func TestInference_CreatesDeploymentServiceAndStatus(t *testing.T) {
	c := mlClient(newInfer("chat"))
	clk := newClock()
	r := newInferReconciler(c, clk)
	res := reconcileOnce(t, r, "ns", "chat")
	if res.RequeueAfter == 0 {
		t.Error("expected a periodic requeue")
	}

	d := &appsv1.Deployment{}
	mustGet(t, c, "ns", "chat-inference", d)
	if ref := metav1.GetControllerOf(d); ref == nil || ref.Kind != "GryviaInferenceService" {
		t.Errorf("deployment owner: %v", d.OwnerReferences)
	}
	if *d.Spec.Replicas != 2 {
		t.Errorf("replicas = %d", *d.Spec.Replicas)
	}
	c0 := d.Spec.Template.Spec.Containers[0]
	if c0.Image != "example/vllm:test" {
		t.Errorf("image %q", c0.Image)
	}
	if _, gpu := c0.Resources.Limits["nvidia.com/gpu"]; gpu {
		t.Error("gpuCount 0 must not request a GPU")
	}
	if c0.Ports[0].ContainerPort != 8000 || c0.ReadinessProbe.HTTPGet.Path != "/health" || c0.StartupProbe == nil {
		t.Errorf("vllm defaults: port %d path %s", c0.Ports[0].ContainerPort, c0.ReadinessProbe.HTTPGet.Path)
	}
	if *c0.SecurityContext.AllowPrivilegeEscalation {
		t.Error("privilege escalation must be off")
	}
	svc := &corev1.Service{}
	mustGet(t, c, "ns", "chat-inference", svc)
	if svc.Spec.Ports[0].Port != 8000 {
		t.Errorf("service port %d", svc.Spec.Ports[0].Port)
	}
	if _, hasTrack := svc.Spec.Selector[labelTrack]; hasTrack {
		t.Error("the service must select stable and canary pods alike")
	}

	setReady(t, c, "chat-inference", 2)
	reconcileOnce(t, r, "ns", "chat")
	s := getInfer(t, c, "chat")
	st := statusJSON(t, s)
	wantKeys(t, st, "phase", "readyReplicas", "endpoint")
	if st["phase"] != "Ready" || st["readyReplicas"].(float64) != 2 || st["endpoint"] != "http://chat-inference.ns.svc.cluster.local:8000" {
		t.Errorf("status = %v", st)
	}
	if s.Status.DeploymentName != "chat-inference" || s.Status.ServiceName != "chat-inference" {
		t.Errorf("names: %+v", s.Status)
	}
}

func TestInference_PhaseFollowsReadiness(t *testing.T) {
	c := mlClient(newInfer("chat"))
	r := newInferReconciler(c, newClock())
	reconcileOnce(t, r, "ns", "chat")
	if p := getInfer(t, c, "chat").Status.Phase; p != "Deploying" {
		t.Errorf("no replica ready: phase %s", p)
	}
	setReady(t, c, "chat-inference", 1)
	reconcileOnce(t, r, "ns", "chat")
	s := getInfer(t, c, "chat")
	if s.Status.Phase != "Ready" || s.Status.ReadyReplicas != 1 {
		t.Errorf("partially ready: %+v", s.Status)
	}
	if cond := findCond(s.Status.Conditions, ConditionInferenceReady); cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "PartiallyReady" {
		t.Errorf("condition: %+v", cond)
	}
}

func findCond(conds []metav1.Condition, t string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == t {
			return &conds[i]
		}
	}
	return nil
}

func TestInference_GPUAndImageAndPathOverrides(t *testing.T) {
	c := mlClient(
		newInfer("gpu", func(s *gryviav1.GryviaInferenceService) {
			s.Spec.GPUCount = 4
			s.Spec.GPUType = "A100"
			s.Spec.Backend = gryviav1.BackendTriton
		}),
		newInfer("own", func(s *gryviav1.GryviaInferenceService) {
			s.Spec.Image = "nginx:1"
			s.Spec.ServicePort = 9000
			s.Spec.HealthCheck = &gryviav1.HealthCheckConfig{Path: "/ready"}
		}),
	)
	r := newInferReconciler(c, newClock())
	r.HealthPath = "/"
	reconcileOnce(t, r, "ns", "gpu")
	reconcileOnce(t, r, "ns", "own")

	g := &appsv1.Deployment{}
	mustGet(t, c, "ns", "gpu-inference", g)
	c0 := g.Spec.Template.Spec.Containers[0]
	q := c0.Resources.Limits["nvidia.com/gpu"]
	if q.Value() != 4 || g.Spec.Template.Spec.NodeSelector["gryvia.io/gpu"] != "A100" {
		t.Errorf("gpu: %v %v", q.String(), g.Spec.Template.Spec.NodeSelector)
	}
	if c0.Image != DefaultInferenceImages[gryviav1.BackendTriton] {
		t.Errorf("triton default image = %q", c0.Image)
	}
	if c0.ReadinessProbe.HTTPGet.Path != "/" {
		t.Errorf("--inference-health-path override = %q", c0.ReadinessProbe.HTTPGet.Path)
	}
	o := &appsv1.Deployment{}
	mustGet(t, c, "ns", "own-inference", o)
	c1 := o.Spec.Template.Spec.Containers[0]
	if c1.Image != "nginx:1" || c1.Ports[0].ContainerPort != 9000 || c1.ReadinessProbe.HTTPGet.Path != "/ready" {
		t.Errorf("spec overrides: %+v", c1)
	}
}

func TestInference_UnknownBackendWithoutImageFails(t *testing.T) {
	c := mlClient(newInfer("bad", func(s *gryviav1.GryviaInferenceService) { s.Spec.Backend = "mystery" }))
	r := newInferReconciler(c, newClock())
	reconcileOnce(t, r, "ns", "bad")
	s := getInfer(t, c, "bad")
	if s.Status.Phase != "Failed" || !strings.Contains(s.Status.Message, "mystery") {
		t.Errorf("status: %+v", s.Status)
	}
	if mlObjectExists(c, "ns", "bad-inference", &appsv1.Deployment{}) {
		t.Error("no deployment for an invalid service")
	}
}

func TestInference_IdempotentAndDriftCorrected(t *testing.T) {
	c := mlClient(newInfer("chat"))
	r := newInferReconciler(c, newClock())
	reconcileOnce(t, r, "ns", "chat")
	reconcileOnce(t, r, "ns", "chat")
	before := getInfer(t, c, "chat")
	d1 := &appsv1.Deployment{}
	mustGet(t, c, "ns", "chat-inference", d1)
	reconcileOnce(t, r, "ns", "chat")
	after := getInfer(t, c, "chat")
	d2 := &appsv1.Deployment{}
	mustGet(t, c, "ns", "chat-inference", d2)
	if before.ResourceVersion != after.ResourceVersion || d1.ResourceVersion != d2.ResourceVersion {
		t.Error("a no-op reconcile changed objects")
	}

	// Image and replica changes reach the Deployment.
	s := getInfer(t, c, "chat")
	s.Spec.Image = "example/new:2"
	s.Spec.Replicas = 3
	if err := c.Update(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "ns", "chat")
	d := &appsv1.Deployment{}
	mustGet(t, c, "ns", "chat-inference", d)
	if d.Spec.Template.Spec.Containers[0].Image != "example/new:2" || *d.Spec.Replicas != 3 {
		t.Errorf("drift not corrected: %s x%d", d.Spec.Template.Spec.Containers[0].Image, *d.Spec.Replicas)
	}
}

func TestInference_HPA(t *testing.T) {
	c := mlClient(newInfer("chat", func(s *gryviav1.GryviaInferenceService) {
		s.Spec.Autoscaling = &gryviav1.AutoscalingConfig{Enabled: true, MinReplicas: 2, MaxReplicas: 6, TargetGPUUtilization: 70}
	}))
	r := newInferReconciler(c, newClock())
	reconcileOnce(t, r, "ns", "chat")
	hpa := &autoscalingv2.HorizontalPodAutoscaler{}
	mustGet(t, c, "ns", "chat-inference-hpa", hpa)
	if *hpa.Spec.MinReplicas != 2 || hpa.Spec.MaxReplicas != 6 || hpa.Spec.ScaleTargetRef.Name != "chat-inference" {
		t.Errorf("hpa spec: %+v", hpa.Spec)
	}
	if metav1.GetControllerOf(hpa) == nil {
		t.Error("the HPA must be owned")
	}
	s := getInfer(t, c, "chat")
	cond := findCond(s.Status.Conditions, ConditionAutoscalingValid)
	if cond == nil || cond.Status != metav1.ConditionTrue || !strings.Contains(cond.Message, "custom GPU/RPS metrics") {
		t.Errorf("autoscaling condition: %+v", cond)
	}

	// The HPA owns the replica count: the reconciler must not fight it.
	d := &appsv1.Deployment{}
	mustGet(t, c, "ns", "chat-inference", d)
	scaled := int32(5)
	d.Spec.Replicas = &scaled
	if err := c.Update(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "ns", "chat")
	mustGet(t, c, "ns", "chat-inference", d)
	if *d.Spec.Replicas != 5 {
		t.Errorf("replicas reset to %d while an HPA manages them", *d.Spec.Replicas)
	}

	// Range change reaches the HPA; disabling removes it.
	s.Spec.Autoscaling.MaxReplicas = 9
	if err := c.Update(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "ns", "chat")
	mustGet(t, c, "ns", "chat-inference-hpa", hpa)
	if hpa.Spec.MaxReplicas != 9 {
		t.Errorf("hpa max = %d", hpa.Spec.MaxReplicas)
	}
	s = getInfer(t, c, "chat")
	s.Spec.Autoscaling.Enabled = false
	if err := c.Update(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "ns", "chat")
	if mlObjectExists(c, "ns", "chat-inference-hpa", &autoscalingv2.HorizontalPodAutoscaler{}) {
		t.Error("the HPA must be removed when autoscaling is disabled")
	}
}

func TestInference_HPAInvalidRange(t *testing.T) {
	c := mlClient(newInfer("chat", func(s *gryviav1.GryviaInferenceService) {
		s.Spec.Autoscaling = &gryviav1.AutoscalingConfig{Enabled: true, MinReplicas: 5, MaxReplicas: 2}
	}))
	r := newInferReconciler(c, newClock())
	reconcileOnce(t, r, "ns", "chat")
	if mlObjectExists(c, "ns", "chat-inference-hpa", &autoscalingv2.HorizontalPodAutoscaler{}) {
		t.Error("an invalid range must not create an HPA")
	}
	s := getInfer(t, c, "chat")
	cond := findCond(s.Status.Conditions, ConditionAutoscalingValid)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "InvalidRange" {
		t.Errorf("condition: %+v", cond)
	}
	if s.Status.Phase == "Failed" {
		t.Error("a bad HPA range must not take the service down")
	}
	if !mlObjectExists(c, "ns", "chat-inference", &appsv1.Deployment{}) {
		t.Error("the deployment must still exist")
	}
}

func TestCanaryReplicas(t *testing.T) {
	for _, tc := range []struct{ stable, weight, want int32 }{
		{2, 50, 2}, {4, 20, 1}, {10, 10, 2}, {1, 10, 1}, {3, 100, 3}, {5, 0, 1}, {9, 90, 81},
	} {
		if got := canaryReplicas(tc.stable, tc.weight); got != tc.want {
			t.Errorf("canaryReplicas(%d, %d) = %d, want %d", tc.stable, tc.weight, got, tc.want)
		}
	}
}

func canarySvc(mutate ...func(*gryviav1.GryviaInferenceService)) *gryviav1.GryviaInferenceService {
	return newInfer("chat", append([]func(*gryviav1.GryviaInferenceService){func(s *gryviav1.GryviaInferenceService) {
		s.Spec.Canary = &gryviav1.CanaryConfig{Enabled: true, Weight: 50, ModelVersion: "v2"}
	}}, mutate...)...)
}

func TestInference_CanaryCreatedAndRemoved(t *testing.T) {
	c := mlClient(canarySvc())
	r := newInferReconciler(c, newClock())
	reconcileOnce(t, r, "ns", "chat")

	cd := &appsv1.Deployment{}
	mustGet(t, c, "ns", "chat-canary", cd)
	if *cd.Spec.Replicas != 2 || cd.Spec.Template.Labels[labelTrack] != trackCanary {
		t.Errorf("canary: replicas %d labels %v", *cd.Spec.Replicas, cd.Spec.Template.Labels)
	}
	env := map[string]string{}
	for _, e := range cd.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if env["CANARY_MODEL"] != "v2" || env["MODEL_VERSION"] != "v2" {
		t.Errorf("canary env: %v", env)
	}
	primary := &appsv1.Deployment{}
	mustGet(t, c, "ns", "chat-inference", primary)
	if primary.Spec.Selector.MatchLabels[labelTrack] != trackStable {
		t.Error("stable and canary deployments must have disjoint selectors")
	}
	s := getInfer(t, c, "chat")
	cs := s.Status.CanaryStatus
	if cs == nil || !cs.Active || cs.Weight != 50 || cs.DeploymentName != "chat-canary" || cs.StartedAt == nil || cs.Health != canaryHealthUnhealthy && cs.Health != canaryHealthPending {
		t.Errorf("canary status: %+v", cs)
	}

	// Disabling removes it.
	s.Spec.Canary.Enabled = false
	if err := c.Update(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "ns", "chat")
	if mlObjectExists(c, "ns", "chat-canary", &appsv1.Deployment{}) {
		t.Error("the canary deployment must be removed")
	}
	if getInfer(t, c, "chat").Status.CanaryStatus != nil {
		t.Error("canaryStatus must be cleared")
	}
}

func TestInference_CanaryAutoPromote(t *testing.T) {
	c := mlClient(canarySvc(func(s *gryviav1.GryviaInferenceService) {
		s.Spec.Canary.AutoPromote = true
		s.Spec.Canary.PromoteAfterSeconds = 60
	}))
	clk := newClock()
	r := newInferReconciler(c, clk)
	reconcileOnce(t, r, "ns", "chat")
	setReady(t, c, "chat-canary", 1)

	clk.Add(30 * time.Second)
	reconcileOnce(t, r, "ns", "chat")
	if !mlObjectExists(c, "ns", "chat-canary", &appsv1.Deployment{}) {
		t.Fatal("promoted too early")
	}
	if h := getInfer(t, c, "chat").Status.CanaryStatus.Health; h != canaryHealthHealthy {
		t.Errorf("canary health %s", h)
	}

	clk.Add(40 * time.Second)
	reconcileOnce(t, r, "ns", "chat")
	if mlObjectExists(c, "ns", "chat-canary", &appsv1.Deployment{}) {
		t.Fatal("the canary deployment must be removed on promotion")
	}
	primary := &appsv1.Deployment{}
	mustGet(t, c, "ns", "chat-inference", primary)
	if primary.Annotations[annotationPromoted] != "v2" {
		t.Errorf("promotion not recorded: %v", primary.Annotations)
	}
	s := getInfer(t, c, "chat")
	if s.Status.CanaryStatus == nil || s.Status.CanaryStatus.Active || s.Status.CanaryStatus.Health != canaryHealthPromoted ||
		!strings.Contains(s.Status.Message, "promoted") {
		t.Errorf("status after promotion: %+v / %q", s.Status.CanaryStatus, s.Status.Message)
	}

	// The next pass rolls the promoted version out to the stable pods and does not start another canary.
	reconcileOnce(t, r, "ns", "chat")
	reconcileOnce(t, r, "ns", "chat")
	mustGet(t, c, "ns", "chat-inference", primary)
	found := false
	for _, e := range primary.Spec.Template.Spec.Containers[0].Env {
		found = found || (e.Name == "MODEL_VERSION" && e.Value == "v2")
	}
	if !found {
		t.Errorf("stable pods do not serve the promoted version: %v", primary.Spec.Template.Spec.Containers[0].Env)
	}
	if mlObjectExists(c, "ns", "chat-canary", &appsv1.Deployment{}) {
		t.Error("a promoted canary must not be recreated")
	}
}

func TestInference_CanaryAutoRollback(t *testing.T) {
	c := mlClient(canarySvc(func(s *gryviav1.GryviaInferenceService) {
		s.Spec.HealthCheck = &gryviav1.HealthCheckConfig{AutoRollback: true, FailureThreshold: 2, IntervalSeconds: 30}
	}))
	clk := newClock()
	r := newInferReconciler(c, clk)
	r.CanaryStartupGrace = time.Minute
	reconcileOnce(t, r, "ns", "chat")

	// Inside the startup grace nothing counts.
	clk.Add(30 * time.Second)
	reconcileOnce(t, r, "ns", "chat")
	if s := getInfer(t, c, "chat"); s.Status.ConsecutiveFailures != 0 || s.Status.CanaryStatus.Health != canaryHealthPending {
		t.Fatalf("grace: %+v %+v", s.Status.ConsecutiveFailures, s.Status.CanaryStatus)
	}

	// First failed check, then several reconciles inside the interval do not add failures.
	clk.Add(40 * time.Second)
	reconcileOnce(t, r, "ns", "chat")
	reconcileOnce(t, r, "ns", "chat")
	reconcileOnce(t, r, "ns", "chat")
	if s := getInfer(t, c, "chat"); s.Status.ConsecutiveFailures != 1 || s.Status.CanaryStatus.Health != canaryHealthUnhealthy {
		t.Fatalf("after first failure: failures %d %+v", s.Status.ConsecutiveFailures, s.Status.CanaryStatus)
	}
	if !mlObjectExists(c, "ns", "chat-canary", &appsv1.Deployment{}) {
		t.Fatal("rolled back too early")
	}

	// Second failed check reaches the threshold.
	clk.Add(31 * time.Second)
	reconcileOnce(t, r, "ns", "chat")
	if mlObjectExists(c, "ns", "chat-canary", &appsv1.Deployment{}) {
		t.Fatal("the canary must be removed by the rollback")
	}
	primary := &appsv1.Deployment{}
	mustGet(t, c, "ns", "chat-inference", primary)
	if primary.Annotations[annotationRolledBack] != "v2" || primary.Annotations[annotationPromoted] != "" {
		t.Errorf("annotations: %v", primary.Annotations)
	}
	s := getInfer(t, c, "chat")
	if s.Status.CanaryStatus == nil || s.Status.CanaryStatus.Health != canaryHealthRolledBack || !strings.Contains(s.Status.Message, "rolled back") {
		t.Errorf("status: %+v / %q", s.Status.CanaryStatus, s.Status.Message)
	}
	if cond := findCond(s.Status.Conditions, ConditionHealthy); cond == nil || cond.Status != metav1.ConditionFalse {
		t.Errorf("healthy condition: %+v", cond)
	}

	// Stays rolled back until the user picks another version.
	reconcileOnce(t, r, "ns", "chat")
	if mlObjectExists(c, "ns", "chat-canary", &appsv1.Deployment{}) {
		t.Fatal("a rolled-back canary must not be recreated")
	}
	s = getInfer(t, c, "chat")
	s.Spec.Canary.ModelVersion = "v3"
	if err := c.Update(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "ns", "chat")
	cd := &appsv1.Deployment{}
	mustGet(t, c, "ns", "chat-canary", cd)
	if cd.Annotations[annotationModelVersion] != "v3" {
		t.Errorf("new canary version: %v", cd.Annotations)
	}
	if s := getInfer(t, c, "chat"); s.Status.ConsecutiveFailures != 0 || !s.Status.CanaryStatus.Active {
		t.Errorf("a new canary starts fresh: %+v", s.Status)
	}
}

func TestInference_NoRollbackWithoutAutoRollback(t *testing.T) {
	c := mlClient(canarySvc())
	clk := newClock()
	r := newInferReconciler(c, clk)
	reconcileOnce(t, r, "ns", "chat")
	for i := 0; i < 5; i++ {
		clk.Add(time.Minute)
		reconcileOnce(t, r, "ns", "chat")
	}
	if !mlObjectExists(c, "ns", "chat-canary", &appsv1.Deployment{}) {
		t.Error("without autoRollback the canary stays")
	}
}

func TestInference_ModelArtifactsReachThePods(t *testing.T) {
	model := &gryviav1.GryviaModelRegistry{
		ObjectMeta: objMeta("ns", "llama"),
		Spec: gryviav1.GryviaModelRegistrySpec{ModelName: "llama", Version: "1", Artifacts: gryviav1.ModelArtifacts{
			S3Path: "s3://bucket/llama", PVCName: "models", SubPath: "llama/1", Format: "safetensors",
		}},
	}
	c := mlClient(newInfer("chat"), model)
	r := newInferReconciler(c, newClock())
	reconcileOnce(t, r, "ns", "chat")
	d := &appsv1.Deployment{}
	mustGet(t, c, "ns", "chat-inference", d)
	c0 := d.Spec.Template.Spec.Containers[0]
	env := map[string]string{}
	for _, e := range c0.Env {
		env[e.Name] = e.Value
	}
	if env["MODEL_S3_PATH"] != "s3://bucket/llama" || env["MODEL_FORMAT"] != "safetensors" || env["MODEL_PATH"] != "/models" {
		t.Errorf("env: %v", env)
	}
	if len(c0.VolumeMounts) != 1 || c0.VolumeMounts[0].SubPath != "llama/1" || !c0.VolumeMounts[0].ReadOnly {
		t.Errorf("mounts: %v", c0.VolumeMounts)
	}
}

func TestInference_Collision(t *testing.T) {
	foreign := &appsv1.Deployment{ObjectMeta: objMeta("ns", "chat-inference")}
	c := mlClient(newInfer("chat"), foreign)
	r := newInferReconciler(c, newClock())
	reconcileOnce(t, r, "ns", "chat")
	s := getInfer(t, c, "chat")
	if s.Status.Phase != "Failed" || !strings.Contains(s.Status.Message, "not owned") {
		t.Errorf("status: %+v", s.Status)
	}
}

func TestInference_MissingModelRefFails(t *testing.T) {
	c := mlClient(newInfer("chat", func(s *gryviav1.GryviaInferenceService) { s.Spec.ModelRef = "" }))
	r := newInferReconciler(c, newClock())
	reconcileOnce(t, r, "ns", "chat")
	if s := getInfer(t, c, "chat"); s.Status.Phase != "Failed" {
		t.Errorf("phase %s", s.Status.Phase)
	}
}

func TestInference_NotFoundAndDeleted(t *testing.T) {
	c := mlClient()
	r := newInferReconciler(c, newClock())
	reconcileOnce(t, r, "ns", "nothing")
}
