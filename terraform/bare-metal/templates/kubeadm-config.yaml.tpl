apiVersion: kubeadm.k8s.io/v1beta3
kind: ClusterConfiguration
clusterName: ${cluster_name}
controlPlaneEndpoint: "${control_endpoint}:6443"
networking:
  podSubnet: ${pod_subnet}
  serviceSubnet: ${service_subnet}
  dnsDomain: cluster.local
kubernetesVersion: v1.28.5
apiServer:
  extraArgs:
    enable-admission-plugins: NodeRestriction,PodNodeSelector,PodTolerationRestriction
    runtime-config: api/all=true
  certSANs:
    - ${control_endpoint}
    - localhost
    - 127.0.0.1
controllerManager:
  extraArgs:
    bind-address: 0.0.0.0
    allocate-node-cidrs: "true"
    cluster-cidr: ${pod_subnet}
scheduler:
  extraArgs:
    bind-address: 0.0.0.0
---
apiVersion: kubeadm.k8s.io/v1beta3
kind: InitConfiguration
nodeRegistration:
  criSocket: unix:///var/run/containerd/containerd.sock
  taints:
    - effect: NoSchedule
      key: node-role.kubernetes.io/control-plane
---
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
cgroupDriver: systemd
containerLogMaxSize: 50Mi
containerLogMaxFiles: 5
maxPods: 250
systemReserved:
  cpu: 500m
  memory: 1Gi
kubeReserved:
  cpu: 500m
  memory: 1Gi
