// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"encoding/pem"
	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	configapi "k8s.io/client-go/tools/clientcmd/api"
	"net/http"
	"net/http/httptest"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
)

func TestFederationVerifiedTLSAndPluginRejection(t *testing.T) {
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" || r.Header.Get("Authorization") != "Bearer token" {
			http.Error(w, "denied", 403)
			return
		}
		w.WriteHeader(200)
	}))
	defer api.Close()
	cfg := configapi.Config{Clusters: map[string]*configapi.Cluster{"remote": {Server: api.URL, CertificateAuthorityData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: api.Certificate().Raw})}}, AuthInfos: map[string]*configapi.AuthInfo{"auth": {Token: "token"}}, Contexts: map[string]*configapi.Context{"remote": {Cluster: "remote", AuthInfo: "auth"}}, CurrentContext: "remote"}
	raw, err := clientcmd.Write(cfg)
	if err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "remote", Namespace: "control"}, Data: map[string][]byte{"kubeconfig": raw}}
	c := fake.NewClientBuilder().WithScheme(mlScheme()).WithObjects(secret).Build()
	r := &GryviaFederationReconciler{Client: c, CredentialsNamespace: "control", AllowedServers: []string{api.URL}}
	cluster := gryviav1.FederationCluster{APIServer: api.URL, Credentials: &gryviav1.FederationCredentials{SecretRef: "remote"}}
	if err := r.probeCluster(context.Background(), cluster); err != nil {
		t.Fatal(err)
	}
	r.AllowedServers = nil
	if err := r.probeCluster(context.Background(), cluster); err == nil {
		t.Fatal("unapproved API server was contacted")
	}
	r.AllowedServers = []string{api.URL}
	cfg.AuthInfos["auth"].Exec = &configapi.ExecConfig{Command: "sh", Args: []string{"-c", "exit 0"}}
	secret.Data["kubeconfig"], _ = clientcmd.Write(cfg)
	if err := c.Update(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	if err := r.probeCluster(context.Background(), cluster); err == nil {
		t.Fatal("kubeconfig exec accepted")
	}
}
