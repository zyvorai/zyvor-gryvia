package controllers

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

// Shared fixtures of the ML controller tests (Workspace, InferenceService, ModelRegistry, Workflow, AutoTuner).

func mlScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = gryviav1.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	_ = appsv1.AddToScheme(s)
	_ = autoscalingv2.AddToScheme(s)
	_ = networkingv1.AddToScheme(s)
	return s
}

func mlClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().
		WithScheme(mlScheme()).
		WithObjects(objs...).
		WithStatusSubresource(
			&gryviav1.GryviaWorkspace{}, &gryviav1.GryviaInferenceService{}, &gryviav1.GryviaModelRegistry{},
			&gryviav1.GryviaWorkflow{}, &gryviav1.GryviaAutoTuner{}, &gryviav1.GryviaAIJob{},
			&gryviav1.GryviaModelWatch{}, &autoscalingv2.HorizontalPodAutoscaler{},
		).
		Build()
}

// fakeClock is a movable clock.
type fakeClock struct{ t time.Time }

func newClock() *fakeClock          { return &fakeClock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)} }
func (c *fakeClock) Now() time.Time { return c.t }
func (c *fakeClock) Add(d time.Duration) {
	c.t = c.t.Add(d)
}

type reconciler interface {
	Reconcile(context.Context, ctrl.Request) (ctrl.Result, error)
}

func key(ns, name string) types.NamespacedName {
	return types.NamespacedName{Namespace: ns, Name: name}
}

func reconcileOnce(t *testing.T, r reconciler, ns, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key(ns, name)})
	if err != nil {
		t.Fatalf("reconcile %s/%s: %v", ns, name, err)
	}
	return res
}

func mustGet(t *testing.T, c client.Client, ns, name string, obj client.Object) {
	t.Helper()
	if err := c.Get(context.Background(), key(ns, name), obj); err != nil {
		t.Fatalf("get %T %s/%s: %v", obj, ns, name, err)
	}
}

func mlObjectExists(c client.Client, ns, name string, obj client.Object) bool {
	return c.Get(context.Background(), key(ns, name), obj) == nil
}

// statusJSON returns the status of obj as the generic map the gateway reads from the API server.
func statusJSON(t *testing.T, obj interface{}) map[string]interface{} {
	t.Helper()
	b, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	st, _ := m["status"].(map[string]interface{})
	if st == nil {
		t.Fatalf("object has no status: %s", b)
	}
	return st
}

func wantKeys(t *testing.T, st map[string]interface{}, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if v, ok := st[k]; !ok || v == nil || v == "" {
			t.Errorf("status.%s is not set: %v", k, st)
		}
	}
}

// markPodReady makes a Pod Running and Ready.
func markPodReady(t *testing.T, c client.Client, ns, name string) {
	t.Helper()
	pod := &corev1.Pod{}
	mustGet(t, c, ns, name, pod)
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if err := c.Status().Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
}

func objMeta(ns, name string) metav1.ObjectMeta { return metav1.ObjectMeta{Namespace: ns, Name: name} }
