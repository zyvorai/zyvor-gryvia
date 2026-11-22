#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

# Example: Monitor cluster status

# Show cluster overview
echo "=== Cluster Overview ==="
gryvia cluster

echo ""
echo "=== GPU Nodes ==="
gryvia list nodes

echo ""
echo "=== Running Jobs ==="
gryvia list jobs

echo ""
echo "=== Team Quotas ==="
gryvia list quotas

echo ""
echo "=== Cost Analysis ==="
gryvia cost

echo ""
echo "=== Health Check ==="
gryvia health
