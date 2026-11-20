// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controllers

import (
	"context"
	"fmt"
	"sort"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gryviav1 "github.com/zyvorai/gryvia/operators/quota-operator/api/v1"
)

const (
	// Tenant roles. Each maps to the ClusterRole gryvia-tenant-<role> shipped by the Helm chart
	// (helm/gryvia/templates/tenant-rbac.yaml); the operator only binds them, it never creates them.
	tenantRoleViewer = "viewer"
	tenantRoleMember = "member"
	tenantRoleAdmin  = "admin"

	tenantClusterRolePrefix = "gryvia-tenant-"
	// labelRBACManagedBy marks the RoleBindings this controller owns; only those are ever pruned.
	labelRBACManagedBy    = "gryvia.io/managed-by"
	valueTenantController = "tenant-controller"
)

//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=bind,resourceNames=gryvia-tenant-viewer;gryvia-tenant-member;gryvia-tenant-admin

var tenantRoleRank = map[string]int{tenantRoleViewer: 1, tenantRoleMember: 2, tenantRoleAdmin: 3}

// tenantRoleBindingName is the name of the RoleBinding for a role in the tenant namespace.
func tenantRoleBindingName(role string) string { return tenantClusterRolePrefix + role }

// desiredTenantBindings computes the RoleBindings a tenant should have in its namespace, one
// per role that has at least one subject (users from spec.members, groups from spec.oidcGroups).
// An empty or unknown role becomes viewer (least privilege) and is reported in invalid. A user
// listed twice keeps the highest role. The result is deterministic (sorted subjects).
func desiredTenantBindings(tenant *gryviav1.GryviaTenant, namespace string) (bindings []*rbacv1.RoleBinding, invalid []string) {
	userRole := map[string]string{}
	for _, m := range tenant.Spec.Members {
		name := strings.TrimSpace(m.Username)
		if name == "" {
			continue
		}
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if _, ok := tenantRoleRank[role]; !ok {
			invalid = append(invalid, fmt.Sprintf("%s: role %q", name, m.Role))
			role = tenantRoleViewer
		}
		if tenantRoleRank[role] > tenantRoleRank[userRole[name]] {
			userRole[name] = role
		}
	}
	subjects := map[string][]rbacv1.Subject{}
	for user, role := range userRole {
		subjects[role] = append(subjects[role], rbacv1.Subject{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: user})
	}
	if len(tenant.Spec.OIDCGroups) > 0 {
		role := strings.ToLower(tenant.Spec.OIDCGroupRole)
		if _, ok := tenantRoleRank[role]; !ok {
			if tenant.Spec.OIDCGroupRole != "" {
				invalid = append(invalid, fmt.Sprintf("oidcGroupRole %q", tenant.Spec.OIDCGroupRole))
			}
			role = tenantRoleMember
		}
		seen := map[string]bool{}
		for _, g := range tenant.Spec.OIDCGroups {
			g = strings.TrimSpace(g)
			if g == "" || seen[g] {
				continue
			}
			seen[g] = true
			subjects[role] = append(subjects[role], rbacv1.Subject{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: g})
		}
	}
	sort.Strings(invalid)
	for _, role := range []string{tenantRoleViewer, tenantRoleMember, tenantRoleAdmin} {
		subs := subjects[role]
		if len(subs) == 0 {
			continue
		}
		sort.Slice(subs, func(i, j int) bool {
			if subs[i].Kind != subs[j].Kind {
				return subs[i].Kind < subs[j].Kind
			}
			return subs[i].Name < subs[j].Name
		})
		bindings = append(bindings, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:      tenantRoleBindingName(role),
				Namespace: namespace,
				Labels: map[string]string{
					labelRBACManagedBy: valueTenantController,
					labelTenant:        tenant.Name,
				},
			},
			RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: tenantClusterRolePrefix + role},
			Subjects: subs,
		})
	}
	return bindings, invalid
}

// reconcileTenantRBAC creates and updates the tenant's RoleBindings and deletes managed
// bindings that are no longer wanted (a member removed, a role emptied). It only ever touches
// RoleBindings labelled gryvia.io/managed-by=tenant-controller. Kubernetes RBAC escalation
// rules apply to the operator: it holds the `bind` verb on the three gryvia-tenant-* ClusterRoles
// only, so it can grant exactly those and nothing else.
func (r *GryviaTenantReconciler) reconcileTenantRBAC(ctx context.Context, tenant *gryviav1.GryviaTenant, namespace string) (invalid []string, err error) {
	want, invalid := desiredTenantBindings(tenant, namespace)
	wanted := map[string]bool{}
	for _, rb := range want {
		wanted[rb.Name] = true
		cur := &rbacv1.RoleBinding{}
		err := r.Get(ctx, client.ObjectKeyFromObject(rb), cur)
		switch {
		case errors.IsNotFound(err):
			if err := r.Create(ctx, rb); err != nil {
				return invalid, fmt.Errorf("create RoleBinding %s: %w", rb.Name, err)
			}
		case err != nil:
			return invalid, err
		default:
			if cur.Labels[labelRBACManagedBy] != valueTenantController {
				return invalid, fmt.Errorf("RoleBinding %s/%s exists and is not managed by the tenant controller; not overwriting it", cur.Namespace, cur.Name)
			}
			if !equality.Semantic.DeepEqual(cur.Subjects, rb.Subjects) || !equality.Semantic.DeepEqual(cur.RoleRef, rb.RoleRef) ||
				cur.Labels[labelTenant] != rb.Labels[labelTenant] {
				if !equality.Semantic.DeepEqual(cur.RoleRef, rb.RoleRef) { // roleRef is immutable
					if err := r.Delete(ctx, cur); err != nil {
						return invalid, err
					}
					if err := r.Create(ctx, rb); err != nil {
						return invalid, err
					}
					continue
				}
				cur.Subjects = rb.Subjects
				if cur.Labels == nil {
					cur.Labels = map[string]string{}
				}
				cur.Labels[labelTenant] = rb.Labels[labelTenant]
				if err := r.Update(ctx, cur); err != nil {
					return invalid, fmt.Errorf("update RoleBinding %s: %w", rb.Name, err)
				}
			}
		}
	}

	all := &rbacv1.RoleBindingList{}
	if err := r.List(ctx, all, client.InNamespace(namespace), client.MatchingLabels{labelRBACManagedBy: valueTenantController}); err != nil {
		return invalid, fmt.Errorf("list RoleBindings: %w", err)
	}
	for i := range all.Items {
		if !wanted[all.Items[i].Name] {
			if err := r.Delete(ctx, &all.Items[i]); err != nil && !errors.IsNotFound(err) {
				return invalid, fmt.Errorf("delete stale RoleBinding %s: %w", all.Items[i].Name, err)
			}
		}
	}
	return invalid, nil
}
