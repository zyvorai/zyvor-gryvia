//go:build integration

package controllers

import (
	"bytes"
	"context"
	gryviav1 "github.com/zyvorai/gryvia/operators/quota-operator/api/v1"
	"io"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	"os"
	"path/filepath"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"testing"
)

func TestAdvancedKueueSchema(t *testing.T) {
	dir := os.Getenv("GRYVIA_KUEUE_CRD_DIR")
	if dir == "" {
		t.Skip("set GRYVIA_KUEUE_CRD_DIR to schemas rendered from pinned Kueue chart")
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{dir}, ErrorIfCRDPathMissing: true}
	env.ControlPlane.GetAPIServer().Configure().Set("advertise-address", "127.0.0.1")
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { env.Stop() })
	c, err := client.New(cfg, client.Options{Scheme: newQuotaTestScheme()})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "examples", "scheduling", "advanced-kueue.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := decoder.Decode(obj); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if err := c.Create(context.Background(), obj); err != nil {
			t.Fatalf("example %s rejected: %v", obj.GetKind(), err)
		}
	}
	r := &GryviaKueueReconciler{FairSharing: true, TopologyName: "rack", AdmissionCheck: "multikueue"}
	for _, obj := range r.desired(&gryviav1.GryviaTenant{ObjectMeta: metav1.ObjectMeta{Name: "team"}}, nil, nil) {
		if obj.GetKind() == "LocalQueue" {
			continue
		}
		if err := c.Create(context.Background(), obj); err != nil {
			t.Fatalf("%s schema rejected: %v", obj.GetKind(), err)
		}
		out := &unstructured.Unstructured{}
		out.SetGroupVersionKind(obj.GroupVersionKind())
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(obj), out); err != nil {
			t.Fatal(err)
		}
		if obj.GetKind() == "ResourceFlavor" {
			name, _, _ := unstructured.NestedString(out.Object, "spec", "topologyName")
			if name != "rack" {
				t.Fatal("topology was pruned")
			}
		}
		if obj.GetKind() == "ClusterQueue" {
			w, _, _ := unstructured.NestedString(out.Object, "spec", "fairSharing", "weight")
			if w != "1" {
				t.Fatal("fair sharing was pruned")
			}
			checks, _, _ := unstructured.NestedSlice(out.Object, "spec", "admissionChecksStrategy", "admissionChecks")
			if len(checks) != 1 {
				t.Fatal("admission check was pruned")
			}
		}
	}
	gen := BuildTopology("gryvia-generated")
	if err := c.Create(context.Background(), gen); err != nil {
		t.Fatalf("generated Topology rejected: %v", err)
	}
	out := &unstructured.Unstructured{}
	out.SetGroupVersionKind(gen.GroupVersionKind())
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gen), out); err != nil {
		t.Fatal(err)
	}
	if levels, _, _ := unstructured.NestedSlice(out.Object, "spec", "levels"); len(levels) != 3 {
		t.Fatalf("generated Topology levels pruned: %v", levels)
	}
}
