package controllers

import (
	"context"
	"fmt"
	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"net/http"
	"strings"
	"time"
)

// restConfigFor validates the member's administrator-allowed server and inline kubeconfig and returns its REST
// config; the probe and the failover fence both connect only through it.
func (r *GryviaFederationReconciler) restConfigFor(ctx context.Context, c gryviav1.FederationCluster) (*rest.Config, error) {
	allowed := false
	for _, server := range r.AllowedServers {
		if strings.TrimRight(c.APIServer, "/") == strings.TrimRight(server, "/") {
			allowed = true
		}
	}
	if !allowed || !strings.HasPrefix(c.APIServer, "https://") {
		return nil, fmt.Errorf("API server is not administrator-allowed HTTPS")
	}
	if c.Credentials == nil || c.Credentials.SecretRef == "" {
		return nil, fmt.Errorf("kubeconfig secret is required")
	}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: r.CredentialsNamespace, Name: c.Credentials.SecretRef}, secret); err != nil {
		return nil, err
	}
	raw := secret.Data["kubeconfig"]
	if len(raw) == 0 || len(raw) > 1048576 {
		return nil, fmt.Errorf("invalid kubeconfig size")
	}
	parsed, err := clientcmd.Load(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid kubeconfig")
	}
	for _, auth := range parsed.AuthInfos {
		if auth.Exec != nil || auth.AuthProvider != nil || auth.ClientKey != "" || auth.ClientCertificate != "" || auth.TokenFile != "" {
			return nil, fmt.Errorf("kubeconfig must use inline credentials; exec, plugins and file references are rejected")
		}
	}
	for _, cluster := range parsed.Clusters {
		if cluster.CertificateAuthority != "" || cluster.InsecureSkipTLSVerify || cluster.ProxyURL != "" {
			return nil, fmt.Errorf("kubeconfig must use inline verified CA and no proxy")
		}
	}
	cfg, err := clientcmd.RESTConfigFromKubeConfig(raw)
	if err != nil {
		return nil, err
	}
	if strings.TrimRight(cfg.Host, "/") != strings.TrimRight(c.APIServer, "/") {
		return nil, fmt.Errorf("kubeconfig server does not match allowed server")
	}
	return cfg, nil
}

func (r *GryviaFederationReconciler) probeCluster(ctx context.Context, c gryviav1.FederationCluster) error {
	cfg, err := r.restConfigFor(ctx, c)
	if err != nil {
		return err
	}
	cfg.Timeout = 3 * time.Second
	transport, err := rest.TransportFor(cfg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(cfg.Host, "/")+"/readyz", nil)
	if err != nil {
		return err
	}
	hc := &http.Client{Transport: transport, Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("remote readiness probe failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("remote readiness HTTP %d", resp.StatusCode)
	}
	return nil
}
