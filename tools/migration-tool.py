#!/usr/bin/env python3
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""
Migration Tool for Gryvia
Migrate workloads between clusters, backup/restore, and upgrade assistance
"""

import argparse
import json
import re
import sys
import os
from datetime import datetime
from typing import Dict, List, Optional
from kubernetes import client, config as k8s_config
import yaml

# Color codes
BLUE = '\033[0;34m'
GREEN = '\033[0;32m'
YELLOW = '\033[1;33m'
RED = '\033[0;31m'
CYAN = '\033[0;36m'
NC = '\033[0m'


class MigrationTool:
    def __init__(self, source_context: Optional[str] = None,
                 dest_context: Optional[str] = None):
        """Initialize with source and destination contexts"""
        self.source_context = source_context
        self.dest_context = dest_context

        # Load source cluster config
        if source_context:
            k8s_config.load_kube_config(context=source_context)
            self.source_api = client.CustomObjectsApi()
            self.source_core = client.CoreV1Api()

    def list_resources(self) -> Dict:
        """List all Gryvia resources in source cluster"""
        print(f"{BLUE}Discovering resources in source cluster...{NC}\n")

        resources = {
            "gryviaaijobs": [],
            "gryviaworkflows": [],
            "gryviaqueues": [],
            "gryviausers": [],
            "configmaps": [],
            "secrets": [],
            "pvcs": []
        }

        # Get custom resources
        for resource_type in ["gryviaaijobs", "gryviaworkflows", "gryviaqueues", "gryviausers"]:
            try:
                items = self.source_api.list_cluster_custom_object(
                    group="gryvia.io",
                    version="v1alpha1",
                    plural=resource_type
                )
                resources[resource_type] = items.get("items", [])
                print(f"Found {len(resources[resource_type])} {resource_type}")
            except Exception as e:
                print(f"{YELLOW}Warning: Could not list {resource_type}: {e}{NC}")

        # Get ConfigMaps and Secrets (filtered by Gryvia label)
        try:
            configmaps = self.source_core.list_config_map_for_all_namespaces(
                label_selector="app.kubernetes.io/part-of=gryvia"
            )
            resources["configmaps"] = [cm.to_dict() for cm in configmaps.items]
            print(f"Found {len(resources['configmaps'])} configmaps")

            secrets = self.source_core.list_secret_for_all_namespaces(
                label_selector="app.kubernetes.io/part-of=gryvia"
            )
            resources["secrets"] = [s.to_dict() for s in secrets.items]
            print(f"Found {len(resources['secrets'])} secrets")

            pvcs = self.source_core.list_persistent_volume_claim_for_all_namespaces(
                label_selector="app.kubernetes.io/part-of=gryvia"
            )
            resources["pvcs"] = [pvc.to_dict() for pvc in pvcs.items]
            print(f"Found {len(resources['pvcs'])} PVCs")
        except Exception as e:
            print(f"{YELLOW}Warning: Could not list native resources: {e}{NC}")

        return resources

    def export_resources(self, resources: Dict, output_dir: str):
        """Export resources to YAML files"""
        print(f"\n{BLUE}Exporting resources to {output_dir}...{NC}\n")

        os.makedirs(output_dir, exist_ok=True)

        for resource_type, items in resources.items():
            if not items:
                continue

            type_dir = os.path.join(output_dir, resource_type)
            os.makedirs(type_dir, exist_ok=True)

            for item in items:
                # Clean up Kubernetes metadata; redact secrets to avoid plaintext export
                cleaned = self.clean_resource(item, redact_secrets=(resource_type == "secrets"))

                # Generate filename (sanitize to prevent path traversal / injection)
                name = re.sub(r'[^a-zA-Z0-9._-]', '_', cleaned["metadata"]["name"])
                namespace = re.sub(r'[^a-zA-Z0-9._-]', '_', cleaned["metadata"].get("namespace", "default"))
                filename = f"{namespace}_{name}.yaml"
                filepath = os.path.join(type_dir, filename)

                # Write to file
                with open(filepath, 'w') as f:
                    yaml.dump(cleaned, f, default_flow_style=False)

                print(f"Exported {resource_type}/{namespace}/{name}")

        # Create manifest file
        manifest = {
            "export_date": datetime.now().isoformat(),
            "source_context": self.source_context,
            "resource_counts": {k: len(v) for k, v in resources.items()},
            "total_resources": sum(len(v) for v in resources.values())
        }

        with open(os.path.join(output_dir, "manifest.json"), 'w') as f:
            json.dump(manifest, f, indent=2)

        print(f"\n{GREEN}✓ Exported {manifest['total_resources']} resources to {output_dir}{NC}")

    def clean_resource(self, resource: Dict, redact_secrets: bool = False) -> Dict:
        """Clean resource for migration (remove cluster-specific fields)"""
        cleaned = resource.copy()

        # Remove runtime fields
        if "metadata" in cleaned:
            metadata = cleaned["metadata"]
            fields_to_remove = [
                "uid", "resourceVersion", "generation", "creationTimestamp",
                "selfLink", "managedFields", "ownerReferences"
            ]
            for field in fields_to_remove:
                metadata.pop(field, None)

            # Remove finalizers (will be recreated)
            metadata.pop("finalizers", None)

        # Remove status
        cleaned.pop("status", None)

        # Redact secret data to avoid plaintext export
        if redact_secrets and cleaned.get("kind") == "Secret":
            if "data" in cleaned:
                cleaned["data"] = {k: "REDACTED" for k in cleaned["data"]}
            if "stringData" in cleaned:
                cleaned["stringData"] = {k: "REDACTED" for k in cleaned["stringData"]}

        return cleaned

    def import_resources(self, import_dir: str, namespace: Optional[str] = None):
        """Import resources to destination cluster"""
        if not self.dest_context:
            print(f"{RED}Error: Destination context not specified{NC}")
            return

        print(f"{BLUE}Importing resources from {import_dir}...{NC}\n")

        # Load destination cluster config
        k8s_config.load_kube_config(context=self.dest_context)
        dest_api = client.CustomObjectsApi()
        dest_core = client.CoreV1Api()

        # Load manifest
        manifest_path = os.path.join(import_dir, "manifest.json")
        if not os.path.exists(manifest_path):
            print(f"{RED}Error: manifest.json not found in {import_dir}{NC}")
            return

        with open(manifest_path, 'r') as f:
            manifest = json.load(f)

        print(f"Import manifest from {manifest['export_date']}")
        print(f"Total resources: {manifest['total_resources']}\n")

        # Import order (dependencies first)
        import_order = [
            "configmaps",
            "secrets",
            "gryviaqueues",
            "gryviausers",
            "pvcs",
            "gryviaworkflows",
            "gryviaaijobs"
        ]

        stats = {"succeeded": 0, "failed": 0, "skipped": 0}

        for resource_type in import_order:
            type_dir = os.path.join(import_dir, resource_type)
            if not os.path.exists(type_dir):
                continue

            print(f"{CYAN}Importing {resource_type}...{NC}")

            for filename in os.listdir(type_dir):
                if not filename.endswith(".yaml"):
                    continue

                filepath = os.path.join(type_dir, filename)
                try:
                    with open(filepath, 'r') as f:
                        resource = yaml.safe_load(f)

                    # Override namespace if specified
                    if namespace:
                        resource["metadata"]["namespace"] = namespace

                    # Import based on type
                    if resource_type in ["gryviaaijobs", "gryviaworkflows",
                                        "gryviaqueues", "gryviausers"]:
                        dest_api.create_namespaced_custom_object(
                            group="gryvia.io",
                            version="v1alpha1",
                            namespace=resource["metadata"].get("namespace", "default"),
                            plural=resource_type,
                            body=resource
                        )
                    elif resource_type == "configmaps":
                        dest_core.create_namespaced_config_map(
                            namespace=resource["metadata"].get("namespace", "default"),
                            body=resource
                        )
                    elif resource_type == "secrets":
                        dest_core.create_namespaced_secret(
                            namespace=resource["metadata"].get("namespace", "default"),
                            body=resource
                        )
                    elif resource_type == "pvcs":
                        dest_core.create_namespaced_persistent_volume_claim(
                            namespace=resource["metadata"].get("namespace", "default"),
                            body=resource
                        )

                    stats["succeeded"] += 1
                    print(f"  ✓ {filename}")

                except Exception as e:
                    if "already exists" in str(e):
                        stats["skipped"] += 1
                        print(f"  ⊘ {filename} (already exists)")
                    else:
                        stats["failed"] += 1
                        print(f"  ✗ {filename}: {e}")

        print(f"\n{GREEN}Import complete:{NC}")
        print(f"  Succeeded: {stats['succeeded']}")
        print(f"  Skipped: {stats['skipped']}")
        print(f"  Failed: {stats['failed']}")

    def migrate_jobs(self, job_filter: Optional[str] = None, dry_run: bool = False):
        """Migrate running jobs from source to destination"""
        print(f"{BLUE}Migrating jobs from source to destination...{NC}\n")

        if dry_run:
            print(f"{YELLOW}DRY RUN MODE - No changes will be made{NC}\n")

        # Get jobs from source
        try:
            jobs = self.source_api.list_cluster_custom_object(
                group="gryvia.io",
                version="v1alpha1",
                plural="gryviaaijobs"
            )
        except Exception as e:
            print(f"{RED}Error listing jobs: {e}{NC}")
            return

        jobs_to_migrate = []
        for job in jobs.get("items", []):
            name = job["metadata"]["name"]
            status = job.get("status", {}).get("phase", "")

            # Filter jobs
            if job_filter and job_filter not in name:
                continue

            # Only migrate pending/running jobs
            if status in ["Pending", "Running", "Queued"]:
                jobs_to_migrate.append(job)

        print(f"Found {len(jobs_to_migrate)} jobs to migrate\n")

        if not jobs_to_migrate:
            print(f"{YELLOW}No jobs to migrate{NC}")
            return

        # Migrate each job
        for job in jobs_to_migrate:
            name = job["metadata"]["name"]
            namespace = job["metadata"].get("namespace", "default")
            status = job.get("status", {}).get("phase", "Unknown")

            print(f"Migrating {namespace}/{name} (status: {status})")

            if not dry_run:
                # Clean and prepare job for destination
                cleaned_job = self.clean_resource(job)

                # Add migration metadata
                if "annotations" not in cleaned_job["metadata"]:
                    cleaned_job["metadata"]["annotations"] = {}

                cleaned_job["metadata"]["annotations"].update({
                    "gryvia.io/migrated-from": self.source_context,
                    "gryvia.io/migration-date": datetime.now().isoformat(),
                    "gryvia.io/original-status": status
                })

                try:
                    # Create in destination
                    k8s_config.load_kube_config(context=self.dest_context)
                    dest_api = client.CustomObjectsApi()

                    dest_api.create_namespaced_custom_object(
                        group="gryvia.io",
                        version="v1alpha1",
                        namespace=namespace,
                        plural="gryviaaijobs",
                        body=cleaned_job
                    )

                    print(f"  ✓ Created in destination cluster")

                    # Optionally delete from source
                    # self.source_api.delete_namespaced_custom_object(...)

                except Exception as e:
                    print(f"  ✗ Failed: {e}")

        print(f"\n{GREEN}Migration complete{NC}")

    def verify_migration(self, import_dir: str):
        """Verify that migration was successful"""
        print(f"{BLUE}Verifying migration...{NC}\n")

        # Load manifest
        manifest_path = os.path.join(import_dir, "manifest.json")
        with open(manifest_path, 'r') as f:
            manifest = json.load(f)

        # Load destination cluster
        k8s_config.load_kube_config(context=self.dest_context)
        dest_api = client.CustomObjectsApi()

        verification = {
            "expected": manifest["resource_counts"],
            "found": {},
            "missing": []
        }

        # Check each resource type
        for resource_type, expected_count in manifest["resource_counts"].items():
            if resource_type in ["configmaps", "secrets", "pvcs"]:
                continue  # Skip native resources for now

            try:
                items = dest_api.list_cluster_custom_object(
                    group="gryvia.io",
                    version="v1alpha1",
                    plural=resource_type
                )
                found_count = len(items.get("items", []))
                verification["found"][resource_type] = found_count

                status_color = GREEN if found_count >= expected_count else RED
                print(f"{resource_type}: {status_color}{found_count}/{expected_count}{NC}")

            except Exception as e:
                verification["found"][resource_type] = 0
                print(f"{resource_type}: {RED}0/{expected_count}{NC} (error: {e})")

        # Summary
        total_expected = sum(manifest["resource_counts"].values())
        total_found = sum(verification["found"].values())

        print(f"\n{CYAN}Verification Summary:{NC}")
        print(f"Expected: {total_expected}")
        print(f"Found: {total_found}")

        if total_found >= total_expected:
            print(f"{GREEN}✓ Migration verified successfully{NC}")
        else:
            print(f"{YELLOW}⚠ Some resources may be missing{NC}")

    def generate_migration_plan(self, output: str):
        """Generate a migration plan document"""
        resources = self.list_resources()

        plan = {
            "migration_plan": {
                "created": datetime.now().isoformat(),
                "source_cluster": self.source_context,
                "destination_cluster": self.dest_context or "TBD",
                "resources_to_migrate": {k: len(v) for k, v in resources.items()},
                "total_resources": sum(len(v) for v in resources.values())
            },
            "pre_migration_checklist": [
                "☐ Backup source cluster",
                "☐ Verify destination cluster is running Gryvia",
                "☐ Check destination cluster has sufficient capacity",
                "☐ Create necessary namespaces in destination",
                "☐ Configure storage classes in destination",
                "☐ Test network connectivity between clusters",
                "☐ Notify users of migration window"
            ],
            "migration_steps": [
                {
                    "step": 1,
                    "action": "Export resources",
                    "command": f"python migration-tool.py export --output /backup/migration",
                    "estimated_time": "5-10 minutes"
                },
                {
                    "step": 2,
                    "action": "Transfer backup to destination",
                    "command": "scp -r /backup/migration user@dest-cluster:/backup/",
                    "estimated_time": "10-30 minutes"
                },
                {
                    "step": 3,
                    "action": "Import configuration resources",
                    "command": "python migration-tool.py import --source /backup/migration",
                    "estimated_time": "5-10 minutes"
                },
                {
                    "step": 4,
                    "action": "Migrate running jobs",
                    "command": "python migration-tool.py migrate-jobs --dry-run",
                    "estimated_time": "15-30 minutes"
                },
                {
                    "step": 5,
                    "action": "Verify migration",
                    "command": "python migration-tool.py verify --source /backup/migration",
                    "estimated_time": "5 minutes"
                },
                {
                    "step": 6,
                    "action": "Update DNS/endpoints",
                    "command": "Update load balancers and DNS records",
                    "estimated_time": "10-15 minutes"
                }
            ],
            "post_migration_checklist": [
                "☐ Verify all critical jobs are running",
                "☐ Check GPU utilization metrics",
                "☐ Test job submission from CLI",
                "☐ Verify Web UI access",
                "☐ Monitor logs for errors",
                "☐ Notify users migration is complete",
                "☐ Keep source cluster running for 24h as backup"
            ],
            "rollback_plan": {
                "if_migration_fails": [
                    "1. Point DNS back to source cluster",
                    "2. Cancel all jobs in destination cluster",
                    "3. Investigate and fix issues",
                    "4. Retry migration when ready"
                ],
                "estimated_rollback_time": "10-15 minutes"
            }
        }

        with open(output, 'w') as f:
            json.dump(plan, f, indent=2)

        print(f"{GREEN}Migration plan generated: {output}{NC}")
        print(f"\nTotal resources to migrate: {plan['migration_plan']['total_resources']}")


def main():
    parser = argparse.ArgumentParser(description="Migration Tool for Gryvia")
    subparsers = parser.add_subparsers(dest="command", help="Command to execute")

    # Export command
    export_parser = subparsers.add_parser("export", help="Export resources from source cluster")
    export_parser.add_argument("--source-context", required=True,
                              help="Source cluster context")
    export_parser.add_argument("--output", required=True,
                              help="Output directory for exported resources")

    # Import command
    import_parser = subparsers.add_parser("import", help="Import resources to destination cluster")
    import_parser.add_argument("--dest-context", required=True,
                              help="Destination cluster context")
    import_parser.add_argument("--source", required=True,
                              help="Directory with exported resources")
    import_parser.add_argument("--namespace", help="Override namespace for all resources")

    # Migrate jobs command
    migrate_parser = subparsers.add_parser("migrate-jobs", help="Migrate running jobs")
    migrate_parser.add_argument("--source-context", required=True)
    migrate_parser.add_argument("--dest-context", required=True)
    migrate_parser.add_argument("--filter", help="Filter jobs by name")
    migrate_parser.add_argument("--dry-run", action="store_true",
                               help="Show what would be migrated without making changes")

    # Verify command
    verify_parser = subparsers.add_parser("verify", help="Verify migration")
    verify_parser.add_argument("--dest-context", required=True)
    verify_parser.add_argument("--source", required=True,
                              help="Directory with exported resources")

    # Plan command
    plan_parser = subparsers.add_parser("plan", help="Generate migration plan")
    plan_parser.add_argument("--source-context", required=True)
    plan_parser.add_argument("--dest-context")
    plan_parser.add_argument("--output", default="migration-plan.json")

    args = parser.parse_args()

    if not args.command:
        parser.print_help()
        return

    try:
        if args.command == "export":
            tool = MigrationTool(source_context=args.source_context)
            resources = tool.list_resources()
            tool.export_resources(resources, args.output)

        elif args.command == "import":
            tool = MigrationTool(dest_context=args.dest_context)
            tool.import_resources(args.source, args.namespace)

        elif args.command == "migrate-jobs":
            tool = MigrationTool(source_context=args.source_context,
                               dest_context=args.dest_context)
            tool.migrate_jobs(job_filter=args.filter, dry_run=args.dry_run)

        elif args.command == "verify":
            tool = MigrationTool(dest_context=args.dest_context)
            tool.verify_migration(args.source)

        elif args.command == "plan":
            tool = MigrationTool(source_context=args.source_context,
                               dest_context=args.dest_context)
            tool.generate_migration_plan(args.output)

    except Exception as e:
        print(f"{RED}Error: {e}{NC}")
        sys.exit(1)


if __name__ == "__main__":
    main()
