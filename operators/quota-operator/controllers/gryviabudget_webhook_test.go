// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gryviav1 "github.com/zyvorai/gryvia/operators/quota-operator/api/v1"
	"github.com/zyvorai/gryvia/operators/quota-operator/pkg/notify"
)

func alertBudget() *gryviav1.GryviaBudget {
	return &gryviav1.GryviaBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "limit", UID: "b"},
		Spec:       gryviav1.GryviaBudgetSpec{Alerts: []gryviav1.BudgetAlert{{Threshold: 80, Recipients: []string{"finops@example.com"}}}},
		Status: gryviav1.GryviaBudgetStatus{Alerts: []gryviav1.BudgetAlertEvent{
			{Timestamp: metav1.NewTime(time.Unix(1700000000, 0)), Threshold: 80, Message: "crossed 80%"}}},
	}
}

// A new alert is delivered once, signed, with the matching recipients; reconciling again does not resend.
func TestBudgetAlertWebhookDeliversOnceAndIsSigned(t *testing.T) {
	var hits int32
	var body []byte
	var sig string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		body, _ = io.ReadAll(r.Body)
		sig = r.Header.Get("X-Gryvia-Signature")
	}))
	defer srv.Close()
	c := fake.NewClientBuilder().WithScheme(newQuotaTestScheme()).Build()
	r := &GryviaBudgetReconciler{Client: c, Webhook: &notify.Webhook{URL: srv.URL, Secret: "k"}}
	for i := 0; i < 3; i++ {
		if err := r.publishAlerts(context.Background(), alertBudget()); err != nil {
			t.Fatal(err)
		}
	}
	if hits != 1 {
		t.Fatalf("webhook called %d times, want 1", hits)
	}
	var p budgetAlertPayload
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	if p.Type != "budget.threshold" || p.Budget != "limit" || p.Threshold != 80 || len(p.Recipients) != 1 || p.Recipients[0] != "finops@example.com" {
		t.Errorf("payload = %+v", p)
	}
	if sig == "" {
		t.Error("request was not signed")
	}
	ev := &corev1.EventList{}
	if err := c.List(context.Background(), ev); err != nil || len(ev.Items) != 1 || ev.Items[0].Annotations[annotationWebhookDelivered] != "true" {
		t.Errorf("event not marked delivered: %v %+v", err, ev.Items)
	}
}

// A failed delivery is an error (so the budget is retried) and is re-sent next time, then marked delivered.
func TestBudgetAlertWebhookRetriesAfterFailure(t *testing.T) {
	var hits int32
	var healthy atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if !healthy.Load() {
			rw.WriteHeader(http.StatusBadGateway)
		}
	}))
	defer srv.Close()
	c := fake.NewClientBuilder().WithScheme(newQuotaTestScheme()).Build()
	r := &GryviaBudgetReconciler{Client: c, Webhook: &notify.Webhook{URL: srv.URL}}
	if err := r.publishAlerts(context.Background(), alertBudget()); err == nil {
		t.Fatal("a failed delivery must be reported")
	}
	healthy.Store(true)
	if err := r.publishAlerts(context.Background(), alertBudget()); err != nil {
		t.Fatal(err)
	}
	if err := r.publishAlerts(context.Background(), alertBudget()); err != nil {
		t.Fatal(err)
	}
	if hits != 2 {
		t.Errorf("hits = %d, want 2 (one failure, one success, none after)", hits)
	}
}

// Without a webhook the behaviour is unchanged: Events only, no annotation.
func TestBudgetAlertsWithoutWebhookOnlyCreateEvents(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newQuotaTestScheme()).Build()
	r := &GryviaBudgetReconciler{Client: c}
	if err := r.publishAlerts(context.Background(), alertBudget()); err != nil {
		t.Fatal(err)
	}
	ev := &corev1.EventList{}
	_ = c.List(context.Background(), ev)
	if len(ev.Items) != 1 || ev.Items[0].Annotations[annotationWebhookDelivered] != "" {
		t.Errorf("unexpected events: %+v", ev.Items)
	}
}
