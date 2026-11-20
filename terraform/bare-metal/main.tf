terraform {
  required_version = ">= 1.5.0"

  required_providers {
    null = {
      source  = "hashicorp/null"
      version = "~> 3.2"
    }
    local = {
      source  = "hashicorp/local"
      version = "~> 2.4"
    }
  }
}

# Bare Metal GPU Cluster Configuration
# This assumes you have physical servers with:
# - NVIDIA GPUs (H100, A100, L40, etc.)
# - RDMA-capable NICs (InfiniBand or RoCE)
# - High-speed storage access (VAST, Weka, DDN)

locals {
  # Cluster configuration
  cluster_name = var.cluster_name

  # GPU nodes inventory
  gpu_nodes = {
    for idx, node in var.gpu_nodes : node.name => {
      ip            = node.ip
      gpu_type      = node.gpu_type
      gpu_count     = node.gpu_count
      rdma_enabled  = node.rdma_enabled
      rdma_device   = node.rdma_device
      storage_mount = node.storage_mount
    }
  }

  # Control plane nodes
  control_nodes = {
    for idx, node in var.control_nodes : node.name => {
      ip   = node.ip
      role = "control-plane"
    }
  }

  # All nodes
  all_nodes = merge(local.control_nodes, local.gpu_nodes)
}

# Generate Ansible inventory
resource "local_file" "ansible_inventory" {
  filename = "${path.module}/generated/inventory.ini"
  content  = templatefile("${path.module}/templates/inventory.ini.tpl", {
    control_nodes = local.control_nodes
    gpu_nodes     = local.gpu_nodes
    cluster_name  = local.cluster_name
    ssh_user      = var.ssh_user
  })
}

# Generate kubeadm config
resource "local_file" "kubeadm_config" {
  filename = "${path.module}/generated/kubeadm-config.yaml"
  content  = templatefile("${path.module}/templates/kubeadm-config.yaml.tpl", {
    cluster_name     = local.cluster_name
    control_endpoint = var.control_plane_endpoint
    pod_subnet       = var.pod_subnet
    service_subnet   = var.service_subnet
  })
}

# Generate GPU node configuration
resource "local_file" "gpu_node_configs" {
  for_each = local.gpu_nodes

  filename = "${path.module}/generated/gpu-nodes/${each.key}.yaml"
  content = templatefile("${path.module}/templates/gpu-node-config.yaml.tpl", {
    node_name     = each.key
    gpu_type      = each.value.gpu_type
    gpu_count     = each.value.gpu_count
    rdma_enabled  = each.value.rdma_enabled
    rdma_device   = each.value.rdma_device
    storage_mount = each.value.storage_mount
  })
}

# Generate network configuration for RDMA
resource "local_file" "rdma_config" {
  filename = "${path.module}/generated/rdma-config.yaml"
  content = templatefile("${path.module}/templates/rdma-config.yaml.tpl", {
    rdma_nodes = [for name, node in local.gpu_nodes : {
      name   = name
      ip     = node.ip
      device = node.rdma_device
    } if node.rdma_enabled]
    rdma_subnet = var.rdma_subnet
  })
}

# Generate storage configuration
resource "local_file" "storage_config" {
  filename = "${path.module}/generated/storage-config.yaml"
  content = templatefile("${path.module}/templates/storage-config.yaml.tpl", {
    storage_backend   = var.storage_backend
    storage_endpoint  = var.storage_endpoint
    storage_mountpath = var.storage_mountpath
    rdma_enabled      = var.storage_rdma_enabled
  })
}

# Generate deployment script
resource "local_file" "deploy_script" {
  filename        = "${path.module}/generated/deploy.sh"
  file_permission = "0755"
  content = templatefile("${path.module}/templates/deploy.sh.tpl", {
    cluster_name = local.cluster_name
  })
}

# Output cluster configuration
output "cluster_name" {
  value = local.cluster_name
}

output "gpu_nodes" {
  value = {
    for name, node in local.gpu_nodes : name => {
      ip        = node.ip
      gpu_type  = node.gpu_type
      gpu_count = node.gpu_count
      rdma      = node.rdma_enabled
    }
  }
}

output "deployment_instructions" {
  value = <<-EOT
    Gryvia Bare Metal Deployment
    =================================

    1. Review generated configuration:
       ls -la generated/

    2. Install dependencies on your workstation:
       cd ../ansible
       ansible-galaxy collection install -r requirements.yaml

    3. Run the deployment:
       ./generated/deploy.sh

    4. Or deploy manually:
       cd ../ansible
       ansible-playbook -i ../terraform/bare-metal/generated/inventory.ini playbooks/site.yaml

    5. Access your cluster:
       export KUBECONFIG=./generated/kubeconfig
       kubectl get nodes

    6. Verify GPU nodes:
       kubectl get gryviagpunodes
  EOT
}
