#!/usr/bin/env python3
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""
Gryvia GPU Profiler
Analyzes GPU utilization and provides optimization recommendations
"""

import sys
import json
import argparse
from datetime import datetime, timedelta
from typing import Dict, List, Optional
from kubernetes import client, config
from kubernetes.client.rest import ApiException

# Color codes
GREEN = '\033[0;32m'
YELLOW = '\033[1;33m'
RED = '\033[0;31m'
BLUE = '\033[0;34m'
CYAN = '\033[0;36m'
NC = '\033[0m'


class GPUProfiler:
    def __init__(self, namespace: str = "default"):
        try:
            config.load_kube_config()
        except config.ConfigException:
            config.load_incluster_config()

        self.api = client.CustomObjectsApi()
        self.core_api = client.CoreV1Api()
        self.namespace = namespace

    def get_job_metrics(self, job_name: str) -> Optional[Dict]:
        """Get GPU metrics for a specific job"""
        try:
            job = self.api.get_namespaced_custom_object(
                group="gryvia.io",
                version="v1alpha1",
                namespace=self.namespace,
                plural="gryviaaijobs",
                name=job_name
            )

            metrics = job.get("status", {}).get("metrics", {})
            return metrics
        except ApiException as e:
            print(f"{RED}Error fetching job: {e}{NC}")
            return None

    def analyze_utilization(self, metrics: Dict) -> Dict:
        """Analyze GPU utilization patterns"""
        gpu_util = metrics.get("avgGPUUtilization", 0)
        gpu_memory = metrics.get("avgGPUMemoryUtilization", 0)
        duration_hours = metrics.get("runningTime", 0) / 3600

        analysis = {
            "gpu_utilization": gpu_util,
            "gpu_memory": gpu_memory,
            "duration_hours": duration_hours,
            "efficiency_score": 0,
            "issues": [],
            "recommendations": []
        }

        # Calculate efficiency score
        efficiency_score = (gpu_util * 0.6 + gpu_memory * 0.4)
        analysis["efficiency_score"] = efficiency_score

        # Identify issues
        if gpu_util < 50:
            analysis["issues"].append({
                "severity": "high",
                "category": "low_gpu_utilization",
                "message": f"Low GPU utilization: {gpu_util:.1f}%"
            })

        if gpu_memory < 30:
            analysis["issues"].append({
                "severity": "medium",
                "category": "low_memory_usage",
                "message": f"Low GPU memory usage: {gpu_memory:.1f}%"
            })

        if gpu_util > 95 and gpu_memory < 40:
            analysis["issues"].append({
                "severity": "medium",
                "category": "compute_bound",
                "message": "Job is compute-bound but memory-underutilized"
            })

        if gpu_memory > 95:
            analysis["issues"].append({
                "severity": "high",
                "category": "memory_pressure",
                "message": "GPU memory is near capacity"
            })

        # Generate recommendations
        analysis["recommendations"] = self.generate_recommendations(analysis)

        return analysis

    def generate_recommendations(self, analysis: Dict) -> List[Dict]:
        """Generate optimization recommendations"""
        recommendations = []
        gpu_util = analysis["gpu_utilization"]
        gpu_memory = analysis["gpu_memory"]

        # Low GPU utilization
        if gpu_util < 50:
            recommendations.append({
                "priority": "high",
                "category": "performance",
                "title": "Increase Batch Size",
                "description": "GPU utilization is low. Increase batch size to improve throughput.",
                "action": f"Try increasing batch size by 50-100%",
                "expected_impact": "30-50% improvement in GPU utilization"
            })

            recommendations.append({
                "priority": "high",
                "category": "performance",
                "title": "Reduce Data Loading Bottleneck",
                "description": "Low GPU utilization may indicate data loading is the bottleneck.",
                "action": "Increase num_workers in DataLoader or use prefetching",
                "expected_impact": "Improved GPU utilization"
            })

        # Low memory usage
        if gpu_memory < 30:
            recommendations.append({
                "priority": "medium",
                "category": "cost",
                "title": "Use Smaller GPU Type",
                "description": f"Memory usage is only {gpu_memory:.1f}%. You may be overpaying for GPU capacity.",
                "action": "Consider switching to smaller GPU (e.g., A100-40G instead of A100-80G)",
                "expected_impact": "40-50% cost reduction"
            })

        # Memory pressure
        if gpu_memory > 95:
            recommendations.append({
                "priority": "high",
                "category": "stability",
                "title": "Reduce Batch Size",
                "description": "GPU memory is near capacity. Risk of OOM errors.",
                "action": "Reduce batch size by 20-30%",
                "expected_impact": "Improved stability"
            })

            recommendations.append({
                "priority": "medium",
                "category": "performance",
                "title": "Enable Gradient Checkpointing",
                "description": "Reduce memory usage with gradient checkpointing.",
                "action": "Enable gradient checkpointing in model config",
                "expected_impact": "30-40% memory reduction"
            })

        # Compute bound
        if gpu_util > 95 and gpu_memory < 40:
            recommendations.append({
                "priority": "medium",
                "category": "performance",
                "title": "Increase Model Size",
                "description": "GPU compute is maxed out but memory is underutilized.",
                "action": "Consider using a larger model variant",
                "expected_impact": "Better model quality without additional cost"
            })

        # Mixed precision
        if gpu_util < 80:
            recommendations.append({
                "priority": "low",
                "category": "performance",
                "title": "Enable Mixed Precision Training",
                "description": "Mixed precision can improve training speed.",
                "action": "Enable AMP (Automatic Mixed Precision)",
                "expected_impact": "2-3x speedup on modern GPUs"
            })

        return recommendations

    def estimate_cost_savings(self, analysis: Dict, gpu_type: str, gpu_count: int) -> Dict:
        """Estimate potential cost savings"""
        # GPU pricing ($/hour)
        pricing = {
            "H100": 8.00,
            "A100-80G": 4.00,
            "A100-40G": 3.50,
            "A100": 3.50,
            "L40": 2.50,
            "A10": 1.50,
            "V100": 2.00,
            "T4": 1.00,
        }

        current_price = pricing.get(gpu_type, 2.0) * gpu_count
        duration_hours = analysis["duration_hours"]
        current_cost = current_price * duration_hours

        savings = {
            "current_cost": current_cost,
            "potential_savings": [],
            "total_potential_savings": 0
        }

        gpu_memory = analysis["gpu_memory"]
        gpu_util = analysis["gpu_utilization"]

        # Opportunity 1: Downgrade GPU type
        if gpu_memory < 30 and gpu_type == "A100-80G":
            new_price = pricing["A100-40G"] * gpu_count
            new_cost = new_price * duration_hours
            saving = current_cost - new_cost

            savings["potential_savings"].append({
                "opportunity": "Downgrade to A100-40G",
                "savings": saving,
                "percentage": (saving / current_cost) * 100
            })
            savings["total_potential_savings"] += saving

        # Opportunity 2: Improve utilization
        if gpu_util < 50:
            # Estimate time reduction with better utilization
            efficiency_gain = 0.5  # 50% faster
            new_duration = duration_hours * (1 - efficiency_gain)
            new_cost = current_price * new_duration
            saving = current_cost - new_cost

            savings["potential_savings"].append({
                "opportunity": "Improve GPU utilization to 80%+",
                "savings": saving,
                "percentage": (saving / current_cost) * 100
            })
            savings["total_potential_savings"] += saving

        return savings

    def profile_job(self, job_name: str, gpu_type: str, gpu_count: int):
        """Profile a specific job"""
        print(f"{BLUE}╔════════════════════════════════════════════════════════════════╗{NC}")
        print(f"{BLUE}║          Gryvia GPU Profiler                              ║{NC}")
        print(f"{BLUE}╚════════════════════════════════════════════════════════════════╝{NC}")
        print()

        print(f"{CYAN}Profiling job: {job_name}{NC}")
        print()

        # Get metrics
        metrics = self.get_job_metrics(job_name)
        if not metrics:
            print(f"{RED}No metrics available for job{NC}")
            return

        # Analyze
        analysis = self.analyze_utilization(metrics)

        # Print summary
        print(f"{CYAN}📊 UTILIZATION SUMMARY{NC}")
        print("─" * 70)

        efficiency_color = GREEN if analysis["efficiency_score"] > 70 else YELLOW if analysis["efficiency_score"] > 50 else RED
        print(f"Efficiency Score:       {efficiency_color}{analysis['efficiency_score']:.1f}/100{NC}")
        print(f"GPU Utilization:        {analysis['gpu_utilization']:.1f}%")
        print(f"GPU Memory Usage:       {analysis['gpu_memory']:.1f}%")
        print(f"Runtime:                {analysis['duration_hours']:.2f} hours")
        print()

        # Print issues
        if analysis["issues"]:
            print(f"{YELLOW}⚠️  ISSUES DETECTED{NC}")
            print("─" * 70)
            for issue in analysis["issues"]:
                severity_color = RED if issue["severity"] == "high" else YELLOW
                print(f"{severity_color}[{issue['severity'].upper()}]{NC} {issue['message']}")
            print()

        # Print recommendations
        if analysis["recommendations"]:
            print(f"{GREEN}💡 RECOMMENDATIONS{NC}")
            print("─" * 70)

            for i, rec in enumerate(analysis["recommendations"], 1):
                priority_color = RED if rec["priority"] == "high" else YELLOW if rec["priority"] == "medium" else BLUE

                print(f"\n{priority_color}[{rec['priority'].upper()}]{NC} {rec['title']}")
                print(f"  Category: {rec['category']}")
                print(f"  {rec['description']}")
                print(f"  Action: {GREEN}{rec['action']}{NC}")
                print(f"  Expected Impact: {rec['expected_impact']}")
            print()

        # Cost analysis
        savings = self.estimate_cost_savings(analysis, gpu_type, gpu_count)

        print(f"{CYAN}💰 COST ANALYSIS{NC}")
        print("─" * 70)
        print(f"Current Cost:           ${savings['current_cost']:.2f}")

        if savings["potential_savings"]:
            print(f"\nPotential Savings:")
            for opp in savings["potential_savings"]:
                print(f"  • {opp['opportunity']}")
                print(f"    ${opp['savings']:.2f} ({opp['percentage']:.1f}%)")

            print(f"\n{GREEN}Total Potential Savings: ${savings['total_potential_savings']:.2f}{NC}")
        else:
            print(f"\n{GREEN}✓ Job is cost-optimized{NC}")
        print()

    def profile_all_jobs(self):
        """Profile all running jobs"""
        try:
            jobs = self.api.list_namespaced_custom_object(
                group="gryvia.io",
                version="v1alpha1",
                namespace=self.namespace,
                plural="gryviaaijobs"
            )

            print(f"{CYAN}Profiling all jobs in namespace: {self.namespace}{NC}\n")

            results = []
            for job in jobs["items"]:
                name = job["metadata"]["name"]
                spec = job.get("spec", {})
                resources = spec.get("resources", {})
                gpu_type = resources.get("gpuType", "A100-80G")
                gpu_count = resources.get("gpuCount", 1)

                metrics = job.get("status", {}).get("metrics", {})
                if metrics:
                    analysis = self.analyze_utilization(metrics)
                    results.append({
                        "name": name,
                        "gpu_type": gpu_type,
                        "gpu_count": gpu_count,
                        "efficiency": analysis["efficiency_score"],
                        "gpu_util": analysis["gpu_utilization"],
                        "issues": len(analysis["issues"])
                    })

            # Print summary table
            print(f"{'JOB NAME':<30} {'GPU':<15} {'EFFICIENCY':<12} {'UTIL':<8} {'ISSUES'}")
            print("─" * 80)

            for result in sorted(results, key=lambda x: x["efficiency"]):
                eff_color = GREEN if result["efficiency"] > 70 else YELLOW if result["efficiency"] > 50 else RED
                issue_color = RED if result["issues"] > 0 else GREEN

                print(f"{result['name']:<30} "
                      f"{result['gpu_count']}x {result['gpu_type']:<10} "
                      f"{eff_color}{result['efficiency']:.1f}/100{NC:<12} "
                      f"{result['gpu_util']:.1f}%    "
                      f"{issue_color}{result['issues']}{NC}")

        except ApiException as e:
            print(f"{RED}Error listing jobs: {e}{NC}")


def main():
    parser = argparse.ArgumentParser(description="Gryvia GPU Profiler")
    parser.add_argument("--job", help="Job name to profile")
    parser.add_argument("--gpu-type", default="A100-80G", help="GPU type")
    parser.add_argument("--gpu-count", type=int, default=1, help="Number of GPUs")
    parser.add_argument("--namespace", default="default", help="Namespace")
    parser.add_argument("--all", action="store_true", help="Profile all jobs")
    parser.add_argument("--output", help="Output file (JSON)")

    args = parser.parse_args()

    profiler = GPUProfiler(namespace=args.namespace)

    if args.all:
        profiler.profile_all_jobs()
    elif args.job:
        profiler.profile_job(args.job, args.gpu_type, args.gpu_count)
    else:
        parser.print_help()
        sys.exit(1)


if __name__ == "__main__":
    main()
