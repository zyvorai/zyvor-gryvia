// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Helpers shared by the ML controllers (Workspace, InferenceService, ModelRegistry, Workflow, AutoTuner).

const (
	// maxDNSLabel is the longest name a Service (DNS-1035) or a label value may have.
	maxDNSLabel = 63

	// Annotation keys used by the ML controllers.
	annotationLastActivity = "gryvia.io/last-activity"
	annotationIdleAction   = "gryvia.io/idle-action"
)

// childName joins parts with "-" and keeps the result within 63 characters, so it is valid as a Service
// name and as a label value. A name that would be too long is truncated and gets a short hash of the full
// name appended, so two long names cannot collide.
func childName(parts ...string) string {
	full := strings.Join(parts, "-")
	if len(full) <= maxDNSLabel {
		return full
	}
	sum := sha256.Sum256([]byte(full))
	suffix := hex.EncodeToString(sum[:])[:8]
	head := strings.TrimRight(full[:maxDNSLabel-len(suffix)-1], "-.")
	return head + "-" + suffix
}

// patchStatus writes the status of obj as a merge patch against orig. A patch is used instead of an update so a
// concurrent change to the object (another status writer, a spec edit) cannot fail the write with a conflict.
// Nothing is sent when the status did not change.
func patchStatus(ctx context.Context, c client.Client, obj, orig client.Object) error {
	patch := client.MergeFrom(orig)
	data, err := patch.Data(obj)
	if err == nil && string(data) == "{}" {
		return nil
	}
	return c.Status().Patch(ctx, obj, patch)
}

// setCondition sets a condition, keeping LastTransitionTime while the status does not change.
func setCondition(conds *[]metav1.Condition, generation int64, condType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(conds, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: generation,
	})
}

// specHash is a short stable hash of any JSON-serialisable value, used to detect drift of generated objects.
func specHash(v interface{}) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

// clock returns f when set, else time.Now. Reconcilers expose a Clock field so tests can move time.
func clock(f func() time.Time) time.Time {
	if f != nil {
		return f()
	}
	return time.Now()
}
