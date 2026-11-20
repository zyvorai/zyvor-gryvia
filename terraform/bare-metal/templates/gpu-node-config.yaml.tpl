apiVersion: gryvia.io/v1alpha1
kind: GryviaGpuNode
metadata:
  name: ${node_name}
spec:
  nodeName: ${node_name}
  gpuType: ${gpu_type}
  gpuCount: ${gpu_count}
  rdma: ${rdma_enabled}
  sriov: true
  interconnect: NVLink
  %{ if rdma_enabled }
  bandwidth: "400Gbps"
  %{ endif }
  healthCheck:
    enabled: true
    intervalSeconds: 60
  labels:
    gryvia.io/gpu: "${gpu_type}"
    gryvia.io/gpu-count: "${gpu_count}"
    gryvia.io/rdma: "${rdma_enabled}"
    %{ if rdma_enabled }
    gryvia.io/rdma-device: "${rdma_device}"
    %{ endif }
