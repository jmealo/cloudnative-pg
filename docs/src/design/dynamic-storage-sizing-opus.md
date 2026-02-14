# RFC: Dynamic PVC Sizing with Maintenance Windows for CloudNativePG

| Field | Value |
|-------|-------|
| **Author** | Jeff Mealo |
| **Status** | Draft / Request for Comments |
| **Created** | 2026-02-08 |
| **Target Release** | TBD |
| **Supersedes** | [RFC: Automatic PVC Resizing with WAL-Aware Safety](pvc-autoresize.md) |

---

## Summary

This RFC proposes **dynamic PVC sizing** for CloudNativePG using a **request/limit model** inspired by Kubernetes resource management. Instead of a static size, operators define:

- **Request**: The minimum storage the cluster needs (floor)
- **Limit**: The maximum storage the cluster can consume (ceiling)

The operator automatically manages storage within this range, growing when usage approaches capacity and shrinking when usage drops significantly. This approach:

1. **Uses familiar Kubernetes semantics** (request/limit)
2. **Supports both growth AND shrinkage** through replica recreation
3. **Uses maintenance windows** for non-urgent resize operations
4. **Reserves emergency capacity** for out-of-window urgent resizes
5. **Updates the Cluster spec** to keep all replicas consistent

This approach:
- Eliminates the GitOps drift problem (spec reflects reality)
- Ensures new replicas have correct sizing immediately
- Provides a clear path to shrink when you over-provision by mistake

**Important:** Shrinking is NOT automatic day-to-day management. Shrinking a volume—even by 1GB on a 1TB database—requires creating new PVCs and copying all data. It's a major operation. The shrink capability exists for **correcting mistakes**, not routine cost optimization.

---

## Motivation

### Problems with the Auto-Resize Approach

The [auto-resize RFC](pvc-autoresize.md) solves disk exhaustion but creates new problems:

**1. Cluster Spec Drift**

Auto-resize patches PVCs directly without updating `spec.storage.size`. After several resize operations:

```
Cluster CR:     spec.storage.size: 10Gi
Actual PVCs:    primary=45Gi, replica1=45Gi, replica2=45Gi
```

This creates confusion: the spec declares 10Gi but all PVCs are 45Gi.

**2. New Replica Size Mismatch**

When scaling up or recreating a replica:

```
1. Operator reads spec.storage.size (10Gi)
2. Creates new PVC with 10Gi
3. New replica immediately triggers auto-resize
4. Resize consumes a daily quota slot
5. Until resize completes, replica has less space than primary
```

This is particularly problematic for:
- Volume snapshot-based cloning (snapshot is 45Gi, PVC spec is 10Gi)
- Rapid scale-up scenarios where multiple replicas need resize
- Rate-limited cloud providers where resize slots are precious

**3. GitOps Incompatibility**

GitOps tools (ArgoCD, Flux) see the PVC size differ from spec but cannot reconcile it because:
- Updating spec to match PVC would be "reverse drift"
- The spec represents user intent, which was 10Gi
- There's no way to express "10Gi minimum, grow as needed"

**4. No Path to Shrink When You Mess Up**

Once a PVC grows, there's no supported way to shrink it. If someone:
- Accidentally provisions 1TB when they needed 100GB
- Has auto-resize grow storage during a temporary spike
- Inherits an over-provisioned cluster

...they're stuck with the over-provisioned storage forever. The only workaround today is full cluster recreation from backup, which is undocumented and error-prone.

**5. Cloud Provider Rate Limits**

Reactive auto-resize can exhaust daily modification quotas on small volumes, leaving no budget for emergencies. The rate-limiting in the auto-resize RFC mitigates this but doesn't solve the fundamental problem: reactive resizing is inherently quota-inefficient.

### The Dynamic Sizing Vision

Instead of "grow when threshold exceeded," dynamic sizing uses the familiar Kubernetes request/limit model:

```yaml
storage:
  request: 10Gi      # Minimum storage (floor) - can't go below this
  limit: 1Ti         # Maximum storage (ceiling) - won't grow beyond this
  buffer: 20%        # Maintain 20% free space
```

**For growth (automatic):**
- Operator monitors disk usage
- When free space drops below buffer%, grows storage (up to limit)
- Updates `spec.storage.currentSize` so new replicas get the right size
- Respects maintenance windows and cloud provider rate limits

**For shrink (intentional, when you need it):**
- Operator provides `kubectl cnpg storage shrink` command
- Estimates time and shows what will happen
- Executes as a rolling operation during maintenance window
- Won't shrink below `request`

This addresses the three rough edges in CNPG storage management:
1. **Avoid running out of disk** → auto-grow with limits
2. **Shrink when you mess up** → intentional shrink operation
3. **Recover from out of disk** → emergency resize procedures

---

## Design Principles

### 1. Request/Limit Semantics

Use familiar Kubernetes terminology:
- **Request**: The minimum guaranteed storage (floor). The operator will never shrink below this.
- **Limit**: The maximum allowed storage (ceiling). The operator will never grow beyond this.
- **Buffer**: The free space percentage to maintain within the request/limit range.

This maps directly to how Kubernetes manages CPU and memory, making the mental model intuitive.

### 2. Cluster Spec as Source of Truth

Dynamic sizing updates the Cluster spec (not just PVCs). The Cluster CR always reflects the current intended size. GitOps tools can reconcile cleanly.

### 3. Growth is Automatic, Shrink is Intentional

- **Growth**: Automatic via CSI volume expansion when buffer is violated (same as today, but updates spec)
- **Shrinkage**: Manual/intentional operation for correcting over-provisioning mistakes

Shrinking is a major operation (full data copy) regardless of how much you shrink. It exists to fix mistakes, not for routine cost optimization. The `request` field sets a floor—the operator will never shrink below it, even manually.

### 4. Maintenance Windows

Non-urgent operations (proactive growth, all shrinkage) happen during defined maintenance windows. This:
- Consolidates operations to reduce API quota consumption
- Enables planned capacity changes during low-traffic periods
- Gives operators predictable timing for storage operations

### 5. Emergency Capacity Reserve

A portion of the daily resize budget is reserved for emergencies. If disk usage spikes outside a maintenance window, the operator can still resize—but only from the emergency reserve.

---

## API Design

### Core Fields (v1)

```go
type StorageConfiguration struct {
    // Size is the static storage size. Mutually exclusive with Request/Limit.
    // When set alone, storage is fixed at this size (current behavior).
    // +optional
    Size string `json:"size,omitempty"`

    // Request is the minimum provisioned size (floor).
    // The operator will never shrink below this value.
    // When set with Limit, enables dynamic sizing.
    // +optional
    Request string `json:"request,omitempty"`

    // Limit is the maximum provisioned size (ceiling).
    // The operator will never grow beyond this value.
    // When set with Request, enables dynamic sizing.
    // +optional
    Limit string `json:"limit,omitempty"`

    // TargetBuffer is the desired free space band the controller maintains
    // between request and limit. Expressed as a percentage (5-50%).
    // When free space drops below this, the operator grows storage.
    // +kubebuilder:validation:Minimum=5
    // +kubebuilder:validation:Maximum=50
    // +kubebuilder:default:=20
    // +optional
    TargetBuffer *int `json:"targetBuffer,omitempty"`

    // MaintenanceWindow defines when non-urgent grow/shrink operations occur.
    // If not set, growth happens immediately when triggered.
    // +optional
    MaintenanceWindow *MaintenanceWindowConfig `json:"maintenanceWindow,omitempty"`

    // EmergencyGrow controls growth outside the maintenance window.
    // Emergency growth still respects Limit unless ExceedLimitOnEmergency is set.
    // +optional
    EmergencyGrow *EmergencyGrowConfig `json:"emergencyGrow,omitempty"`

    // CurrentSize is the current target size, managed by the operator when
    // dynamic sizing is enabled. This field should not be set manually.
    // New replicas are created with this size.
    // +optional
    CurrentSize string `json:"currentSize,omitempty"`

    // Existing fields (unchanged)...
    StorageClass       *string `json:"storageClass,omitempty"`
    ResizeInUseVolumes *bool   `json:"resizeInUseVolumes,omitempty"`
}
```

### Configuration Modes

| Fields Set | Mode | Behavior |
|------------|------|----------|
| `size` only | **Static** | Fixed size, no auto-management (current behavior) |
| `request` + `limit` | **Dynamic** | Operator manages size within range |
| `size` + `request` + `limit` | **Invalid** | Webhook rejects |

### WAL Safety (Phase 2)

WAL volumes need additional safety checks to prevent masking archive/replication failures:

```go
type WALSafetyPolicy struct {
    // AcknowledgeWALRisk must be true for single-volume clusters.
    // +optional
    AcknowledgeWALRisk bool `json:"acknowledgeWALRisk,omitempty"`

    // RequireArchiveHealthy blocks resize if WAL archiving is failing.
    // +kubebuilder:default:=true
    RequireArchiveHealthy *bool `json:"requireArchiveHealthy,omitempty"`

    // MaxPendingWALFiles blocks resize if too many files await archiving.
    // +kubebuilder:default:=100
    MaxPendingWALFiles *int `json:"maxPendingWALFiles,omitempty"`

    // MaxSlotRetentionBytes blocks resize if inactive slots retain too much WAL.
    // +optional
    MaxSlotRetentionBytes *int64 `json:"maxSlotRetentionBytes,omitempty"`
}
```

These checks are deferred to Phase 2 because:
- They add significant complexity
- They're only needed for WAL volumes (or single-volume clusters)
- Phase 1 delivers value for data/tablespace volumes without them

### `MaintenanceWindowConfig`

```go
type MaintenanceWindowConfig struct {
    // Schedule defines when non-urgent operations can occur.
    // Uses cron syntax: "minute hour day-of-month month day-of-week"
    // Examples:
    //   "0 2 * * *"     - Daily at 2:00 AM
    //   "0 3 * * 0"     - Sundays at 3:00 AM
    //   "0 1 * * 1-5"   - Weekdays at 1:00 AM
    // +kubebuilder:default:="0 3 * * *"
    Schedule string `json:"schedule,omitempty"`

    // Duration is how long the maintenance window stays open.
    // +kubebuilder:default:="2h"
    Duration string `json:"duration,omitempty"`

    // Timezone for interpreting the schedule.
    // +kubebuilder:default:="UTC"
    Timezone string `json:"timezone,omitempty"`
}
```

### `EmergencyGrowConfig`

```go
type EmergencyGrowConfig struct {
    // Enabled allows emergency growth outside maintenance windows.
    // Emergency growth fires when free space drops below CriticalThreshold.
    // +kubebuilder:default:=true
    Enabled *bool `json:"enabled,omitempty"`

    // CriticalThreshold is the usage percentage that triggers emergency growth.
    // +kubebuilder:validation:Minimum=80
    // +kubebuilder:validation:Maximum=99
    // +kubebuilder:default:=95
    CriticalThreshold int `json:"criticalThreshold,omitempty"`

    // CriticalMinimumFree triggers emergency growth when free space drops below this.
    // +kubebuilder:default:="1Gi"
    CriticalMinimumFree string `json:"criticalMinimumFree,omitempty"`

    // ExceedLimitOnEmergency allows emergency growth to exceed Limit as a last resort.
    // Use with caution - this can lead to unexpected costs.
    // +kubebuilder:default:=false
    ExceedLimitOnEmergency *bool `json:"exceedLimitOnEmergency,omitempty"`

    // MaxActionsPerDay limits total resize operations per 24-hour window.
    // Reserves some budget for emergencies vs. planned operations.
    // +kubebuilder:default:=4
    MaxActionsPerDay *int `json:"maxActionsPerDay,omitempty"`
}
```

### `EmergencyResizePolicy`

```go
type EmergencyResizePolicy struct {
    // Enabled allows emergency resizes outside maintenance windows.
    // Emergency resizes only fire when free space drops below CriticalThreshold.
    // +kubebuilder:default:=true
    Enabled *bool `json:"enabled,omitempty"`

    // CriticalThreshold is the usage percentage that triggers emergency resize.
    // Unlike regular buffer maintenance, this is a reactive threshold for true emergencies.
    // +kubebuilder:validation:Minimum=80
    // +kubebuilder:validation:Maximum=99
    // +kubebuilder:default:=95
    CriticalThreshold int `json:"criticalThreshold,omitempty"`

    // CriticalMinimumFree triggers emergency resize when free space drops below this value.
    // +kubebuilder:default:="1Gi"
    CriticalMinimumFree string `json:"criticalMinimumFree,omitempty"`

    // ReservedActionsPerDay is the portion of daily budget reserved for emergencies.
    // For example, if cloud provider allows 4 modifications and this is 1,
    // then 3 are for planned operations and 1 is reserved for emergencies.
    // +kubebuilder:validation:Minimum=0
    // +kubebuilder:validation:Maximum=10
    // +kubebuilder:default:=1
    ReservedActionsPerDay int `json:"reservedActionsPerDay,omitempty"`

    // MaxActionsPerDay is the total budget including reserved actions.
    // +kubebuilder:validation:Minimum=1
    // +kubebuilder:validation:Maximum=10
    // +kubebuilder:default:=4
    MaxActionsPerDay int `json:"maxActionsPerDay,omitempty"`

    // EmergencyGrowthStep is how much to grow during an emergency.
    // Should be larger than regular growth to create more breathing room.
    // +kubebuilder:default:="25%"
    EmergencyGrowthStep string `json:"emergencyGrowthStep,omitempty"`
}
```

---

## Example Configurations

### Minimal (Just the Essentials)

```yaml
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: simple-db
spec:
  instances: 3

  storage:
    storageClass: gp3
    request: 10Gi      # Floor
    limit: 100Gi       # Ceiling
    # targetBuffer defaults to 20%
    # emergencyGrow enabled by default
```

The operator will:
- Auto-grow when free space drops below 20%
- Stop at 100Gi (limit)
- Update `spec.storage.currentSize` after each growth
- New replicas get the current size immediately

### With Maintenance Window

```yaml
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: production-db
spec:
  instances: 3

  storage:
    storageClass: gp3
    request: 100Gi
    limit: 2Ti
    targetBuffer: 20
    maintenanceWindow:
      schedule: "0 3 * * 0"        # Sundays at 3 AM
      duration: "4h"
      timezone: "America/New_York"
    emergencyGrow:
      criticalThreshold: 95        # Emergency outside window at 95%
      criticalMinimumFree: 10Gi
      maxActionsPerDay: 4
```

### Tablespace Example

```yaml
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: analytics-db
spec:
  instances: 3

  storage:
    size: 100Gi  # Static for main data

  tablespaces:
    - name: timeseries_data
      storage:
        storageClass: gp3-cold
        request: 500Gi
        limit: 10Ti
        targetBuffer: 25
        maintenanceWindow:
          schedule: "0 4 * * 0"    # Sunday 4 AM
          duration: "6h"
```

### Phase 2: With WAL Safety

```yaml
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: with-wal-safety
spec:
  instances: 3

  storage:
    storageClass: gp3
    request: 100Gi
    limit: 2Ti
    targetBuffer: 20

  walStorage:
    storageClass: gp3
    request: 20Gi
    limit: 200Gi
    targetBuffer: 30
    walSafetyPolicy:               # Phase 2
      requireArchiveHealthy: true
      maxPendingWALFiles: 50
```

---

## How Dynamic Sizing Works

### Automatic Growth Flow

```
Every reconciliation cycle:
    │
    ├─► Calculate current usage and free space
    │
    ├─► Is free space below critical threshold (e.g., 5% or 1Gi)?
    │     │
    │     └── YES: EMERGENCY → Immediate resize using reserved budget
    │
    ├─► Is free space below buffer% or minimumFreeSpace?
    │     │
    │     ├── Is AllowGrowthOutsideWindow AND below minimumFreeSpace?
    │     │     │
    │     │     └── YES: Immediate growth (regular budget)
    │     │
    │     └── Otherwise: Queue for next maintenance window
    │
    └─► Otherwise: No action needed
```

### Intentional Shrink Flow

Shrink is NOT automatic. It's triggered by operator action:

```
kubectl cnpg storage shrink <cluster> --target-size 100Gi
    │
    ├─► Validate: target >= request (floor)
    │
    ├─► Validate: target < current size
    │
    ├─► Estimate: Calculate data size, time to copy, downtime impact
    │
    ├─► Show plan: Which replicas will be recreated, in what order
    │
    ├─► Require confirmation (--yes to skip)
    │
    └─► Execute during next maintenance window (or --now for immediate)
        │
        ├─► For each replica (one at a time):
        │     ├─► Fence replica
        │     ├─► Delete PVC
        │     ├─► Create new PVC at target size
        │     ├─► Clone data from primary (pg_basebackup or restore from backup)
        │     ├─► Unfence and rejoin cluster
        │     └─► Wait for sync before proceeding to next replica
        │
        └─► Finally, switchover and shrink former primary
```

The key insight: **shrinking 1GB or 1TB takes roughly the same time** because you're copying all the data regardless. This is why shrink is intentional, not automatic.

### Maintenance Window Processing

During a maintenance window:

1. **Collect pending operations** from all volumes
2. **Sort by priority**: emergency > growth > shrink
3. **Execute within budget**:
   - Growth: Patch PVC `spec.resources.requests.storage`
   - Shrink: Mark oldest replica for recreation with smaller PVC
4. **Update Cluster spec** with new size
5. **Wait for next cycle** to verify operations completed

### Shrink Implementation

Kubernetes does not support PVC shrinking. Dynamic sizing implements shrinkage through **replica recreation**:

1. **Identify shrink candidate**: Volume with free space exceeding shrink threshold
2. **Calculate new size**: Current usage + target buffer, clamped to sizeRange
3. **Update Cluster spec**: Set new target size in spec
4. **Recreate replicas**:
   - During maintenance window
   - One replica at a time (preserve HA)
   - New PVC created at new size
   - Data synced from primary via streaming replication
5. **Primary shrink**: After all replicas are at new size
   - Switchover to a smaller replica
   - Recreate former primary with smaller PVC
   - Rejoin as replica

This is a **controlled, gradual process** that happens over multiple maintenance windows for large clusters.

---

## Cluster Status Additions

```go
type ClusterStatus struct {
    // Existing fields...

    // DynamicStorageStatus tracks dynamic sizing state
    // +optional
    DynamicStorageStatus *DynamicStorageStatus `json:"dynamicStorageStatus,omitempty"`
}

type DynamicStorageStatus struct {
    // DataVolume status for the data volume
    // +optional
    DataVolume *VolumeStatus `json:"dataVolume,omitempty"`

    // WALVolume status for the WAL volume (if separate)
    // +optional
    WALVolume *VolumeStatus `json:"walVolume,omitempty"`

    // Tablespaces status for tablespace volumes
    // +optional
    Tablespaces map[string]*VolumeStatus `json:"tablespaces,omitempty"`
}

type VolumeStatus struct {
    // ActualSizes maps instance names to their actual PVC sizes
    // (may differ during rolling resize)
    ActualSizes map[string]string `json:"actualSizes,omitempty"`

    // State is the current sizing state
    // +kubebuilder:validation:Enum=Balanced;NeedsGrow;NeedsShrink;Emergency;PendingGrowth;PendingShrink;Resizing
    State string `json:"state,omitempty"`

    // LastOperation records the most recent sizing operation
    // +optional
    LastOperation *SizingOperation `json:"lastOperation,omitempty"`

    // PendingOperations lists operations waiting for maintenance window
    // +optional
    PendingOperations []PendingSizingOperation `json:"pendingOperations,omitempty"`

    // BudgetStatus tracks daily operation budget
    // +optional
    BudgetStatus *BudgetStatus `json:"budgetStatus,omitempty"`

    // NextMaintenanceWindow shows when pending operations can execute
    // +optional
    NextMaintenanceWindow *metav1.Time `json:"nextMaintenanceWindow,omitempty"`
}

type SizingOperation struct {
    Type         string      `json:"type,omitempty"`          // "grow", "shrink", "emergency"
    PreviousSize string      `json:"previousSize,omitempty"`
    NewSize      string      `json:"newSize,omitempty"`
    Timestamp    metav1.Time `json:"timestamp,omitempty"`
    Trigger      string      `json:"trigger,omitempty"`       // "buffer", "minimum", "critical"
    InstanceName string      `json:"instanceName,omitempty"`  // For shrink operations
    Result       string      `json:"result,omitempty"`
}

type PendingSizingOperation struct {
    Type          string      `json:"type,omitempty"`
    TargetSize    string      `json:"targetSize,omitempty"`
    QueuedAt      metav1.Time `json:"queuedAt,omitempty"`
    Reason        string      `json:"reason,omitempty"`
    AffectedPVCs  []string    `json:"affectedPVCs,omitempty"`
}

type BudgetStatus struct {
    TotalActionsPerDay    int         `json:"totalActionsPerDay,omitempty"`
    ReservedForEmergency  int         `json:"reservedForEmergency,omitempty"`
    UsedInLast24h         int         `json:"usedInLast24h,omitempty"`
    AvailableForPlanned   int         `json:"availableForPlanned,omitempty"`
    AvailableForEmergency int         `json:"availableForEmergency,omitempty"`
    BudgetResetsAt        metav1.Time `json:"budgetResetsAt,omitempty"`
}
```

---

## Metrics

### New Metrics for Dynamic Sizing

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `cnpg_dynamic_storage_target_size_bytes` | Gauge | `volume_type`, `tablespace` | Target size from dynamic config |
| `cnpg_dynamic_storage_actual_size_bytes` | Gauge | `volume_type`, `tablespace`, `instance` | Actual PVC size per instance |
| `cnpg_dynamic_storage_state` | Gauge | `volume_type`, `tablespace`, `state` | 1 for current state, 0 for others |
| `cnpg_dynamic_storage_pending_operations` | Gauge | `volume_type`, `tablespace`, `type` | Count of pending grow/shrink ops |
| `cnpg_dynamic_storage_budget_total` | Gauge | `volume_type` | Total daily operations budget |
| `cnpg_dynamic_storage_budget_used` | Gauge | `volume_type` | Operations used in last 24h |
| `cnpg_dynamic_storage_budget_emergency_reserved` | Gauge | `volume_type` | Emergency reserve remaining |
| `cnpg_dynamic_storage_next_window_seconds` | Gauge | `volume_type` | Seconds until next maintenance window |
| `cnpg_dynamic_storage_operations_total` | Counter | `volume_type`, `tablespace`, `type`, `result` | Total sizing operations |
| `cnpg_dynamic_storage_shrink_progress` | Gauge | `volume_type`, `tablespace` | 0-1 progress of rolling shrink |

### Existing Metrics (from auto-resize RFC)

The disk usage metrics from the auto-resize RFC remain unchanged:
- `cnpg_disk_total_bytes`
- `cnpg_disk_used_bytes`
- `cnpg_disk_available_bytes`
- `cnpg_disk_percent_used`

---

## Comparison with Auto-Resize RFC

| Aspect | Auto-Resize RFC | Dynamic Sizing (Phase 1) |
|--------|-----------------|--------------------------|
| **Mental Model** | Threshold-triggered | Request/Limit bounds |
| **Core Fields** | 15+ | 5 |
| **Spec Updates** | PVC only, spec unchanged | Updates Cluster spec |
| **GitOps** | Creates drift | No drift |
| **New Replicas** | Wrong size, need immediate resize | Correct size immediately |
| **Maintenance Windows** | Not supported | Built-in |
| **Shrink Support** | Not possible | Phase 2 |
| **Maturity** | Implemented | Proposed |

### Configuration Comparison

**Auto-Resize RFC** (current implementation):
```yaml
storage:
  size: 100Gi
  resize:
    enabled: true
    triggers:
      usageThreshold: 80
      minAvailable: 10Gi
    expansion:
      step: "20%"
      minStep: 2Gi
      maxStep: 500Gi
      limit: 500Gi
    strategy:
      maxActionsPerDay: 3
      walSafetyPolicy:
        acknowledgeWALRisk: true
        requireArchiveHealthy: true
        maxPendingWALFiles: 100
```

**Dynamic Sizing RFC** (Phase 1 - complete feature):
```yaml
storage:
  request: 100Gi
  limit: 500Gi
  targetBuffer: 20
  maintenanceWindow:
    schedule: "0 3 * * 0"
  emergencyGrow:
    criticalThreshold: 95
```

Simpler to understand, easier to configure, more complete.

---

## Migration Path

### From Static Sizing

```yaml
# Before
storage:
  size: 100Gi

# After
storage:
  request: 100Gi     # Original size becomes floor
  limit: 1Ti         # Set your ceiling
  # targetBuffer defaults to 20%
```

The operator detects the migration and:
1. Sets `currentSize` to match the actual PVC sizes
2. Begins managing storage within the request/limit range

### From Auto-Resize

```yaml
# Before (auto-resize)
storage:
  size: 100Gi
  resize:
    enabled: true
    triggers:
      usageThreshold: 80
    expansion:
      step: "20%"
      limit: 500Gi

# After (dynamic sizing)
storage:
  request: 100Gi           # Original size
  limit: 500Gi             # expansion.limit
  targetBuffer: 20         # 100 - usageThreshold
```

The auto-resize `resize:` block is removed. PVC sizes are preserved, and `currentSize` is set to match actual PVC sizes.

### Backwards Compatibility

The `size` field continues to work for static sizing:

| Configuration | Behavior |
|---------------|----------|
| `size: 100Gi` only | Static sizing (current behavior, unchanged) |
| `request: 10Gi` + `limit: 100Gi` | Dynamic sizing |
| `size` + `request` + `limit` | Invalid (webhook rejects) |

---

## Safety Considerations

### WAL Safety

Dynamic sizing inherits the WAL safety checks from the auto-resize RFC:
- `requireArchiveHealthy`
- `maxPendingWALFiles`
- `maxSlotRetentionBytes`
- `acknowledgeWALRisk` for single-volume clusters

These checks block **both growth and shrinkage** when WAL health is degraded.

### Shrink Safety

Shrinkage operations have additional safety gates:

1. **Replica quorum**: Shrink is blocked if it would reduce replicas below quorum during recreation
2. **Sync replication**: Shrink waits for synchronous replicas to be fully synced before recreation
3. **Backup validation**: Optional check that a recent backup exists before shrink
4. **Minimum retention**: Configurable minimum time after growth before shrink is allowed (prevents thrashing)

```go
type ShrinkSafetyPolicy struct {
    // MinimumRetentionAfterGrowth is how long to wait after a growth operation
    // before considering shrinkage. Prevents grow/shrink thrashing.
    // +kubebuilder:default:="24h"
    MinimumRetentionAfterGrowth string `json:"minimumRetentionAfterGrowth,omitempty"`

    // RequireRecentBackup blocks shrink if no backup exists within this duration.
    // Set to "0" to disable.
    // +kubebuilder:default:="24h"
    RequireRecentBackup string `json:"requireRecentBackup,omitempty"`

    // MaxConcurrentRecreations limits how many replicas can be recreated simultaneously.
    // +kubebuilder:validation:Minimum=1
    // +kubebuilder:default:=1
    MaxConcurrentRecreations int `json:"maxConcurrentRecreations,omitempty"`
}
```

---

## Implementation Phases

### Phase 1: Dynamic Sizing for Data & Tablespaces

The complete feature for data and tablespace volumes:

```yaml
storage:
  request: 10Gi              # Floor
  limit: 100Gi               # Ceiling
  targetBuffer: 20           # % free space to maintain
  maintenanceWindow:
    schedule: "0 3 * * 0"    # Non-urgent ops during window
    duration: "4h"
  emergencyGrow:
    criticalThreshold: 95    # Emergency growth outside window
    exceedLimitOnEmergency: false
```

**Scope:**
- Auto-grow when free space drops below targetBuffer%
- Non-urgent growth during maintenance window
- Emergency growth outside window (respecting limit by default)
- Update `spec.storage.currentSize` after growth
- Rate limiting for cloud provider quotas
- Works for data volumes and tablespaces

**What this replaces:** The entire auto-resize RFC (15+ fields → 5 core fields).

**Deferred to v2:** WAL safety checks, WAL volume support, shrink.

### Phase 2: WAL Safety & Shrink

WAL volumes are more complex because growth might mask archive/replication failures:

```yaml
walStorage:
  request: 10Gi
  limit: 100Gi
  targetBuffer: 30
  walSafetyPolicy:
    requireArchiveHealthy: true
    maxPendingWALFiles: 50
```

**Scope:**
- WAL-aware safety checks (archive health, replication slots)
- `acknowledgeWALRisk` for single-volume clusters
- `kubectl cnpg storage shrink` command for intentional shrink

**Why v2:**
- WAL safety is the most complex part of the auto-resize RFC
- Shrinking is a major operation (full data copy) regardless of size
- Both deserve focused attention after core dynamic sizing is proven

### Phase 3: Observability and Tooling

- `kubectl cnpg storage status` command
- Grafana dashboard panels
- PrometheusRule alerts
- Recovery time estimation for shrink operations

---

## Alternatives Considered

### Alternative 1: Keep Auto-Resize, Add Spec Sync

Add an option to auto-resize to update `spec.storage.size` after PVC growth.

**Rejected**: This permanently ratchets the spec floor upward, preventing any future shrink workflow. It also doesn't solve the fundamental issue that threshold-based reactive resizing is less predictable than buffer-based maintenance.

### Alternative 2: Implement PVC Shrink via Restore

Shrink by restoring from backup to a smaller PVC.

**Rejected**: Too disruptive for routine operations. Backup/restore is the right path for major downsizing, but not for routine buffer maintenance. Replica recreation preserves streaming replication and is less disruptive.

### Alternative 3: External Volume Manager

Build a separate controller that manages PVC sizing.

**Rejected**: Adds operational complexity and another component to manage. The CNPG operator already has the context needed for intelligent sizing decisions.

### Alternative 4: Complex Buffer Configuration

Early drafts of this RFC used a `targetBuffer` configuration with `targetFreePercent`, `minimumFreeSpace`, and `shrinkThresholdPercent` fields.

**Rejected** in favor of request/limit: The request/limit model is simpler and maps to familiar Kubernetes semantics. Most operators can use just `request`, `limit`, and `buffer` with defaults for everything else.

### Alternative 5: Keep Auto-Resize with Threshold + Hysteresis

Keep threshold-based triggers but add hysteresis (e.g., grow at 80%, consider shrink at 60%).

**Rejected**: This is essentially what dynamic sizing does, but expressed less clearly. The request/limit framing is more intuitive and aligns with Kubernetes conventions.

---

## Previously Considered: Auto-Resize RFC

The [auto-resize RFC](pvc-autoresize.md) represents significant design work and a working implementation. Key elements that informed dynamic sizing:

- **Behavior-driven configuration model** (triggers, expansion, strategy)
- **WAL-aware safety checks** (archive health, pending files, slots)
- **Rate-limit budget tracking** (maxActionsPerDay)
- **Clamping logic** (minStep, maxStep)
- **Metrics and observability**

Dynamic sizing builds on these foundations while addressing the limitations identified in production feedback. The auto-resize RFC should be considered the "simple mode" implementation that can coexist with dynamic sizing:

- **Auto-resize**: Growth-only, reactive, doesn't touch spec
- **Dynamic sizing**: Bidirectional, proactive, spec-driven

Operators can choose the model that fits their operational needs.

---

## Open Questions

1. **Should maintenance windows be cluster-wide or per-volume?**
   *Recommendation: Per-volume, to allow different schedules for data vs. tablespaces.*

2. **How should shrink interact with volume snapshots?**
   *A snapshot taken at 100Gi may need to restore to a PVC that's now 50Gi. Need to validate CSI behavior.*

3. **Should shrink require manual approval for production clusters?**
   *Consider adding `shrinkApprovalMode: Automatic|Manual` field.*

4. **How do we handle shrink on the primary?**
   *Current design requires switchover. Alternative: pg_repack to reclaim space without shrink.*

5. **What's the interaction with cluster hibernation?**
   *Hibernation should pause dynamic sizing. Resume should recalculate state.*

6. **Should we support instance-specific sizing?**
   *E.g., primary gets 2x the buffer of replicas. Current design is uniform.*

---

## References

- [RFC: Automatic PVC Resizing with WAL-Aware Safety](pvc-autoresize.md) - predecessor RFC
- [Kubernetes Volume Expansion](https://kubernetes.io/docs/concepts/storage/persistent-volumes/#expanding-persistent-volumes-claims)
- [CloudNativePG Storage Documentation](https://cloudnative-pg.io/documentation/current/storage/)
- [AWS EBS Volume Modification Constraints](https://docs.aws.amazon.com/ebs/latest/userguide/modify-volume-requirements.html)
- [Azure Disk Online Resize](https://learn.microsoft.com/en-us/azure/virtual-machines/linux/expand-disks)
- [GCP Persistent Disk Resizing](https://cloud.google.com/compute/docs/disks/resize-persistent-disk)

---

## Summary

The request/limit model transforms PVC management from a static allocation problem into familiar Kubernetes resource semantics:

```yaml
storage:
  request: 10Gi           # Floor (minimum provisioned)
  limit: 100Gi            # Ceiling (maximum provisioned)
  targetBuffer: 20        # % free space to maintain
  maintenanceWindow:      # When non-urgent ops occur
    schedule: "0 3 * * 0"
  emergencyGrow:          # Outside window when critical
    criticalThreshold: 95
```

**Phase 1** delivers the complete feature for data/tablespace volumes:
- Request/limit bounds with target buffer
- Maintenance windows for planned operations
- Emergency growth for critical situations
- Spec updates so new replicas get the right size

**Phase 2** adds WAL-specific complexity and shrink support.

### The Three Rough Edges This Addresses

| Problem | Solution |
|---------|----------|
| **Avoid running out of disk** | Auto-grow up to limit |
| **GitOps drift / wrong replica size** | Update spec after growth |
| **Shrink when you mess up** | `kubectl cnpg storage shrink` (Phase 2) |

---

*This RFC supersedes the auto-resize RFC with a simpler, more complete model. Feedback on the request/limit semantics and phasing is welcome.*
