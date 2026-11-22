#!/usr/bin/env python3
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""
Gryvia Cost Calculator
Analyzes job history and provides cost projections
"""

import argparse
import json
from datetime import datetime, timedelta
from typing import Dict, List
from kubernetes import client, config
from kubernetes.client.rest import ApiException

# GPU pricing per hour
GPU_PRICING = {
    "H100": 8.00,
    "A100-80G": 4.00,
    "A100-40G": 3.50,
    "L40": 2.50,
    "V100": 2.00,
    "T4": 1.00,
}


class CostCalculator:
    def __init__(self):
        try:
            config.load_kube_config()
        except config.ConfigException:
            config.load_incluster_config()

        self.api = client.CustomObjectsApi()

    def get_jobs(self, namespace="default", days=30):
        """Get all jobs from the last N days"""
        try:
            jobs = self.api.list_namespaced_custom_object(
                group="gryvia.io",
                version="v1alpha1",
                namespace=namespace,
                plural="gryviaaijobs"
            )
            return jobs.get("items", [])
        except ApiException as e:
            print(f"Error fetching jobs: {e}")
            return []

    def calculate_job_cost(self, job):
        """Calculate cost for a single job"""
        spec = job.get("spec", {})
        status = job.get("status", {})

        gpu_type = spec.get("resources", {}).get("gpuType", "unknown")
        gpu_count = spec.get("resources", {}).get("gpuCount", 1)
        gpu_price = GPU_PRICING.get(gpu_type, 0)

        start_time = status.get("startTime")
        completion_time = status.get("completionTime")

        if not start_time:
            return 0

        start = datetime.fromisoformat(start_time.replace('Z', '+00:00'))

        if completion_time:
            end = datetime.fromisoformat(completion_time.replace('Z', '+00:00'))
        else:
            end = datetime.now(start.tzinfo)

        duration_hours = (end - start).total_seconds() / 3600

        cost = duration_hours * gpu_count * gpu_price

        return cost

    def analyze_costs(self, namespace="default", days=30):
        """Analyze costs for all jobs"""
        jobs = self.get_jobs(namespace, days)

        total_cost = 0
        cost_by_team = {}
        cost_by_gpu_type = {}
        cost_by_project = {}
        job_details = []

        for job in jobs:
            cost = self.calculate_job_cost(job)
            metadata = job.get("metadata", {})
            spec = job.get("spec", {})
            status = job.get("status", {})

            job_name = metadata.get("name", "unknown")
            labels = metadata.get("labels", {})
            team = labels.get("team", "unknown")
            project = labels.get("project", "unknown")
            gpu_type = spec.get("resources", {}).get("gpuType", "unknown")

            total_cost += cost

            # By team
            if team not in cost_by_team:
                cost_by_team[team] = {"cost": 0, "jobs": 0}
            cost_by_team[team]["cost"] += cost
            cost_by_team[team]["jobs"] += 1

            # By GPU type
            if gpu_type not in cost_by_gpu_type:
                cost_by_gpu_type[gpu_type] = {"cost": 0, "jobs": 0}
            cost_by_gpu_type[gpu_type]["cost"] += cost
            cost_by_gpu_type[gpu_type]["jobs"] += 1

            # By project
            if project not in cost_by_project:
                cost_by_project[project] = {"cost": 0, "jobs": 0}
            cost_by_project[project]["cost"] += cost
            cost_by_project[project]["jobs"] += 1

            job_details.append({
                "name": job_name,
                "team": team,
                "project": project,
                "gpu_type": gpu_type,
                "status": status.get("phase", "unknown"),
                "cost": round(cost, 2)
            })

        return {
            "total_cost": round(total_cost, 2),
            "total_jobs": len(jobs),
            "cost_by_team": {k: {**v, "cost": round(v["cost"], 2)} for k, v in cost_by_team.items()},
            "cost_by_gpu_type": {k: {**v, "cost": round(v["cost"], 2)} for k, v in cost_by_gpu_type.items()},
            "cost_by_project": {k: {**v, "cost": round(v["cost"], 2)} for k, v in cost_by_project.items()},
            "job_details": sorted(job_details, key=lambda x: x["cost"], reverse=True)[:20]
        }

    def project_monthly_cost(self, namespace="default"):
        """Project cost for current month"""
        now = datetime.now()
        next_month = now.month % 12 + 1
        next_year = now.year + (1 if now.month == 12 else 0)
        days_in_month = (datetime(next_year, next_month, 1) - timedelta(days=1)).day
        days_elapsed = now.day

        analysis = self.analyze_costs(namespace, days=days_elapsed)
        current_cost = analysis["total_cost"]

        # Simple linear projection
        if days_elapsed == 0:
            projected_cost = 0.0
        else:
            projected_cost = (current_cost / days_elapsed) * days_in_month

        return {
            "current_month_cost": round(current_cost, 2),
            "projected_month_cost": round(projected_cost, 2),
            "days_elapsed": days_elapsed,
            "days_remaining": days_in_month - days_elapsed
        }


def print_report(analysis, projection):
    """Print formatted cost report"""
    print("\n" + "=" * 80)
    print(" " * 25 + "GRYVIA COST REPORT")
    print("=" * 80)

    print(f"\nGenerated: {datetime.now().strftime('%Y-%m-%d %H:%M:%S')}")

    print("\n📊 SUMMARY")
    print("-" * 80)
    print(f"Total Cost (last 30 days):    ${analysis['total_cost']:,.2f}")
    print(f"Total Jobs:                   {analysis['total_jobs']}")
    print(f"Average Cost per Job:         ${analysis['total_cost'] / max(analysis['total_jobs'], 1):,.2f}")

    print("\n📅 MONTHLY PROJECTION")
    print("-" * 80)
    print(f"Current Month (MTD):          ${projection['current_month_cost']:,.2f}")
    print(f"Projected Month Total:        ${projection['projected_month_cost']:,.2f}")
    print(f"Days Elapsed / Remaining:     {projection['days_elapsed']} / {projection['days_remaining']}")

    print("\n👥 COST BY TEAM")
    print("-" * 80)
    print(f"{'Team':<30} {'Cost':>15} {'Jobs':>10} {'Avg/Job':>15}")
    print("-" * 80)
    for team, data in sorted(analysis['cost_by_team'].items(), key=lambda x: x[1]['cost'], reverse=True):
        avg_cost = data['cost'] / max(data['jobs'], 1)
        print(f"{team:<30} ${data['cost']:>14,.2f} {data['jobs']:>10} ${avg_cost:>14,.2f}")

    print("\n🖥️  COST BY GPU TYPE")
    print("-" * 80)
    print(f"{'GPU Type':<30} {'Cost':>15} {'Jobs':>10} {'$/Hour':>15}")
    print("-" * 80)
    for gpu_type, data in sorted(analysis['cost_by_gpu_type'].items(), key=lambda x: x[1]['cost'], reverse=True):
        price = GPU_PRICING.get(gpu_type, 0)
        print(f"{gpu_type:<30} ${data['cost']:>14,.2f} {data['jobs']:>10} ${price:>14,.2f}")

    print("\n📁 COST BY PROJECT")
    print("-" * 80)
    print(f"{'Project':<30} {'Cost':>15} {'Jobs':>10}")
    print("-" * 80)
    for project, data in sorted(analysis['cost_by_project'].items(), key=lambda x: x[1]['cost'], reverse=True)[:10]:
        print(f"{project:<30} ${data['cost']:>14,.2f} {data['jobs']:>10}")

    print("\n💰 TOP 20 MOST EXPENSIVE JOBS")
    print("-" * 80)
    print(f"{'Job Name':<35} {'Team':<15} {'GPU Type':<12} {'Cost':>12}")
    print("-" * 80)
    for job in analysis['job_details']:
        print(f"{job['name'][:34]:<35} {job['team'][:14]:<15} {job['gpu_type']:<12} ${job['cost']:>11,.2f}")

    print("\n" + "=" * 80)


def main():
    parser = argparse.ArgumentParser(description="Gryvia Cost Calculator")
    parser.add_argument("--namespace", "-n", default="default", help="Kubernetes namespace")
    parser.add_argument("--days", "-d", type=int, default=30, help="Number of days to analyze")
    parser.add_argument("--output", "-o", help="Output file (JSON)")
    parser.add_argument("--team", "-t", help="Filter by team")
    args = parser.parse_args()

    calculator = CostCalculator()

    print("Analyzing costs...")
    analysis = calculator.analyze_costs(args.namespace, args.days)
    projection = calculator.project_monthly_cost(args.namespace)

    if args.team:
        # Filter by team
        analysis['cost_by_team'] = {k: v for k, v in analysis['cost_by_team'].items() if k == args.team}
        analysis['job_details'] = [j for j in analysis['job_details'] if j['team'] == args.team]

    if args.output:
        # Save to JSON
        with open(args.output, 'w') as f:
            json.dump({
                "analysis": analysis,
                "projection": projection
            }, f, indent=2)
        print(f"\nReport saved to: {args.output}")
    else:
        # Print to console
        print_report(analysis, projection)


if __name__ == "__main__":
    main()
