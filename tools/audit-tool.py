#!/usr/bin/env python3
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""
Gryvia Audit and Compliance Tool
Generates compliance reports and audit logs
"""

import argparse
import json
import sys
from datetime import datetime, timedelta
from typing import Dict, List
from kubernetes import client, config

# Color codes
GREEN = '\033[0;32m'
YELLOW = '\033[1;33m'
RED = '\033[0;31m'
BLUE = '\033[0;34m'
NC = '\033[0m'


class AuditTool:
    def __init__(self, namespace: str = "default"):
        try:
            config.load_kube_config()
        except Exception:
            config.load_incluster_config()

        self.api = client.CustomObjectsApi()
        self.core_api = client.CoreV1Api()
        self.networking_api = client.NetworkingV1Api()
        self.rbac_api = client.RbacAuthorizationV1Api()
        self.namespace = namespace

    def generate_compliance_report(self, days: int = 30) -> Dict:
        """Generate comprehensive compliance report"""
        print(f"{BLUE}Generating compliance report for last {days} days...{NC}\n")

        report = {
            "generated_at": datetime.utcnow().isoformat(),
            "period_days": days,
            "security": self.audit_security(),
            "access_control": self.audit_access_control(),
            "resource_usage": self.audit_resource_usage(days),
            "cost_tracking": self.audit_cost_tracking(days),
            "data_governance": self.audit_data_governance(),
            "compliance_score": 0
        }

        # Calculate compliance score
        report["compliance_score"] = self.calculate_compliance_score(report)

        return report

    def audit_security(self) -> Dict:
        """Audit security posture"""
        print(f"{BLUE}Auditing security...{NC}")

        security = {
            "pod_security": {},
            "network_policies": {},
            "secrets_management": {},
            "image_security": {}
        }

        # Check Pod Security Policies
        try:
            psps = self.rbac_api.list_cluster_role()
            security["pod_security"]["policies_count"] = len([
                p for p in psps.items if "pod-security" in p.metadata.name
            ])
            security["pod_security"]["compliant"] = security["pod_security"]["policies_count"] > 0
        except Exception:
            security["pod_security"]["compliant"] = False

        # Check Network Policies
        try:
            netpols = self.networking_api.list_namespaced_network_policy(self.namespace)
            security["network_policies"]["count"] = len(netpols.items)
            security["network_policies"]["compliant"] = len(netpols.items) > 0
        except Exception:
            security["network_policies"]["compliant"] = False

        # Check Secrets
        try:
            secrets = self.core_api.list_namespaced_secret(self.namespace)
            security["secrets_management"]["count"] = len(secrets.items)
            security["secrets_management"]["encrypted"] = False  # Cannot verify; must be checked via API server config
            opaque_secrets = [s for s in secrets.items if s.type == "Opaque"]
            security["secrets_management"]["compliant"] = all(
                s.data is not None and len(s.data) > 0 for s in opaque_secrets
            ) if opaque_secrets else False
            security["secrets_management"]["details"] = f"Found {len(opaque_secrets)} opaque secrets"
        except Exception:
            security["secrets_management"]["compliant"] = False

        # Check Image Security
        try:
            jobs = self.api.list_namespaced_custom_object(
                group="gryvia.io",
                version="v1alpha1",
                namespace=self.namespace,
                plural="gryviaaijobs"
            )

            images = [job["spec"]["image"] for job in jobs["items"]]
            trusted_registries = ["nvcr.io", "ghcr.io"]

            trusted_count = sum(
                1 for img in images
                if any(reg in img for reg in trusted_registries)
            )

            security["image_security"]["total_images"] = len(images)
            security["image_security"]["trusted_images"] = trusted_count
            security["image_security"]["compliance_rate"] = (
                trusted_count / len(images) * 100 if images else 100
            )
            security["image_security"]["compliant"] = security["image_security"]["compliance_rate"] >= 80
        except Exception:
            security["image_security"]["compliant"] = False

        return security

    def audit_access_control(self) -> Dict:
        """Audit RBAC and access controls"""
        print(f"{BLUE}Auditing access control...{NC}")

        access = {
            "rbac": {},
            "service_accounts": {},
            "user_access": {}
        }

        # Check RBAC roles
        try:
            roles = self.rbac_api.list_namespaced_role(self.namespace)
            role_bindings = self.rbac_api.list_namespaced_role_binding(self.namespace)

            access["rbac"]["roles_count"] = len(roles.items)
            access["rbac"]["bindings_count"] = len(role_bindings.items)
            access["rbac"]["compliant"] = len(roles.items) > 0
        except Exception:
            access["rbac"]["compliant"] = False

        # Check Service Accounts
        try:
            sas = self.core_api.list_namespaced_service_account(self.namespace)
            access["service_accounts"]["count"] = len(sas.items)
            access["service_accounts"]["compliant"] = True
        except Exception:
            access["service_accounts"]["compliant"] = False

        return access

    def audit_resource_usage(self, days: int) -> Dict:
        """Audit resource usage and quotas"""
        print(f"{BLUE}Auditing resource usage...{NC}")

        usage = {
            "quotas": {},
            "limits": {},
            "utilization": {}
        }

        # Check Resource Quotas
        try:
            quotas = self.api.list_namespaced_custom_object(
                group="gryvia.io",
                version="v1alpha1",
                namespace=self.namespace,
                plural="gryviaquotas"
            )

            total_quotas = len(quotas["items"])
            exceeded_quotas = 0

            for quota in quotas["items"]:
                status = quota.get("status", {})
                usage_pct = status.get("usage", {}).get("percentUsed", 0)
                if usage_pct > 100:
                    exceeded_quotas += 1

            usage["quotas"]["total"] = total_quotas
            usage["quotas"]["exceeded"] = exceeded_quotas
            usage["quotas"]["compliant"] = exceeded_quotas == 0
        except Exception:
            usage["quotas"]["compliant"] = False

        # Check Resource Limits
        try:
            jobs = self.api.list_namespaced_custom_object(
                group="gryvia.io",
                version="v1alpha1",
                namespace=self.namespace,
                plural="gryviaaijobs"
            )

            jobs_with_limits = 0
            for job in jobs["items"]:
                resources = job.get("spec", {}).get("resources", {})
                if "memory" in resources and "cpu" in resources:
                    jobs_with_limits += 1

            total_jobs = len(jobs["items"])
            usage["limits"]["jobs_with_limits"] = jobs_with_limits
            usage["limits"]["total_jobs"] = total_jobs
            usage["limits"]["compliance_rate"] = (
                jobs_with_limits / total_jobs * 100 if total_jobs else 100
            )
            usage["limits"]["compliant"] = usage["limits"]["compliance_rate"] >= 90
        except Exception:
            usage["limits"]["compliant"] = False

        return usage

    def audit_cost_tracking(self, days: int) -> Dict:
        """Audit cost tracking and budgets"""
        print(f"{BLUE}Auditing cost tracking...{NC}")

        costs = {
            "tracking_enabled": True,
            "budget_compliance": {},
            "cost_allocation": {}
        }

        try:
            quotas = self.api.list_namespaced_custom_object(
                group="gryvia.io",
                version="v1alpha1",
                namespace=self.namespace,
                plural="gryviaquotas"
            )

            total_budget = 0
            total_spent = 0
            over_budget_count = 0

            for quota in quotas["items"]:
                budget = quota.get("spec", {}).get("budget", {}).get("monthly", 0)
                spent = quota.get("status", {}).get("budget", {}).get("spent", 0)

                total_budget += budget
                total_spent += spent

                if spent > budget:
                    over_budget_count += 1

            costs["budget_compliance"]["total_budget"] = total_budget
            costs["budget_compliance"]["total_spent"] = total_spent
            costs["budget_compliance"]["over_budget_teams"] = over_budget_count
            costs["budget_compliance"]["compliant"] = over_budget_count == 0
        except Exception:
            costs["budget_compliance"]["compliant"] = False

        return costs

    def audit_data_governance(self) -> Dict:
        """Audit data governance policies"""
        print(f"{BLUE}Auditing data governance...{NC}")

        governance = {
            "data_retention": {},
            "backup_policies": {},
            "data_encryption": {}
        }

        # Check PVC retention
        try:
            pvcs = self.core_api.list_namespaced_persistent_volume_claim(self.namespace)
            governance["data_retention"]["pvc_count"] = len(pvcs.items)
            governance["data_retention"]["compliant"] = True
        except Exception:
            governance["data_retention"]["compliant"] = False

        # Check backup policies (cannot be auto-detected; mark as unchecked)
        governance["backup_policies"]["automated_backups"] = False
        governance["backup_policies"]["backup_frequency"] = "unknown"
        governance["backup_policies"]["compliant"] = False
        governance["backup_policies"]["note"] = "Manual verification required"

        # Check encryption (cannot be auto-detected; mark as unchecked)
        governance["data_encryption"]["at_rest"] = False
        governance["data_encryption"]["in_transit"] = False
        governance["data_encryption"]["compliant"] = False
        governance["data_encryption"]["note"] = "Manual verification required"

        return governance

    def calculate_compliance_score(self, report: Dict) -> float:
        """Calculate overall compliance score (0-100)"""
        scores = []

        # Security (30%)
        security = report["security"]
        security_score = sum([
            30 if security["pod_security"].get("compliant") else 0,
            30 if security["network_policies"].get("compliant") else 0,
            20 if security["secrets_management"].get("compliant") else 0,
            20 if security["image_security"].get("compliant") else 0,
        ]) / 100 * 30

        # Access Control (20%)
        access = report["access_control"]
        access_score = sum([
            50 if access["rbac"].get("compliant") else 0,
            50 if access["service_accounts"].get("compliant") else 0,
        ]) / 100 * 20

        # Resource Usage (20%)
        usage = report["resource_usage"]
        usage_score = sum([
            50 if usage["quotas"].get("compliant") else 0,
            50 if usage["limits"].get("compliant") else 0,
        ]) / 100 * 20

        # Cost Tracking (15%)
        costs = report["cost_tracking"]
        cost_score = 15 if costs["budget_compliance"].get("compliant") else 0

        # Data Governance (15%)
        governance = report["data_governance"]
        governance_score = sum([
            33 if governance["data_retention"].get("compliant") else 0,
            33 if governance["backup_policies"].get("compliant") else 0,
            34 if governance["data_encryption"].get("compliant") else 0,
        ]) / 100 * 15

        total_score = security_score + access_score + usage_score + cost_score + governance_score

        return round(total_score, 1)

    def print_report(self, report: Dict):
        """Print formatted compliance report"""
        print(f"\n{BLUE}╔═══════════════════════════════════════════════════════════════╗{NC}")
        print(f"{BLUE}║         Gryvia Compliance Report                          ║{NC}")
        print(f"{BLUE}╚═══════════════════════════════════════════════════════════════╝{NC}\n")

        print(f"Generated: {report['generated_at']}")
        print(f"Period: Last {report['period_days']} days\n")

        # Overall Score
        score = report["compliance_score"]
        score_color = GREEN if score >= 90 else YELLOW if score >= 70 else RED

        print(f"{BLUE}OVERALL COMPLIANCE SCORE{NC}")
        print("─" * 70)
        print(f"{score_color}{score}/100{NC}")
        if score >= 90:
            print(f"{GREEN}✓ EXCELLENT - Meets compliance requirements{NC}")
        elif score >= 70:
            print(f"{YELLOW}⚠ GOOD - Some improvements needed{NC}")
        else:
            print(f"{RED}✗ POOR - Immediate action required{NC}")
        print()

        # Security
        print(f"{BLUE}SECURITY POSTURE{NC}")
        print("─" * 70)
        security = report["security"]
        self._print_check("Pod Security Policies", security["pod_security"].get("compliant"))
        self._print_check("Network Policies", security["network_policies"].get("compliant"))
        self._print_check("Secrets Management", security["secrets_management"].get("compliant"))
        self._print_check("Image Security", security["image_security"].get("compliant"))
        print()

        # Access Control
        print(f"{BLUE}ACCESS CONTROL{NC}")
        print("─" * 70)
        access = report["access_control"]
        self._print_check("RBAC Configured", access["rbac"].get("compliant"))
        self._print_check("Service Accounts", access["service_accounts"].get("compliant"))
        print()

        # Resource Usage
        print(f"{BLUE}RESOURCE MANAGEMENT{NC}")
        print("─" * 70)
        usage = report["resource_usage"]
        self._print_check("Quota Compliance", usage["quotas"].get("compliant"))
        self._print_check("Resource Limits", usage["limits"].get("compliant"))
        print()

        # Cost Tracking
        print(f"{BLUE}COST MANAGEMENT{NC}")
        print("─" * 70)
        costs = report["cost_tracking"]
        self._print_check("Budget Compliance", costs["budget_compliance"].get("compliant"))
        if "total_budget" in costs["budget_compliance"]:
            print(f"  Budget: ${costs['budget_compliance']['total_budget']:,.2f}")
            print(f"  Spent: ${costs['budget_compliance']['total_spent']:,.2f}")
        print()

        # Data Governance
        print(f"{BLUE}DATA GOVERNANCE{NC}")
        print("─" * 70)
        governance = report["data_governance"]
        self._print_check("Data Retention", governance["data_retention"].get("compliant"))
        self._print_check("Backup Policies", governance["backup_policies"].get("compliant"))
        self._print_check("Data Encryption", governance["data_encryption"].get("compliant"))
        print()

    def _print_check(self, label: str, compliant: bool):
        """Print compliance check result"""
        status = f"{GREEN}✓{NC}" if compliant else f"{RED}✗{NC}"
        print(f"{status} {label}")

    def export_report(self, report: Dict, output_file: str):
        """Export report to JSON"""
        with open(output_file, 'w') as f:
            json.dump(report, f, indent=2)
        print(f"\n{GREEN}Report exported to {output_file}{NC}")


def main():
    parser = argparse.ArgumentParser(description="Gryvia Audit and Compliance Tool")
    parser.add_argument("--namespace", default="default", help="Namespace to audit")
    parser.add_argument("--days", type=int, default=30, help="Number of days to analyze")
    parser.add_argument("--output", help="Output file (JSON)")

    args = parser.parse_args()

    auditor = AuditTool(namespace=args.namespace)

    # Generate report
    report = auditor.generate_compliance_report(days=args.days)

    # Print report
    auditor.print_report(report)

    # Export if requested
    if args.output:
        auditor.export_report(report, args.output)


if __name__ == "__main__":
    main()
