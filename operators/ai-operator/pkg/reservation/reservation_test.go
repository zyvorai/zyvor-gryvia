// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package reservation

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestNamed(t *testing.T) {
	if Named(map[string]string{Key: " r1 "}, map[string]string{Key: "r2"}) != "r1" {
		t.Error("annotation wins and is trimmed")
	}
	if Named(nil, map[string]string{Key: "r2"}) != "r2" {
		t.Error("label fallback")
	}
	if Named(nil, nil) != "" {
		t.Error("none")
	}
}

func TestMayUse(t *testing.T) {
	cases := []struct {
		name string
		info Info
		id   Identity
		want bool
	}{
		{"tenant owner", Info{OwnerType: "tenant", OwnerName: "acme"}, Identity{Namespace: "tenant-acme"}, true},
		{"tenant owner with prefix", Info{OwnerType: "tenant", OwnerName: "tenant-acme"}, Identity{Namespace: "tenant-acme"}, true},
		{"other tenant", Info{OwnerType: "tenant", OwnerName: "acme"}, Identity{Namespace: "tenant-evil"}, false},
		{"team owner by tenant name", Info{OwnerType: "team", OwnerName: "acme"}, Identity{Namespace: "tenant-acme"}, true},
		{"team owner by label", Info{OwnerType: "team", OwnerName: "vision"}, Identity{Namespace: "ml-prod", Team: "vision"}, true},
		{"team owner wrong team", Info{OwnerType: "team", OwnerName: "vision"}, Identity{Namespace: "ml-prod", Team: "nlp"}, false},
		{"team owner no team", Info{OwnerType: "team", OwnerName: "vision"}, Identity{Namespace: "ml-prod"}, false},
		{"namespace owner", Info{OwnerType: "namespace", OwnerName: "ml-prod"}, Identity{Namespace: "ml-prod"}, true},
		{"namespace owner other ns", Info{OwnerType: "namespace", OwnerName: "ml-prod"}, Identity{Namespace: "ml-dev"}, false},
		{"user owner never matches", Info{OwnerType: "user", OwnerName: "alice"}, Identity{Namespace: "alice"}, false},
		{"empty owner", Info{OwnerType: "team"}, Identity{Namespace: "x", Team: ""}, false},
		{"plain namespace is not a tenant", Info{OwnerType: "tenant", OwnerName: "prod"}, Identity{Namespace: "prod"}, false},
	}
	for _, c := range cases {
		if got := c.info.MayUse(c.id); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestOwnerValue(t *testing.T) {
	// Must equal quota-operator controllers.ReservationOwnerValue for the same input.
	for in, want := range map[string]string{"ml-team": "ml-team", "team/a b": "team-a-b", "--x--": "x", "Alice@example.com": "Alice-example.com"} {
		if got := OwnerValue(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestApplyAddsTolerationAndSelectorWithoutMutatingInput(t *testing.T) {
	own := []corev1.Toleration{{Key: "nvidia.com/gpu", Operator: corev1.TolerationOpExists}, {Key: TaintKey, Value: "stale"}}
	sel := map[string]string{"zone": "a"}
	spec := &corev1.PodSpec{Tolerations: own, NodeSelector: sel}
	Apply(spec, Info{Name: "r1", OwnerName: "ml team"})

	if len(spec.Tolerations) != 2 || spec.Tolerations[0].Key != "nvidia.com/gpu" {
		t.Fatalf("tolerations = %+v", spec.Tolerations)
	}
	got := spec.Tolerations[1]
	if got.Key != TaintKey || got.Value != "ml-team" || got.Operator != corev1.TolerationOpEqual || got.Effect != corev1.TaintEffectNoSchedule {
		t.Errorf("toleration = %+v", got)
	}
	if spec.NodeSelector[NodeLabel] != "r1" || spec.NodeSelector["zone"] != "a" {
		t.Errorf("selector = %v", spec.NodeSelector)
	}
	if len(own) != 2 || own[1].Value != "stale" || len(sel) != 1 {
		t.Error("the job's own tolerations and nodeSelector must not be mutated")
	}
}

func TestUsable(t *testing.T) {
	for state, want := range map[string]bool{"": true, "pending": true, "active": true, "expired": false, "cancelled": false} {
		if (Info{State: state}).Usable() != want {
			t.Errorf("%q", state)
		}
	}
}
