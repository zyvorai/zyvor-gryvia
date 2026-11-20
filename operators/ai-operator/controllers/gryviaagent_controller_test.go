// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controllers

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/ai-operator/pkg/llmgateway"
)

const agentGatewayURL = "http://gryvia-llm-gateway.gryvia-system.svc.cluster.local:8080"

func agentScheme() *runtime.Scheme {
	s := mlScheme()
	_ = networkingv1.AddToScheme(s)
	return s
}

func agentClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(agentScheme()).WithObjects(objs...).
		WithStatusSubresource(&gryviav1.GryviaAgent{}, &appsv1.Deployment{}).Build()
}

func newAgentReconciler(c client.Client) *GryviaAgentReconciler {
	return &GryviaAgentReconciler{Client: c, Scheme: agentScheme(), Log: ctrl.Log.WithName("test"),
		GatewayURL: agentGatewayURL, KeyNamespace: ragKeyNS, Image: "agent:1"}
}

func newAgent(name string, mutate ...func(*gryviav1.GryviaAgent)) *gryviav1.GryviaAgent {
	ag := &gryviav1.GryviaAgent{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: name, Generation: 1},
		Spec: gryviav1.GryviaAgentSpec{
			Model:        "chat",
			SystemPrompt: "Be brief.",
			Tools: []gryviav1.AgentTool{
				{Name: "search", Type: gryviav1.AgentToolRetrieval, Retrieval: &gryviav1.AgentRetrievalTool{VectorIndexRef: "handbook"}},
				{Name: "status", Type: gryviav1.AgentToolHTTP, HTTP: &gryviav1.AgentHTTPTool{
					URLs: []string{"https://status.example.com/api/", "http://inventory.shop.svc:9000/items", "http://10.1.2.3:8081/x"}}},
			},
		},
	}
	for _, f := range mutate {
		f(ag)
	}
	return ag
}

func getAgent(t *testing.T, c client.Client, name string) *gryviav1.GryviaAgent {
	t.Helper()
	ag := &gryviav1.GryviaAgent{}
	mustGet(t, c, "tenant-a", name, ag)
	return ag
}

func markAgentReady(t *testing.T, c client.Client, name string) {
	t.Helper()
	dep := &appsv1.Deployment{}
	mustGet(t, c, "tenant-a", name+"-agent", dep)
	dep.Status.ReadyReplicas = 1
	dep.Status.ObservedGeneration = dep.Generation
	if err := c.Status().Update(context.Background(), dep); err != nil {
		t.Fatal(err)
	}
}

func TestAgentLifecycle(t *testing.T) {
	idx := newIndex("handbook")
	c := agentClient(newAgent("helper"), idx)
	r := newAgentReconciler(c)

	res := reconcileOnce(t, r, "tenant-a", "helper")
	ag := getAgent(t, c, "helper")
	if ag.Status.Phase != gryviav1.AgentPending || res.RequeueAfter == 0 {
		t.Fatalf("phase %q requeue %v: %s", ag.Status.Phase, res.RequeueAfter, ag.Status.Message)
	}
	if ag.Status.Endpoint != "http://helper-agent.tenant-a.svc.cluster.local:8080" || ag.Status.KeySecret != "helper-llm-key" {
		t.Fatalf("status %+v", ag.Status)
	}

	raw := &corev1.Secret{}
	mustGet(t, c, "tenant-a", "helper-llm-key", raw)
	hashed := &corev1.Secret{}
	mustGet(t, c, ragKeyNS, llmgateway.OwnedKeySecretName("tenant-a", "agent", "helper"), hashed)
	if string(hashed.Data["hash"]) != llmgateway.HashKey(string(raw.Data[llmgateway.OwnedKeyField])) || hashed.Labels[llmgateway.LabelKeyOwner] != "agent" {
		t.Fatalf("hashed key %v %v", hashed.Labels, hashed.Data)
	}

	cm := &corev1.ConfigMap{}
	mustGet(t, c, "tenant-a", "helper-agent", cm)
	var conf agentRuntimeConfig
	if err := json.Unmarshal([]byte(cm.Data[agentConfigFile]), &conf); err != nil {
		t.Fatal(err)
	}
	if conf.Model != "chat" || conf.MaxSteps != defaultAgentMaxSteps || conf.SystemPrompt != "Be brief." || len(conf.Tools) != 2 {
		t.Fatalf("config %+v", conf)
	}
	if conf.Tools[0].Index != "handbook" || conf.Tools[0].TopK != defaultAgentTopK || conf.Tools[1].Method != "GET" || len(conf.Tools[1].URLs) != 3 {
		t.Fatalf("tools %+v", conf.Tools)
	}

	dep := &appsv1.Deployment{}
	mustGet(t, c, "tenant-a", "helper-agent", dep)
	ctr := dep.Spec.Template.Spec.Containers[0]
	env := containerEnv(ctr)
	if ctr.Image != "agent:1" || env["GATEWAY_URL"].Value != agentGatewayURL || env["AGENT_CONFIG"].Value != "/etc/agent/config.json" {
		t.Fatalf("container %+v", ctr)
	}
	if ref := env["GRYVIA_LLM_KEY"].ValueFrom.SecretKeyRef; ref.Name != "helper-llm-key" || ref.Key != llmgateway.OwnedKeyField {
		t.Fatalf("key ref %+v", ref)
	}
	if *dep.Spec.Replicas != 1 || !*ctr.SecurityContext.ReadOnlyRootFilesystem || *dep.Spec.Template.Spec.AutomountServiceAccountToken {
		t.Fatalf("deployment spec %+v", dep.Spec)
	}
	svc := &corev1.Service{}
	mustGet(t, c, "tenant-a", "helper-agent", svc)
	if svc.Spec.Selector[labelAgent] != "helper" || svc.Spec.Ports[0].Port != agentPort {
		t.Fatalf("service %+v", svc.Spec)
	}

	markAgentReady(t, c, "helper")
	reconcileOnce(t, r, "tenant-a", "helper")
	ag = getAgent(t, c, "helper")
	if ag.Status.Phase != gryviav1.AgentReady || ag.Status.ReadyReplicas != 1 {
		t.Fatalf("phase %q: %s", ag.Status.Phase, ag.Status.Message)
	}

	// A changed system prompt rolls the pods through the config hash.
	before := dep.Spec.Template.Annotations[annotationAgentConf]
	ag.Spec.SystemPrompt = "Be thorough."
	ag.Generation = 2
	if err := c.Update(context.Background(), ag); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "tenant-a", "helper")
	mustGet(t, c, "tenant-a", "helper-agent", dep)
	if dep.Spec.Template.Annotations[annotationAgentConf] == before {
		t.Fatal("config hash did not change")
	}
}

func TestAgentNetworkPolicy(t *testing.T) {
	c := agentClient(newAgent("helper"), newIndex("handbook"))
	reconcileOnce(t, newAgentReconciler(c), "tenant-a", "helper")
	np := &networkingv1.NetworkPolicy{}
	mustGet(t, c, "tenant-a", "helper-agent", np)
	s := np.Spec
	if s.PodSelector.MatchLabels[labelAgent] != "helper" || len(s.PolicyTypes) != 2 {
		t.Fatalf("policy %+v", s)
	}
	// DNS, the gateway, then the tool hosts sorted: the 10.1.2.3 IP, namespace shop, public :443.
	if len(s.Egress) != 5 {
		t.Fatalf("egress %+v", s.Egress)
	}
	if s.Egress[0].Ports[0].Port.IntValue() != 53 {
		t.Fatalf("dns %+v", s.Egress[0])
	}
	gw := s.Egress[1].To[0]
	if gw.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "gryvia-system" ||
		gw.PodSelector.MatchLabels["app.kubernetes.io/component"] != "llm-gateway" {
		t.Fatalf("gateway peer %+v", gw)
	}
	if ip := s.Egress[2]; ip.To[0].IPBlock.CIDR != "10.1.2.3/32" || ip.Ports[0].Port.IntValue() != 8081 {
		t.Fatalf("ip rule %+v", ip)
	}
	if ns := s.Egress[3]; ns.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "shop" || len(ns.Ports) != 0 {
		t.Fatalf("namespace rule %+v", ns)
	}
	pub := s.Egress[4]
	if pub.To[0].IPBlock.CIDR != "0.0.0.0/0" || len(pub.To[0].IPBlock.Except) == 0 || pub.Ports[0].Port.IntValue() != 443 {
		t.Fatalf("public rule %+v", pub)
	}
	from := s.Ingress[0].From
	if len(from) != 2 || from[1].PodSelector.MatchLabels["app"] != apiGatewayPodLabel || s.Ingress[0].Ports[0].Port.IntValue() != agentPort {
		t.Fatalf("ingress %+v", s.Ingress)
	}
}

func TestAgentValidation(t *testing.T) {
	cases := map[string]func(*gryviav1.GryviaAgent){
		"used twice": func(a *gryviav1.GryviaAgent) { a.Spec.Tools[1].Name = "search" },
		"needs retrieval.vectorIndexRef": func(a *gryviav1.GryviaAgent) {
			a.Spec.Tools[0].Retrieval = nil
		},
		"needs http.urls": func(a *gryviav1.GryviaAgent) { a.Spec.Tools[1].HTTP = nil },
		"absolute http":   func(a *gryviav1.GryviaAgent) { a.Spec.Tools[1].HTTP.URLs = []string{"ftp://x/"} },
		"http is set": func(a *gryviav1.GryviaAgent) {
			a.Spec.Tools[0].HTTP = &gryviav1.AgentHTTPTool{URLs: []string{"http://x/"}}
		},
	}
	for want, mutate := range cases {
		c := agentClient(newAgent("bad", mutate))
		reconcileOnce(t, newAgentReconciler(c), "tenant-a", "bad")
		ag := getAgent(t, c, "bad")
		if ag.Status.Phase != gryviav1.AgentFailed || !strings.Contains(ag.Status.Message, want) {
			t.Errorf("%s: phase %q message %q", want, ag.Status.Phase, ag.Status.Message)
		}
		if mlObjectExists(c, "tenant-a", "bad-agent", &appsv1.Deployment{}) {
			t.Errorf("%s: deployment created for an invalid agent", want)
		}
	}
}

func zyntraTool(mutate ...func(*gryviav1.AgentZyntraTool)) gryviav1.AgentTool {
	z := &gryviav1.AgentZyntraTool{
		URL: "http://sa-zyntra.gryvia-system.svc:8080",
		TokenSecretRef: corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "zyntra-token"}, Key: "token"},
		Propose: true,
		Actions: []string{"raise-inference-priority"},
	}
	for _, f := range mutate {
		f(z)
	}
	return gryviav1.AgentTool{Name: "ops-graph", Type: gryviav1.AgentToolZyntra, Zyntra: z}
}

func TestAgentZyntraTool(t *testing.T) {
	ag := newAgent("ops", func(a *gryviav1.GryviaAgent) { a.Spec.Tools = []gryviav1.AgentTool{zyntraTool()} })
	c := agentClient(ag)
	reconcileOnce(t, newAgentReconciler(c), "tenant-a", "ops")
	if got := getAgent(t, c, "ops"); got.Status.Phase == gryviav1.AgentFailed {
		t.Fatalf("failed: %s", got.Status.Message)
	}

	cm := &corev1.ConfigMap{}
	mustGet(t, c, "tenant-a", "ops-agent", cm)
	var conf agentRuntimeConfig
	if err := json.Unmarshal([]byte(cm.Data[agentConfigFile]), &conf); err != nil {
		t.Fatal(err)
	}
	tc := conf.Tools[0]
	if tc.Type != "zyntra" || tc.URL != "http://sa-zyntra.gryvia-system.svc:8080" || tc.TokenEnv != "ZYNTRA_TOKEN_OPS_GRAPH" ||
		!tc.Propose || len(tc.Actions) != 1 {
		t.Fatalf("tool config %+v", tc)
	}
	if strings.Contains(cm.Data[agentConfigFile], "zyntra-token") {
		t.Fatal("the config names the token Secret; the runtime only needs the env var")
	}

	dep := &appsv1.Deployment{}
	mustGet(t, c, "tenant-a", "ops-agent", dep)
	ref := containerEnv(dep.Spec.Template.Spec.Containers[0])["ZYNTRA_TOKEN_OPS_GRAPH"].ValueFrom.SecretKeyRef
	if ref.Name != "zyntra-token" || ref.Key != "token" {
		t.Fatalf("token ref %+v", ref)
	}

	np := &networkingv1.NetworkPolicy{}
	mustGet(t, c, "tenant-a", "ops-agent", np)
	if len(np.Spec.Egress) != 3 {
		t.Fatalf("egress %+v", np.Spec.Egress)
	}
	if ns := np.Spec.Egress[2].To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]; ns != "gryvia-system" {
		t.Fatalf("zyntra egress to namespace %q", ns)
	}
}

func TestAgentZyntraValidation(t *testing.T) {
	cases := map[string]gryviav1.AgentTool{
		"needs zyntra.url":                 {Name: "z", Type: gryviav1.AgentToolZyntra},
		"at most 56 characters":            func() gryviav1.AgentTool { z := zyntraTool(); z.Name = strings.Repeat("n", 57); return z }(),
		"absolute http":                    zyntraTool(func(z *gryviav1.AgentZyntraTool) { z.URL = "sa-zyntra:8080" }),
		"tokenSecretRef":                   zyntraTool(func(z *gryviav1.AgentZyntraTool) { z.TokenSecretRef.Key = "" }),
		"only applies with zyntra.propose": zyntraTool(func(z *gryviav1.AgentZyntraTool) { z.Propose = false }),
		"zyntra is set but type is http": {Name: "z", Type: gryviav1.AgentToolHTTP,
			HTTP: &gryviav1.AgentHTTPTool{URLs: []string{"http://x/"}}, Zyntra: &gryviav1.AgentZyntraTool{URL: "http://x/"}},
	}
	for want, tool := range cases {
		c := agentClient(newAgent("bad", func(a *gryviav1.GryviaAgent) { a.Spec.Tools = []gryviav1.AgentTool{tool} }))
		reconcileOnce(t, newAgentReconciler(c), "tenant-a", "bad")
		if ag := getAgent(t, c, "bad"); ag.Status.Phase != gryviav1.AgentFailed || !strings.Contains(ag.Status.Message, want) {
			t.Errorf("%s: phase %q message %q", want, ag.Status.Phase, ag.Status.Message)
		}
	}
}

func TestAgentMissingIndexAndScaleToZero(t *testing.T) {
	zero := int32(0)
	c := agentClient(newAgent("helper", func(a *gryviav1.GryviaAgent) { a.Spec.Replicas = &zero }))
	r := newAgentReconciler(c)
	reconcileOnce(t, r, "tenant-a", "helper")
	ag := getAgent(t, c, "helper")
	var tools *metav1.Condition
	for i := range ag.Status.Conditions {
		if ag.Status.Conditions[i].Type == conditionTools {
			tools = &ag.Status.Conditions[i]
		}
	}
	if tools == nil || tools.Status != metav1.ConditionFalse || !strings.Contains(tools.Message, "handbook") {
		t.Fatalf("tools condition %+v", tools)
	}
	if ag.Status.Phase != gryviav1.AgentPending || !strings.Contains(ag.Status.Message, "Scaled to zero") {
		t.Fatalf("phase %q: %s", ag.Status.Phase, ag.Status.Message)
	}
	dep := &appsv1.Deployment{}
	mustGet(t, c, "tenant-a", "helper-agent", dep)
	if *dep.Spec.Replicas != 0 {
		t.Fatalf("replicas %d", *dep.Spec.Replicas)
	}
}

func TestAgentDeletionRemovesKeyOnly(t *testing.T) {
	ag := newAgent("helper")
	c := agentClient(ag, newIndex("handbook"))
	r := newAgentReconciler(c)
	reconcileOnce(t, r, "tenant-a", "helper")
	// An index's store credential that shares the agent's name must survive the agent.
	if err := llmgateway.MirrorStoreKey(context.Background(), c, ragKeyNS, "tenant-a", "helper", "store-key"); err != nil {
		t.Fatal(err)
	}
	cur := getAgent(t, c, "helper")
	if err := c.Delete(context.Background(), cur); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "tenant-a", "helper")
	if mlObjectExists(c, ragKeyNS, llmgateway.OwnedKeySecretName("tenant-a", "agent", "helper"), &corev1.Secret{}) {
		t.Fatal("hashed agent key left behind")
	}
	if !mlObjectExists(c, ragKeyNS, llmgateway.StoreSecretName("tenant-a", "helper"), &corev1.Secret{}) {
		t.Fatal("deleting the agent removed an index's store credential")
	}
	if mlObjectExists(c, "tenant-a", "helper", &gryviav1.GryviaAgent{}) {
		t.Fatal("agent not released")
	}
}

func TestAgentEgressHelpers(t *testing.T) {
	if ns := gatewayNamespace(agentGatewayURL); ns != "gryvia-system" {
		t.Fatalf("gateway namespace %q", ns)
	}
	if ns := gatewayNamespace("https://llm.example.com"); ns != "" {
		t.Fatalf("external gateway namespace %q", ns)
	}
	r := &GryviaAgentReconciler{GatewayURL: "https://llm.example.com"}
	spec := r.networkPolicySpec(newAgent("x", func(a *gryviav1.GryviaAgent) { a.Spec.Tools = nil }))
	if len(spec.Egress) != 2 || spec.Egress[1].Ports[0].Port.IntValue() != 443 || len(spec.Ingress[0].From) != 1 {
		t.Fatalf("external gateway policy %+v", spec)
	}
	if _, key := egressRule("http://search/", "tenant-a"); key != "ns:tenant-a" {
		t.Fatalf("short host key %q", key)
	}
}
