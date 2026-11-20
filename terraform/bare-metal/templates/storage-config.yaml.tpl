apiVersion: gryvia.io/v1alpha1
kind: GryviaStorage
metadata:
  name: ${storage_backend}-storage
spec:
  backend: ${storage_backend}
  capacity: "1Pi"
  rdma: ${rdma_enabled}
  endpoint: ${storage_endpoint}
  mountOptions:
    - hard
    - nointr
    - timeo=600
    - retrans=2
    %{ if rdma_enabled }
    - rdma
    - port=20049
    %{ endif }
  storageClass:
    name: ${storage_backend}-fast
    reclaimPolicy: Retain
    volumeBindingMode: WaitForFirstConsumer
    allowVolumeExpansion: true
    parameters:
      type: "nfs"
      server: "${storage_endpoint}"
      path: "/gryvia"
  performance:
    tier: hot
    caching: true
    compression: false
    deduplication: false
    encryption: true
