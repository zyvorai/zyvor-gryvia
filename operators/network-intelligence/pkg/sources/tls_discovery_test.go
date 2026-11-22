// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package sources

import (
	"context"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestCollectorTLSWithCAFile(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(graphA)) }))
	defer srv.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	p, _ := strconv.Atoi(port)
	disc := func(context.Context) ([]Target, error) { return []Target{{Node: "n", Host: host}}, nil }

	ok := NewCollectorClient(CollectorConfig{Port: p, CAFile: ca}, disc)
	if _, _, err := ok.Graph(context.Background()); err != nil {
		t.Fatalf("pinned CA must verify the server: %v", err)
	}
	system := NewCollectorClient(CollectorConfig{Port: p, TLS: true}, disc) // httptest CA is not in the system store
	if _, _, err := system.Graph(context.Background()); err == nil {
		t.Fatal("an unknown CA must be rejected")
	}
	bad := NewCollectorClient(CollectorConfig{Port: p, CAFile: filepath.Join(t.TempDir(), "missing")}, disc)
	if _, _, err := bad.Graph(context.Background()); err == nil || !strings.Contains(err.Error(), "Misconfigured") {
		t.Fatalf("unreadable CA must be Misconfigured, got %v", err)
	}
}

func TestKubeDiscovererFiltersPods(t *testing.T) {
	sc := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(sc)
	mk := func(name, ns, ip string, phase corev1.PodPhase, labels map[string]string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
			Spec: corev1.PodSpec{NodeName: "node-" + name}, Status: corev1.PodStatus{Phase: phase, PodIP: ip}}
	}
	col := map[string]string{CollectorSelectorKey: CollectorSelectorValue}
	c := fake.NewClientBuilder().WithScheme(sc).WithObjects(
		mk("b", "gryvia-network", "10.0.0.2", corev1.PodRunning, col),
		mk("a", "gryvia-network", "10.0.0.1", corev1.PodRunning, col),
		mk("pending", "gryvia-network", "", corev1.PodPending, col),
		mk("other-ns", "elsewhere", "10.0.0.3", corev1.PodRunning, col),
		mk("not-collector", "gryvia-network", "10.0.0.4", corev1.PodRunning, map[string]string{"app.kubernetes.io/component": "operator"}),
	).Build()
	got, err := KubeDiscoverer(c, "gryvia-network")(context.Background())
	if err != nil || len(got) != 2 || got[0].Pod != "a" || got[0].Host != "10.0.0.1" || got[1].Node != "node-b" {
		t.Fatalf("%+v %v", got, err)
	}
}
