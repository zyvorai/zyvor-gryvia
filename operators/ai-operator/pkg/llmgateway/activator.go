// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package llmgateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

// Activator writes the scale-to-zero annotations on a GryviaInferenceService.
type Activator interface {
	// Wake asks the controller to scale the service up (annotation gryvia.io/wake-requested).
	Wake(ctx context.Context, namespace, service string, at time.Time) error
	// Touch records a proxied request (annotation gryvia.io/last-request).
	Touch(ctx context.Context, namespace, service string, at time.Time) error
}

// KubeActivator patches the annotations through the API server.
type KubeActivator struct{ Client client.Client }

func (a *KubeActivator) annotate(ctx context.Context, namespace, service, key string, at time.Time) error {
	patch := []byte(fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, key, at.UTC().Format(time.RFC3339)))
	obj := &gryviav1.GryviaInferenceService{}
	obj.Namespace, obj.Name = namespace, service
	return a.Client.Patch(ctx, obj, client.RawPatch(types.MergePatchType, patch))
}

func (a *KubeActivator) Wake(ctx context.Context, namespace, service string, at time.Time) error {
	return a.annotate(ctx, namespace, service, gryviav1.AnnotationWakeRequested, at)
}

func (a *KubeActivator) Touch(ctx context.Context, namespace, service string, at time.Time) error {
	return a.annotate(ctx, namespace, service, gryviav1.AnnotationLastRequest, at)
}

const (
	defaultWakePoll = time.Second
	// A wake request is repeated at most this often per service while requests wait.
	wakeEvery = 5 * time.Second
	// Last-request annotations are written at most once a minute per service, and more often for short idle times.
	maxTouchEvery = time.Minute
	retryAfter    = 30
	// After a wake-up, refused connections are retried this long: the Service's endpoints can trail the ready replica.
	refusedRetry = 30 * time.Second
)

// throttle remembers when an action last ran per key.
type throttle struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// due reports whether the action for key may run at now, and if so records it.
func (t *throttle) due(key string, now time.Time, every time.Duration) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.last == nil {
		t.last = map[string]time.Time{}
	}
	if last, ok := t.last[key]; ok && now.Sub(last) < every {
		return false
	}
	t.last[key] = now
	return true
}

func touchEvery(idle time.Duration) time.Duration {
	if d := idle / 3; d > 0 && d < maxTouchEvery {
		return d
	}
	return maxTouchEvery
}

// awaitReady wakes a scaled-to-zero route and polls the routes until its service is serving or the cold-start
// timeout passes. It returns the ready route, or writes the 503 (or nothing, when the caller went away).
func (g *Gateway) awaitReady(w http.ResponseWriter, r *http.Request, k Key, model string, route Route) (Route, bool) {
	ctx := r.Context()
	deadline := g.now().Add(route.ColdStart)
	poll := g.WakePoll
	if poll <= 0 {
		poll = defaultWakePoll
	}
	id := route.Namespace + "/" + route.Service
	for {
		if route.Phase == PhaseScaledToZero && g.Activator != nil && g.wakes.due(id, g.now(), wakeEvery) {
			if err := g.Activator.Wake(ctx, route.Namespace, route.Service, g.now()); err != nil {
				g.Meter.Request(k.Tenant, model, http.StatusServiceUnavailable)
				w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
				WriteError(w, http.StatusServiceUnavailable, "api_error", fmt.Sprintf("model %q is scaled to zero and could not be woken", model))
				return Route{}, false
			}
		}
		if !g.now().Before(deadline) {
			g.Meter.Request(k.Tenant, model, http.StatusServiceUnavailable)
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			WriteError(w, http.StatusServiceUnavailable, "api_error",
				fmt.Sprintf("model %q is starting from zero and was not ready within %s; retry shortly", model, route.ColdStart))
			return Route{}, false
		}
		select {
		case <-ctx.Done():
			return Route{}, false
		case <-time.After(poll):
		}
		routes, err := g.Source.Routes(ctx)
		if err != nil {
			continue
		}
		next, ok := findRoute(routes, route.Namespace, route.Service)
		if !ok {
			g.Meter.Request(k.Tenant, model, http.StatusNotFound)
			WriteError(w, http.StatusNotFound, openAIErrorInvalid, fmt.Sprintf("model %q is no longer published", model))
			return Route{}, false
		}
		if !next.Waking() {
			return next, true
		}
		route = next
	}
}

// retryRefused resends a request whose connection was refused, until refusedRetry passes. A refused connection
// never reached the model, so resending cannot run it twice.
func (g *Gateway) retryRefused(ctx context.Context, resp *http.Response, err error, send func() (*http.Response, error)) (*http.Response, error) {
	poll := g.WakePoll
	if poll <= 0 {
		poll = defaultWakePoll
	}
	deadline := time.Now().Add(refusedRetry)
	for err != nil && errors.Is(err, syscall.ECONNREFUSED) && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return resp, err
		case <-time.After(poll):
		}
		resp, err = send()
	}
	return resp, err
}

func findRoute(routes []Route, namespace, service string) (Route, bool) {
	for _, rt := range routes {
		if rt.Namespace == namespace && rt.Service == service {
			return rt, true
		}
	}
	return Route{}, false
}

// touch records traffic on a scale-to-zero service, at most once per touchEvery(idle), without delaying the reply.
func (g *Gateway) touch(route Route) {
	if !route.ScaleToZero || g.Activator == nil {
		return
	}
	now := g.now()
	if !g.touches.due(route.Namespace+"/"+route.Service, now, touchEvery(route.Idle)) {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = g.Activator.Touch(ctx, route.Namespace, route.Service, now)
	}()
}
