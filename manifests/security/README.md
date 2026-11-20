# Gryvia Security Policies

Security configurations for Gryvia platform.

> **Status: example manifests, not applied by the Helm charts and not audited.**
> `helm/gryvia` ships its own RBAC; the files here are optional hardening templates you
> apply by hand and adapt (namespaces, group names, CIDRs). They have not been tested on
> a live cluster in CI. The "Compliance" section below (SOC 2, HIPAA, PCI DSS) lists
> general controls a deployer would need; Gryvia holds no compliance certification or
> attestation. Gryvia's own security posture and known limitations are in the top-level
> `SECURITY.md`.

## Components

### 1. Pod Security Standards (namespace labels)

**File**: `pod-security-policies.yaml`

The file does not define PodSecurityPolicy objects (PSP was removed in Kubernetes 1.25).
It sets Pod Security Admission labels on three namespaces and a ResourceQuota:

- `gryvia-system` (operators): `restricted` enforced
- `gryvia` (UI, API gateway): `baseline` enforced, `restricted` audit/warn
- `gpu-workloads` (GPU training jobs): `privileged` enforced (needed for GPU device
  access and host IPC), `baseline` audit/warn, plus a `gpu-workload-limits`
  ResourceQuota (100 pods, 64 GPUs)

Note that the collector DaemonSet (privileged, hostNetwork) cannot run in a namespace
enforcing `restricted`; see `collector/` and `helm/network-intelligence`.

### 2. Network Policies

**File**: `network-policies.yaml`

Network segmentation for:

#### Operators (targets `gryvia-system` namespace)
- Allow Kubernetes API access
- Allow Prometheus metrics scraping
- Allow webhook traffic
- Deny all other ingress

#### Web UI
- Allow ingress from external
- Allow egress to API Gateway
- Allow egress to Kubernetes API

#### API Gateway
- Allow ingress from Web UI
- Allow egress to Kubernetes API
- Allow egress to Prometheus

#### GPU Jobs (in `gpu-workloads` namespace)
- Allow intra-job communication (distributed training)
- Allow storage access (NFS, object storage)
- HTTPS egress restricted from private IP ranges (10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16)
- DNS egress rule uses AND logic (port AND protocol must both match), not OR
- Deny unnecessary traffic

#### Default Deny
- Deny all ingress by default in the `gryvia-system` namespace (`default-deny-ingress`)
- Explicit allow required for all traffic

### 3. RBAC Policies

**File**: `rbac-policies.yaml`

Four role levels:

#### Platform Admin
- Full access to all resources
- Cluster-wide permissions
- Node management
- RBAC management

#### Team Admin
- Full access to team jobs
- Read access to quotas and nodes
- Pod log access
- PVC management within namespace

#### Team User
- Create and manage own jobs
- Read quotas and nodes
- View own pods and logs
- Limited PVC access

#### Team Viewer
- Read-only access to all resources
- View jobs, quotas, nodes
- View pod status
- No modification permissions

## Deployment

### Deploy all security policies

```bash
kubectl apply -f manifests/security/
```

### Deploy specific policy

```bash
kubectl apply -f manifests/security/pod-security-policies.yaml
kubectl apply -f manifests/security/network-policies.yaml
kubectl apply -f manifests/security/rbac-policies.yaml
```

## Usage

### Assign roles to users

```bash
# Create RoleBinding for team user
kubectl create rolebinding alice-ml-user \
  --clusterrole=gryvia:team-user \
  --user=alice@example.com \
  --namespace=default

# Create RoleBinding for team admin
kubectl create rolebinding bob-ml-admin \
  --clusterrole=gryvia:team-admin \
  --user=bob@example.com \
  --namespace=default
```

### Assign roles to groups

```bash
# Bind role to OIDC group
kubectl create rolebinding ml-research-users \
  --clusterrole=gryvia:team-user \
  --group=ml-research-users \
  --namespace=default
```

### Use service account in jobs

Design sketch, not accepted by the current CRD schema:

```text
apiVersion: gryvia.io/v1alpha1
kind: GryviaAIJob
metadata:
  name: my-job
spec:
  serviceAccountName: gryvia-job-runner
  # ... rest of spec
```

## Security Best Practices

### 1. Principle of Least Privilege

- Users get minimum permissions needed
- Jobs run with restricted ServiceAccounts
- Operators use dedicated ServiceAccounts

### 2. Network Segmentation

- Default deny network policies
- Explicit allow for required traffic
- Isolate control plane from data plane

### 3. Pod Security

- Run as non-root where possible
- Drop unnecessary capabilities
- Use read-only root filesystem
- Enable seccomp profiles

### 4. Secret Management

Store sensitive data in Secrets:

```bash
kubectl create secret generic vast-credentials \
  --from-literal=username=admin \
  --from-literal=password=secret \
  --namespace=gryvia
```

Reference in jobs:

```yaml
env:
  - name: STORAGE_PASSWORD
    valueFrom:
      secretKeyRef:
        name: vast-credentials
        key: password
```

### 5. Image Security

Use signed images:

```yaml
spec:
  image: nvcr.io/nvidia/pytorch:24.01-py3
  imagePullPolicy: Always
  imagePullSecrets:
    - name: nvcr-secret
```

### 6. Audit Logging

Enable audit logs for sensitive operations:

```yaml
apiVersion: audit.k8s.io/v1
kind: Policy
rules:
  - level: RequestResponse
    resources:
      - group: gryvia.io
        resources: ["gryviaaijobs", "gryviaquotas"]
```

### 7. Resource Quotas

Set namespace quotas:

```yaml
apiVersion: v1
kind: ResourceQuota
metadata:
  name: compute-quota
  namespace: ml-research
spec:
  hard:
    requests.nvidia.com/gpu: "32"
    limits.memory: "1Ti"
    persistentvolumeclaims: "50"
```

## Authentication Integration

### OIDC Integration

Configure API server for OIDC:

```yaml
--oidc-issuer-url=https://accounts.google.com
--oidc-client-id=kubernetes
--oidc-username-claim=email
--oidc-groups-claim=groups
```

### LDAP Integration

Use a webhook for LDAP:

```yaml
--authentication-token-webhook-config-file=/etc/kubernetes/ldap-webhook.yaml
```

### Certificate-based Auth

Create user certificates:

```bash
# Generate user key
openssl genrsa -out alice.key 2048

# Create CSR
openssl req -new -key alice.key -out alice.csr -subj "/CN=alice/O=ml-research"

# Approve and issue certificate
kubectl certificate approve alice

# Create kubeconfig
kubectl config set-credentials alice \
  --client-certificate=alice.crt \
  --client-key=alice.key
```

## Compliance

### SOC 2 Compliance

- Audit logging enabled
- RBAC enforced
- Network policies active
- Encryption in transit (TLS)
- Encryption at rest (for storage)

### HIPAA Compliance

- Data encryption required
- Access logging enabled
- Role-based access control
- Network isolation
- Audit trails

### PCI DSS Compliance

- Secure authentication
- Network segmentation
- Access control lists
- Regular security updates
- Audit logs retention

## Monitoring Security

### Check RBAC permissions

```bash
# Check user permissions
kubectl auth can-i create gryviaaijobs --as alice@example.com

# List all permissions for user
kubectl auth can-i --list --as alice@example.com
```

### Audit network policies

```bash
# List all network policies
kubectl get networkpolicies -A

# Verify policy is applied
kubectl describe networkpolicy gryvia-operators-policy
```

### Review Pod Security labels

```bash
kubectl get ns --show-labels | grep pod-security
```

## Incident Response

### Security Incident Checklist

1. **Detect**: Monitor alerts, logs, audit trails
2. **Contain**: Isolate affected resources
3. **Investigate**: Analyze logs, check access patterns
4. **Remediate**: Patch vulnerabilities, revoke access
5. **Document**: Record incident details
6. **Review**: Update policies and procedures

### Emergency Access Revocation

```bash
# Revoke user access
kubectl delete rolebinding alice-ml-user

# Disable ServiceAccount
kubectl patch serviceaccount gryvia-job-runner \
  -p '{"secrets": []}'

# Block pod network access (requires you to author a NetworkPolicy that selects
# this label; none of the shipped policies does)
kubectl label pod <pod-name> network-policy=deny
```

## Support

For security issues:
- Report privately through GitHub (Security > Report a vulnerability); see `SECURITY.md`
- Include: Description, impact, reproduction steps
- No response-time SLA is committed to; see `SECURITY.md`
