// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controllers

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gryviav1 "github.com/zyvorai/gryvia/operators/quota-operator/api/v1"
)

func rbacTenant(members ...gryviav1.TenantMember) *gryviav1.GryviaTenant {
	return &gryviav1.GryviaTenant{
		ObjectMeta: metav1.ObjectMeta{Name: "a"},
		Spec:       gryviav1.GryviaTenantSpec{Members: members},
	}
}

func mem(user, role string) gryviav1.TenantMember {
	return gryviav1.TenantMember{Username: user, Role: role}
}

func rbacReconciler(objs ...client.Object) (*GryviaTenantReconciler, client.Client) {
	s := runtime.NewScheme()
	_ = gryviav1.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	_ = rbacv1.AddToScheme(s)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	return &GryviaTenantReconciler{Client: c, Scheme: s, TenantRBAC: true}, c
}

func listBindings(t *testing.T, c client.Client) map[string]rbacv1.RoleBinding {
	t.Helper()
	l := &rbacv1.RoleBindingList{}
	if err := c.List(context.Background(), l, client.InNamespace("tenant-a")); err != nil {
		t.Fatal(err)
	}
	out := map[string]rbacv1.RoleBinding{}
	for _, rb := range l.Items {
		out[rb.Name] = rb
	}
	return out
}

func subjectNames(rb rbacv1.RoleBinding) []string {
	var out []string
	for _, s := range rb.Subjects {
		out = append(out, s.Kind+":"+s.Name)
	}
	return out
}

func TestTenantRBAC_BindingsPerRole(t *testing.T) {
	r, c := rbacReconciler()
	tn := rbacTenant(mem("bob", "member"), mem("alice", "admin"), mem("carol", "viewer"), mem("dave", "member"))
	inv, err := r.reconcileTenantRBAC(context.Background(), tn, "tenant-a")
	if err != nil || len(inv) != 0 {
		t.Fatalf("err=%v invalid=%v", err, inv)
	}
	got := listBindings(t, c)
	if len(got) != 3 {
		t.Fatalf("bindings = %v", got)
	}
	m := got["gryvia-tenant-member"]
	if m.RoleRef.Kind != "ClusterRole" || m.RoleRef.Name != "gryvia-tenant-member" || m.RoleRef.APIGroup != "rbac.authorization.k8s.io" {
		t.Errorf("roleRef = %+v", m.RoleRef)
	}
	if s := subjectNames(m); len(s) != 2 || s[0] != "User:bob" || s[1] != "User:dave" {
		t.Errorf("member subjects = %v", s)
	}
	if m.Subjects[0].APIGroup != "rbac.authorization.k8s.io" {
		t.Errorf("subject apiGroup = %q", m.Subjects[0].APIGroup)
	}
	if s := subjectNames(got["gryvia-tenant-admin"]); len(s) != 1 || s[0] != "User:alice" {
		t.Errorf("admin subjects = %v", s)
	}
	if got["gryvia-tenant-viewer"].Labels["gryvia.io/managed-by"] != "tenant-controller" || got["gryvia-tenant-viewer"].Labels["gryvia.io/tenant"] != "a" {
		t.Errorf("labels = %v", got["gryvia-tenant-viewer"].Labels)
	}
}

func TestTenantRBAC_UnknownAndEmptyRoleIsViewer(t *testing.T) {
	r, c := rbacReconciler()
	inv, err := r.reconcileTenantRBAC(context.Background(), rbacTenant(mem("x", ""), mem("y", "superuser"), mem("z", "Admin")), "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(inv) != 2 {
		t.Errorf("invalid = %v", inv)
	}
	got := listBindings(t, c)
	if s := subjectNames(got["gryvia-tenant-viewer"]); len(s) != 2 || s[0] != "User:x" || s[1] != "User:y" {
		t.Errorf("viewer = %v", s)
	}
	if s := subjectNames(got["gryvia-tenant-admin"]); len(s) != 1 || s[0] != "User:z" {
		t.Errorf("role matching is case-insensitive: %v", s)
	}
	if _, ok := got["gryvia-tenant-member"]; ok {
		t.Error("nobody is a member: no member binding")
	}
}

func TestTenantRBAC_OIDCGroups(t *testing.T) {
	r, c := rbacReconciler()
	tn := rbacTenant(mem("alice", "admin"))
	tn.Spec.OIDCGroups = []string{"ml-team", "ml-team", " ", "data"}
	if _, err := r.reconcileTenantRBAC(context.Background(), tn, "tenant-a"); err != nil {
		t.Fatal(err)
	}
	m := listBindings(t, c)["gryvia-tenant-member"]
	if s := subjectNames(m); len(s) != 2 || s[0] != "Group:data" || s[1] != "Group:ml-team" {
		t.Errorf("groups default to member: %v", s)
	}
	tn.Spec.OIDCGroupRole = "viewer"
	if _, err := r.reconcileTenantRBAC(context.Background(), tn, "tenant-a"); err != nil {
		t.Fatal(err)
	}
	got := listBindings(t, c)
	if _, ok := got["gryvia-tenant-member"]; ok {
		t.Error("member binding must go once the groups are viewers")
	}
	if s := subjectNames(got["gryvia-tenant-viewer"]); len(s) != 2 || s[0] != "Group:data" {
		t.Errorf("viewer groups: %v", s)
	}
}

func TestTenantRBAC_StaleBindingsRemovedAndForeignKept(t *testing.T) {
	foreign := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "custom", Namespace: "tenant-a"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "view"},
	}
	r, c := rbacReconciler(foreign)
	ctx := context.Background()
	tn := rbacTenant(mem("alice", "admin"), mem("bob", "member"))
	if _, err := r.reconcileTenantRBAC(ctx, tn, "tenant-a"); err != nil {
		t.Fatal(err)
	}
	// bob leaves, carol joins as a viewer, alice is demoted to member.
	tn.Spec.Members = []gryviav1.TenantMember{mem("carol", "viewer"), mem("alice", "member")}
	if _, err := r.reconcileTenantRBAC(ctx, tn, "tenant-a"); err != nil {
		t.Fatal(err)
	}
	got := listBindings(t, c)
	if _, ok := got["gryvia-tenant-admin"]; ok {
		t.Error("stale admin binding must be deleted")
	}
	if s := subjectNames(got["gryvia-tenant-member"]); len(s) != 1 || s[0] != "User:alice" {
		t.Errorf("member = %v", s)
	}
	if _, ok := got["custom"]; !ok {
		t.Error("a RoleBinding not managed by the controller must never be touched")
	}
	// No members at all: everything managed goes.
	tn.Spec.Members = nil
	if _, err := r.reconcileTenantRBAC(ctx, tn, "tenant-a"); err != nil {
		t.Fatal(err)
	}
	if got := listBindings(t, c); len(got) != 1 {
		t.Errorf("only the foreign binding should remain: %v", got)
	}
}

func TestTenantRBAC_DuplicateUserKeepsHighestRole(t *testing.T) {
	r, c := rbacReconciler()
	if _, err := r.reconcileTenantRBAC(context.Background(), rbacTenant(mem("u", "viewer"), mem("u", "admin"), mem("u", "member"), mem("", "admin")), "tenant-a"); err != nil {
		t.Fatal(err)
	}
	got := listBindings(t, c)
	if len(got) != 1 || len(got["gryvia-tenant-admin"].Subjects) != 1 {
		t.Errorf("bindings = %v", got)
	}
}

func TestTenantRBAC_DoesNotOverwriteForeignBindingWithSameName(t *testing.T) {
	foreign := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "gryvia-tenant-member", Namespace: "tenant-a"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"},
		Subjects:   []rbacv1.Subject{{Kind: "User", Name: "evil", APIGroup: rbacv1.GroupName}},
	}
	r, c := rbacReconciler(foreign)
	if _, err := r.reconcileTenantRBAC(context.Background(), rbacTenant(mem("bob", "member")), "tenant-a"); err == nil {
		t.Error("must refuse to adopt an unmanaged binding")
	}
	rb := &rbacv1.RoleBinding{}
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "tenant-a", Name: "gryvia-tenant-member"}, rb)
	if rb.RoleRef.Name != "cluster-admin" {
		t.Error("foreign binding was modified")
	}
}

func TestTenantRBAC_IdempotentAndFullReconcileSetsCondition(t *testing.T) {
	tn := rbacTenant(mem("bob", "wizard"))
	tn.Finalizers = []string{gryviaTenantFinalizer}
	r, c := rbacReconciler(tn)
	for i := 0; i < 2; i++ {
		if _, err := r.reconcileTenant(context.Background(), tn); err != nil {
			t.Fatal(err)
		}
	}
	if got := listBindings(t, c); len(got) != 1 {
		t.Fatalf("bindings = %v", got)
	}
	var cond *metav1.Condition
	for i := range tn.Status.Conditions {
		if tn.Status.Conditions[i].Type == "RBACReady" {
			cond = &tn.Status.Conditions[i]
		}
	}
	if cond == nil || cond.Reason != "UnknownRole" {
		t.Errorf("RBACReady = %+v", cond)
	}
	ns := &corev1.Namespace{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "tenant-a"}, ns); err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
}

func TestTenantRBAC_OffByDefault(t *testing.T) {
	tn := rbacTenant(mem("bob", "member"))
	r, c := rbacReconciler(tn)
	r.TenantRBAC = false
	if _, err := r.reconcileTenant(context.Background(), tn); err != nil {
		t.Fatal(err)
	}
	if got := listBindings(t, c); len(got) != 0 {
		t.Errorf("no RoleBindings unless --tenant-rbac: %v", got)
	}
	for _, cond := range tn.Status.Conditions {
		if cond.Type == "RBACReady" {
			t.Errorf("no RBACReady condition when disabled: %+v", cond)
		}
	}
}
