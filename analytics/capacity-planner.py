#!/usr/bin/env python3
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""
Capacity Planning Tool for Gryvia
Predicts future resource needs and provides recommendations
"""

import argparse
import json
import sys
from datetime import datetime, timedelta
from typing import Dict, List, Tuple
import pandas as pd
import numpy as np
from kubernetes import client, config

# Color codes
BLUE = '\033[0;34m'
GREEN = '\033[0;32m'
YELLOW = '\033[1;33m'
RED = '\033[0;31m'
CYAN = '\033[0;36m'
NC = '\033[0m'


class CapacityPlanner:
    def __init__(self):
        try:
            config.load_kube_config()
        except config.ConfigException:
            config.load_incluster_config()

        self.api = client.CustomObjectsApi()
        self.core_api = client.CoreV1Api()

    def analyze_capacity(self, forecast_days: int = 90) -> Dict:
        """Analyze current capacity and predict future needs"""
        print(f"{BLUE}Analyzing Capacity Requirements...{NC}\n")

        # Get current cluster capacity
        current_capacity = self.get_current_capacity()

        # Get historical usage
        historical_usage = self.get_historical_usage()

        # Forecast future demand
        forecast = self.forecast_demand(historical_usage, forecast_days)

        # Identify gaps
        gaps = self.identify_capacity_gaps(current_capacity, forecast)

        # Generate recommendations
        recommendations = self.generate_capacity_recommendations(gaps, forecast_days)

        return {
            "current_capacity": current_capacity,
            "current_usage": self.get_current_usage(),
            "forecast": forecast,
            "gaps": gaps,
            "recommendations": recommendations,
            "cost_estimates": self.estimate_expansion_costs(recommendations)
        }

    def get_current_capacity(self) -> Dict:
        """Get current GPU capacity across cluster"""
        nodes = self.core_api.list_node()

        capacity = {
            "total_nodes": 0,
            "gpu_nodes": 0,
            "gpus_by_type": {},
            "total_gpus": 0,
            "total_memory_gb": 0,
            "total_cpu_cores": 0,
            "mig_instances": {}
        }

        for node in nodes.items:
            capacity["total_nodes"] += 1

            # Check if GPU node
            allocatable = node.status.allocatable or {}

            # Count standard GPUs
            if "nvidia.com/gpu" in allocatable:
                capacity["gpu_nodes"] += 1
                gpu_count = int(allocatable["nvidia.com/gpu"])
                capacity["total_gpus"] += gpu_count

                # Determine GPU type from label
                labels = node.metadata.labels or {}
                gpu_type = labels.get("gryvia.io/gpu-type", "unknown")

                if gpu_type not in capacity["gpus_by_type"]:
                    capacity["gpus_by_type"][gpu_type] = 0
                capacity["gpus_by_type"][gpu_type] += gpu_count

            # Count MIG instances
            for key, value in allocatable.items():
                if key.startswith("nvidia.com/mig-"):
                    profile = key.replace("nvidia.com/mig-", "")
                    if profile not in capacity["mig_instances"]:
                        capacity["mig_instances"][profile] = 0
                    capacity["mig_instances"][profile] += int(value)

            # Memory and CPU
            if "memory" in allocatable:
                memory_str = str(allocatable["memory"])
                # Convert Kubernetes memory units to GB
                try:
                    if memory_str.endswith("Ki"):
                        memory_gb = int(memory_str[:-2]) / (1024 * 1024)
                    elif memory_str.endswith("Mi"):
                        memory_gb = int(memory_str[:-2]) / 1024
                    elif memory_str.endswith("Gi"):
                        memory_gb = int(memory_str[:-2])
                    elif memory_str.endswith("Ti"):
                        memory_gb = int(memory_str[:-2]) * 1024
                    else:
                        # Plain bytes
                        memory_gb = int(memory_str) / (1024 ** 3)
                except (ValueError, TypeError):
                    memory_gb = 0
                elif memory_str.endswith("Ti"):
                    memory_gb = int(memory_str[:-2]) * 1024
                else:
                    # Bare bytes
                    memory_gb = int(memory_str) / (1024 ** 3)
                capacity["total_memory_gb"] += memory_gb

            if "cpu" in allocatable:
                capacity["total_cpu_cores"] += int(allocatable["cpu"])

        return capacity

    def get_current_usage(self) -> Dict:
        """Get current resource usage"""
        try:
            jobs = self.api.list_cluster_custom_object(
                group="gryvia.io",
                version="v1alpha1",
                plural="gryviaaijobs"
            )

            usage = {
                "running_jobs": 0,
                "pending_jobs": 0,
                "gpus_in_use": 0,
                "gpus_in_use_by_type": {},
                "utilization_percent": 0
            }

            for job in jobs["items"]:
                status = job.get("status", {}).get("phase", "")

                if status == "Running":
                    usage["running_jobs"] += 1
                    gpu_count = job.get("spec", {}).get("resources", {}).get("gpuCount", 0)
                    gpu_type = job.get("spec", {}).get("resources", {}).get("gpuType", "unknown")

                    usage["gpus_in_use"] += gpu_count
                    if gpu_type not in usage["gpus_in_use_by_type"]:
                        usage["gpus_in_use_by_type"][gpu_type] = 0
                    usage["gpus_in_use_by_type"][gpu_type] += gpu_count

                elif status == "Pending":
                    usage["pending_jobs"] += 1

            return usage
        except Exception as e:
            print(f"{RED}Error getting usage: {e}{NC}")
            return {}

    def get_historical_usage(self) -> List[Dict]:
        """Get historical usage data (simulated for demo)"""
        # In production, query time-series database
        # For now, simulate historical data

        historical = []
        base_date = datetime.now() - timedelta(days=90)

        for day in range(90):
            date = base_date + timedelta(days=day)

            # Simulate growth trend
            growth_factor = 1 + (day / 90) * 0.3  # 30% growth over 90 days

            historical.append({
                "date": date.isoformat(),
                "jobs_submitted": int(80 * growth_factor + np.random.normal(0, 10)),
                "peak_gpus_used": int(120 * growth_factor + np.random.normal(0, 15)),
                "avg_queue_time_minutes": max(5, 15 - (day / 90) * 5 + np.random.normal(0, 3))
            })

        return historical

    def forecast_demand(self, historical: List[Dict], days: int) -> Dict:
        """Forecast future resource demand"""
        # Simple linear regression for forecast
        # In production, use more sophisticated models

        if not historical:
            return {}

        # Extract data
        jobs = [h["jobs_submitted"] for h in historical]
        gpus = [h["peak_gpus_used"] for h in historical]

        # Calculate trend
        x = np.arange(len(jobs))

        # Job trend
        job_coef = np.polyfit(x, jobs, 1)
        job_forecast = np.poly1d(job_coef)

        # GPU trend
        gpu_coef = np.polyfit(x, gpus, 1)
        gpu_forecast = np.poly1d(gpu_coef)

        # Forecast future
        future_x = len(historical) + days

        forecast = {
            "forecast_period_days": days,
            "current_daily_jobs": int(jobs[-1]),
            "forecast_daily_jobs": int(job_forecast(future_x)),
            "job_growth_percent": ((job_forecast(future_x) - jobs[-1]) / jobs[-1] * 100),
            "current_peak_gpus": int(gpus[-1]),
            "forecast_peak_gpus": int(gpu_forecast(future_x)),
            "gpu_growth_percent": ((gpu_forecast(future_x) - gpus[-1]) / gpus[-1] * 100),
            "confidence": 0.85
        }

        return forecast

    def identify_capacity_gaps(self, capacity: Dict, forecast: Dict) -> Dict:
        """Identify gaps between capacity and forecasted demand"""
        current_gpus = capacity.get("total_gpus", 0)
        forecast_gpus = forecast.get("forecast_peak_gpus", 0)

        gpu_gap = max(0, forecast_gpus - current_gpus)

        gaps = {
            "gpu_shortage": gpu_gap,
            "shortage_percent": (gpu_gap / current_gpus * 100) if current_gpus > 0 else 0,
            "capacity_exhaustion_date": self.estimate_exhaustion_date(capacity, forecast),
            "risk_level": self.assess_capacity_risk(gpu_gap, current_gpus)
        }

        return gaps

    def estimate_exhaustion_date(self, capacity: Dict, forecast: Dict) -> str:
        """Estimate when capacity will be exhausted"""
        current_gpus = capacity.get("total_gpus", 0)
        growth_rate = forecast.get("gpu_growth_percent", 0) / 100

        if growth_rate <= 0:
            return "No exhaustion predicted"

        # Assume 85% utilization threshold
        utilization_threshold = 0.85
        available_capacity = current_gpus * utilization_threshold
        current_usage = forecast.get("current_peak_gpus", 0)

        if current_usage >= available_capacity:
            return "Already at capacity"

        # Simple linear extrapolation
        daily_growth = current_usage * growth_rate / 90
        if daily_growth <= 0 or current_usage <= 0:
            return "No exhaustion predicted"
        days_to_exhaustion = int((available_capacity - current_usage) / daily_growth)

        exhaustion_date = datetime.now() + timedelta(days=days_to_exhaustion)
        return exhaustion_date.strftime("%Y-%m-%d")

    def assess_capacity_risk(self, gap: int, current: int) -> str:
        """Assess capacity risk level"""
        if gap == 0:
            return "low"

        gap_percent = (gap / current * 100) if current > 0 else 100

        if gap_percent > 50:
            return "critical"
        elif gap_percent > 25:
            return "high"
        elif gap_percent > 10:
            return "medium"
        else:
            return "low"

    def generate_capacity_recommendations(self, gaps: Dict, forecast_days: int) -> List[Dict]:
        """Generate capacity expansion recommendations"""
        recommendations = []

        gpu_shortage = gaps.get("gpu_shortage", 0)
        risk_level = gaps.get("risk_level", "low")

        if gpu_shortage > 0:
            # Recommend GPU additions
            recommendations.append({
                "priority": "high" if risk_level in ["critical", "high"] else "medium",
                "category": "capacity_expansion",
                "title": f"Add {gpu_shortage} GPUs",
                "description": f"Forecasted demand exceeds current capacity by {gpu_shortage} GPUs. "
                             f"Risk level: {risk_level}",
                "gpu_count": gpu_shortage,
                "recommended_types": self.recommend_gpu_types(gpu_shortage),
                "timeline": self.get_expansion_timeline(risk_level),
                "implementation": [
                    "1. Obtain budget approval",
                    "2. Purchase/provision GPU nodes",
                    "3. Install and configure nodes",
                    "4. Add to Gryvia cluster",
                    "5. Validate and enable for production"
                ]
            })

        # Check for specific GPU type shortages
        recommendations.extend(self.check_gpu_type_balance())

        # Check for MIG opportunities
        recommendations.extend(self.check_mig_opportunities())

        return recommendations

    def recommend_gpu_types(self, count: int) -> List[Dict]:
        """Recommend specific GPU types to purchase"""
        # Based on workload analysis
        return [
            {
                "type": "A100-80G",
                "count": int(count * 0.6),
                "reason": "General purpose training and inference"
            },
            {
                "type": "H100",
                "count": int(count * 0.3),
                "reason": "Large model training, best performance"
            },
            {
                "type": "T4",
                "count": int(count * 0.1),
                "reason": "Inference and development workloads"
            }
        ]

    def get_expansion_timeline(self, risk: str) -> str:
        """Get recommended timeline based on risk"""
        timelines = {
            "critical": "Immediate (1-2 weeks)",
            "high": "Urgent (1 month)",
            "medium": "Planned (2-3 months)",
            "low": "Future (6+ months)"
        }
        return timelines.get(risk, "Planned (2-3 months)")

    def check_gpu_type_balance(self) -> List[Dict]:
        """Check if GPU type distribution is balanced"""
        recommendations = []

        # Simulated workload analysis
        # In production, analyze actual job requirements

        recommendations.append({
            "priority": "medium",
            "category": "optimization",
            "title": "Balance GPU Type Distribution",
            "description": "Current cluster is heavy on training GPUs. Add more inference GPUs.",
            "recommendation": "Add 16 T4 GPUs for inference workloads",
            "expected_benefit": "30% cost reduction for inference jobs",
            "timeline": "2 months"
        })

        return recommendations

    def check_mig_opportunities(self) -> List[Dict]:
        """Check for MIG deployment opportunities"""
        recommendations = []

        recommendations.append({
            "priority": "high",
            "category": "cost_optimization",
            "title": "Deploy MIG on A100 GPUs",
            "description": "50% of A100 utilization is from small workloads (<20GB memory). "
                         "Enable MIG to serve more workloads.",
            "recommendation": "Configure 8 A100 GPUs with all-1g.10gb profile",
            "expected_benefit": "Serve 7x more workloads, reduce queue times by 60%",
            "cost_savings": "$15,000/month",
            "timeline": "2 weeks"
        })

        return recommendations

    def estimate_expansion_costs(self, recommendations: List[Dict]) -> Dict:
        """Estimate costs for capacity expansion"""
        # GPU pricing (hardware + infrastructure)
        gpu_prices = {
            "H100": 35000,
            "A100-80G": 15000,
            "A100-40G": 10000,
            "V100": 6000,
            "T4": 2500
        }

        # Operating costs per GPU per month
        operating_costs = {
            "H100": 1200,
            "A100-80G": 800,
            "A100-40G": 500,
            "V100": 350,
            "T4": 150
        }

        total_capex = 0
        total_opex_monthly = 0
        breakdown = []

        for rec in recommendations:
            if rec.get("category") == "capacity_expansion":
                for gpu_type_rec in rec.get("recommended_types", []):
                    gpu_type = gpu_type_rec["type"]
                    count = gpu_type_rec["count"]

                    capex = gpu_prices.get(gpu_type, 10000) * count
                    opex = operating_costs.get(gpu_type, 500) * count

                    total_capex += capex
                    total_opex_monthly += opex

                    breakdown.append({
                        "gpu_type": gpu_type,
                        "count": count,
                        "capex_per_gpu": gpu_prices.get(gpu_type, 10000),
                        "total_capex": capex,
                        "opex_per_month": opex
                    })

        return {
            "total_capex": total_capex,
            "total_opex_monthly": total_opex_monthly,
            "total_opex_annual": total_opex_monthly * 12,
            "breakdown": breakdown,
            "3_year_tco": total_capex + (total_opex_monthly * 36)
        }

    def print_analysis(self, analysis: Dict):
        """Print capacity analysis"""
        print(f"\n{BLUE}╔══════════════════════════════════════════════════════════════╗{NC}")
        print(f"{BLUE}║           Gryvia Capacity Planning Report              ║{NC}")
        print(f"{BLUE}╚══════════════════════════════════════════════════════════════╝{NC}\n")

        # Current Capacity
        capacity = analysis["current_capacity"]
        print(f"{CYAN}📦 CURRENT CAPACITY{NC}")
        print("─" * 70)
        print(f"Total Nodes: {capacity['total_nodes']}")
        print(f"GPU Nodes: {capacity['gpu_nodes']}")
        print(f"Total GPUs: {capacity['total_gpus']}")
        print(f"\nGPUs by Type:")
        for gpu_type, count in capacity.get("gpus_by_type", {}).items():
            print(f"  • {gpu_type}: {count}")

        if capacity.get("mig_instances"):
            print(f"\nMIG Instances:")
            for profile, count in capacity["mig_instances"].items():
                print(f"  • {profile}: {count}")
        print()

        # Current Usage
        usage = analysis["current_usage"]
        print(f"{CYAN}📊 CURRENT USAGE{NC}")
        print("─" * 70)
        print(f"Running Jobs: {usage.get('running_jobs', 0)}")
        print(f"Pending Jobs: {usage.get('pending_jobs', 0)}")
        print(f"GPUs in Use: {usage.get('gpus_in_use', 0)} / {capacity['total_gpus']}")
        utilization = (usage.get('gpus_in_use', 0) / capacity['total_gpus'] * 100) if capacity['total_gpus'] > 0 else 0
        color = GREEN if utilization < 80 else YELLOW if utilization < 90 else RED
        print(f"Utilization: {color}{utilization:.1f}%{NC}")
        print()

        # Forecast
        forecast = analysis["forecast"]
        print(f"{CYAN}🔮 FORECAST ({forecast['forecast_period_days']} days){NC}")
        print("─" * 70)
        print(f"Current Daily Jobs: {forecast['current_daily_jobs']}")
        print(f"Forecast Daily Jobs: {forecast['forecast_daily_jobs']} "
              f"({forecast['job_growth_percent']:+.1f}%)")
        print(f"Current Peak GPUs: {forecast['current_peak_gpus']}")
        print(f"Forecast Peak GPUs: {forecast['forecast_peak_gpus']} "
              f"({forecast['gpu_growth_percent']:+.1f}%)")
        print(f"Confidence: {forecast['confidence']*100:.0f}%")
        print()

        # Capacity Gaps
        gaps = analysis["gaps"]
        print(f"{CYAN}⚠️  CAPACITY GAPS{NC}")
        print("─" * 70)
        risk_color = RED if gaps['risk_level'] == 'critical' else YELLOW if gaps['risk_level'] == 'high' else GREEN
        print(f"GPU Shortage: {gaps['gpu_shortage']} ({gaps['shortage_percent']:.1f}%)")
        print(f"Risk Level: {risk_color}{gaps['risk_level'].upper()}{NC}")
        print(f"Exhaustion Date: {gaps['capacity_exhaustion_date']}")
        print()

        # Recommendations
        print(f"{CYAN}💡 RECOMMENDATIONS{NC}")
        print("─" * 70)
        for i, rec in enumerate(analysis["recommendations"][:5], 1):
            priority_color = RED if rec["priority"] == "high" else YELLOW
            print(f"\n{priority_color}[{rec['priority'].upper()}]{NC} {i}. {rec['title']}")
            print(f"  {rec['description']}")
            if "timeline" in rec:
                print(f"  Timeline: {rec['timeline']}")
        print()

        # Cost Estimates
        costs = analysis["cost_estimates"]
        if costs["total_capex"] > 0:
            print(f"{CYAN}💰 EXPANSION COSTS{NC}")
            print("─" * 70)
            print(f"Capital Expenditure: ${costs['total_capex']:,.2f}")
            print(f"Monthly Operating Cost: ${costs['total_opex_monthly']:,.2f}")
            print(f"Annual Operating Cost: ${costs['total_opex_annual']:,.2f}")
            print(f"{YELLOW}3-Year TCO: ${costs['3_year_tco']:,.2f}{NC}")
            print()

    def export_plan(self, analysis: Dict, output: str):
        """Export capacity plan to JSON"""
        with open(output, 'w') as f:
            json.dump(analysis, f, indent=2)
        print(f"{GREEN}Capacity plan exported to {output}{NC}")


def main():
    parser = argparse.ArgumentParser(description="Capacity Planning for Gryvia")
    parser.add_argument("--forecast-days", type=int, default=90,
                       help="Days to forecast (default: 90)")
    parser.add_argument("--output", help="Export plan to JSON file")

    args = parser.parse_args()

    planner = CapacityPlanner()
    analysis = planner.analyze_capacity(forecast_days=args.forecast_days)

    planner.print_analysis(analysis)

    if args.output:
        planner.export_plan(analysis, args.output)


if __name__ == "__main__":
    main()
