# Disaster Recovery

Comprehensive disaster recovery and business continuity for Gryvia.

> **Status: design document; automated DR is not implemented.** Nothing in this repo runs continuous backup,
> replication, automatic failover or DR drills (the `GryviaDRTest` CRD, which never had a controller, was removed). The
> `BackupPolicy`, `DataReplication`, `MultiRegionConfig` and `DRDrill` kinds do not exist, the `kfctl` CLI
> used throughout does not exist (the `gryvia` CLI has no `backup`, `restore` or `dr` command), and the
> `gryvia_backup_*` / replication metrics are not exported. The RPO/RTO tiers are targets you would have
> to design and test, not measured or promised figures. What does exist is `scripts/backup-crs.sh`, a manual
> kubectl-based export/restore of every `gryvia.io` custom resource (no Secrets, namespaces or CRDs; see
> `website/docs/guides/OPERATIONS.md`). Its round trip is exercised on a kind cluster by `scripts/upgrade-test.sh`
> in CI. It does not back up checkpoints, datasets, PVC contents or model artifacts, and a restore has not been
> validated on a production cluster. Use a proper cluster backup tool (for example Velero
> plus your storage vendor's replication) for real DR.

## Overview

```
┌──────────────────────────────────────────────────────┐
│              Primary Cluster (us-west-2)              │
│  ┌────────────┐  ┌────────────┐  ┌────────────┐    │
│  │    Jobs    │  │   Data     │  │  Models    │    │
│  └────────────┘  └────────────┘  └────────────┘    │
└────────────┬──────────────┬──────────────┬──────────┘
             │              │              │
          Continuous    Replication    Async Sync
          Backup           │              │
             │              ↓              ↓
             │     ┌──────────────────────────────────┐
             │     │   Secondary Cluster (us-east-1)   │
             │     │  ┌────────────┐  ┌────────────┐  │
             ↓     │  │   Data     │  │  Models    │  │
        ┌─────────┐│  └────────────┘  └────────────┘  │
        │ Backup  ││                                    │
        │ Storage ││  Status: Standby / Active-Passive │
        └─────────┘└────────────────────────────────────┘
```

## RPO and RTO Targets

| Tier | RPO | RTO | Cost | Use Case |
|------|-----|-----|------|----------|
| **Platinum** | <5 min | <15 min | High | Critical production |
| **Gold** | <30 min | <1 hour | Medium | Standard production |
| **Silver** | <4 hours | <8 hours | Low | Development |
| **Bronze** | <24 hours | <24 hours | Minimal | Testing |

## Components

### 1. Continuous Backup

Design sketch, not accepted by the current CRD schema:

```text
apiVersion: gryvia.io/v1alpha1
kind: BackupPolicy
metadata:
  name: production-backup
  namespace: gryvia-system
spec:
  tier: platinum

  # Backup schedule
  schedule:
    full: "0 2 * * 0"      # Weekly full backup
    incremental: "0 */4 * * *"  # Every 4 hours

  # What to backup
  resources:
    - gryviaaijobs
    - gryviaquotas
    - gryviagpunodes
    - configmaps
    - secrets
    - persistentvolumeclaims

  # Where to backup
  storage:
    type: s3
    bucket: gryvia-backups-us-west-2
    region: us-west-2
    encryption: AES256

  # Retention policy
  retention:
    daily: 7
    weekly: 4
    monthly: 12
    yearly: 3

  # Validation
  verification:
    enabled: true
    samplePercent: 10  # Verify 10% of backups
```

### 2. Data Replication

Design sketch, not accepted by the current CRD schema:

```text
apiVersion: gryvia.io/v1alpha1
kind: DataReplication
metadata:
  name: primary-to-secondary
spec:
  source:
    cluster: us-west-2
    namespace: default

  destination:
    cluster: us-east-1
    namespace: default

  # Replication mode
  mode: async  # or sync for synchronous

  # What to replicate
  resources:
    - type: pvc
      selector:
        tier: production
    - type: configmap
      selector:
        replicate: "true"

  # Replication schedule
  schedule:
    - every: 15m
      type: incremental
    - every: 24h
      type: full

  # Bandwidth limit
  bandwidth:
    maxMbps: 1000
```

### 3. Multi-Region Setup

Design sketch, not accepted by the current CRD schema:

```text
apiVersion: gryvia.io/v1alpha1
kind: MultiRegionConfig
metadata:
  name: dr-config
spec:
  regions:
    - name: us-west-2
      role: primary
      priority: 1

    - name: us-east-1
      role: secondary
      priority: 2

    - name: eu-west-1
      role: tertiary
      priority: 3

  # Failover policy
  failover:
    automatic: true
    healthCheckInterval: 30s
    failureThreshold: 3

    # Failback when primary recovers
    autoFailback: false
    manualApproval: true
```

## Backup Procedures

### Automated Backup

```bash
# Backup runs automatically via CronJob
# View backup status
kfctl backup status

# List recent backups
kfctl backup list --days 7

# Verify backup integrity
kfctl backup verify backup-20240115-020000
```

### Manual Backup

```bash
# Full backup
kfctl backup create --type full --name manual-backup-$(date +%Y%m%d)

# Incremental backup
kfctl backup create --type incremental

# Backup specific namespace
kfctl backup create --namespace production

# Export to external storage
kfctl backup export backup-20240115 \
  --destination s3://dr-backups/gryvia/
```

## Recovery Procedures

### Complete Cluster Recovery

```bash
# 1. Provision new cluster
# 2. Install Gryvia
helm install gryvia gryvia/gryvia -n gryvia-system

# 3. Restore from backup
kfctl restore \
  --backup s3://backups/gryvia-20240115-020000.tar.gz \
  --verify

# 4. Verify restoration
kfctl cluster verify

# 5. Resume operations
kfctl cluster resume
```

### Partial Recovery

```bash
# Restore specific resources
kfctl restore \
  --backup gryvia-20240115 \
  --resources gryviaaijobs,gryviaquotas \
  --namespace default

# Restore single job
kfctl restore \
  --backup gryvia-20240115 \
  --resource gryviaaijob/training-job-123

# Restore to different namespace
kfctl restore \
  --backup gryvia-20240115 \
  --target-namespace recovery
```

### Point-in-Time Recovery

```bash
# Restore to specific timestamp
kfctl restore \
  --point-in-time "2024-01-15T10:30:00Z" \
  --namespace default

# List available restore points
kfctl restore points --days 7
```

## Failover Procedures

### Automatic Failover

Triggers automatically when:
- Primary cluster unreachable for 90 seconds
- Primary cluster health check fails 3 times
- Critical component failure detected

```bash
# View failover status
kfctl dr status

# Manual failover trigger
kfctl dr failover \
  --from us-west-2 \
  --to us-east-1 \
  --reason "Primary cluster maintenance"

# Verify failover
kfctl dr verify
```

### Failback Procedure

```bash
# 1. Verify primary cluster is healthy
kfctl cluster health us-west-2

# 2. Sync data from secondary to primary
kfctl dr sync \
  --from us-east-1 \
  --to us-west-2 \
  --verify

# 3. Failback (requires approval)
kfctl dr failback \
  --to us-west-2 \
  --require-approval

# 4. Verify failback
kfctl dr verify
```

## Disaster Scenarios

### Scenario 1: Data Center Failure

**Detection:**
- Primary cluster unreachable
- Health checks failing
- Network partitioning detected

**Automatic Response:**
1. Promote secondary cluster to primary
2. Update DNS to point to secondary
3. Resume job scheduling on secondary
4. Notify operations team

**Manual Steps:**
```bash
# Verify secondary is active
kfctl cluster status us-east-1

# Check job continuity
kfctl jobs list --all

# Monitor for issues
kfctl cluster health --watch
```

**Recovery:**
```bash
# Once primary recovers
kfctl dr sync --from us-east-1 --to us-west-2
kfctl dr failback --to us-west-2
```

### Scenario 2: Data Corruption

**Detection:**
- Checksum validation fails
- Database inconsistencies
- Application errors

**Response:**
```bash
# 1. Identify corruption timeframe
kfctl backup verify --range 24h

# 2. Find last good backup
kfctl backup list --verified

# 3. Restore from last good backup
kfctl restore \
  --backup gryvia-20240114-020000 \
  --point-in-time "2024-01-14T23:30:00Z"

# 4. Verify data integrity
kfctl verify data-integrity
```

### Scenario 3: Ransomware Attack

**Detection:**
- Unusual encryption activity
- Mass file modifications
- Suspicious access patterns

**Response:**
```bash
# 1. Immediately isolate cluster
kfctl security isolate --reason ransomware

# 2. Preserve evidence
kfctl security snapshot --forensics

# 3. Restore from immutable backup
kfctl restore \
  --backup s3://immutable-backups/pre-attack \
  --verify-clean

# 4. Security audit
kfctl security audit --full
```

### Scenario 4: Accidental Deletion

**Detection:**
- Resources deleted unexpectedly
- Audit logs show deletion events

**Response:**
```bash
# 1. Check recent deletions
kfctl audit deletions --hours 24

# 2. Restore deleted resources
kfctl restore \
  --resource gryviaaijob/critical-training \
  --timestamp "2024-01-15T14:30:00Z"

# 3. Verify restoration
kubectl get gryviaaijob critical-training
```

## Testing DR Procedures

### Regular DR Drills

Design sketch, not accepted by the current CRD schema:

```text
apiVersion: gryvia.io/v1alpha1
kind: DRDrill
metadata:
  name: quarterly-dr-drill
spec:
  schedule: "0 10 1 */3 *"  # First day of quarter

  scenarios:
    - name: cluster-failover
      duration: 2h
      steps:
        - simulateOutage: primary
        - verifyFailover: secondary
        - testJobContinuity: true
        - failback: primary

    - name: data-corruption
      duration: 1h
      steps:
        - corruptData: sample
        - detectCorruption: true
        - restoreFromBackup: true
        - verifyIntegrity: true

  notifications:
    - type: email
      recipients: [dr-team@company.com]
    - type: slack
      channel: "#dr-alerts"
```

### Test Checklist

- [ ] Backup creation successful
- [ ] Backup verification passes
- [ ] Restore completes within RTO
- [ ] Data integrity verified
- [ ] Jobs resume successfully
- [ ] Failover automatic and transparent
- [ ] Failback successful
- [ ] Documentation updated
- [ ] Team trained on procedures

## Monitoring

### Backup Health

```prometheus
# Backup success rate
gryvia_backup_success_rate 99.8

# Last successful backup age
gryvia_backup_last_success_hours 4.2

# Backup size trend
gryvia_backup_size_gb{type="full"} 485
gryvia_backup_size_gb{type="incremental"} 23

# Verification failures
gryvia_backup_verification_failures_total 0
```

### Replication Lag

```prometheus
# Replication lag in seconds
gryvia_replication_lag_seconds{
  source="us-west-2",
  destination="us-east-1"
} 45

# Replication throughput
gryvia_replication_throughput_mbps 850

# Failed replications
gryvia_replication_failures_total 2
```

### DR Readiness Score

```bash
# Check DR readiness
kfctl dr readiness

# Output:
# DR Readiness Score: 95/100
#
# ✓ Backups current (last: 2h ago)
# ✓ Replication lag: 45 seconds (target: <5 min)
# ✓ Secondary cluster healthy
# ✓ Failover tested (last: 30 days ago)
# ⚠ DR drill overdue (90 days since last drill)
#
# Recommendations:
# - Schedule DR drill
# - Update runbooks
```

## Cost Optimization

### Tiered Backup Storage

```yaml
storage:
  # Hot tier: Recent backups
  hot:
    class: s3-standard
    retention: 7d

  # Warm tier: Last 30 days
  warm:
    class: s3-ia
    retention: 30d

  # Cold tier: Long-term retention
  cold:
    class: s3-glacier
    retention: 1y
```

### Backup Deduplication

```yaml
deduplication:
  enabled: true
  algorithm: sha256
  expectedSavings: 60%  # Typical 60% reduction
```

### Compression

```yaml
compression:
  enabled: true
  algorithm: zstd
  level: 3  # Balance speed vs ratio
```

## Compliance

### Regulatory Requirements

```yaml
compliance:
  # SOC 2
  soc2:
    backupRetention: 7y
    encryptionAtRest: required
    accessLogging: required

  # HIPAA
  hipaa:
    backupRetention: 6y
    encryptionStandard: AES-256
    accessControl: strict
    auditTrail: comprehensive

  # GDPR
  gdpr:
    dataLocality: eu-only
    rightToErasure: supported
    breachNotification: 72h
```

### Audit Trail

```bash
# View backup audit trail
kfctl audit backups --days 90

# Export for compliance
kfctl audit export \
  --start 2024-01-01 \
  --end 2024-12-31 \
  --format pdf \
  --output annual-backup-audit.pdf
```

## Best Practices

1. **Test Regularly**: Run DR drills quarterly
2. **Verify Backups**: Always verify backup integrity
3. **Document Procedures**: Keep runbooks updated
4. **Monitor Continuously**: Set up alerts for backup failures
5. **Encrypt Everything**: Use encryption at rest and in transit
6. **Multi-Region**: Use at least 2 regions for critical data
7. **Immutable Backups**: Enable immutability for ransomware protection
8. **Automate**: Automate as much as possible
9. **Train Team**: Ensure team knows procedures
10. **Review Costs**: Optimize storage tiers regularly

## Runbooks

See `disaster-recovery/runbooks/` for detailed procedures:
- Complete cluster recovery
- Regional failover
- Data corruption recovery
- Ransomware response
- Partial restoration
- DR drill procedures

## Support

- DR Issues: https://github.com/zyvorai/gryvia/issues
- Emergency Hotline: [Configure your support line]
- DR Slack: #disaster-recovery
