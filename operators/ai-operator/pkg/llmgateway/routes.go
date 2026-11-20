// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Package llmgateway is an OpenAI-compatible front door for the GryviaInferenceServices annotated
// gryvia.io/llm-model: it authenticates per-tenant keys, routes by the request's model, enforces
// GryviaQuota.spec.tokensPerDay and meters tokens into GryviaUsageRecords (kind tokens).
package llmgateway

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

const (
	// LabelKey marks a key Secret; its data holds "hash" (sha256 hex of the key) and "namespace".
	LabelKey    = "gryvia.io/llm-key"
	LabelTenant = "gryvia.io/tenant"
	LabelModel  = "gryvia.io/llm-model"

	// AnnotationModel on a GryviaInferenceService publishes it under that model name.
	AnnotationModel = "gryvia.io/llm-model"
	// AnnotationServedModel is the name the server knows the model by (vLLM --served-model-name), when it differs.
	AnnotationServedModel = "gryvia.io/llm-served-model"
	// AnnotationShared "true" lets keys of every namespace use the model, not only keys of its namespace.
	AnnotationShared = "gryvia.io/llm-shared"
	// AnnotationPriceIn and AnnotationPriceOut are prices per million input and output tokens.
	AnnotationPriceIn  = "gryvia.io/llm-price-input-per-1m"
	AnnotationPriceOut = "gryvia.io/llm-price-output-per-1m"
)

var modelNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// Key is an authenticated caller: the key Secret's name, tenant and namespace.
type Key struct {
	Name      string
	Tenant    string
	Namespace string
}

// HashKey is how keys are stored: the hex sha256 of the bearer token.
func HashKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// keysFrom indexes key Secrets by hash. Secrets without a hash or a namespace are ignored.
func keysFrom(secrets []corev1.Secret) map[string]Key {
	out := make(map[string]Key, len(secrets))
	for _, s := range secrets {
		hash := strings.TrimSpace(string(s.Data["hash"]))
		ns := strings.TrimSpace(string(s.Data["namespace"]))
		if len(hash) != sha256.Size*2 || ns == "" || s.Labels[LabelKey] != "true" {
			continue
		}
		tenant := s.Labels[LabelTenant]
		if tenant == "" {
			tenant = ns
		}
		out[strings.ToLower(hash)] = Key{Name: s.Name, Tenant: tenant, Namespace: ns}
	}
	return out
}

// Route is one published model.
type Route struct {
	Model       string
	Namespace   string
	Service     string
	Upstream    string
	ServedModel string
	Shared      bool
	PriceIn     float64
	PriceOut    float64
	// ScaleToZero routes are woken by the gateway and get last-request annotations; Idle and ColdStart are the
	// service's settings, Phase its status.phase.
	ScaleToZero bool
	Idle        time.Duration
	ColdStart   time.Duration
	Phase       string
}

// Waking reports whether requests for the route must wait for the service to start.
func (r Route) Waking() bool {
	return r.ScaleToZero && (r.Phase == PhaseScaledToZero || r.Phase == PhaseDeploying)
}

// Inference service phases the gateway acts on (set by the ai-operator's inference controller).
const (
	PhaseScaledToZero = "ScaledToZero"
	PhaseDeploying    = "Deploying"
)

// routesFrom publishes the annotated services that have an endpoint, sorted by model and namespace.
func routesFrom(items []gryviav1.GryviaInferenceService, defIn, defOut float64) []Route {
	var out []Route
	for _, s := range items {
		model := strings.TrimSpace(s.Annotations[AnnotationModel])
		if !modelNameRE.MatchString(model) || s.Status.Endpoint == "" || !s.DeletionTimestamp.IsZero() {
			continue
		}
		r := Route{
			Model: model, Namespace: s.Namespace, Service: s.Name, Upstream: strings.TrimRight(s.Status.Endpoint, "/"),
			ServedModel: s.Annotations[AnnotationServedModel], Shared: s.Annotations[AnnotationShared] == "true",
			PriceIn: price(s.Annotations[AnnotationPriceIn], defIn), PriceOut: price(s.Annotations[AnnotationPriceOut], defOut),
			ScaleToZero: s.Spec.ScaleToZero != nil && s.Spec.ScaleToZero.Enabled,
			Idle:        s.Spec.ScaleToZero.Idle(), ColdStart: s.Spec.ScaleToZero.ColdStart(), Phase: s.Status.Phase,
		}
		if r.ServedModel == "" {
			r.ServedModel = model
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return out[i].Namespace < out[j].Namespace
	})
	return out
}

func price(v string, def float64) float64 {
	p, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || p < 0 {
		return def
	}
	return p
}

// CanUse reports whether the key may call the route: same namespace, or a shared model.
func (k Key) CanUse(r Route) bool { return r.Namespace == k.Namespace || r.Shared }

// pick resolves a model name for a key: the key's own namespace first, then shared models (by namespace).
func pick(routes []Route, k Key, model string) (Route, bool) {
	var shared *Route
	for i := range routes {
		r := &routes[i]
		if r.Model != model || !k.CanUse(*r) {
			continue
		}
		if r.Namespace == k.Namespace {
			return *r, true
		}
		if shared == nil {
			shared = r
		}
	}
	if shared != nil {
		return *shared, true
	}
	return Route{}, false
}

// visible is the distinct model names a key may call.
func visible(routes []Route, k Key) []Route {
	seen := map[string]bool{}
	var out []Route
	for _, r := range routes {
		if k.CanUse(r) && !seen[r.Model] {
			if best, ok := pick(routes, k, r.Model); ok {
				out = append(out, best)
				seen[r.Model] = true
			}
		}
	}
	return out
}
