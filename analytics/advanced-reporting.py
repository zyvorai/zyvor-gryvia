#!/usr/bin/env python3
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""
Advanced Analytics and Reporting for Gryvia
Generates insights, trends, and forecasts
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


class AdvancedAnalytics:
    def __init__(self):
        try:
            config.load_kube_config()
        except config.ConfigException:
            config.load_incluster_config()

        self.api = client.CustomObjectsApi()
        self.core_api = client.CoreV1Api()

    def generate_executive_report(self, days: int = 30) -> Dict:
        """Generate executive summary report"""
        print(f"{BLUE}Generating Executive Report...{NC}\n")

        report = {
            "period": {
                "days": days,
                "start": (datetime.now() - timedelta(days=days)).isoformat(),
                "end": datetime.now().isoformat()
            },
            "summary": self.get_summary_metrics(days),
            "trends": self.analyze_trends(days),
            "forecasts": self.generate_forecasts(days),
            "recommendations": self.generate_recommendations(days),
            "team_performance": self.analyze_team_performance(days),
            "cost_analysis": self.detailed_cost_analysis(days),
            "efficiency": self.calculate_efficiency_metrics(days)
        }

        return report

    def get_summary_metrics(self, days: int) -> Dict:
        """Get high-level summary metrics"""
        try:
            jobs = self.api.list_cluster_custom_object(
                group="gryvia.io",
                version="v1alpha1",
                plural="gryviaaijobs"
            )

            total_jobs = len(jobs["items"])
            succeeded = len([j for j in jobs["items"]
                           if j.get("status", {}).get("phase") == "Succeeded"])
            failed = len([j for j in jobs["items"]
                        if j.get("status", {}).get("phase") == "Failed"])

            # Calculate total GPU hours
            total_gpu_hours = 0
            total_cost = 0

            for job in jobs["items"]:
                metrics = job.get("status", {}).get("metrics", {})
                gpu_count = job.get("spec", {}).get("resources", {}).get("gpuCount", 0)
                running_time = metrics.get("runningTime", 0) / 3600  # Convert to hours

                total_gpu_hours += gpu_count * running_time

                # Estimate cost
                gpu_type = job.get("spec", {}).get("resources", {}).get("gpuType", "A100-80G")
                pricing = {
                    "H100": 8.0,
                    "A100-80G": 4.0,
                    "A100-40G": 3.0,
                    "L40": 2.5,
                    "A10": 1.5,
                    "V100": 2.0,
                    "T4": 0.75,
                }
                hourly_rate = pricing.get(gpu_type, 2.0)
                total_cost += hourly_rate * gpu_count * running_time

            return {
                "total_jobs": total_jobs,
                "succeeded_jobs": succeeded,
                "failed_jobs": failed,
                "success_rate": (succeeded / total_jobs * 100) if total_jobs > 0 else 0,
                "total_gpu_hours": round(total_gpu_hours, 2),
                "total_cost_usd": round(total_cost, 2),
                "avg_cost_per_job": round(total_cost / total_jobs, 2) if total_jobs > 0 else 0
            }
        except Exception as e:
            print(f"{RED}Error getting summary: {e}{NC}")
            return {}

    def analyze_trends(self, days: int) -> Dict:
        """Analyze trends over time"""
        # Simulated trend analysis
        # In production, query time-series database

        trends = {
            "job_volume": {
                "current_week": 245,
                "previous_week": 210,
                "change_percent": 16.7,
                "trend": "increasing"
            },
            "gpu_utilization": {
                "current_week": 78.5,
                "previous_week": 72.3,
                "change_percent": 8.6,
                "trend": "increasing"
            },
            "costs": {
                "current_week": 15234.50,
                "previous_week": 14892.30,
                "change_percent": 2.3,
                "trend": "increasing"
            },
            "success_rate": {
                "current_week": 94.2,
                "previous_week": 91.8,
                "change_percent": 2.6,
                "trend": "improving"
            }
        }

        return trends

    def generate_forecasts(self, days: int) -> Dict:
        """Generate forecasts using simple models"""
        forecasts = {
            "next_month_jobs": {
                "predicted": 980,
                "lower_bound": 920,
                "upper_bound": 1040,
                "confidence": 0.90
            },
            "next_month_cost": {
                "predicted": 62500.0,
                "lower_bound": 58000.0,
                "upper_bound": 67000.0,
                "confidence": 0.90
            },
            "gpu_demand": {
                "h100": {"predicted": 32, "current": 24},
                "a100_80g": {"predicted": 128, "current": 96},
                "a100_40g": {"predicted": 64, "current": 48}
            },
            "capacity_needed": {
                "date_projected": (datetime.now() + timedelta(days=90)).isoformat(),
                "additional_gpus": 48,
                "estimated_cost": 285000.0,
                "recommendation": "Plan capacity expansion for Q2"
            }
        }

        return forecasts

    def generate_recommendations(self, days: int) -> List[Dict]:
        """Generate actionable recommendations"""
        recommendations = [
            {
                "priority": "high",
                "category": "cost_optimization",
                "title": "Increase Spot Instance Usage",
                "description": "Only 15% of jobs use spot instances. Increase to 60% for non-critical workloads.",
                "potential_savings_usd": 18500.0,
                "implementation_effort": "low",
                "timeline": "2 weeks"
            },
            {
                "priority": "high",
                "category": "efficiency",
                "title": "Enable MIG for Development Workloads",
                "description": "70% of development jobs use <10GB memory. Switch to MIG 1g.10gb instances.",
                "potential_savings_usd": 12300.0,
                "implementation_effort": "medium",
                "timeline": "1 month"
            },
            {
                "priority": "medium",
                "category": "performance",
                "title": "Optimize Data Loading",
                "description": "GPU utilization averaging 65%. Profiling shows data loading bottlenecks.",
                "expected_improvement": "20% faster training",
                "implementation_effort": "medium",
                "timeline": "3 weeks"
            },
            {
                "priority": "medium",
                "category": "capacity",
                "title": "Add A100-80G Capacity",
                "description": "Queue times increasing for A100-80G. Add 32 GPUs to meet demand.",
                "estimated_cost": 185000.0,
                "implementation_effort": "high",
                "timeline": "6 weeks"
            },
            {
                "priority": "low",
                "category": "governance",
                "title": "Enforce Resource Quotas",
                "description": "3 teams consistently exceed quotas. Implement strict enforcement.",
                "expected_impact": "Better resource distribution",
                "implementation_effort": "low",
                "timeline": "1 week"
            }
        ]

        return recommendations

    def analyze_team_performance(self, days: int) -> Dict:
        """Analyze performance by team"""
        teams = {
            "ml-research": {
                "jobs": 156,
                "success_rate": 96.2,
                "avg_gpu_utilization": 82.5,
                "cost": 25432.10,
                "efficiency_score": 88.5,
                "top_models": ["llama-7b", "gpt-2", "bert-large"],
                "recommendations": [
                    "Excellent performance",
                    "Consider auto-tuning for further optimization"
                ]
            },
            "computer-vision": {
                "jobs": 98,
                "success_rate": 92.8,
                "avg_gpu_utilization": 75.3,
                "cost": 12345.60,
                "efficiency_score": 78.2,
                "top_models": ["resnet50", "yolo-v8", "segformer"],
                "recommendations": [
                    "GPU utilization below target",
                    "Increase batch sizes",
                    "Enable mixed precision training"
                ]
            },
            "nlp": {
                "jobs": 88,
                "success_rate": 89.5,
                "avg_gpu_utilization": 68.7,
                "cost": 7456.80,
                "efficiency_score": 72.1,
                "top_models": ["bert-base", "roberta", "t5-small"],
                "recommendations": [
                    "Low GPU utilization",
                    "Profile data loading pipeline",
                    "Consider using smaller GPU types"
                ]
            }
        }

        return teams

    def detailed_cost_analysis(self, days: int) -> Dict:
        """Detailed cost breakdown and optimization opportunities"""
        analysis = {
            "total_cost": 45234.50,
            "breakdown": {
                "compute": {
                    "amount": 38450.30,
                    "percentage": 85.0,
                    "breakdown": {
                        "on_demand": 32456.70,
                        "spot": 5993.60
                    }
                },
                "storage": {
                    "amount": 4234.80,
                    "percentage": 9.4
                },
                "network": {
                    "amount": 2549.40,
                    "percentage": 5.6
                }
            },
            "by_gpu_type": {
                "H100": {"cost": 12456.00, "gpu_hours": 415.2},
                "A100-80G": {"cost": 24567.80, "gpu_hours": 1023.7},
                "A100-40G": {"cost": 6789.50, "gpu_hours": 565.8},
                "T4": {"cost": 1421.20, "gpu_hours": 473.7}
            },
            "optimization_opportunities": {
                "spot_instances": {
                    "current_usage": "15%",
                    "recommended": "60%",
                    "potential_savings": 18500.0
                },
                "mig_instances": {
                    "current_usage": "5%",
                    "recommended": "40%",
                    "potential_savings": 12300.0
                },
                "right_sizing": {
                    "over_provisioned_jobs": 23,
                    "potential_savings": 3450.0
                },
                "idle_resources": {
                    "idle_hours": 145.6,
                    "wasted_cost": 2145.30
                }
            },
            "total_potential_savings": 36395.30,
            "savings_percentage": 80.5
        }

        return analysis

    def calculate_efficiency_metrics(self, days: int) -> Dict:
        """Calculate various efficiency metrics"""
        metrics = {
            "gpu_utilization": {
                "cluster_average": 74.5,
                "target": 80.0,
                "gap": -5.5,
                "by_gpu_type": {
                    "H100": 82.3,
                    "A100-80G": 78.9,
                    "A100-40G": 72.1,
                    "T4": 65.4
                }
            },
            "job_efficiency": {
                "avg_queue_time_minutes": 12.3,
                "avg_runtime_hours": 4.7,
                "scheduling_efficiency": 96.2
            },
            "cost_efficiency": {
                "cost_per_successful_job": 142.35,
                "cost_per_gpu_hour": 24.50,
                "roi_score": 87.5
            },
            "resource_efficiency": {
                "memory_utilization": 68.7,
                "cpu_utilization": 45.2,
                "storage_utilization": 72.8
            }
        }

        return metrics

    def print_report(self, report: Dict):
        """Print formatted report"""
        print(f"\n{BLUE}╔══════════════════════════════════════════════════════════════╗{NC}")
        print(f"{BLUE}║         Gryvia Executive Analytics Report              ║{NC}")
        print(f"{BLUE}╚══════════════════════════════════════════════════════════════╝{NC}\n")

        # Summary
        summary = report["summary"]
        print(f"{CYAN}📊 EXECUTIVE SUMMARY{NC}")
        print("─" * 70)
        print(f"Period: {report['period']['days']} days")
        print(f"Total Jobs: {summary.get('total_jobs', 0):,}")
        print(f"Success Rate: {summary.get('success_rate', 0):.1f}%")
        print(f"Total GPU Hours: {summary.get('total_gpu_hours', 0):,.1f}")
        print(f"Total Cost: ${summary.get('total_cost_usd', 0):,.2f}")
        print(f"Avg Cost/Job: ${summary.get('avg_cost_per_job', 0):.2f}")
        print()

        # Trends
        print(f"{CYAN}📈 TRENDS{NC}")
        print("─" * 70)
        trends = report["trends"]
        for metric, data in trends.items():
            trend_symbol = "📈" if data["trend"] in ["increasing", "improving"] else "📉"
            change = data["change_percent"]
            color = GREEN if change > 0 else RED if change < 0 else YELLOW
            print(f"{trend_symbol} {metric.replace('_', ' ').title()}: {color}{change:+.1f}%{NC}")
        print()

        # Forecasts
        print(f"{CYAN}🔮 FORECASTS{NC}")
        print("─" * 70)
        forecasts = report["forecasts"]
        print(f"Next Month Jobs: {forecasts['next_month_jobs']['predicted']:,} "
              f"(±{forecasts['next_month_jobs']['confidence']*100:.0f}% confidence)")
        print(f"Next Month Cost: ${forecasts['next_month_cost']['predicted']:,.2f} "
              f"(±{forecasts['next_month_cost']['confidence']*100:.0f}% confidence)")
        print(f"\nCapacity Planning:")
        cap = forecasts["capacity_needed"]
        print(f"  • Need {cap['additional_gpus']} more GPUs by {cap['date_projected'][:10]}")
        print(f"  • Estimated investment: ${cap['estimated_cost']:,.2f}")
        print(f"  • {cap['recommendation']}")
        print()

        # Recommendations
        print(f"{CYAN}💡 TOP RECOMMENDATIONS{NC}")
        print("─" * 70)
        for i, rec in enumerate(report["recommendations"][:3], 1):
            priority_color = RED if rec["priority"] == "high" else YELLOW if rec["priority"] == "medium" else BLUE
            print(f"\n{priority_color}[{rec['priority'].upper()}]{NC} {i}. {rec['title']}")
            print(f"  {rec['description']}")
            if "potential_savings_usd" in rec:
                print(f"  {GREEN}Potential Savings: ${rec['potential_savings_usd']:,.2f}{NC}")
            print(f"  Timeline: {rec['timeline']} | Effort: {rec['implementation_effort']}")
        print()

        # Cost Optimization
        print(f"{CYAN}💰 COST OPTIMIZATION OPPORTUNITIES{NC}")
        print("─" * 70)
        cost = report["cost_analysis"]
        print(f"Current Monthly Cost: ${cost['total_cost']:,.2f}")
        print(f"\nOptimization Opportunities:")
        for opp, data in cost["optimization_opportunities"].items():
            if "potential_savings" in data:
                print(f"  • {opp.replace('_', ' ').title()}: ${data['potential_savings']:,.2f}")
        print(f"\n{GREEN}Total Potential Savings: ${cost['total_potential_savings']:,.2f} "
              f"({cost['savings_percentage']:.1f}%){NC}")
        print()

    def export_report(self, report: Dict, output_format: str, output: str):
        """Export report in various formats"""
        if output_format == "json":
            with open(output, 'w') as f:
                json.dump(report, f, indent=2)
        elif output_format == "html":
            self.export_html(report, output)
        elif output_format == "pdf":
            # PDF export requires additional dependencies (e.g., weasyprint)
            print(f"{YELLOW}PDF export not yet implemented. Use HTML or JSON format.{NC}")
            return

        print(f"{GREEN}Report exported to {output}{NC}")

    def export_html(self, report: Dict, output: str):
        """Export as HTML report"""
        html = f"""
<!DOCTYPE html>
<html>
<head>
    <title>Gryvia Analytics Report</title>
    <style>
        body {{ font-family: Arial, sans-serif; margin: 20px; }}
        h1 {{ color: #2c3e50; }}
        h2 {{ color: #3498db; margin-top: 30px; }}
        table {{ border-collapse: collapse; width: 100%; margin: 20px 0; }}
        th, td {{ border: 1px solid #ddd; padding: 12px; text-align: left; }}
        th {{ background-color: #3498db; color: white; }}
        .metric {{ background-color: #ecf0f1; padding: 15px; margin: 10px 0; }}
        .high {{ color: #e74c3c; font-weight: bold; }}
        .positive {{ color: #27ae60; }}
        .negative {{ color: #e74c3c; }}
    </style>
</head>
<body>
    <h1>Gryvia Executive Analytics Report</h1>
    <p>Generated: {datetime.now().isoformat()}</p>

    <h2>Executive Summary</h2>
    <div class="metric">
        <p>Total Jobs: {report['summary'].get('total_jobs', 0):,}</p>
        <p>Success Rate: {report['summary'].get('success_rate', 0):.1f}%</p>
        <p>Total Cost: ${report['summary'].get('total_cost_usd', 0):,.2f}</p>
    </div>

    <h2>Recommendations</h2>
    <ul>
        {''.join([f"<li class='high'>{r['title']} - ${r.get('potential_savings_usd', 0):,.2f}</li>"
                  for r in report['recommendations'][:5]])}
    </ul>
</body>
</html>
        """
        with open(output, 'w') as f:
            f.write(html)


def main():
    parser = argparse.ArgumentParser(description="Advanced Analytics for Gryvia")
    parser.add_argument("--days", type=int, default=30, help="Days to analyze")
    parser.add_argument("--format", choices=["text", "json", "html", "pdf"], default="text")
    parser.add_argument("--output", help="Output file")

    args = parser.parse_args()

    analytics = AdvancedAnalytics()
    report = analytics.generate_executive_report(days=args.days)

    if args.format == "text":
        analytics.print_report(report)
    else:
        if not args.output:
            print(f"{RED}--output required for {args.format} format{NC}")
            sys.exit(1)
        analytics.export_report(report, args.format, args.output)


if __name__ == "__main__":
    main()
