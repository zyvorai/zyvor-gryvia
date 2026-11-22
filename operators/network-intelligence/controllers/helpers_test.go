// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gryviav1 "github.com/zyvorai/gryvia/operators/network-intelligence/api/v1"
	"github.com/zyvorai/gryvia/operators/network-intelligence/pkg/sources"
)

// fakeCollector is a canned sources.Collector; err makes every call fail like an absent collector.
type fakeCollector struct {
	graph     sources.Graph
	anomalies []sources.Anomaly
	alerts    []sources.SecurityAlert
	stats     sources.Stats
	err       error
	calls     int
}

func (f *fakeCollector) Graph(context.Context) (sources.Graph, sources.Stats, error) {
	f.calls++
	return f.graph, f.stats, f.err
}
func (f *fakeCollector) Anomalies(context.Context) ([]sources.Anomaly, sources.Stats, error) {
	return f.anomalies, f.stats, f.err
}
func (f *fakeCollector) SecurityAlerts(context.Context) ([]sources.SecurityAlert, sources.Stats, error) {
	return f.alerts, f.stats, f.err
}

type fakeNetra struct {
	on      bool
	records []sources.FlowRecord
	err     error
}

func (f *fakeNetra) Configured() bool { return f.on }
func (f *fakeNetra) History(context.Context, time.Duration, int) ([]sources.FlowRecord, error) {
	return f.records, f.err
}

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = gryviav1.AddToScheme(s)
	return s
}

func newClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objs...).
		WithStatusSubresource(&gryviav1.GryviaFlowPolicy{}, &gryviav1.GryviaTrafficInsight{}, &gryviav1.GryviaAutoPolicy{},
			&gryviav1.GryviaTraceSession{}, &gryviav1.GryviaServiceGraph{}, &gryviav1.GryviaNetworkAnomaly{},
			&gryviav1.GryviaSecurityPolicy{}, &gryviav1.GryviaNetworkCost{}, &gryviav1.GryviaTrainingInsight{},
			&gryviav1.GryviaInferenceInsight{}).Build()
}

func req(ns, name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}
}

func svc(ns, name string, selector map[string]string) *corev1.Service {
	return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: corev1.ServiceSpec{Selector: selector}}
}

func namespaceObj(name string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func cond(conds []metav1.Condition, typ string) *metav1.Condition {
	return meta.FindStatusCondition(conds, typ)
}

func mustGet(t *testing.T, c client.Client, ns, name string, obj client.Object) {
	t.Helper()
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, obj); err != nil {
		t.Fatalf("get %s/%s: %v", ns, name, err)
	}
}

func mustReconcile(t *testing.T, r interface {
	Reconcile(context.Context, ctrl.Request) (ctrl.Result, error)
}, ns, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), req(ns, name))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return res
}

func unstr(apiKind, ns, name string, spec, status map[string]interface{}) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "gryvia.io/v1alpha1", "kind": apiKind,
		"metadata": map[string]interface{}{"name": name},
		"spec":     spec,
	}}
	if ns != "" {
		u.SetNamespace(ns)
	}
	if status != nil {
		u.Object["status"] = status
	}
	return u
}

func notConfigured() error {
	return &sources.SourceError{Source: "collector", Reason: sources.ReasonNoCollectors, Message: "no running collector pod"}
}
