### Is there an existing issue already for this feature request/idea?

- [x] I have searched for an existing issue, and could not find anything. I believe this is a new feature request to be evaluated.

### What problem is this feature going to solve? Why should it be added?

## Problem Statement

PostgreSQL storage requirements are inherently difficult to predict. Databases grow over time, and running out of disk space causes PostgreSQL crashes, WAL archiving failures, replication breakage, and service outages. Today, CNPG supports manual PVC resizing by updating `spec.storage.size`, but this requires external monitoring, manual intervention, and carries risk of human error or delayed response.

This is a **recurring pain point** across the CNPG community:

- Clusters entering unrecoverable states when disk fills (#9927, #1808)
- Operators stuck in reconciliation loops due to storage pressure (#9885, #9301)
- PVC resize deadlocks when combined with pod spec changes (#9786, #7997)
- WAL volumes filling due to archive failures with no automatic safeguards (#6152, #8791)
- Replicas showing healthy status after I/O errors from storage exhaustion (#7827)

External solutions like [topolvm/pvc-autoresizer](https://github.com/topolvm/pvc-autoresizer) exist but lack PostgreSQL awareness: they cannot distinguish WAL growth from data growth, cannot check archive health, and cannot detect stuck replication slots. Issue #7100 specifically requested per-PVC label support to enable TopoLVM integration, highlighting demand for this capability.

## Proposed Solution

Add **native automatic PVC resizing** to CloudNativePG with PostgreSQL-aware safety mechanisms. The feature monitors disk usage from within instance pods using `statfs()` syscalls, exposes Prometheus metrics, and automatically expands PVCs when configurable thresholds are exceeded. Crucially, it **blocks resize when WAL health is compromised** to prevent masking underlying archive or replication failures.

The configuration uses a **behavior-driven model** inspired by the Kubernetes HorizontalPodAutoscaler v2 scaling behaviors. Rather than treating resize as a static value, expansion is defined as a dynamic behavior constrained by clamping logic and rate-limited by cloud provider realities.

### Key Capabilities

1. **Automatic PVC expansion** for data, WAL, and tablespace volumes
2. **Accurate disk metrics** via filesystem `statfs()` (not K8s spec or SQL functions)
3. **WAL-aware safety logic** that blocks resize when archive/replication is unhealthy
4. **Single-volume safety** requiring explicit risk acknowledgment
5. **Behavior-driven configuration** with clamped expansion steps and rate limiting
6. **Prometheus metrics** for disk usage, capacity, free space, and WAL health
7. **PrometheusRule alerts** for disk pressure, blocked resizes, and budget exhaustion
8. **CSI failure detection** by comparing actual filesystem size vs. requested PVC size

### Example Configuration

```yaml
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: production-db
spec:
  instances: 3
  storage:
    size: 100Gi
    storageClass: fast-ssd
    resize:
      enabled: true
      triggers:
        usageThreshold: 85       # Resize when 85% used
      expansion:
        step: "20%"              # Exponential growth, adapts to volume size
        maxStep: "500Gi"         # Prevents timeout-inducing massive resizes
        limit: "2Ti"             # Hard cap
      strategy:
        maxActionsPerDay: 3      # Leaves 1 slot for manual intervention
  walStorage:
    size: 20Gi
    storageClass: fast-ssd
    resize:
      enabled: true
      triggers:
        usageThreshold: 70
        minAvailable: "5Gi"      # Also resize if < 5Gi free (protects small volumes)
      expansion:
        step: "10Gi"             # Fixed step for WAL (predictable growth)
        limit: "100Gi"
      strategy:
        maxActionsPerDay: 3
        walSafetyPolicy:
          requireArchiveHealthy: true
          maxPendingWALFiles: 50
```

### Why This Must Be Native to CNPG

| External Autoresizer Limitation | Impact on PostgreSQL |
|--------------------------------|---------------------|
| No PostgreSQL awareness | Cannot distinguish WAL growth from data growth |
| No archive health checks | May mask archive failures by growing storage |
| No replication slot awareness | May mask stuck replication slots |
| Requires Prometheus as hard dependency | Additional infrastructure requirement |
| Generic PVC annotations | Doesn't integrate with Cluster CRD |

### Safety: The WAL Foot-Gun

The most critical design consideration: when `walStorage` is not configured, WAL files live inside PGDATA on a single volume. If WAL archiving fails, WAL files accumulate. A naive auto-resizer would grow the volume indefinitely, masking the archive failure until the expansion limit is reached and the archive backlog becomes unrecoverable.

This proposal addresses this with:

- **`acknowledgeWALRisk: true`**: required for single-volume clusters to opt in
- **`requireArchiveHealthy`**: blocks resize if WAL archiving is failing
- **`maxPendingWALFiles`**: blocks resize if too many files await archiving
- **`maxSlotRetentionBytes`**: blocks resize if inactive slots retain too much WAL

## Implementation Phases

| Phase | Scope |
|-------|-------|
| **Phase 1: Metrics Foundation** | `statfs()`-based disk probing, Prometheus metrics, status endpoint updates, basic Grafana panels |
| **Phase 2: Auto-Resize Core** | `ResizeConfiguration` CRD field (triggers/expansion/strategy), resize reconciler with clamping, rate-limit budget, events |
| **Phase 3: WAL Safety** | `WALSafetyPolicy` in strategy block, WAL health evaluation, archive/slot blocking, single-volume acknowledgment |
| **Phase 4: Tablespaces & Polish** | Tablespace auto-resize, complete alerts/dashboard, documentation (including disk shrinking guide), `kubectl cnpg disk status` |

## New Prometheus Metrics

| Metric | Type | Description |
|--------|------|-------------|
| `cnpg_disk_total_bytes` | Gauge | Total volume capacity |
| `cnpg_disk_used_bytes` | Gauge | Used space |
| `cnpg_disk_available_bytes` | Gauge | Available space |
| `cnpg_disk_percent_used` | Gauge | Percentage used |
| `cnpg_disk_at_limit` | Gauge | 1 if volume hit expansion limit |
| `cnpg_disk_resize_budget_remaining` | Gauge | Remaining resize operations in 24h window |
| `cnpg_disk_resize_blocked` | Gauge | 1 if resize blocked (with reason label) |
| `cnpg_disk_resizes_total` | Counter | Total resize operations |
| `cnpg_wal_archive_healthy` | Gauge | 1 if archive is healthy |
| `cnpg_wal_pending_archive_files` | Gauge | Files awaiting archive |
| `cnpg_wal_inactive_slots` | Gauge | Count of inactive slots |

## Non-Goals

- **PVC shrinking**: Kubernetes does not support this. Documentation for reclaiming disk space (restore from backup, pg_dump to a new cluster, logical replication) will ship alongside this feature.
- **Automatic data cleanup**: the operator will not delete data to free space
- **Non-CSI storage**: requires a CSI driver with volume expansion support

## Related Issues

| Issue | Title | Status | Relevance |
|-------|-------|--------|-----------|
| #9927 | Improve handling disk full scenario | Open | Direct motivation: disk full recovery |
| #9885 | Operator stuck in reconciliation loop | Open | Storage pressure causes reconciliation failures |
| #9786 | Invalid PATCH operation with storage and resource resize | Open | PVC resize deadlock |
| #9447 | WAL disk space check fails due to node ephemeral storage | Open | Incorrect disk space detection |
| #9385 | Storage Autoscaling Support | Open | Directly related feature request |
| #9301 | Can't increase storage because CNPG won't operate | Closed | Circular dependency in storage expansion |
| #8791 | WAL disk running out and dealing with it | Open | Documentation gap for WAL exhaustion |
| #8369 | Increase storage above EBS size limit, unrecoverable state | Closed | Limit validation needed |
| #7997 | Pod creation stuck during PVC resize | Open | Resize deadlock |
| #7827 | Replica shows healthy after I/O error from storage exhaustion | Closed | Unhealthy replica detection gap |
| #7505 | Master node pod deleted during disk space increase | Closed | Offline resize disruption |
| #7324 | PVC resize on Azure not properly detected | Closed | CSI failure detection gap |
| #7150 | Detected low-disk space condition | Closed | Manual PVC resize required |
| #7100 | Define unique labels/annotations for each PVC | Closed | TopoLVM autoresizer integration |
| #7064 | [Draft PR] Auto switch cluster to read-only on high disk usage | Open | Complementary feature |
| #6152 | walStorage PVC will not grow | Closed | WAL accumulation from archive lag |
| #5083 | Handling PVC volume shrink by recreating instances | Closed | Volume shrink (not supported by K8s) |
| #4521 | Graceful handling of WAL disk space exhaustion | Closed | WAL fencing implemented |
| #2829 | Cannot rollback from resizing a volume when not allowed | Closed | Irreversible resize operations |
| #1808 | Out of disk space, refusing to create primary instance | Closed | Disk full deadlock |

## Full Design Document

A comprehensive RFC with detailed design, API types, implementation code, and E2E testing strategy is available as a [GitHub Discussion](https://github.com/cloudnative-pg/cloudnative-pg/discussions/9929) for community review.

## Willingness to Contribute

I am interested in contributing this feature and have prepared a detailed design document and E2E testing plan. Looking for feedback on the approach before beginning implementation.

### Describe the solution you'd like

Add native automatic PVC resizing to CloudNativePG via a new `resize` field in `StorageConfiguration`. The feature would monitor disk usage from within instance pods using `statfs()` syscalls, expose Prometheus metrics (`cnpg_disk_*`), and automatically expand data, WAL, and tablespace PVCs when configurable thresholds are exceeded.

The configuration uses a behavior-driven model with three sub-blocks: **triggers** (when to resize, supporting both percentage `usageThreshold` and absolute `minAvailable`), **expansion** (how much to grow, with `step`/`minStep`/`maxStep` clamping to handle volumes from 1Gi to 10Ti safely), and **strategy** (rate limiting via `maxActionsPerDay` to respect cloud provider quotas and reserve manual intervention slots, plus `walSafetyPolicy`).

Critical to this design is WAL-aware safety logic: auto-resize is blocked when WAL archiving is failing or inactive replication slots are retaining too much WAL, preventing the feature from masking underlying issues that could lead to data loss.

See the full RFC and design document in the [companion Discussion](https://github.com/cloudnative-pg/cloudnative-pg/discussions/9929).

### Describe alternatives you've considered

External solutions like [topolvm/pvc-autoresizer](https://github.com/topolvm/pvc-autoresizer) exist but lack PostgreSQL awareness: they cannot distinguish WAL growth from data growth, cannot check archive health, and cannot detect stuck replication slots. Using them with CNPG risks masking archive failures by blindly growing WAL storage. As documented in [topolvm/pvc-autoresizer#346](https://github.com/topolvm/pvc-autoresizer/issues/346) and [topolvm/pvc-autoresizer#348](https://github.com/topolvm/pvc-autoresizer/pull/348), patching arbitrary paths on a Custom Resource to resize a PVC is outside the purview of a generic PVC autoresizer.

SQL-based monitoring (`pg_database_size()`, `pg_tablespace_size()`) is also insufficient as PostgreSQL has no knowledge of underlying volume capacity or free space. Only `statfs()` from within the container provides accurate usage data.

An earlier iteration of this design used a flat configuration with percentage thresholds and time-based cooldowns. Community feedback identified that straight percentages are problematic across different volume scales and that cooldown periods don't map to cloud provider rate limits. The redesigned behavior-driven model with clamping and budget-based rate limiting addresses these concerns.

### Additional context

_No response_

### Backport?

No

### Are you willing to actively contribute to this feature?

Yes

### Code of Conduct

- [x] I agree to follow this project's Code of Conduct
