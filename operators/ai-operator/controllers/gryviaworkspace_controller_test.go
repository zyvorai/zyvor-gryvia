package controllers

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

func newWSReconciler(c client.Client, clk *fakeClock) *GryviaWorkspaceReconciler {
	return &GryviaWorkspaceReconciler{
		Client: c, Scheme: mlScheme(), Log: ctrl.Log.WithName("test"),
		JupyterImage: "example/jupyter:test", CodeImage: "example/code:test", Clock: clk.Now,
		ActivityProbe: func(context.Context, *gryviav1.GryviaWorkspace) (time.Time, bool) { return time.Time{}, false },
	}
}

func newWS(name string, mutate ...func(*gryviav1.GryviaWorkspace)) *gryviav1.GryviaWorkspace {
	ws := &gryviav1.GryviaWorkspace{
		ObjectMeta: objMeta("ns", name),
		Spec:       gryviav1.GryviaWorkspaceSpec{Type: gryviav1.WorkspaceTypeVSCode, Storage: "1Gi"},
	}
	for _, m := range mutate {
		m(ws)
	}
	return ws
}

func getWS(t *testing.T, c client.Client, name string) *gryviav1.GryviaWorkspace {
	t.Helper()
	ws := &gryviav1.GryviaWorkspace{}
	mustGet(t, c, "ns", name, ws)
	return ws
}

func TestWorkspace_CreatesPVCPodServiceAndStatus(t *testing.T) {
	c := mlClient(newWS("dev"))
	clk := newClock()
	r := newWSReconciler(c, clk)

	reconcileOnce(t, r, "ns", "dev")

	pvc := &corev1.PersistentVolumeClaim{}
	mustGet(t, c, "ns", "dev-workspace", pvc)
	pod := &corev1.Pod{}
	mustGet(t, c, "ns", "dev-workspace", pod)
	svc := &corev1.Service{}
	mustGet(t, c, "ns", "dev-workspace", svc)
	for _, o := range []metav1.Object{pvc, pod, svc} {
		if ref := metav1.GetControllerOf(o); ref == nil || ref.Kind != "GryviaWorkspace" || ref.Name != "dev" {
			t.Errorf("%T is not controlled by the workspace: %v", o, o.GetOwnerReferences())
		}
	}
	c0 := pod.Spec.Containers[0]
	if c0.Image != "example/code:test" || c0.Ports[0].ContainerPort != 8080 {
		t.Errorf("vscode pod: image %q port %d", c0.Image, c0.Ports[0].ContainerPort)
	}
	if _, has := c0.Resources.Limits["nvidia.com/gpu"]; has {
		t.Error("a CPU workspace (gpuCount 0) must not request a GPU")
	}
	if len(pod.Spec.NodeSelector) != 0 {
		t.Errorf("unexpected node selector %v", pod.Spec.NodeSelector)
	}
	if sc := c0.SecurityContext; sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("privilege escalation must be off")
	}
	if pod.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != "dev-workspace" {
		t.Errorf("pod does not mount the workspace PVC: %v", pod.Spec.Volumes)
	}

	ws := getWS(t, c, "dev")
	if ws.Status.Phase != "Pending" || ws.Status.PodName != "dev-workspace" || ws.Status.PVCName != "dev-workspace" || ws.Status.ServiceName != "dev-workspace" {
		t.Errorf("status after create: %+v", ws.Status)
	}
	if ws.Status.URL != "http://dev-workspace.ns.svc.cluster.local:8080" {
		t.Errorf("url = %q", ws.Status.URL)
	}

	markPodReady(t, c, "ns", "dev-workspace")
	reconcileOnce(t, r, "ns", "dev")
	ws = getWS(t, c, "dev")
	// The exact fields the gateway's workspace to_ui reads.
	st := statusJSON(t, ws)
	wantKeys(t, st, "phase", "url", "startTime", "lastActivity")
	if st["phase"] != "Running" {
		t.Errorf("phase = %v", st["phase"])
	}
	if ws.Status.StartTime == nil || !ws.Status.StartTime.Time.Equal(clk.Now()) {
		t.Errorf("startTime = %v", ws.Status.StartTime)
	}
}

func TestWorkspace_JupyterDefaultsAndSpecImage(t *testing.T) {
	c := mlClient(
		newWS("j", func(w *gryviav1.GryviaWorkspace) { w.Spec.Type = gryviav1.WorkspaceTypeJupyter; w.Spec.Storage = "" }),
		newWS("custom", func(w *gryviav1.GryviaWorkspace) { w.Spec.Image = "busybox:1"; w.Spec.Storage = "" }),
	)
	r := newWSReconciler(c, newClock())
	reconcileOnce(t, r, "ns", "j")
	reconcileOnce(t, r, "ns", "custom")

	j := &corev1.Pod{}
	mustGet(t, c, "ns", "j-workspace", j)
	if j.Spec.Containers[0].Image != "example/jupyter:test" || j.Spec.Containers[0].Ports[0].ContainerPort != 8888 {
		t.Errorf("jupyter pod: %+v", j.Spec.Containers[0])
	}
	if len(j.Spec.Volumes) != 0 {
		t.Errorf("no storage requested, yet volumes = %v", j.Spec.Volumes)
	}
	if mlObjectExists(c, "ns", "j-workspace", &corev1.PersistentVolumeClaim{}) {
		t.Error("a PVC was created without spec.storage")
	}
	cu := &corev1.Pod{}
	mustGet(t, c, "ns", "custom-workspace", cu)
	if cu.Spec.Containers[0].Image != "busybox:1" {
		t.Errorf("spec.image was ignored: %q", cu.Spec.Containers[0].Image)
	}

	// Operator defaults when the flags are empty.
	r2 := &GryviaWorkspaceReconciler{}
	if got := r2.image(&gryviav1.GryviaWorkspace{Spec: gryviav1.GryviaWorkspaceSpec{Type: "jupyter"}}); got != DefaultJupyterImage {
		t.Errorf("default jupyter image = %q", got)
	}
	if got := r2.image(&gryviav1.GryviaWorkspace{Spec: gryviav1.GryviaWorkspaceSpec{Type: "vscode"}}); got != DefaultVSCodeImage {
		t.Errorf("default vscode image = %q", got)
	}
}

func TestWorkspace_GPU(t *testing.T) {
	c := mlClient(newWS("g", func(w *gryviav1.GryviaWorkspace) {
		w.Spec.GPUCount = 2
		w.Spec.GPUType = "H100"
		w.Spec.CPURequest, w.Spec.MemLimit = "2", "8Gi"
		w.Spec.Env = map[string]string{"B": "2", "A": "1"}
	}))
	r := newWSReconciler(c, newClock())
	reconcileOnce(t, r, "ns", "g")
	pod := &corev1.Pod{}
	mustGet(t, c, "ns", "g-workspace", pod)
	q := pod.Spec.Containers[0].Resources.Limits["nvidia.com/gpu"]
	if q.Value() != 2 {
		t.Errorf("gpu limit = %v", q.String())
	}
	if pod.Spec.NodeSelector["gryvia.io/gpu"] != "H100" {
		t.Errorf("node selector = %v", pod.Spec.NodeSelector)
	}
	env := pod.Spec.Containers[0].Env
	if len(env) != 2 || env[0].Name != "A" || env[1].Name != "B" {
		t.Errorf("env must be sorted for a stable pod: %v", env)
	}
	found := false
	for _, v := range pod.Spec.Volumes {
		found = found || v.Name == "shm"
	}
	if !found {
		t.Error("GPU workspaces need a shared memory volume")
	}
}

func TestWorkspace_IdempotentReconcile(t *testing.T) {
	c := mlClient(newWS("dev"))
	r := newWSReconciler(c, newClock())
	reconcileOnce(t, r, "ns", "dev")
	markPodReady(t, c, "ns", "dev-workspace")
	reconcileOnce(t, r, "ns", "dev")
	before := getWS(t, c, "dev")
	for i := 0; i < 3; i++ {
		reconcileOnce(t, r, "ns", "dev")
	}
	after := getWS(t, c, "dev")
	if before.ResourceVersion != after.ResourceVersion {
		t.Errorf("a no-op reconcile rewrote the object: %s -> %s", before.ResourceVersion, after.ResourceVersion)
	}
	pods := &corev1.PodList{}
	if err := c.List(context.Background(), pods); err != nil || len(pods.Items) != 1 {
		t.Errorf("pods = %d, err %v", len(pods.Items), err)
	}
}

func TestWorkspace_PauseAndResumeKeepsStorage(t *testing.T) {
	c := mlClient(newWS("dev"))
	clk := newClock()
	r := newWSReconciler(c, clk)
	reconcileOnce(t, r, "ns", "dev")
	markPodReady(t, c, "ns", "dev-workspace")
	reconcileOnce(t, r, "ns", "dev")
	pvcBefore := &corev1.PersistentVolumeClaim{}
	mustGet(t, c, "ns", "dev-workspace", pvcBefore)

	ws := getWS(t, c, "dev")
	ws.Spec.Paused = true
	if err := c.Update(context.Background(), ws); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "ns", "dev")
	reconcileOnce(t, r, "ns", "dev") // idempotent
	if mlObjectExists(c, "ns", "dev-workspace", &corev1.Pod{}) {
		t.Fatal("pause must delete the pod")
	}
	ws = getWS(t, c, "dev")
	if ws.Status.Phase != "Paused" || ws.Status.PodName != "" || ws.Status.StartTime != nil {
		t.Errorf("paused status: %+v", ws.Status)
	}
	if !mlObjectExists(c, "ns", "dev-workspace", &corev1.PersistentVolumeClaim{}) || !mlObjectExists(c, "ns", "dev-workspace", &corev1.Service{}) {
		t.Fatal("pause must keep the PVC and the service")
	}

	// Resume: a new pod mounts the same PVC.
	clk.Add(time.Hour)
	ws.Spec.Paused = false
	if err := c.Update(context.Background(), ws); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "ns", "dev")
	pod := &corev1.Pod{}
	mustGet(t, c, "ns", "dev-workspace", pod)
	pvcAfter := &corev1.PersistentVolumeClaim{}
	mustGet(t, c, "ns", "dev-workspace", pvcAfter)
	if pvcAfter.UID != pvcBefore.UID || pvcAfter.CreationTimestamp != pvcBefore.CreationTimestamp {
		t.Error("resume must reuse the PVC")
	}
	if pod.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != "dev-workspace" {
		t.Error("the resumed pod does not mount the PVC")
	}
	markPodReady(t, c, "ns", "dev-workspace")
	reconcileOnce(t, r, "ns", "dev")
	ws = getWS(t, c, "dev")
	if ws.Status.Phase != "Running" || !ws.Status.StartTime.Time.Equal(clk.Now()) {
		t.Errorf("resumed status: phase %s start %v", ws.Status.Phase, ws.Status.StartTime)
	}
}

func TestWorkspace_ResumeWaitsForTerminatingPod(t *testing.T) {
	ws := newWS("dev")
	now := metav1.Now()
	old := &corev1.Pod{ObjectMeta: objMeta("ns", "dev-workspace")}
	old.DeletionTimestamp = &now
	old.Finalizers = []string{"test/hold"}
	old.OwnerReferences = []metav1.OwnerReference{{APIVersion: "gryvia.io/v1", Kind: "GryviaWorkspace", Name: "dev", Controller: boolPtr(true)}}
	c := mlClient(ws, old)
	r := newWSReconciler(c, newClock())
	res := reconcileOnce(t, r, "ns", "dev")
	got := getWS(t, c, "dev")
	if got.Status.Phase != "Pending" || res.RequeueAfter == 0 || !strings.Contains(got.Status.Message, "terminate") {
		t.Errorf("phase %s, requeue %v, message %q", got.Status.Phase, res.RequeueAfter, got.Status.Message)
	}
}

func TestWorkspace_IdleTimeout(t *testing.T) {
	c := mlClient(newWS("dev", func(w *gryviav1.GryviaWorkspace) { w.Spec.IdleTimeoutMinutes = 10 }))
	clk := newClock()
	r := newWSReconciler(c, clk)
	reconcileOnce(t, r, "ns", "dev")
	markPodReady(t, c, "ns", "dev-workspace")
	reconcileOnce(t, r, "ns", "dev")
	if p := getWS(t, c, "dev").Status.Phase; p != "Running" {
		t.Fatalf("phase %s", p)
	}

	clk.Add(11 * time.Minute)
	reconcileOnce(t, r, "ns", "dev")
	ws := getWS(t, c, "dev")
	if ws.Status.Phase != "Idle" || !strings.Contains(ws.Status.Message, "No activity") {
		t.Errorf("idle: phase %s message %q", ws.Status.Phase, ws.Status.Message)
	}
	if !mlObjectExists(c, "ns", "dev-workspace", &corev1.Pod{}) {
		t.Error("Idle is informational: the pod must keep running")
	}

	// Activity reported through the annotation brings it back to Running.
	ws.Annotations = map[string]string{annotationLastActivity: clk.Now().Format(time.RFC3339)}
	if err := c.Update(context.Background(), ws); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "ns", "dev")
	ws = getWS(t, c, "dev")
	if ws.Status.Phase != "Running" || ws.Status.LastActivity == nil || !ws.Status.LastActivity.Time.Equal(clk.Now().Truncate(time.Second)) {
		t.Errorf("after activity: phase %s lastActivity %v", ws.Status.Phase, ws.Status.LastActivity)
	}
}

func TestWorkspace_IdleActionPause(t *testing.T) {
	c := mlClient(newWS("dev", func(w *gryviav1.GryviaWorkspace) {
		w.Spec.IdleTimeoutMinutes = 5
		w.Annotations = map[string]string{annotationIdleAction: "pause"}
	}))
	clk := newClock()
	r := newWSReconciler(c, clk)
	reconcileOnce(t, r, "ns", "dev")
	markPodReady(t, c, "ns", "dev-workspace")
	reconcileOnce(t, r, "ns", "dev")
	clk.Add(6 * time.Minute)
	reconcileOnce(t, r, "ns", "dev")
	if !getWS(t, c, "dev").Spec.Paused {
		t.Fatal("idle-action=pause must set spec.paused")
	}
	reconcileOnce(t, r, "ns", "dev")
	ws := getWS(t, c, "dev")
	if ws.Status.Phase != "Paused" || !strings.HasPrefix(ws.Status.Message, "Paused after being idle") {
		t.Errorf("phase %s message %q", ws.Status.Phase, ws.Status.Message)
	}
	if mlObjectExists(c, "ns", "dev-workspace", &corev1.Pod{}) {
		t.Error("the pod must be gone")
	}
}

func TestWorkspace_MaxLifetimePauses(t *testing.T) {
	c := mlClient(newWS("dev", func(w *gryviav1.GryviaWorkspace) { w.Spec.MaxLifetimeHours = 2 }))
	clk := newClock()
	r := newWSReconciler(c, clk)
	reconcileOnce(t, r, "ns", "dev")
	markPodReady(t, c, "ns", "dev-workspace")
	reconcileOnce(t, r, "ns", "dev")
	clk.Add(3 * time.Hour)
	reconcileOnce(t, r, "ns", "dev")
	reconcileOnce(t, r, "ns", "dev")
	ws := getWS(t, c, "dev")
	if !ws.Spec.Paused || ws.Status.Phase != "Paused" || !strings.Contains(ws.Status.Message, "maximum lifetime") {
		t.Errorf("paused=%v phase %s message %q", ws.Spec.Paused, ws.Status.Phase, ws.Status.Message)
	}
}

func TestWorkspace_EvictedPodIsReplaced(t *testing.T) {
	c := mlClient(newWS("dev"))
	r := newWSReconciler(c, newClock())
	reconcileOnce(t, r, "ns", "dev")
	pod := &corev1.Pod{}
	mustGet(t, c, "ns", "dev-workspace", pod)
	pod.Status.Phase = corev1.PodFailed
	pod.Status.Reason = "Evicted"
	if err := c.Status().Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "ns", "dev")
	if mlObjectExists(c, "ns", "dev-workspace", &corev1.Pod{}) {
		t.Error("the failed pod must be deleted")
	}
	if ws := getWS(t, c, "dev"); ws.Status.Phase != "Pending" || !strings.Contains(ws.Status.Message, "Evicted") {
		t.Errorf("status: %+v", ws.Status)
	}
	reconcileOnce(t, r, "ns", "dev")
	if !mlObjectExists(c, "ns", "dev-workspace", &corev1.Pod{}) {
		t.Error("the pod must be recreated")
	}
}

func TestWorkspace_Collisions(t *testing.T) {
	foreign := &corev1.Pod{ObjectMeta: objMeta("ns", "dev-workspace")}
	c := mlClient(newWS("dev"), foreign)
	r := newWSReconciler(c, newClock())
	reconcileOnce(t, r, "ns", "dev")
	ws := getWS(t, c, "dev")
	if ws.Status.Phase != "Failed" || !strings.Contains(ws.Status.Message, "not owned") {
		t.Errorf("status: %+v", ws.Status)
	}
	// The foreign pod is untouched.
	p := &corev1.Pod{}
	mustGet(t, c, "ns", "dev-workspace", p)
	if len(p.OwnerReferences) != 0 {
		t.Error("the foreign pod was modified")
	}
}

func TestWorkspace_InvalidStorageFails(t *testing.T) {
	c := mlClient(newWS("dev", func(w *gryviav1.GryviaWorkspace) { w.Spec.Storage = "lots" }))
	r := newWSReconciler(c, newClock())
	reconcileOnce(t, r, "ns", "dev")
	ws := getWS(t, c, "dev")
	if ws.Status.Phase != "Failed" || !strings.Contains(ws.Status.Message, "invalid storage size") {
		t.Errorf("status: %+v", ws.Status)
	}
}

func TestWorkspace_LongNamesStayValid(t *testing.T) {
	long := strings.Repeat("a", 60)
	c := mlClient(newWS(long))
	r := newWSReconciler(c, newClock())
	reconcileOnce(t, r, "ns", long)
	ws := getWS(t, c, long)
	if len(ws.Status.ServiceName) > 63 || ws.Status.ServiceName == "" {
		t.Errorf("service name %q (%d)", ws.Status.ServiceName, len(ws.Status.ServiceName))
	}
	if !mlObjectExists(c, "ns", ws.Status.ServiceName, &corev1.Service{}) {
		t.Error("the service is missing")
	}
}

func TestWorkspace_NotFoundAndDeleted(t *testing.T) {
	c := mlClient()
	r := newWSReconciler(c, newClock())
	reconcileOnce(t, r, "ns", "nothing")

	ws := newWS("dying")
	now := metav1.Now()
	ws.DeletionTimestamp = &now
	ws.Finalizers = []string{"test/hold"}
	c = mlClient(ws)
	r = newWSReconciler(c, newClock())
	reconcileOnce(t, r, "ns", "dying")
	if mlObjectExists(c, "ns", "dying-workspace", &corev1.Pod{}) {
		t.Error("nothing must be created for an object being deleted")
	}
}
