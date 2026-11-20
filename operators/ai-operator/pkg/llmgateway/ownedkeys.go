// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package llmgateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	// LabelKeyName, LabelKeyNamespace and LabelKeyOwner describe a key Secret (the api-gateway lists keys by them).
	LabelKeyName      = "gryvia.io/llm-key-name"
	LabelKeyNamespace = "gryvia.io/llm-key-namespace"
	LabelKeyOwner     = "gryvia.io/llm-key-owner"
	// KeyStore is the LabelKey value of a mirrored vector-store credential (not an API key).
	KeyStore = "store"
	// OwnedKeyField is the data key of the raw key in the owner's Secret.
	OwnedKeyField = "key"
)

// NewKey returns a fresh API key.
func NewKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "gk-" + hex.EncodeToString(b), nil
}

// TenantOf is the tenant a namespace's keys are attributed to (tenant-<name> belongs to tenant <name>).
func TenantOf(namespace string) string { return strings.TrimPrefix(namespace, "tenant-") }

// OwnedKeySecretName is the hashed key's Secret in the key namespace for an operator-owned key.
func OwnedKeySecretName(namespace, kind, name string) string {
	return fmt.Sprintf("%s.%s-%s", namespace, kind, name)
}

// EnsureOwnedKey gives an object (a vector index or an agent) its own gateway key: the raw key in Secret
// rawName of the owner's namespace (owned by it, so it goes with it) and its hash in the key namespace. An existing
// raw key is kept; a missing or stale hash Secret is (re)written. kind is a short lowercase word ("index",
// "agent") that keeps the hash Secret names of different owners apart.
func EnsureOwnedKey(ctx context.Context, c client.Client, scheme *runtime.Scheme, keyNamespace string,
	owner client.Object, kind, rawName string) error {
	ns := owner.GetNamespace()
	raw := &corev1.Secret{}
	err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: rawName}, raw)
	key := ""
	switch {
	case errors.IsNotFound(err):
		if key, err = NewKey(); err != nil {
			return err
		}
		raw = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: rawName, Namespace: ns, Labels: map[string]string{LabelKeyOwner: kind}},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{OwnedKeyField: []byte(key)},
		}
		if err := controllerutil.SetControllerReference(owner, raw, scheme); err != nil {
			return err
		}
		if err := c.Create(ctx, raw); err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		key = string(raw.Data[OwnedKeyField])
		if key == "" {
			if key, err = NewKey(); err != nil {
				return err
			}
			if raw.Data == nil {
				raw.Data = map[string][]byte{}
			}
			raw.Data[OwnedKeyField] = []byte(key)
			if err := c.Update(ctx, raw); err != nil {
				return err
			}
		}
	}

	hashName := OwnedKeySecretName(ns, kind, owner.GetName())
	labels := map[string]string{
		LabelKey: "true", LabelTenant: labelValue(TenantOf(ns)), LabelKeyName: labelValue(kind + "-" + owner.GetName()),
		LabelKeyNamespace: ns, LabelKeyOwner: kind,
	}
	data := map[string][]byte{"hash": []byte(HashKey(key)), "namespace": []byte(ns)}
	hashed := &corev1.Secret{}
	err = c.Get(ctx, types.NamespacedName{Namespace: keyNamespace, Name: hashName}, hashed)
	if errors.IsNotFound(err) {
		hashed = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: hashName, Namespace: keyNamespace, Labels: labels,
				Annotations: map[string]string{"gryvia.io/description": fmt.Sprintf("managed by %s %s/%s", kind, ns, owner.GetName())},
			},
			Type: corev1.SecretTypeOpaque,
			Data: data,
		}
		return c.Create(ctx, hashed)
	}
	if err != nil {
		return err
	}
	if string(hashed.Data["hash"]) == string(data["hash"]) && string(hashed.Data["namespace"]) == ns && hashed.Labels[LabelKey] == "true" {
		return nil
	}
	hashed.Data = data
	hashed.Labels = labels
	return c.Update(ctx, hashed)
}

// DeleteOwnedKey removes the hashed key (and an index's mirrored store credential) of an owner from the key namespace.
func DeleteOwnedKey(ctx context.Context, c client.Client, keyNamespace, namespace, kind, name string) error {
	names := []string{OwnedKeySecretName(namespace, kind, name)}
	if kind == "index" {
		names = append(names, StoreSecretName(namespace, name))
	}
	for _, n := range names {
		err := c.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: keyNamespace, Name: n}})
		if err != nil && !errors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// StoreSecretName is the key-namespace copy of an external vector store's API key, read by /v1/retrieve.
func StoreSecretName(namespace, index string) string {
	return fmt.Sprintf("%s.%s.vectorstore", namespace, index)
}

// MirrorStoreKey writes (or removes, when apiKey is empty) the key-namespace copy of an index's store API key.
func MirrorStoreKey(ctx context.Context, c client.Client, keyNamespace, namespace, index, apiKey string) error {
	name := StoreSecretName(namespace, index)
	cur := &corev1.Secret{}
	err := c.Get(ctx, types.NamespacedName{Namespace: keyNamespace, Name: name}, cur)
	if apiKey == "" {
		if err == nil {
			return client.IgnoreNotFound(c.Delete(ctx, cur))
		}
		return client.IgnoreNotFound(err)
	}
	if errors.IsNotFound(err) {
		return c.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: keyNamespace,
				Labels: map[string]string{LabelKey: KeyStore, LabelKeyNamespace: namespace}},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{"apiKey": []byte(apiKey)},
		})
	}
	if err != nil {
		return err
	}
	if string(cur.Data["apiKey"]) == apiKey {
		return nil
	}
	cur.Data = map[string][]byte{"apiKey": []byte(apiKey)}
	return c.Update(ctx, cur)
}
