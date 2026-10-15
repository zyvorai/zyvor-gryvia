package controllers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

var testProxyKey = []byte("0123456789abcdef0123456789abcdef")

func proxied(r *GryviaWorkspaceReconciler) *GryviaWorkspaceReconciler {
	r.Proxy = &WorkspaceProxy{PublicURL: "https://gryvia.example.com/", TokenKey: testProxyKey, PlatformNamespace: "gryvia-system"}
	return r
}

func TestWorkspaceToken_StableAndScoped(t *testing.T) {
	a := WorkspaceToken(testProxyKey, "ns", "dev", "uid-1")
	// The api-gateway derives the same value (services/api-gateway/tests/test_workspace_proxy.py).
	if a != "a5398f1ed0ccc9b98d605eb8c65143e92dd2923c9093192bb0909108b6d68e3c" {
		t.Fatalf("token %q differs from the gateway's derivation", a)
	}
	for _, other := range []string{
		WorkspaceToken(testProxyKey, "ns", "dev", "uid-2"),
		WorkspaceToken(testProxyKey, "ns2", "dev", "uid-1"),
		WorkspaceToken(testProxyKey, "ns", "dev2", "uid-1"),
		WorkspaceToken([]byte("another-key-another-key-another-k"), "ns", "dev", "uid-1"),
	} {
		if other == a {
			t.Error("token must differ per workspace, UID and key")
		}
	}
}

func TestWorkspace_ProxiedJupyter(t *testing.T) {
	c := mlClient(newWS("nb", func(w *gryviav1.GryviaWorkspace) { w.Spec.Type = gryviav1.WorkspaceTypeJupyter }))
	r := proxied(newWSReconciler(c, newClock()))
	reconcileOnce(t, r, "ns", "nb")

	ws := getWS(t, c, "nb")
	if ws.Status.URL != "https://gryvia.example.com/ws/ns/nb/" {
		t.Errorf("url = %q", ws.Status.URL)
	}
	pod := &corev1.Pod{}
	mustGet(t, c, "ns", "nb-workspace", pod)
	c0 := pod.Spec.Containers[0]
	env := containerEnv(c0)
	if env["JUPYTER_TOKEN"].Value != WorkspaceToken(testProxyKey, "ns", "nb", string(ws.UID)) {
		t.Errorf("JUPYTER_TOKEN = %q", env["JUPYTER_TOKEN"].Value)
	}
	if env["NOTEBOOK_ARGS"].Value != "--ServerApp.base_url=/ws/ns/nb/" {
		t.Errorf("NOTEBOOK_ARGS = %q", env["NOTEBOOK_ARGS"].Value)
	}
	if c0.ReadinessProbe.HTTPGet.Path != "/ws/ns/nb/api" {
		t.Errorf("probe path %q", c0.ReadinessProbe.HTTPGet.Path)
	}

	np := &networkingv1.NetworkPolicy{}
	mustGet(t, c, "ns", "nb-workspace", np)
	if np.Spec.PodSelector.MatchLabels["gryvia.io/workspace"] != "nb" || len(np.Spec.Ingress) != 1 {
		t.Fatalf("policy %+v", np.Spec)
	}
	from := np.Spec.Ingress[0].From
	if len(from) != 2 || from[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "gryvia-system" ||
		from[0].PodSelector.MatchLabels["app"] != apiGatewayPodLabel {
		t.Errorf("ingress from %+v", from)
	}
	if np.Spec.Ingress[0].Ports[0].Port.IntValue() != 8888 {
		t.Errorf("port %v", np.Spec.Ingress[0].Ports[0].Port)
	}
}

func TestWorkspace_ProxiedVSCode(t *testing.T) {
	c := mlClient(
		newWS("code"),
		newWS("custom", func(w *gryviav1.GryviaWorkspace) { w.Spec.Image = "my/code:1" }),
	)
	r := proxied(newWSReconciler(c, newClock()))
	reconcileOnce(t, r, "ns", "code")
	reconcileOnce(t, r, "ns", "custom")

	pod := &corev1.Pod{}
	mustGet(t, c, "ns", "code-workspace", pod)
	c0 := pod.Spec.Containers[0]
	if len(c0.Args) != 2 || c0.Args[0] != "--auth" || c0.Args[1] != "none" {
		t.Errorf("default code-server args %v", c0.Args)
	}
	if c0.ReadinessProbe.HTTPGet.Path != "/healthz" {
		t.Errorf("probe %q", c0.ReadinessProbe.HTTPGet.Path)
	}
	if _, ok := containerEnv(c0)["JUPYTER_TOKEN"]; ok {
		t.Error("code-server gets no Jupyter token")
	}
	cu := &corev1.Pod{}
	mustGet(t, c, "ns", "custom-workspace", cu)
	if len(cu.Spec.Containers[0].Args) != 0 {
		t.Errorf("a custom image keeps its own authentication: args %v", cu.Spec.Containers[0].Args)
	}
}

func TestWorkspace_NotProxiedKeepsInClusterURL(t *testing.T) {
	c := mlClient(newWS("dev", func(w *gryviav1.GryviaWorkspace) { w.Spec.Type = gryviav1.WorkspaceTypeJupyter }))
	r := newWSReconciler(c, newClock())
	r.Proxy = &WorkspaceProxy{PublicURL: "https://x"} // no key: not proxied
	reconcileOnce(t, r, "ns", "dev")
	if u := getWS(t, c, "dev").Status.URL; u != "http://dev-workspace.ns.svc.cluster.local:8888" {
		t.Errorf("url %q", u)
	}
	if mlObjectExists(c, "ns", "dev-workspace", &networkingv1.NetworkPolicy{}) {
		t.Error("no NetworkPolicy without the proxy")
	}
}

func TestWorkspace_ServerActivityKeepsItRunning(t *testing.T) {
	clk := newClock()
	var heartbeat time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/healthz" {
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write([]byte(`{"status":"alive","lastHeartbeat":` + strconv.FormatInt(heartbeat.UnixMilli(), 10) + `}`))
	}))
	defer srv.Close()

	c := mlClient(newWS("dev", func(w *gryviav1.GryviaWorkspace) { w.Spec.IdleTimeoutMinutes = 10 }))
	r := newWSReconciler(c, clk)
	r.ActivityProbe = nil
	r.InternalURL = func(*gryviav1.GryviaWorkspace) string { return srv.URL }
	heartbeat = clk.Now()
	reconcileOnce(t, r, "ns", "dev")
	markPodReady(t, c, "ns", "dev-workspace")
	reconcileOnce(t, r, "ns", "dev")

	clk.Add(11 * time.Minute)
	heartbeat = clk.Now().Add(-2 * time.Minute)
	reconcileOnce(t, r, "ns", "dev")
	ws := getWS(t, c, "dev")
	if ws.Status.Phase != "Running" {
		t.Fatalf("a recent heartbeat must keep it Running: %s %q", ws.Status.Phase, ws.Status.Message)
	}
	if !ws.Status.LastActivity.Time.Equal(heartbeat.Truncate(time.Second)) {
		t.Errorf("lastActivity %v, want %v", ws.Status.LastActivity, heartbeat)
	}

	clk.Add(15 * time.Minute)
	reconcileOnce(t, r, "ns", "dev")
	if p := getWS(t, c, "dev").Status.Phase; p != "Idle" {
		t.Errorf("an old heartbeat must make it Idle, got %s", p)
	}
}

func TestWorkspace_JupyterActivityUsesToken(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotAuth, gotPath = req.Header.Get("Authorization"), req.URL.Path
		_, _ = w.Write([]byte(`{"last_activity":"2026-01-01T11:58:30.123456Z","connections":1}`))
	}))
	defer srv.Close()

	ws := newWS("nb", func(w *gryviav1.GryviaWorkspace) { w.Spec.Type = gryviav1.WorkspaceTypeJupyter })
	r := proxied(newWSReconciler(mlClient(), newClock()))
	r.ActivityProbe = nil
	r.InternalURL = func(*gryviav1.GryviaWorkspace) string { return srv.URL }
	at, ok := r.observeActivity(context.Background(), ws)
	if !ok || !at.Equal(time.Date(2026, 1, 1, 11, 58, 30, 123456000, time.UTC)) {
		t.Fatalf("activity %v %v", at, ok)
	}
	if gotPath != "/ws/ns/nb/api/status" || gotAuth != "token "+WorkspaceToken(testProxyKey, "ns", "nb", "") {
		t.Errorf("path %q auth %q", gotPath, gotAuth)
	}

	// Without the proxy the Jupyter token is unknown: no poll.
	r.Proxy = nil
	if _, ok := r.observeActivity(context.Background(), ws); ok {
		t.Error("unproxied Jupyter must not report activity")
	}
}
