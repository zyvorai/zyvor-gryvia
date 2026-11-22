// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/ai-operator/pkg/llmgateway"
)

const (
	// DefaultAgentImage is the --agent-image default: the reference runtime in examples/agents.
	DefaultAgentImage = "ghcr.io/zyvorai/gryvia-agent-runtime:latest"

	agentFinalizer       = "gryvia.io/agent"
	labelAgent           = "gryvia.io/agent"
	annotationAgentConf  = "gryvia.io/agent-config-hash"
	conditionTools       = "ToolsResolved"
	conditionAvailable   = "Available"
	agentPort            = 8080
	agentConfigFile      = "config.json"
	agentConfigMountPath = "/etc/agent"
	defaultAgentMaxSteps = 5
	defaultAgentTopK     = 4
	// apiGatewayPodLabel selects the api-gateway pods, which proxy POST /api/agents/{name}/chat.
	apiGatewayPodLabel = "gryvia-api-gateway"
)

// privateRanges are excluded from the egress opened for external tool hosts, so allowlisting a public host does
// not open the cluster network on that port.
var privateRanges = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "100.64.0.0/10"}

// GryviaAgentReconciler serves tool-calling agents: a runtime Deployment and Service, a per-agent gateway key and a
// NetworkPolicy that limits egress to DNS, the LLM gateway and the allowlisted tool hosts.
type GryviaAgentReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	Log          logr.Logger
	Image        string
	GatewayURL   string
	KeyNamespace string
}

//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaagents,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaagents/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviaagents/finalizers,verbs=update
//+kubebuilder:rbac:groups=gryvia.io,resources=gryviavectorindexes,verbs=get;list;watch
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;delete
//+kubebuilder:rbac:groups="",resources=services;configmaps;secrets,verbs=get;list;watch;create;update;delete
//+kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;delete

func (r *GryviaAgentReconciler) keyNamespace() string {
	if r.KeyNamespace != "" {
		return r.KeyNamespace
	}
	return "gryvia-llm-keys"
}

// Reconcile drives one agent towards a running runtime.
func (r *GryviaAgentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ag := &gryviav1.GryviaAgent{}
	if err := r.Get(ctx, req.NamespacedName, ag); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !ag.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(ag, agentFinalizer) {
			if err := llmgateway.DeleteOwnedKey(ctx, r.Client, r.keyNamespace(), ag.Namespace, "agent", ag.Name); err != nil {
				return ctrl.Result{}, err
			}
			controllerutil.RemoveFinalizer(ag, agentFinalizer)
			return ctrl.Result{}, r.Update(ctx, ag)
		}
		return ctrl.Result{}, nil
	}
	if !controllerutil.ContainsFinalizer(ag, agentFinalizer) {
		controllerutil.AddFinalizer(ag, agentFinalizer)
		if err := r.Update(ctx, ag); err != nil {
			return ctrl.Result{}, err
		}
	}
	orig := ag.DeepCopy()
	res, err := r.reconcileAgent(ctx, ag)
	ag.Status.ObservedGeneration = ag.Generation
	if perr := patchStatus(ctx, r.Client, ag, orig); perr != nil && err == nil {
		err = perr
	}
	return res, err
}

func validateAgent(ag *gryviav1.GryviaAgent) error {
	seen := map[string]bool{}
	for _, t := range ag.Spec.Tools {
		if seen[t.Name] {
			return fmt.Errorf("tool name %q is used twice", t.Name)
		}
		seen[t.Name] = true
		if other := otherToolBlock(t); other != "" {
			return fmt.Errorf("tool %s: %s is set but type is %s", t.Name, other, t.Type)
		}
		switch t.Type {
		case gryviav1.AgentToolRetrieval:
			if t.Retrieval == nil || t.Retrieval.VectorIndexRef == "" {
				return fmt.Errorf("tool %s: type retrieval needs retrieval.vectorIndexRef", t.Name)
			}
		case gryviav1.AgentToolHTTP:
			if t.HTTP == nil || len(t.HTTP.URLs) == 0 {
				return fmt.Errorf("tool %s: type http needs http.urls", t.Name)
			}
			for _, raw := range t.HTTP.URLs {
				if _, err := parseToolURL(raw); err != nil {
					return fmt.Errorf("tool %s: %v", t.Name, err)
				}
			}
		case gryviav1.AgentToolZyntra:
			z := t.Zyntra
			if len(t.Name) > 64-len("_propose") {
				return fmt.Errorf("tool %s: a zyntra tool name is at most 56 characters (the model sees %s_propose)", t.Name, t.Name)
			}
			if z == nil || z.URL == "" {
				return fmt.Errorf("tool %s: type zyntra needs zyntra.url", t.Name)
			}
			if _, err := parseToolURL(z.URL); err != nil {
				return fmt.Errorf("tool %s: %v", t.Name, err)
			}
			if z.TokenSecretRef.Name == "" || z.TokenSecretRef.Key == "" {
				return fmt.Errorf("tool %s: type zyntra needs zyntra.tokenSecretRef name and key", t.Name)
			}
			if len(z.Actions) > 0 && !z.Propose {
				return fmt.Errorf("tool %s: zyntra.actions only applies with zyntra.propose", t.Name)
			}
		default:
			return fmt.Errorf("tool %s: type must be retrieval, http or zyntra", t.Name)
		}
	}
	return nil
}

// otherToolBlock names a configuration block set for a type other than the tool's own, or "".
func otherToolBlock(t gryviav1.AgentTool) string {
	switch {
	case t.Retrieval != nil && t.Type != gryviav1.AgentToolRetrieval:
		return "retrieval"
	case t.HTTP != nil && t.Type != gryviav1.AgentToolHTTP:
		return "http"
	case t.Zyntra != nil && t.Type != gryviav1.AgentToolZyntra:
		return "zyntra"
	}
	return ""
}

// zyntraTokenEnv is the runtime environment variable holding a zyntra tool's token.
func zyntraTokenEnv(tool string) string {
	return "ZYNTRA_TOKEN_" + strings.ToUpper(strings.ReplaceAll(tool, "-", "_"))
}

func parseToolURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return nil, fmt.Errorf("url %q must be an absolute http:// or https:// URL", raw)
	}
	return u, nil
}

func agentName(ag *gryviav1.GryviaAgent) string { return childName(ag.Name, "agent") }

func agentLabels(ag *gryviav1.GryviaAgent) map[string]string {
	return map[string]string{labelAgent: ag.Name, "gryvia.io/component": "agent"}
}

// AgentEndpoint is the in-cluster base URL of an agent's runtime.
func AgentEndpoint(namespace, agent string) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", childName(agent, "agent"), namespace, agentPort)
}

func (r *GryviaAgentReconciler) reconcileAgent(ctx context.Context, ag *gryviav1.GryviaAgent) (ctrl.Result, error) {
	if err := validateAgent(ag); err != nil {
		ag.Status.Phase, ag.Status.Message = gryviav1.AgentFailed, "Invalid agent: "+err.Error()
		return ctrl.Result{}, nil
	}

	missing, err := r.missingIndexes(ctx, ag)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(missing) > 0 {
		setCondition(&ag.Status.Conditions, ag.Generation, conditionTools, metav1.ConditionFalse, "IndexNotFound",
			"Vector index not found: "+strings.Join(missing, ", "))
	} else {
		setCondition(&ag.Status.Conditions, ag.Generation, conditionTools, metav1.ConditionTrue, "Resolved", "All tools resolve")
	}

	keySecret := childName(ag.Name, "llm-key")
	if err := llmgateway.EnsureOwnedKey(ctx, r.Client, r.Scheme, r.keyNamespace(), ag, "agent", keySecret); err != nil {
		return ctrl.Result{}, fmt.Errorf("gateway key: %w", err)
	}
	ag.Status.KeySecret = keySecret

	conf, err := agentConfig(ag)
	if err != nil {
		return ctrl.Result{}, err
	}
	name := agentName(ag)
	labels := agentLabels(ag)

	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ag.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = labels
		cm.Data = map[string]string{agentConfigFile: conf}
		return controllerutil.SetControllerReference(ag, cm, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, err
	}

	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ag.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, np, func() error {
		np.Labels = labels
		np.Spec = r.networkPolicySpec(ag)
		return controllerutil.SetControllerReference(ag, np, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, err
	}

	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ag.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = labels
		svc.Spec.Selector = labels
		svc.Spec.Ports = []corev1.ServicePort{{Name: "http", Port: agentPort, TargetPort: intstr.FromInt32(agentPort), Protocol: corev1.ProtocolTCP}}
		return controllerutil.SetControllerReference(ag, svc, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, err
	}

	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ag.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		r.mutateDeployment(ag, dep, conf)
		return controllerutil.SetControllerReference(ag, dep, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, err
	}

	ag.Status.Endpoint = AgentEndpoint(ag.Namespace, ag.Name)
	ag.Status.ReadyReplicas = dep.Status.ReadyReplicas
	want := agentReplicas(ag)
	switch {
	case want == 0:
		setCondition(&ag.Status.Conditions, ag.Generation, conditionAvailable, metav1.ConditionFalse, "ScaledToZero", "spec.replicas is 0")
		ag.Status.Phase, ag.Status.Message = gryviav1.AgentPending, "Scaled to zero (spec.replicas is 0)"
		return ctrl.Result{}, nil
	case dep.Status.ReadyReplicas >= 1 && dep.Status.ObservedGeneration >= dep.Generation:
		setCondition(&ag.Status.Conditions, ag.Generation, conditionAvailable, metav1.ConditionTrue, "Ready",
			fmt.Sprintf("%d/%d replicas ready", dep.Status.ReadyReplicas, want))
		ag.Status.Phase = gryviav1.AgentReady
		ag.Status.Message = fmt.Sprintf("Serving %s with model %s and %d tools", ag.Status.Endpoint, ag.Spec.Model, len(ag.Spec.Tools))
		if len(missing) > 0 {
			ag.Status.Message += "; vector index not found: " + strings.Join(missing, ", ")
			return ctrl.Result{RequeueAfter: time.Minute}, nil
		}
		return ctrl.Result{}, nil
	default:
		setCondition(&ag.Status.Conditions, ag.Generation, conditionAvailable, metav1.ConditionFalse, "Starting", "Waiting for a ready replica")
		ag.Status.Phase, ag.Status.Message = gryviav1.AgentPending, "Waiting for the agent runtime to be ready"
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
}

func (r *GryviaAgentReconciler) missingIndexes(ctx context.Context, ag *gryviav1.GryviaAgent) ([]string, error) {
	var missing []string
	for _, t := range ag.Spec.Tools {
		if t.Type != gryviav1.AgentToolRetrieval {
			continue
		}
		idx := &gryviav1.GryviaVectorIndex{}
		err := r.Get(ctx, types.NamespacedName{Namespace: ag.Namespace, Name: t.Retrieval.VectorIndexRef}, idx)
		if errors.IsNotFound(err) {
			missing = append(missing, t.Retrieval.VectorIndexRef)
			continue
		}
		if err != nil {
			return nil, err
		}
	}
	return missing, nil
}

func agentReplicas(ag *gryviav1.GryviaAgent) int32 {
	if ag.Spec.Replicas == nil {
		return 1
	}
	return *ag.Spec.Replicas
}

// agentToolConfig is a tool as the runtime reads it from AGENT_CONFIG.
type agentToolConfig struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Type        string   `json:"type"`
	Index       string   `json:"index,omitempty"`
	TopK        int32    `json:"topK,omitempty"`
	URLs        []string `json:"urls,omitempty"`
	Method      string   `json:"method,omitempty"`
	URL         string   `json:"url,omitempty"`
	TokenEnv    string   `json:"tokenEnv,omitempty"`
	Propose     bool     `json:"propose,omitempty"`
	Actions     []string `json:"actions,omitempty"`
}

type agentRuntimeConfig struct {
	Name         string            `json:"name"`
	Namespace    string            `json:"namespace"`
	Model        string            `json:"model"`
	SystemPrompt string            `json:"systemPrompt,omitempty"`
	MaxSteps     int32             `json:"maxSteps"`
	Tools        []agentToolConfig `json:"tools"`
}

func agentConfig(ag *gryviav1.GryviaAgent) (string, error) {
	c := agentRuntimeConfig{
		Name: ag.Name, Namespace: ag.Namespace, Model: ag.Spec.Model, SystemPrompt: ag.Spec.SystemPrompt,
		MaxSteps: ag.Spec.MaxSteps, Tools: []agentToolConfig{},
	}
	if c.MaxSteps <= 0 {
		c.MaxSteps = defaultAgentMaxSteps
	}
	for _, t := range ag.Spec.Tools {
		tc := agentToolConfig{Name: t.Name, Description: t.Description, Type: t.Type}
		switch t.Type {
		case gryviav1.AgentToolRetrieval:
			tc.Index, tc.TopK = t.Retrieval.VectorIndexRef, t.Retrieval.TopK
			if tc.TopK <= 0 {
				tc.TopK = defaultAgentTopK
			}
		case gryviav1.AgentToolHTTP:
			tc.URLs, tc.Method = t.HTTP.URLs, t.HTTP.Method
			if tc.Method == "" {
				tc.Method = "GET"
			}
		case gryviav1.AgentToolZyntra:
			tc.URL, tc.TokenEnv = t.Zyntra.URL, zyntraTokenEnv(t.Name)
			tc.Propose, tc.Actions = t.Zyntra.Propose, t.Zyntra.Actions
		}
		c.Tools = append(c.Tools, tc)
	}
	b, err := json.MarshalIndent(c, "", "  ")
	return string(b), err
}

func (r *GryviaAgentReconciler) image(ag *gryviav1.GryviaAgent) string {
	switch {
	case ag.Spec.Image != "":
		return ag.Spec.Image
	case r.Image != "":
		return r.Image
	}
	return DefaultAgentImage
}

func (r *GryviaAgentReconciler) mutateDeployment(ag *gryviav1.GryviaAgent, dep *appsv1.Deployment, conf string) {
	labels := agentLabels(ag)
	replicas := agentReplicas(ag)
	res := ag.Spec.Resources
	if len(res.Requests) == 0 && len(res.Limits) == 0 {
		res = corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("128Mi")}}
	}
	env := []corev1.EnvVar{
		{Name: "GATEWAY_URL", Value: r.GatewayURL},
		{Name: "GRYVIA_LLM_KEY", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: ag.Status.KeySecret}, Key: llmgateway.OwnedKeyField}}},
		{Name: "AGENT_CONFIG", Value: agentConfigMountPath + "/" + agentConfigFile},
		{Name: "PORT", Value: strconv.Itoa(agentPort)},
	}
	for _, t := range ag.Spec.Tools {
		if t.Type == gryviav1.AgentToolZyntra && t.Zyntra != nil {
			ref := t.Zyntra.TokenSecretRef
			env = append(env, corev1.EnvVar{Name: zyntraTokenEnv(t.Name), ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &ref}})
		}
	}
	probe := func(period int32) *corev1.Probe {
		return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(agentPort)}}, PeriodSeconds: period}
	}
	dep.Labels = labels
	dep.Spec.Replicas = &replicas
	dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
	dep.Spec.Template.Labels = labels
	dep.Spec.Template.Annotations = map[string]string{annotationAgentConf: specHash(conf)}
	dep.Spec.Template.Spec = corev1.PodSpec{
		AutomountServiceAccountToken: boolPtr(false),
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot: boolPtr(true), RunAsUser: int64Ptr(65534),
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		Containers: []corev1.Container{{
			Name:           "agent",
			Image:          r.image(ag),
			Ports:          []corev1.ContainerPort{{Name: "http", ContainerPort: agentPort, Protocol: corev1.ProtocolTCP}},
			Env:            env,
			Resources:      res,
			ReadinessProbe: probe(5),
			LivenessProbe:  probe(20),
			VolumeMounts: []corev1.VolumeMount{
				{Name: "config", MountPath: agentConfigMountPath, ReadOnly: true},
				{Name: "tmp", MountPath: "/tmp"},
			},
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: boolPtr(false),
				ReadOnlyRootFilesystem:   boolPtr(true),
				Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			},
		}},
		Volumes: []corev1.Volume{
			{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: agentName(ag)}}}},
			{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		},
	}
}

// gatewayNamespace is the namespace of an in-cluster LLM gateway URL (<svc>.<ns>.svc...), or "".
func gatewayNamespace(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if parts := strings.Split(u.Hostname(), "."); len(parts) >= 3 && parts[2] == "svc" {
		return parts[1]
	}
	return ""
}

func namespacePeer(ns string, pods map[string]string) networkingv1.NetworkPolicyPeer {
	p := networkingv1.NetworkPolicyPeer{NamespaceSelector: &metav1.LabelSelector{
		MatchLabels: map[string]string{"kubernetes.io/metadata.name": ns}}}
	if pods != nil {
		p.PodSelector = &metav1.LabelSelector{MatchLabels: pods}
	}
	return p
}

func tcpPort(n int) []networkingv1.NetworkPolicyPort {
	tcp := corev1.ProtocolTCP
	port := intstr.FromInt32(int32(n))
	return []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}}
}

// egressRule opens one URL's host: an in-cluster service by namespace (service and container ports may differ, so
// any port), an IP literal by /32 on the URL port, and a public hostname on its port outside the private ranges.
func egressRule(raw, agentNS string) (networkingv1.NetworkPolicyEgressRule, string) {
	u, err := parseToolURL(raw)
	if err != nil {
		return networkingv1.NetworkPolicyEgressRule{}, ""
	}
	host := u.Hostname()
	port := 80
	if u.Scheme == "https" {
		port = 443
	}
	if p, err := strconv.ParseUint(u.Port(), 10, 16); err == nil && p > 0 {
		port = int(p)
	}
	if ip := net.ParseIP(host); ip != nil {
		cidr := ip.String() + "/32"
		if ip.To4() == nil {
			cidr = ip.String() + "/128"
		}
		return networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: cidr}}}, Ports: tcpPort(port),
		}, "ip:" + cidr + ":" + strconv.Itoa(port)
	}
	parts := strings.Split(host, ".")
	switch {
	case len(parts) == 1:
		return networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{namespacePeer(agentNS, nil)}}, "ns:" + agentNS
	case len(parts) == 2 || parts[2] == "svc":
		return networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{namespacePeer(parts[1], nil)}}, "ns:" + parts[1]
	}
	return networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{
			{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: privateRanges}},
			{IPBlock: &networkingv1.IPBlock{CIDR: "::/0", Except: []string{"fc00::/7", "fe80::/10"}}},
		},
		Ports: tcpPort(port),
	}, "public:" + strconv.Itoa(port)
}

func (r *GryviaAgentReconciler) networkPolicySpec(ag *gryviav1.GryviaAgent) networkingv1.NetworkPolicySpec {
	udp, tcp := corev1.ProtocolUDP, corev1.ProtocolTCP
	dns := intstr.FromInt32(53)
	egress := []networkingv1.NetworkPolicyEgressRule{{
		To:    []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{}}},
		Ports: []networkingv1.NetworkPolicyPort{{Protocol: &udp, Port: &dns}, {Protocol: &tcp, Port: &dns}},
	}}
	platformNS := gatewayNamespace(r.GatewayURL)
	if platformNS != "" {
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{
			namespacePeer(platformNS, map[string]string{"app.kubernetes.io/component": "llm-gateway"})}})
	} else if r.GatewayURL != "" {
		rule, _ := egressRule(r.GatewayURL, ag.Namespace)
		egress = append(egress, rule)
	}
	seen := map[string]bool{}
	var keys []string
	rules := map[string]networkingv1.NetworkPolicyEgressRule{}
	for _, t := range ag.Spec.Tools {
		var urls []string
		switch {
		case t.Type == gryviav1.AgentToolHTTP && t.HTTP != nil:
			urls = t.HTTP.URLs
		case t.Type == gryviav1.AgentToolZyntra && t.Zyntra != nil:
			urls = []string{t.Zyntra.URL}
		}
		for _, raw := range urls {
			rule, key := egressRule(raw, ag.Namespace)
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			keys = append(keys, key)
			rules[key] = rule
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		egress = append(egress, rules[k])
	}

	from := []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}}
	if platformNS != "" && platformNS != ag.Namespace {
		from = append(from, namespacePeer(platformNS, map[string]string{"app": apiGatewayPodLabel}))
	}
	return networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{MatchLabels: agentLabels(ag)},
		PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		Ingress:     []networkingv1.NetworkPolicyIngressRule{{From: from, Ports: tcpPort(agentPort)}},
		Egress:      egress,
	}
}

// SetupWithManager watches agents and the objects they own.
func (r *GryviaAgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gryviav1.GryviaAgent{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Complete(r)
}
