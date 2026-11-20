apiVersion: gryvia.io/v1alpha1
kind: GryviaNetwork
metadata:
  name: rdma-fabric
spec:
  mode: rdma
  rdma: true
  bandwidth: "400Gbps"
  fabric: InfiniBand
  rdmaConfig:
    protocol: InfiniBand
    queuePairs: 8
    priority: 7
  cni: multus
---
apiVersion: "k8s.cni.cncf.io/v1"
kind: NetworkAttachmentDefinition
metadata:
  name: rdma-network
  namespace: gryvia-system
spec:
  config: '{
    "cniVersion": "0.3.1",
    "type": "ib-sriov",
    "mode": "rdma",
    "ipam": {
      "type": "host-local",
      "subnet": "${rdma_subnet}",
      "rangeStart": "${cidrhost(rdma_subnet, 10)}",
      "rangeEnd": "${cidrhost(rdma_subnet, 250)}",
      "gateway": "${cidrhost(rdma_subnet, 1)}"
    }
  }'
