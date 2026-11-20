// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package webhook

import (
	"fmt"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

const (
	// ValidatingWebhookPath is the HTTP path for the validating webhook.
	ValidatingWebhookPath = "/validate-gryvia-ai-v1-gryviaaijob"

	// MutatingWebhookPath is the HTTP path for the mutating webhook.
	MutatingWebhookPath = "/mutate-gryvia-ai-v1-gryviaaijob"

	// DefaultWebhookPort is the default port for the webhook server.
	DefaultWebhookPort = 9443

	// DefaultCertDir is the default directory for TLS certificates.
	// cert-manager or the operator's deployment should provision the certs.
	DefaultCertDir = "/tmp/k8s-webhook-server/serving-certs"
)

// WebhookConfig holds configuration for the webhook server.
type WebhookConfig struct {
	// Port is the port to listen on. Defaults to 9443.
	Port int
	// CertDir is the directory containing tls.crt and tls.key.
	CertDir string
}

// SetupWebhooks registers the validating and mutating webhooks with the
// controller-runtime manager. The manager handles TLS termination and
// certificate management.
//
// The caller must ensure that cert-manager (or equivalent) provisions
// a TLS certificate to the CertDir before the manager starts. A typical
// setup uses a Certificate + Issuer pair:
//
//	apiVersion: cert-manager.io/v1
//	kind: Certificate
//	metadata:
//	  name: ai-operator-webhook-cert
//	spec:
//	  secretName: ai-operator-webhook-cert
//	  dnsNames:
//	  - ai-operator-webhook.gryvia-system.svc
//	  - ai-operator-webhook.gryvia-system.svc.cluster.local
//	  issuerRef:
//	    name: gryvia-ca-issuer
//	    kind: ClusterIssuer
func SetupWebhooks(mgr ctrl.Manager, config WebhookConfig) error {
	log := ctrl.Log.WithName("webhook").WithName("setup")

	if config.Port == 0 {
		config.Port = DefaultWebhookPort
	}
	if config.CertDir == "" {
		config.CertDir = DefaultCertDir
	}

	c := mgr.GetClient()

	// Register the validating webhook.
	if err := registerValidatingWebhook(mgr, c, log); err != nil {
		return fmt.Errorf("failed to register validating webhook: %w", err)
	}

	// Register the mutating webhook.
	if err := registerMutatingWebhook(mgr, c, log); err != nil {
		return fmt.Errorf("failed to register mutating webhook: %w", err)
	}

	log.Info("Webhook server configured",
		"port", config.Port,
		"certDir", config.CertDir,
		"validatingPath", ValidatingWebhookPath,
		"mutatingPath", MutatingWebhookPath,
	)

	return nil
}

// registerValidatingWebhook creates and registers the validating webhook handler.
func registerValidatingWebhook(mgr ctrl.Manager, c client.Client, log logr.Logger) error {
	validator := NewGryviaAIJobValidator(c)

	hookServer := mgr.GetWebhookServer()
	hookServer.Register(ValidatingWebhookPath, &webhook.Admission{
		Handler: validator,
	})

	log.Info("Registered validating webhook", "path", ValidatingWebhookPath)
	return nil
}

// registerMutatingWebhook creates and registers the mutating webhook handler.
func registerMutatingWebhook(mgr ctrl.Manager, c client.Client, log logr.Logger) error {
	mutator := NewGryviaAIJobMutator(c)

	hookServer := mgr.GetWebhookServer()
	hookServer.Register(MutatingWebhookPath, &webhook.Admission{
		Handler: mutator,
	})

	log.Info("Registered mutating webhook", "path", MutatingWebhookPath)
	return nil
}

// NewWebhookServer creates a standalone webhook server with TLS configuration.
// This is useful for testing or running the webhooks outside the controller
// manager.
func NewWebhookServer(config WebhookConfig) *webhook.DefaultServer {
	if config.Port == 0 {
		config.Port = DefaultWebhookPort
	}
	if config.CertDir == "" {
		config.CertDir = DefaultCertDir
	}

	return &webhook.DefaultServer{
		Options: webhook.Options{
			Port:    config.Port,
			CertDir: config.CertDir,
		},
	}
}
