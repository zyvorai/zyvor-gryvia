# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

"""A stand-in for Gryvia's api-gateway with the response shapes the Sovereign AIOS Zyntra pack reads.

Usage: python3 fake_gryvia_gateway.py PORT TOKEN   (answers 401 unless Authorization is "Bearer TOKEN")
"""
import json
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

DATA = {
    "/api/tenants": {"items": [{"metadata": {"name": "alpha"},
                                "spec": {"namespace": "tenant-alpha", "displayName": "Alpha"},
                                "status": {"health": "Healthy"}}]},
    "/api/nodes": {"items": [{"metadata": {"name": "gpu-0"},
                              "spec": {"nodeName": "gpu-0", "gpuType": "B300", "gpuCount": 8, "interconnect": "nvswitch"},
                              "status": {"phase": "Ready"}}]},
    "/api/jobs": {"items": [{"metadata": {"name": "llama-ft", "namespace": "tenant-alpha"},
                             "spec": {"type": "fine-tuning", "gpus": 8, "gpuType": "B300"},
                             "status": {"phase": "Running"}}]},
    "/api/models": {"items": [{"metadata": {"name": "qwen"}, "spec": {"version": "2.5", "stage": "production"},
                               "status": {"phase": "Ready"}}]},
    "/api/inference": {"items": [{"metadata": {"name": "qwen-svc"},
                                  "spec": {"backend": "vllm", "replicas": 2, "modelRef": "qwen"},
                                  "status": {"phase": "Running"}}]},
    "/api/datasets": {"items": [{"metadata": {"name": "docs", "namespace": "tenant-alpha"},
                                 "status": {"phase": "Ready"}}]},
    "/api/cluster/stats": {"utilizationPercent": 42.5, "pendingJobs": 5, "runningJobs": 1, "availableGPUs": 0},
    "/api/metrics/costs": {"totalCost": 1234.5},
}


def main():
    port, token = int(sys.argv[1]), sys.argv[2]

    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            if self.headers.get("Authorization") != f"Bearer {token}":
                self.send_response(401)
                self.end_headers()
                return
            body = DATA.get(self.path.split("?")[0])
            self.send_response(200 if body else 404)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps(body or {}).encode())

        def log_message(self, *args):
            pass

    ThreadingHTTPServer(("127.0.0.1", port), Handler).serve_forever()


if __name__ == "__main__":
    main()
