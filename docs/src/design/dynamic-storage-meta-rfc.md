# RFC: Convergent Dynamic Storage Management

| Field | Value |
|-------|-------|
| **Author** | Jeff Mealo (synthesized from Gemini, Codex, Opus proposals) |
| **Status** | Draft / Request for Comments |
| **Created** | 2026-02-08 |
| **Target Release** | TBD |
| **Supersedes** | [RFC: Automatic PVC Resizing with WAL-Aware Safety](pvc-autoresize.md) |

---

## Summary

This RFC proposes **Convergent Dynamic Storage Management** for CloudNativePG. The operator autonomously maintains storage within user-defined bounds by converging toward a target free-space buffer. The model distinguishes between:

- **Emergency Growth**: Immediate action to prevent disk-full failures (outside maintenance windows)
- **Scheduled Convergence**: Planned grow/shrink operations during maintenance windows

Core API fields (familiar Kubernetes semantics):

```yaml
storage:
  request: 10Gi           # Floor (minimum provisioned size)
  limit: 100Gi            # Ceiling (maximum provisioned size)
  targetBuffer: 20        # % free space to maintain
  maintenanceWindow:      # When non-urgent operations occur
    schedule: "0 3 * * 0"
    duration: "4h"
  emergencyGrow:          # Outside-window safety behavior
    criticalThreshold: 95
```

**Key outcomes:**
1. No GitOps drift (policy in spec, state in status)
2. New replicas match operational size immediately
3. Clear path to shrink when over-provisioned
4. Predictable maintenance window scheduling
5. Emergency growth preserves availability

---

## Motivation

### The Three Rough Edges in CNPG Storage

| Problem | Current State | Impact |
|---------|---------------|--------|
| **Out of disk** | Manual intervention required | Downtime, data loss risk |
| **Spec drift after growth** | Auto-resize patches PVCs, not spec | GitOps tools confused, new replicas wrong size |
| **Over-provisioned storage** | No shrink path | Permanent cost overhead |

### Why Auto-Resize Isn't Enough

The [auto-resize RFC](pvc-autoresize.md) addresses disk exhaustion but introduces:

1. **Spec/PVC divergence**: `spec.storage.size: 10Gi` but PVCs are 45Gi
2. **New replica mismatch**: Created at spec size, immediately resize
3. **Snapshot restore friction**: Source snapshot 45Gi, target spec 10Gi
4. **No reclaim path**: Temporary spike permanently over-provisions

### The Convergent Model

Instead of reactive threshold-triggered resizing, this RFC proposes **convergent sizing**:

- Define `request` (floor) and `limit` (ceiling)
- Specify `targetBuffer` (desired free-space percentage)
- Operator continuously converges actual size toward target
- **Policy in spec, state in status** — GitOps-stable

---

## Design Principles

### 1. Policy in Spec, State in Status

- **Spec**: User declares intent (`request`, `limit`, `targetBuffer`)
- **Status**: Controller reports operational state (`effectiveSize`, `targetSize`, `lastAction`)

The controller NEVER rewrites user policy fields. GitOps workflows remain clean.

### 2. Logical Volume Identity

Budget tracking and policy decisions are keyed by **logical identity**:
- `cluster + data`
- `cluster + wal`
- `cluster + tablespace/<name>`

NOT by PVC name. Budget survives PVC replacement and pod recreation.

### 3. Urgency-Aware Orchestration

| Action Type | Trigger | Timing | Respects Limit |
|-------------|---------|--------|----------------|
| **EmergencyGrow** | Free space critical (<5% or <1Gi) | Immediate | Yes (unless override) |
| **ScheduledGrow** | Free space below buffer | Maintenance window | Yes |
| **ScheduledShrink** | Free space significantly above buffer | Maintenance window | N/A |

### 4. Growth is Automatic, Shrink is Intentional

- **Growth**: Automatic via CSI volume expansion
- **Shrinkage**: Triggered via `kubectl cnpg storage shrink` (full data copy operation)

Shrinking 1GB or 1TB takes similar time — it's always a major operation. It exists to correct mistakes, not for routine optimization.

---

## API Design

### V1 Policy Fields

```go
type StorageConfiguration struct {
    // Size is the static storage size. Mutually exclusive with Request/Limit.
    // +optional
    Size string `json:"size,omitempty"`

    // Request is the minimum provisioned size (floor).
    // The operator will never shrink below this value.
    // +optional
    Request string `json:"request,omitempty"`

    // Limit is the maximum provisioned size (ceiling).
    // The operator will never grow beyond this value.
    // +optional
    Limit string `json:"limit,omitempty"`

    // TargetBuffer is the desired free space percentage (5-50%).
    // +kubebuilder:validation:Minimum=5
    // +kubebuilder:validation:Maximum=50
    // +kubebuilder:default:=20
    // +optional
    TargetBuffer *int `json:"targetBuffer,omitempty"`

    // MaintenanceWindow defines when non-urgent operations occur.
    // +optional
    MaintenanceWindow *MaintenanceWindowConfig `json:"maintenanceWindow,omitempty"`

    // EmergencyGrow controls growth outside the maintenance window.
    // +optional
    EmergencyGrow *EmergencyGrowConfig `json:"emergencyGrow,omitempty"`

    // Existing fields unchanged...
    StorageClass       *string `json:"storageClass,omitempty"`
    ResizeInUseVolumes *bool   `json:"resizeInUseVolumes,omitempty"`
}
```

### Configuration Modes

| Fields Set | Mode | Behavior |
|------------|------|----------|
| `size` only | **Static** | Fixed size (current behavior) |
| `request` + `limit` | **Dynamic** | Convergent sizing within bounds |
| `size` + `request` + `limit` | **Invalid** | Webhook rejects |

### MaintenanceWindowConfig

```go
type MaintenanceWindowConfig struct {
    // Schedule in cron syntax: "minute hour day-of-month month day-of-week"
    // +kubebuilder:default:="0 3 * * *"
    Schedule string `json:"schedule,omitempty"`

    // Duration of the maintenance window.
    // +kubebuilder:default:="2h"
    Duration string `json:"duration,omitempty"`

    // Timezone for interpreting the schedule.
    // +kubebuilder:default:="UTC"
    Timezone string `json:"timezone,omitempty"`
}
```

### EmergencyGrowConfig

```go
type EmergencyGrowConfig struct {
    // Enabled allows emergency growth outside maintenance windows.
    // +kubebuilder:default:=true
    Enabled *bool `json:"enabled,omitempty"`

    // CriticalThreshold (usage %) that triggers emergency growth.
    // +kubebuilder:validation:Minimum=80
    // +kubebuilder:validation:Maximum=99
    // +kubebuilder:default:=95
    CriticalThreshold int `json:"criticalThreshold,omitempty"`

    // CriticalMinimumFree triggers emergency growth when free space drops below.
    // +kubebuilder:default:="1Gi"
    CriticalMinimumFree string `json:"criticalMinimumFree,omitempty"`

    // ExceedLimitOnEmergency allows growth beyond Limit as last resort.
    // +kubebuilder:default:=false
    ExceedLimitOnEmergency *bool `json:"exceedLimitOnEmergency,omitempty"`

    // MaxActionsPerDay limits total resize operations per 24h window.
    // +kubebuilder:default:=4
    MaxActionsPerDay *int `json:"maxActionsPerDay,omitempty"`

    // ReservedActionsForEmergency from the daily budget.
    // +kubebuilder:default:=1
    ReservedActionsForEmergency *int `json:"reservedActionsForEmergency,omitempty"`
}
```

---

## Status Model

```go
type ClusterStatus struct {
    // Existing fields...

    // StorageSizing tracks dynamic sizing state per logical volume.
    // +optional
    StorageSizing *StorageSizingStatus `json:"storageSizing,omitempty"`
}

type StorageSizingStatus struct {
    Data        *VolumeSizingStatus            `json:"data,omitempty"`
    WAL         *VolumeSizingStatus            `json:"wal,omitempty"`
    Tablespaces map[string]*VolumeSizingStatus `json:"tablespaces,omitempty"`
}

type VolumeSizingStatus struct {
    // EffectiveSize is the current target size for new PVCs.
    EffectiveSize string `json:"effectiveSize,omitempty"`

    // TargetSize is the calculated ideal size based on usage + buffer.
    TargetSize string `json:"targetSize,omitempty"`

    // ActualSizes maps instance names to their current PVC sizes.
    ActualSizes map[string]string `json:"actualSizes,omitempty"`

    // State: Balanced, NeedsGrow, NeedsShrink, Emergency, Resizing
    State string `json:"state,omitempty"`

    // LastAction records the most recent sizing operation.
    LastAction *SizingAction `json:"lastAction,omitempty"`

    // Budget tracks daily operation limits.
    Budget *BudgetStatus `json:"budget,omitempty"`

    // NextMaintenanceWindow when pending operations can execute.
    NextMaintenanceWindow *metav1.Time `json:"nextMaintenanceWindow,omitempty"`

    // Conditions for detailed state.
    Conditions []metav1.Condition `json:"conditions,omitempty"`
}

type SizingAction struct {
    Kind      string      `json:"kind,omitempty"` // EmergencyGrow, ScheduledGrow, ScheduledShrink
    From      string      `json:"from,omitempty"`
    To        string      `json:"to,omitempty"`
    Timestamp metav1.Time `json:"timestamp,omitempty"`
    Instance  string      `json:"instance,omitempty"`
    Result    string      `json:"result,omitempty"`
}

type BudgetStatus struct {
    ActionsLast24h        int         `json:"actionsLast24h,omitempty"`
    ReservedForEmergency  int         `json:"reservedForEmergency,omitempty"`
    AvailableForPlanned   int         `json:"availableForPlanned,omitempty"`
    AvailableForEmergency int         `json:"availableForEmergency,omitempty"`
    BudgetResetsAt        metav1.Time `json:"budgetResetsAt,omitempty"`
}
```

---

## Control Loop Behavior

### Decision Flow

```
Every reconciliation cycle:
    │
    ├─► Collect disk status and WAL health from all instances
    │
    ├─► Compute targetSize = currentUsage / (1 - targetBuffer%)
    │
    ├─► Classify action:
    │     ├── EmergencyGrow: free space < critical threshold
    │     ├── ScheduledGrow: effectiveSize < targetSize (but not critical)
    │     ├── ScheduledShrink: effectiveSize > targetSize + hysteresis
    │     └── NoOp: within acceptable range
    │
    ├─► Evaluate gates:
    │     ├── Budget available?
    │     ├── Maintenance window open? (skip for emergency)
    │     ├── WAL safety passed? (Phase 2)
    │     └── Hysteresis/cooldown satisfied?
    │
    ├─► Execute action:
    │     ├── EmergencyGrow: Patch PVC immediately
    │     ├── ScheduledGrow: Patch PVC during window
    │     └── ScheduledShrink: Mark for replica recreation
    │
    └─► Update status (effectiveSize, budget, lastAction)
```

### Anti-Oscillation Defaults (Internal)

V1 keeps these as internal defaults to minimize API surface:

- **Cooldown**: 1 hour between non-emergency actions on same volume
- **Hysteresis**: Shrink only when free space exceeds `targetBuffer + 20%`
- **Min delta**: Don't resize for <5% change in target size
- **Step sizing**: 10% growth steps with 2Gi min and 500Gi max

---

## Replica Provisioning

In dynamic mode, new PVCs use **effectiveSize** (from status), not the raw `request`:

```
1. New replica requested
2. Controller reads status.storageSizing.data.effectiveSize
3. Creates PVC with effectiveSize
4. Replica starts with correct size immediately
```

This eliminates:
- The "create small, immediately resize" pattern
- Snapshot restore size mismatches
- Cloud provider quota waste

---

## Example Configurations

### Minimal

```yaml
storage:
  request: 10Gi
  limit: 100Gi
```

Defaults: 20% buffer, daily maintenance window at 3 AM UTC, emergency growth enabled.

### Production

```yaml
storage:
  request: 100Gi
  limit: 2Ti
  targetBuffer: 20
  maintenanceWindow:
    schedule: "0 3 * * 0"  # Sundays 3 AM
    duration: "4h"
    timezone: "America/New_York"
  emergencyGrow:
    criticalThreshold: 95
    criticalMinimumFree: 10Gi
    maxActionsPerDay: 4
    reservedActionsForEmergency: 1
```

### Tablespace

```yaml
tablespaces:
  - name: timeseries
    storage:
      request: 500Gi
      limit: 10Ti
      targetBuffer: 25
      maintenanceWindow:
        schedule: "0 4 * * 0"
        duration: "6h"
```

---

## Shrink via kubectl (Phase 2)

Shrink is intentional, not automatic:

```bash
kubectl cnpg storage shrink my-cluster --target-size 50Gi
```

Flow:
1. Validate target >= request
2. Estimate time and show plan
3. Require confirmation
4. Execute during maintenance window:
   - Recreate replicas one at a time with smaller PVCs
   - Clone data via streaming replication
   - Switchover to shrink primary last

---

## Phasing

### Phase 1: Dynamic Sizing for Data & Tablespaces

**Scope:**
- `request`, `limit`, `targetBuffer` API fields
- `maintenanceWindow` and `emergencyGrow` configuration
- Status model with effectiveSize, budget tracking
- Emergency growth outside maintenance window
- Scheduled growth during maintenance window
- New replica provisioning at effectiveSize
- Metrics and events
- `kubectl cnpg storage status` command

**NOT in Phase 1:**
- WAL safety checks (deferred due to complexity)
- Shrink support (deferred for focused delivery)
- WAL volume dynamic sizing

### Phase 2: WAL Safety & Shrink

**Scope:**
- WAL-aware safety checks (archive health, slot retention)
- `walSafetyPolicy` configuration
- `acknowledgeWALRisk` for single-volume clusters
- `kubectl cnpg storage shrink` command
- WAL volume dynamic sizing

### Phase 3: Observability & Tooling

**Scope:**
- Grafana dashboard panels
- PrometheusRule alerts
- Recovery time estimation for shrink
- `kubectl cnpg storage converge` for manual trigger

---

## E2E Validation Requirements

Formal E2E coverage requirements are tracked in:

- [Dynamic Storage E2E Requirements](dynamic-storage-e2e-requirements-codex.md)

That document defines:

1. P0 merge-gating scenarios for in-flight operational events (restart, failover, spec mutation, drain, backup)
2. Required topology matrix coverage (`instances=1`, `instances=2`, `instances>2`)
3. Required pass/fail invariants for correctness, safety, and convergence

---

## Comparison with Auto-Resize RFC

| Aspect | Auto-Resize RFC | Dynamic Sizing |
|--------|-----------------|----------------|
| **Mental model** | Threshold-triggered | Convergent to target |
| **Configuration** | 15+ fields | 5 core fields |
| **Spec updates** | PVC only | Status (effectiveSize) |
| **GitOps** | Creates drift | No drift |
| **New replicas** | Wrong size initially | Correct size immediately |
| **Maintenance windows** | Not supported | Built-in |
| **Shrink support** | Not possible | Phase 2 |

---

## Migration Path

### From Static Sizing

```yaml
# Before
storage:
  size: 100Gi

# After
storage:
  request: 100Gi
  limit: 1Ti
```

### From Auto-Resize

```yaml
# Before
storage:
  size: 100Gi
  resize:
    enabled: true
    triggers:
      usageThreshold: 80
    expansion:
      limit: 500Gi

# After
storage:
  request: 100Gi
  limit: 500Gi
  targetBuffer: 20
```

---

## Open Questions

1. **Maintenance window scope**: Per-cluster or per-volume?
2. **Snapshot restore**: How to handle shrink below snapshot source size?
3. **Hibernation interaction**: Pause dynamic sizing during hibernation?
4. **Strategy knobs**: Expose step sizing in V1 or keep as internal defaults?

---

## References

- [RFC: Automatic PVC Resizing with WAL-Aware Safety](pvc-autoresize.md)
- [Kubernetes Volume Expansion](https://kubernetes.io/docs/concepts/storage/persistent-volumes/#expanding-persistent-volumes-claims)
- [AWS EBS Volume Modification Constraints](https://docs.aws.amazon.com/ebs/latest/userguide/modify-volume-requirements.html)
- [Azure Disk Online Resize](https://learn.microsoft.com/en-us/azure/virtual-machines/linux/expand-disks)

---

*This RFC synthesizes the best elements from three independent proposals (Gemini, Codex, Opus) to deliver a unified, production-ready dynamic storage management solution for CloudNativePG.*
