package controllers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

// WorkspaceProxy makes workspaces reachable from a browser through the api-gateway at
// <PublicURL>/ws/<namespace>/<name>/. The gateway authenticates the user and forwards to the workspace Service;
// a NetworkPolicy admits only the platform namespace's gateway and ai-operator pods.
type WorkspaceProxy struct {
	// PublicURL is the origin users open (the dashboard), e.g. https://gryvia.example.com.
	PublicURL string
	// TokenKey derives each Jupyter server's token; the gateway holds the same key and sends the token upstream,
	// so users never see it.
	TokenKey []byte
	// PlatformNamespace is where the api-gateway and the ai-operator run.
	PlatformNamespace string
}

// workspaceTokenContext separates workspace tokens from any other use of the shared key.
const workspaceTokenContext = "gryvia-workspace-token-v1"

// WorkspaceToken is the Jupyter token of one workspace. It changes when the workspace is recreated (new UID).
func WorkspaceToken(key []byte, namespace, name, uid string) string {
	m := hmac.New(sha256.New, key)
	fmt.Fprintf(m, "%s\x00%s\x00%s\x00%s", workspaceTokenContext, namespace, name, uid)
	return hex.EncodeToString(m.Sum(nil))
}

// WorkspacePathPrefix is the gateway path of a workspace, with the trailing slash.
func WorkspacePathPrefix(namespace, name string) string {
	return "/ws/" + namespace + "/" + name + "/"
}

func (r *GryviaWorkspaceReconciler) proxied() bool {
	return r.Proxy != nil && r.Proxy.PublicURL != "" && len(r.Proxy.TokenKey) > 0
}

func (r *GryviaWorkspaceReconciler) workspaceToken(ws *gryviav1.GryviaWorkspace) string {
	return WorkspaceToken(r.Proxy.TokenKey, ws.Namespace, ws.Name, string(ws.UID))
}

// internalURL is the in-cluster address of the workspace Service.
func internalURL(ws *gryviav1.GryviaWorkspace) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", workspaceChildName(ws), ws.Namespace, workspacePort(ws))
}

// browserURL is what status.url shows: the gateway path when proxied, else the in-cluster address.
func (r *GryviaWorkspaceReconciler) browserURL(ws *gryviav1.GryviaWorkspace) string {
	if !r.proxied() {
		return internalURL(ws)
	}
	return strings.TrimRight(r.Proxy.PublicURL, "/") + WorkspacePathPrefix(ws.Namespace, ws.Name)
}

// applyProxy configures the server behind the gateway: Jupyter serves under the gateway prefix with a derived
// token; the default code-server image runs without its own password (the gateway strips the prefix and
// authenticates). Custom code-server images keep their own authentication.
func (r *GryviaWorkspaceReconciler) applyProxy(ws *gryviav1.GryviaWorkspace, c *corev1.Container) {
	if !r.proxied() {
		return
	}
	prefix := WorkspacePathPrefix(ws.Namespace, ws.Name)
	if ws.Spec.Type == gryviav1.WorkspaceTypeVSCode {
		if ws.Spec.Image == "" {
			c.Args = []string{"--auth", "none"}
		}
		c.ReadinessProbe.HTTPGet.Path = "/healthz"
		return
	}
	c.Env = append(c.Env,
		corev1.EnvVar{Name: "JUPYTER_TOKEN", Value: r.workspaceToken(ws)},
		// Read by the Jupyter Docker Stacks start script; other images must set the base URL themselves.
		corev1.EnvVar{Name: "NOTEBOOK_ARGS", Value: "--ServerApp.base_url=" + prefix},
	)
	c.ReadinessProbe.HTTPGet.Path = prefix + "api"
}

// ensureNetworkPolicy admits traffic to the workspace pod only from the gateway and the ai-operator.
func (r *GryviaWorkspaceReconciler) ensureNetworkPolicy(ctx context.Context, ws *gryviav1.GryviaWorkspace, name string) error {
	if !r.proxied() || r.Proxy.PlatformNamespace == "" {
		return nil
	}
	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ws.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, np, func() error {
		np.Labels = map[string]string{"gryvia.io/workspace": ws.Name, "gryvia.io/component": "workspace-policy"}
		np.Spec = networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{
				"gryvia.io/workspace": ws.Name, "gryvia.io/component": "workspace-pod",
			}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{
					namespacePeer(r.Proxy.PlatformNamespace, map[string]string{"app": apiGatewayPodLabel}),
					namespacePeer(r.Proxy.PlatformNamespace, map[string]string{"app.kubernetes.io/component": "ai-operator"}),
				},
				Ports: tcpPort(int(workspacePort(ws))),
			}},
		}
		return controllerutil.SetControllerReference(ws, np, r.Scheme)
	})
	return err
}

// observeActivity asks the running server when it was last used: Jupyter's /api/status last_activity (needs the
// derived token, so only when proxied) or code-server's /healthz lastHeartbeat. ok is false when unknown.
func (r *GryviaWorkspaceReconciler) observeActivity(ctx context.Context, ws *gryviav1.GryviaWorkspace) (time.Time, bool) {
	if r.ActivityProbe != nil {
		return r.ActivityProbe(ctx, ws)
	}
	base := internalURL(ws)
	if r.InternalURL != nil {
		base = r.InternalURL(ws)
	}
	var url, token string
	switch ws.Spec.Type {
	case gryviav1.WorkspaceTypeVSCode:
		url = base + "/healthz"
	default:
		if !r.proxied() {
			return time.Time{}, false
		}
		url = base + WorkspacePathPrefix(ws.Namespace, ws.Name) + "api/status"
		token = r.workspaceToken(ws)
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return time.Time{}, false
	}
	if token != "" {
		req.Header.Set("Authorization", "token "+token)
	}
	hc := r.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return time.Time{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return time.Time{}, false
	}
	var body struct {
		LastActivity  string `json:"last_activity"`
		LastHeartbeat int64  `json:"lastHeartbeat"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 64<<10)).Decode(&body); err != nil {
		return time.Time{}, false
	}
	if body.LastActivity != "" {
		if t, err := time.Parse(time.RFC3339Nano, body.LastActivity); err == nil {
			return t, true
		}
	}
	if body.LastHeartbeat > 0 {
		return time.UnixMilli(body.LastHeartbeat), true
	}
	return time.Time{}, false
}
