// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

// Package reservation is the AI operator's side of GryviaReservation: how a job that names a
// reservation (annotation or label gryvia.io/reservation) is allowed onto the reservation's
// tainted nodes. The quota operator owns the reservation, the node taint gryvia.io/reserved
// and the label gryvia.io/reserved-for=<reservation name>; this package only reads them
// (as unstructured objects, the quota operator being a separate module).
package reservation

import (
	"context"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// Key is the annotation (or label) on a GryviaAIJob naming the reservation to use.
	Key = "gryvia.io/reservation"
	// TaintKey is the NoSchedule taint on reserved nodes.
	TaintKey = "gryvia.io/reserved"
	// NodeLabel holds the reservation name on a reserved node.
	NodeLabel = "gryvia.io/reserved-for"

	tenantPrefix = "tenant-"
	teamLabel    = "gryvia.io/team"
)

var reservationGVK = schema.GroupVersionKind{Group: "gryvia.io", Version: "v1alpha1", Kind: "GryviaReservation"}

// Info is what the gate needs to know about a reservation.
type Info struct {
	Name      string
	OwnerType string
	OwnerName string
	State     string
}

// Named returns the reservation a job asks for ("" when none): annotation first, then label.
func Named(annotations, labels map[string]string) string {
	if v := strings.TrimSpace(annotations[Key]); v != "" {
		return v
	}
	return strings.TrimSpace(labels[Key])
}

// Get reads the reservation. found is false when it does not exist.
func Get(ctx context.Context, c client.Client, name string) (Info, bool, error) {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(reservationGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: name}, u); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return Info{}, false, nil
		}
		return Info{}, false, err
	}
	in := Info{Name: name}
	in.OwnerType, _, _ = unstructured.NestedString(u.Object, "spec", "owner", "type")
	in.OwnerName, _, _ = unstructured.NestedString(u.Object, "spec", "owner", "name")
	in.State, _, _ = unstructured.NestedString(u.Object, "status", "state")
	return in, true, nil
}

// Usable reports whether jobs may still be pointed at the reservation: expired and cancelled
// ones no longer hold nodes, so a nodeSelector on them could never be satisfied.
func (i Info) Usable() bool { return i.State != "expired" && i.State != "cancelled" }

// Identity is who a job belongs to: its namespace and the team of that namespace (label
// gryvia.io/team on the job or the namespace).
type Identity struct {
	Namespace string
	Team      string
}

// Tenant is the tenant name when the namespace is tenant-<name>, else "".
func (id Identity) Tenant() string {
	if strings.HasPrefix(id.Namespace, tenantPrefix) {
		return strings.TrimPrefix(id.Namespace, tenantPrefix)
	}
	return ""
}

// TeamOf resolves the team of a job: its own label, else its namespace's label.
func TeamOf(ctx context.Context, c client.Client, namespace string, jobLabels map[string]string) string {
	if t := jobLabels[teamLabel]; t != "" {
		return t
	}
	ns := &corev1.Namespace{}
	if err := c.Get(ctx, types.NamespacedName{Name: namespace}, ns); err == nil {
		return ns.Labels[teamLabel]
	}
	return ""
}

// MayUse reports whether a job of identity id may use the reservation: the owner must be the
// job's tenant (owner type tenant or team, name = tenant, with or without the tenant-
// prefix), its namespace (type namespace) or its team (type team). Other owner types (user,
// project) cannot be attributed to a job and never match.
func (i Info) MayUse(id Identity) bool {
	name := i.OwnerName
	if name == "" {
		return false
	}
	tenant := id.Tenant()
	switch strings.ToLower(i.OwnerType) {
	case "tenant":
		return (tenant != "" && (name == tenant || name == tenantPrefix+tenant)) || (id.Team != "" && name == id.Team)
	case "namespace":
		return name == id.Namespace
	case "team":
		return (id.Team != "" && name == id.Team) || (tenant != "" && name == tenant)
	}
	return false
}

var valueInvalid = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

// OwnerValue turns an owner name into the taint value the quota operator sets
// (controllers.ReservationOwnerValue there): invalid characters become "-", at most 63
// characters, alphanumeric at both ends. Keep the two in step.
func OwnerValue(name string) string {
	v := valueInvalid.ReplaceAllString(name, "-")
	if len(v) > 63 {
		v = v[:63]
	}
	return strings.Trim(v, "-_.")
}

// Apply adds what a job needs to run on the reservation's nodes to a pod spec: a toleration
// for the reservation's taint and a nodeSelector on the reservation label. It copies
// the toleration slice (the job's own spec is never mutated).
func Apply(spec *corev1.PodSpec, in Info) {
	tols := make([]corev1.Toleration, 0, len(spec.Tolerations)+1)
	for _, t := range spec.Tolerations {
		if t.Key == TaintKey {
			continue // the reservation's toleration replaces a hand-written one
		}
		tols = append(tols, t)
	}
	spec.Tolerations = append(tols, corev1.Toleration{
		Key: TaintKey, Operator: corev1.TolerationOpEqual, Value: OwnerValue(in.OwnerName), Effect: corev1.TaintEffectNoSchedule,
	})
	ns := make(map[string]string, len(spec.NodeSelector)+1)
	for k, v := range spec.NodeSelector {
		ns[k] = v
	}
	ns[NodeLabel] = in.Name
	spec.NodeSelector = ns
}
