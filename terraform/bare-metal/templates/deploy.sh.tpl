#!/bin/bash
set -euo pipefail

CLUSTER_NAME="${cluster_name}"
SCRIPT_DIR="$(cd "$(dirname "$${BASH_SOURCE[0]}")" && pwd)"
ANSIBLE_DIR="$SCRIPT_DIR/../../ansible"

echo "========================================="
echo "Gryvia Bare Metal Deployment"
echo "Cluster: $CLUSTER_NAME"
echo "========================================="

# Check prerequisites
echo "Checking prerequisites..."
command -v ansible-playbook >/dev/null 2>&1 || {
    echo "Error: ansible-playbook is not installed"
    exit 1
}

# Run Ansible playbook
echo "Running Ansible deployment..."
cd "$ANSIBLE_DIR"
ansible-playbook -i "$SCRIPT_DIR/inventory.ini" playbooks/site.yaml

echo ""
echo "========================================="
echo "Deployment Complete!"
echo "========================================="
echo ""
echo "To access your cluster:"
echo "  export KUBECONFIG=$SCRIPT_DIR/kubeconfig"
echo "  kubectl get nodes"
echo ""
echo "To verify GPU nodes:"
echo "  kubectl get gryviagpunodes"
echo ""
echo "To submit a test job:"
echo "  kubectl apply -f $SCRIPT_DIR/../../../examples/simple-training-job.yaml"
echo ""
