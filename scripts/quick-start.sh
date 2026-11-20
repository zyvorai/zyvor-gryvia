#!/bin/bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

# Quick start script for Gryvia
# Deploys a minimal Gryvia setup for development/testing

set -euo pipefail

GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m' # No Color

echo -e "${GREEN}Gryvia Quick Start${NC}"
echo "=================================="

# Check prerequisites
echo -e "\n${YELLOW}Checking prerequisites...${NC}"
command -v kubectl >/dev/null 2>&1 || { echo -e "${RED}kubectl is required${NC}"; exit 1; }
command -v helm >/dev/null 2>&1 || { echo -e "${RED}helm is required${NC}"; exit 1; }

# Check if cluster is accessible
kubectl cluster-info >/dev/null 2>&1 || { echo -e "${RED}Kubernetes cluster is not accessible${NC}"; exit 1; }
echo -e "${GREEN}✓ Prerequisites met${NC}"

# Create namespace
echo -e "\n${YELLOW}Creating gryvia namespace...${NC}"
kubectl create namespace gryvia-system --dry-run=client -o yaml | kubectl apply -f -
echo -e "${GREEN}✓ Namespace created${NC}"

# Install CRDs
echo -e "\n${YELLOW}Installing CRDs...${NC}"
kubectl apply -f crds/
echo -e "${GREEN}✓ CRDs installed${NC}"

# Deploy operators
echo -e "\n${YELLOW}Deploying operators...${NC}"
for operator in gpu-operator ai-operator storage-operator network-operator quota-operator; do
    echo "  - Deploying $operator..."
    kubectl apply -f operators/$operator/config/deployment.yaml
done
echo -e "${GREEN}✓ Operators deployed${NC}"

# Deploy API Gateway
echo -e "\n${YELLOW}Deploying API Gateway...${NC}"
kubectl apply -f manifests/deploy/api-gateway-deployment.yaml
echo -e "${GREEN}✓ API Gateway deployed${NC}"

# Deploy Web UI
echo -e "\n${YELLOW}Deploying Web UI...${NC}"
kubectl apply -f manifests/deploy/ui-deployment.yaml
echo -e "${GREEN}✓ Web UI deployed${NC}"

# Wait for deployments
echo -e "\n${YELLOW}Waiting for deployments to be ready...${NC}"
kubectl wait --for=condition=available --timeout=300s \
    deployment/gryvia-gpu-operator \
    deployment/gryvia-ai-operator \
    deployment/gryvia-storage-operator \
    deployment/gryvia-network-operator \
    deployment/gryvia-quota-operator \
    deployment/gryvia-api-gateway \
    deployment/gryvia-ui \
    -n gryvia-system

echo -e "${GREEN}✓ All deployments ready${NC}"

# Create example resources
echo -e "\n${YELLOW}Creating example resources...${NC}"
kubectl apply -f examples/quota/team-ml-quota.yaml
echo -e "${GREEN}✓ Example quota created${NC}"

# Get Web UI access info
echo -e "\n${GREEN}=================================="
echo "Gryvia is ready!"
echo "==================================${NC}"
echo ""
echo "Access the Web UI:"
echo "  kubectl port-forward -n gryvia-system svc/gryvia-ui 8443:443"
echo "  Then open: https://localhost:8443 (self-signed certificate; accept the browser warning)"
echo ""
echo "Use the CLI:"
echo "  ./cli/target/release/gryvia cluster"
echo ""
echo "Check operator status:"
echo "  kubectl get pods -n gryvia-system"
echo ""
echo "Submit a test job:"
echo "  kubectl apply -f examples/jobs/pytorch-training.yaml"
echo ""
