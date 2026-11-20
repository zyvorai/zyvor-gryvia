variable "cluster_name" {
  description = "Name of the Gryvia cluster"
  type        = string
  default     = "gryvia-baremetal"
}

variable "control_plane_endpoint" {
  description = "Control plane endpoint (load balancer or single master IP)"
  type        = string
}

variable "control_nodes" {
  description = "Control plane nodes"
  type = list(object({
    name = string
    ip   = string
  }))
}

variable "gpu_nodes" {
  description = "GPU worker nodes"
  type = list(object({
    name          = string
    ip            = string
    gpu_type      = string # H100, A100, L40, V100, T4
    gpu_count     = number
    rdma_enabled  = bool
    rdma_device   = string # e.g., mlx5_0, ib0
    storage_mount = string # e.g., /mnt/vast
  }))
}

variable "pod_subnet" {
  description = "Pod network CIDR"
  type        = string
  default     = "10.244.0.0/16"

  validation {
    condition     = can(cidrhost(var.pod_subnet, 0))
    error_message = "pod_subnet must be a valid CIDR block (e.g. 10.244.0.0/16)."
  }
}

variable "service_subnet" {
  description = "Service network CIDR"
  type        = string
  default     = "10.96.0.0/12"

  validation {
    condition     = can(cidrhost(var.service_subnet, 0))
    error_message = "service_subnet must be a valid CIDR block (e.g. 10.96.0.0/12)."
  }
}

variable "rdma_subnet" {
  description = "RDMA network CIDR (InfiniBand/RoCE)"
  type        = string
  default     = "192.168.100.0/24"

  validation {
    condition     = can(cidrhost(var.rdma_subnet, 0))
    error_message = "rdma_subnet must be a valid CIDR block."
  }
}

variable "storage_backend" {
  description = "Storage backend (vast, weka, ddn, lustre)"
  type        = string
  default     = "vast"

  validation {
    condition     = contains(["vast", "weka", "ddn", "lustre", "ceph"], var.storage_backend)
    error_message = "storage_backend must be one of: vast, weka, ddn, lustre, ceph."
  }
}

variable "storage_endpoint" {
  description = "Storage cluster endpoint"
  type        = string
}

variable "storage_mountpath" {
  description = "Storage mount path on nodes"
  type        = string
  default     = "/mnt/gryvia-storage"
}

variable "storage_rdma_enabled" {
  description = "Enable RDMA for storage access"
  type        = bool
  default     = true
}

variable "kubernetes_version" {
  description = "Kubernetes version to install"
  type        = string
  default     = "1.28.5"
}

variable "ssh_user" {
  description = "SSH user for node access (avoid root for security)"
  type        = string
  default     = "ubuntu"
}

variable "ssh_private_key_path" {
  description = "Path to SSH private key"
  type        = string
  default     = "~/.ssh/id_rsa"
}
