# Ralph Implementation Prompt: Convergent Dynamic Storage Management

## Objective

Implement **Convergent Dynamic Storage Management** for CloudNativePG on a new branch `feat/dynamic-storage`. This feature enables automatic storage management within user-defined bounds (`request`/`limit`) while maintaining a target free-space buffer.

The implementation must pass all E2E tests on **Azure AKS** with Premium SSD storage (supports online volume expansion), using this required runner: `/Users/jmealo/repos/cloudnative-pg/hack/e2e/run-aks-e2e.sh`.

---

## Prerequisites (User Must Complete Before Running)

```bash
# Create new branch from main
git checkout main
git pull origin main
git checkout -b feat/dynamic-storage

# Verify Azure CLI is logged in
az account show

# Set environment variables for Azure E2E
export AZURE_SUBSCRIPTION_ID="your-subscription-id"
export AZURE_RESOURCE_GROUP="cnpg-e2e-tests"
export AZURE_LOCATION="eastus"
```

---

## Reference Documents

Read these documents before starting implementation:

1. **Meta RFC**: `docs/src/design/dynamic-storage-meta-rfc.md` - The authoritative design document
2. **Codex E2E Requirements**: `docs/src/design/dynamic-storage-e2e-requirements-codex.md` - Required topology matrix and P0/P1 operational scenarios
3. **Auto-resize RFC**: `docs/src/design/pvc-autoresize.md` - Reference for disk probing patterns
4. **Existing storage code**: `api/v1/cluster_types.go` - Current StorageConfiguration
5. **Instance status**: `pkg/postgres/status.go` - How instance status is reported

---

## Phase 1: API Types

### 1.1 Add Dynamic Storage Types

Add to `api/v1/cluster_types.go`:

```go
// StorageConfiguration defines the storage configuration for a volume.
type StorageConfiguration struct {
    // Size is the static storage size. Mutually exclusive with Request/Limit.
    // +optional
    Size string `json:"size,omitempty"`

    // Request is the minimum provisioned size (floor).
    // When set with Limit, enables dynamic sizing mode.
    // +optional
    Request string `json:"request,omitempty"`

    // Limit is the maximum provisioned size (ceiling).
    // When set with Request, enables dynamic sizing mode.
    // +optional
    Limit string `json:"limit,omitempty"`

    // TargetBuffer is the desired free space percentage (5-50%).
    // Only applies when Request and Limit are set.
    // +kubebuilder:validation:Minimum=5
    // +kubebuilder:validation:Maximum=50
    // +kubebuilder:default:=20
    // +optional
    TargetBuffer *int `json:"targetBuffer,omitempty"`

    // MaintenanceWindow defines when non-urgent sizing operations occur.
    // +optional
    MaintenanceWindow *MaintenanceWindowConfig `json:"maintenanceWindow,omitempty"`

    // EmergencyGrow controls growth outside the maintenance window.
    // +optional
    EmergencyGrow *EmergencyGrowConfig `json:"emergencyGrow,omitempty"`

    // Existing fields unchanged...
    StorageClass       *string `json:"storageClass,omitempty"`
    ResizeInUseVolumes *bool   `json:"resizeInUseVolumes,omitempty"`
    PvcTemplate        *corev1.PersistentVolumeClaimSpec `json:"pvcTemplate,omitempty"`
}

// MaintenanceWindowConfig defines when non-urgent operations can occur.
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

// EmergencyGrowConfig controls emergency growth outside maintenance windows.
type EmergencyGrowConfig struct {
    // Enabled allows emergency growth outside maintenance windows.
    // +kubebuilder:default:=true
    Enabled *bool `json:"enabled,omitempty"`

    // CriticalThreshold is the usage percentage that triggers emergency growth.
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

### 1.2 Add Status Types

Add to cluster status:

```go
// StorageSizingStatus tracks dynamic sizing state per logical volume.
type StorageSizingStatus struct {
    // Data volume sizing status.
    // +optional
    Data *VolumeSizingStatus `json:"data,omitempty"`

    // WAL volume sizing status (Phase 2).
    // +optional
    WAL *VolumeSizingStatus `json:"wal,omitempty"`

    // Tablespaces sizing status.
    // +optional
    Tablespaces map[string]*VolumeSizingStatus `json:"tablespaces,omitempty"`
}

// VolumeSizingStatus tracks the sizing state of a logical volume.
type VolumeSizingStatus struct {
    // EffectiveSize is the current target size for new PVCs.
    EffectiveSize string `json:"effectiveSize,omitempty"`

    // TargetSize is the calculated ideal size based on usage + buffer.
    TargetSize string `json:"targetSize,omitempty"`

    // ActualSizes maps instance names to their current PVC sizes.
    // +optional
    ActualSizes map[string]string `json:"actualSizes,omitempty"`

    // State: Balanced, NeedsGrow, Emergency, PendingGrowth, Resizing
    // +kubebuilder:validation:Enum=Balanced;NeedsGrow;Emergency;PendingGrowth;Resizing
    State string `json:"state,omitempty"`

    // LastAction records the most recent sizing operation.
    // +optional
    LastAction *SizingAction `json:"lastAction,omitempty"`

    // Budget tracks daily operation limits.
    // +optional
    Budget *BudgetStatus `json:"budget,omitempty"`

    // NextMaintenanceWindow when pending operations can execute.
    // +optional
    NextMaintenanceWindow *metav1.Time `json:"nextMaintenanceWindow,omitempty"`
}

// SizingAction records a sizing operation.
type SizingAction struct {
    // Kind: EmergencyGrow, ScheduledGrow
    Kind string `json:"kind,omitempty"`

    // From size before the action.
    From string `json:"from,omitempty"`

    // To size after the action.
    To string `json:"to,omitempty"`

    // Timestamp when the action occurred.
    Timestamp metav1.Time `json:"timestamp,omitempty"`

    // Instance affected by the action.
    Instance string `json:"instance,omitempty"`

    // Result: Success, Failed, Pending
    Result string `json:"result,omitempty"`
}

// BudgetStatus tracks daily resize operation budget.
type BudgetStatus struct {
    // ActionsLast24h count of resize operations in the last 24 hours.
    ActionsLast24h int `json:"actionsLast24h,omitempty"`

    // AvailableForPlanned actions remaining for scheduled operations.
    AvailableForPlanned int `json:"availableForPlanned,omitempty"`

    // AvailableForEmergency actions remaining for emergency operations.
    AvailableForEmergency int `json:"availableForEmergency,omitempty"`

    // BudgetResetsAt when the rolling 24h window resets.
    BudgetResetsAt metav1.Time `json:"budgetResetsAt,omitempty"`
}
```

### 1.3 Generate DeepCopy and CRDs

```bash
make generate
make manifests
```

### Verification Gate

```bash
go build ./...
make generate manifests
git diff --name-only  # Should show api/ and config/crd/ changes
```

---

## Phase 2: Webhook Validation

### 2.1 Add Validation Logic

Add to `internal/webhook/v1/cluster_webhook.go`:

```go
func (r *Cluster) validateDynamicStorage() error {
    for _, sc := range []struct {
        name string
        cfg  *StorageConfiguration
    }{
        {"storage", r.Spec.Storage},
        {"walStorage", r.Spec.WalStorage},
    } {
        if sc.cfg == nil {
            continue
        }
        if err := validateStorageConfiguration(sc.name, sc.cfg); err != nil {
            return err
        }
    }

    for _, ts := range r.Spec.Tablespaces {
        if err := validateStorageConfiguration(
            fmt.Sprintf("tablespace %s", ts.Name),
            &ts.Storage,
        ); err != nil {
            return err
        }
    }
    return nil
}

func validateStorageConfiguration(name string, cfg *StorageConfiguration) error {
    hasSize := cfg.Size != ""
    hasRequest := cfg.Request != ""
    hasLimit := cfg.Limit != ""

    // Mutual exclusivity check
    if hasSize && (hasRequest || hasLimit) {
        return fmt.Errorf("%s: size and request/limit are mutually exclusive", name)
    }

    // Both request and limit required for dynamic mode
    if (hasRequest && !hasLimit) || (!hasRequest && hasLimit) {
        return fmt.Errorf("%s: both request and limit must be set for dynamic sizing", name)
    }

    // Validate quantities
    if hasRequest {
        request, err := resource.ParseQuantity(cfg.Request)
        if err != nil {
            return fmt.Errorf("%s: invalid request: %w", name, err)
        }
        limit, err := resource.ParseQuantity(cfg.Limit)
        if err != nil {
            return fmt.Errorf("%s: invalid limit: %w", name, err)
        }
        if request.Cmp(limit) > 0 {
            return fmt.Errorf("%s: request (%s) cannot exceed limit (%s)", name, cfg.Request, cfg.Limit)
        }
    }

    // Validate maintenance window cron
    if cfg.MaintenanceWindow != nil && cfg.MaintenanceWindow.Schedule != "" {
        if _, err := cron.ParseStandard(cfg.MaintenanceWindow.Schedule); err != nil {
            return fmt.Errorf("%s: invalid maintenance window schedule: %w", name, err)
        }
    }

    // Validate emergency grow
    if cfg.EmergencyGrow != nil {
        if cfg.EmergencyGrow.CriticalMinimumFree != "" {
            if _, err := resource.ParseQuantity(cfg.EmergencyGrow.CriticalMinimumFree); err != nil {
                return fmt.Errorf("%s: invalid criticalMinimumFree: %w", name, err)
            }
        }
    }

    return nil
}
```

### 2.2 Add Unit Tests

Create `internal/webhook/v1/cluster_webhook_dynamic_storage_test.go` with tests for:
- Valid static configuration (size only)
- Valid dynamic configuration (request + limit)
- Invalid mixed configuration (size + request)
- Request > limit validation
- Invalid cron schedule
- Invalid quantity values

### Verification Gate

```bash
make test
go test ./internal/webhook/... -v -run Dynamic
```

---

## Phase 3: Disk Probing

### 3.1 Disk Status Collection

Reuse/adapt the disk probing from the auto-resize implementation:

```go
// pkg/management/postgres/disk/probe.go
package disk

import (
    "syscall"
)

// Status represents filesystem statistics for a mount point.
type Status struct {
    TotalBytes     uint64
    UsedBytes      uint64
    AvailableBytes uint64
    PercentUsed    float64
}

// Probe returns disk status for the given path using statfs.
func Probe(path string) (*Status, error) {
    var stat syscall.Statfs_t
    if err := syscall.Statfs(path, &stat); err != nil {
        return nil, err
    }

    total := stat.Blocks * uint64(stat.Bsize)
    free := stat.Bfree * uint64(stat.Bsize)
    available := stat.Bavail * uint64(stat.Bsize)
    used := total - free

    percentUsed := float64(0)
    if total > 0 {
        percentUsed = float64(used) / float64(total) * 100
    }

    return &Status{
        TotalBytes:     total,
        UsedBytes:      used,
        AvailableBytes: available,
        PercentUsed:    percentUsed,
    }, nil
}
```

### 3.2 Instance Status Extension

Add disk status to instance status endpoint so the operator can read it.

### 3.3 Prometheus Metrics

Add metrics:
- `cnpg_disk_total_bytes{volume_type, instance}`
- `cnpg_disk_used_bytes{volume_type, instance}`
- `cnpg_disk_available_bytes{volume_type, instance}`
- `cnpg_disk_percent_used{volume_type, instance}`
- `cnpg_dynamic_storage_effective_size_bytes{volume_type}`
- `cnpg_dynamic_storage_budget_remaining{volume_type}`

### Verification Gate

```bash
make test
go test ./pkg/management/postgres/disk/... -v
```

---

## Phase 4: Dynamic Storage Reconciler

### 4.1 Create Reconciler Package

Create `pkg/reconciler/dynamicstorage/`:

```
pkg/reconciler/dynamicstorage/
├── reconciler.go       # Main reconciliation logic
├── maintenance.go      # Maintenance window evaluation
├── budget.go           # Rate limit budget tracking
├── sizing.go           # Size calculation (target, step)
├── pvc.go              # PVC patching
└── reconciler_test.go  # Unit tests
```

### 4.2 Core Reconciler Logic

```go
// pkg/reconciler/dynamicstorage/reconciler.go
package dynamicstorage

type Reconciler struct {
    client    client.Client
    recorder  record.EventRecorder
}

type ReconcileResult struct {
    Action       ActionType // NoOp, EmergencyGrow, ScheduledGrow, PendingGrowth
    TargetSize   resource.Quantity
    CurrentSize  resource.Quantity
    Reason       string
}

func (r *Reconciler) Reconcile(ctx context.Context, cluster *apiv1.Cluster) error {
    // 1. Check if dynamic sizing is enabled
    if !isDynamicSizingEnabled(cluster.Spec.Storage) {
        return nil
    }

    // 2. Collect disk status from all instances
    diskStatus, err := r.collectDiskStatus(ctx, cluster)
    if err != nil {
        return fmt.Errorf("collecting disk status: %w", err)
    }

    // 3. Evaluate sizing for each logical volume
    for volumeType, status := range diskStatus {
        result := r.evaluateSizing(cluster, volumeType, status)

        // 4. Execute action if needed
        if result.Action != NoOp {
            if err := r.executeAction(ctx, cluster, volumeType, result); err != nil {
                return fmt.Errorf("executing %s action for %s: %w", result.Action, volumeType, err)
            }
        }
    }

    return nil
}

func (r *Reconciler) evaluateSizing(
    cluster *apiv1.Cluster,
    volumeType string,
    diskStatus *DiskStatus,
) *ReconcileResult {
    cfg := getStorageConfig(cluster, volumeType)

    // Calculate target size: usage / (1 - targetBuffer%)
    targetBuffer := getTargetBuffer(cfg)
    targetSize := calculateTargetSize(diskStatus.UsedBytes, targetBuffer)

    // Clamp to request/limit bounds
    request := mustParseQuantity(cfg.Request)
    limit := mustParseQuantity(cfg.Limit)
    targetSize = clamp(targetSize, request, limit)

    currentSize := diskStatus.TotalSize

    // Check for emergency condition
    if isEmergencyCondition(cfg, diskStatus) && hasBudgetForEmergency(cluster, volumeType) {
        return &ReconcileResult{
            Action:      EmergencyGrow,
            TargetSize:  calculateEmergencySize(currentSize, limit),
            CurrentSize: currentSize,
            Reason:      "critical disk usage",
        }
    }

    // Check if growth is needed
    if targetSize.Cmp(currentSize) > 0 {
        if isMaintenanceWindowOpen(cfg) && hasBudgetForScheduled(cluster, volumeType) {
            return &ReconcileResult{
                Action:      ScheduledGrow,
                TargetSize:  targetSize,
                CurrentSize: currentSize,
                Reason:      "free space below target buffer",
            }
        }
        return &ReconcileResult{
            Action:      PendingGrowth,
            TargetSize:  targetSize,
            CurrentSize: currentSize,
            Reason:      "waiting for maintenance window",
        }
    }

    return &ReconcileResult{Action: NoOp}
}
```

### 4.3 Maintenance Window Logic

```go
// pkg/reconciler/dynamicstorage/maintenance.go
package dynamicstorage

import (
    "time"
    "github.com/robfig/cron/v3"
)

func isMaintenanceWindowOpen(cfg *apiv1.StorageConfiguration) bool {
    if cfg.MaintenanceWindow == nil {
        return true // No window configured = always open
    }

    schedule, err := cron.ParseStandard(cfg.MaintenanceWindow.Schedule)
    if err != nil {
        return false
    }

    loc := time.UTC
    if cfg.MaintenanceWindow.Timezone != "" {
        loc, _ = time.LoadLocation(cfg.MaintenanceWindow.Timezone)
    }

    now := time.Now().In(loc)
    duration, _ := time.ParseDuration(cfg.MaintenanceWindow.Duration)
    if duration == 0 {
        duration = 2 * time.Hour
    }

    // Find the most recent window start
    windowStart := schedule.Next(now.Add(-24 * time.Hour))
    for windowStart.Before(now) {
        next := schedule.Next(windowStart)
        if next.After(now) {
            break
        }
        windowStart = next
    }

    windowEnd := windowStart.Add(duration)
    return now.After(windowStart) && now.Before(windowEnd)
}

func nextMaintenanceWindow(cfg *apiv1.StorageConfiguration) time.Time {
    if cfg.MaintenanceWindow == nil {
        return time.Time{}
    }

    schedule, err := cron.ParseStandard(cfg.MaintenanceWindow.Schedule)
    if err != nil {
        return time.Time{}
    }

    loc := time.UTC
    if cfg.MaintenanceWindow.Timezone != "" {
        loc, _ = time.LoadLocation(cfg.MaintenanceWindow.Timezone)
    }

    return schedule.Next(time.Now().In(loc))
}
```

### 4.4 Budget Tracking

```go
// pkg/reconciler/dynamicstorage/budget.go
package dynamicstorage

import (
    "time"
)

const defaultMaxActionsPerDay = 4
const defaultReservedForEmergency = 1

func hasBudgetForEmergency(cluster *apiv1.Cluster, volumeType string) bool {
    status := getVolumeSizingStatus(cluster, volumeType)
    if status == nil || status.Budget == nil {
        return true
    }

    maxActions := defaultMaxActionsPerDay
    reserved := defaultReservedForEmergency
    cfg := getStorageConfig(cluster, volumeType)
    if cfg.EmergencyGrow != nil {
        if cfg.EmergencyGrow.MaxActionsPerDay != nil {
            maxActions = *cfg.EmergencyGrow.MaxActionsPerDay
        }
        if cfg.EmergencyGrow.ReservedActionsForEmergency != nil {
            reserved = *cfg.EmergencyGrow.ReservedActionsForEmergency
        }
    }

    actionsUsed := countActionsInLast24h(status)
    return actionsUsed < maxActions && status.Budget.AvailableForEmergency > 0
}

func hasBudgetForScheduled(cluster *apiv1.Cluster, volumeType string) bool {
    status := getVolumeSizingStatus(cluster, volumeType)
    if status == nil || status.Budget == nil {
        return true
    }

    return status.Budget.AvailableForPlanned > 0
}

func countActionsInLast24h(status *apiv1.VolumeSizingStatus) int {
    if status.Budget == nil {
        return 0
    }
    return status.Budget.ActionsLast24h
}
```

### 4.5 PVC Patching

```go
// pkg/reconciler/dynamicstorage/pvc.go
package dynamicstorage

func (r *Reconciler) patchPVCSize(
    ctx context.Context,
    pvc *corev1.PersistentVolumeClaim,
    newSize resource.Quantity,
) error {
    patch := client.MergeFrom(pvc.DeepCopy())
    pvc.Spec.Resources.Requests[corev1.ResourceStorage] = newSize
    return r.client.Patch(ctx, pvc, patch)
}
```

### 4.6 Unit Tests

Create comprehensive unit tests for:
- Target size calculation
- Emergency condition detection
- Maintenance window evaluation
- Budget tracking
- Size clamping to request/limit

### Verification Gate

```bash
make test
go test ./pkg/reconciler/dynamicstorage/... -v -cover
```

---

## Phase 5: Controller Wiring

### 5.1 Wire Reconciler into Cluster Controller

Add dynamic storage reconciliation to `internal/controller/cluster_controller.go`:

```go
func (r *ClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    // ... existing reconciliation ...

    // Dynamic storage reconciliation
    if err := r.reconcileDynamicStorage(ctx, cluster); err != nil {
        log.Error(err, "failed to reconcile dynamic storage")
        // Don't fail the whole reconciliation, but record the error
        r.Recorder.Event(cluster, "Warning", "DynamicStorageError", err.Error())
    }

    // ... rest of reconciliation ...
}
```

### 5.2 Update Status After Actions

Ensure status is updated with:
- `effectiveSize` after growth
- `budget` after any action
- `lastAction` with details
- `state` reflecting current condition

### 5.3 New Replica Provisioning

Modify PVC creation to use `effectiveSize` from status when dynamic sizing is enabled:

```go
func (r *ClusterReconciler) createPVC(ctx context.Context, cluster *apiv1.Cluster, instance string) error {
    size := cluster.Spec.Storage.Size

    // Use effectiveSize from status if dynamic sizing is enabled
    if isDynamicSizingEnabled(cluster.Spec.Storage) {
        if status := cluster.Status.StorageSizing; status != nil && status.Data != nil {
            if status.Data.EffectiveSize != "" {
                size = status.Data.EffectiveSize
            }
        }
    }

    // ... create PVC with size ...
}
```

### Verification Gate

```bash
make build
make test
```

---

## Phase 6: kubectl Plugin

### 6.1 Add Storage Status Command

Create `internal/cmd/plugin/storage/status.go`:

```bash
kubectl cnpg storage status <cluster>
```

Output:
```
Cluster: my-cluster
Dynamic Sizing: Enabled

Data Volume:
  Request:        10Gi
  Limit:          100Gi
  Target Buffer:  20%
  Effective Size: 25Gi
  State:          Balanced

  Instance        PVC Size    Used     Available   Usage%
  my-cluster-1    25Gi        18Gi     7Gi         72%
  my-cluster-2    25Gi        17Gi     8Gi         68%
  my-cluster-3    25Gi        18Gi     7Gi         72%

Budget:
  Max Actions/Day:      4
  Used (24h):           1
  Available (Planned):  2
  Available (Emergency): 1
  Resets At:            2026-02-08T15:30:00Z

Next Maintenance Window: 2026-02-09T03:00:00Z (in 14h)
```

### Verification Gate

```bash
make build
./bin/kubectl-cnpg storage status --help
```

---

## Phase 7: E2E Tests

### 7.1 Test Structure

Create `tests/e2e/dynamic_storage_test.go`.

**CRITICAL NAMING CONVENTION:**
- Use `tests.LabelDynamicStorage` constant (resolves to `"dynamic-storage"`)
- NEVER use abbreviated notation like `"ds-e2e"`
- Follow existing CNPG patterns in `tests/labels.go`
- See `docs/src/design/testing-conventions.md` for full guidelines

Test structure:

```go
// IMPORTANT: Use tests.LabelDynamicStorage constant, NOT hardcoded "ds-e2e" or other abbreviations
// Follow CNPG project conventions: tests/labels.go defines LabelDynamicStorage = "dynamic-storage"
var _ = Describe("Dynamic Storage", Label(tests.LabelStorage, tests.LabelDynamicStorage), func() {
    Context("Basic growth", func() {
        It("grows storage when usage exceeds target buffer", func() {
            // 1. Create cluster with request=5Gi, limit=20Gi, targetBuffer=20
            // 2. Fill disk to 85% (above 80% = 100-20% buffer)
            // 3. Verify PVC is resized
            // 4. Verify status.storageSizing.data.effectiveSize is updated
        })
    })

    Context("Emergency growth", func() {
        It("grows immediately when critical threshold reached", func() {
            // 1. Create cluster with criticalThreshold=95
            // 2. Fill disk to 96%
            // 3. Verify immediate resize (no maintenance window wait)
        })
    })

    Context("Maintenance window", func() {
        It("queues growth for maintenance window when not critical", func() {
            // 1. Create cluster with maintenance window in the future
            // 2. Fill disk to 85%
            // 3. Verify state=PendingGrowth (not immediate resize)
        })

        It("executes growth during maintenance window", func() {
            // 1. Create cluster with maintenance window now
            // 2. Fill disk to 85%
            // 3. Verify resize occurs
        })
    })

    Context("Rate limiting", func() {
        It("respects maxActionsPerDay budget", func() {
            // 1. Create cluster with maxActionsPerDay=2
            // 2. Trigger 2 resizes
            // 3. Trigger condition for 3rd resize
            // 4. Verify resize is blocked
        })
    })

    Context("Limit enforcement", func() {
        It("does not grow beyond limit", func() {
            // 1. Create cluster with limit=15Gi
            // 2. Fill disk to trigger multiple growths
            // 3. Verify PVC never exceeds 15Gi
        })
    })

    Context("New replica provisioning", func() {
        It("creates new replicas at effectiveSize", func() {
            // 1. Create cluster, grow to effectiveSize=20Gi
            // 2. Scale up to add replica
            // 3. Verify new PVC is 20Gi (not request size)
        })
    })

    Context("Tablespace support", func() {
        It("manages tablespace storage independently", func() {
            // 1. Create cluster with tablespace using dynamic sizing
            // 2. Fill tablespace disk
            // 3. Verify tablespace PVC is resized
        })
    })

    Context("Metrics", func() {
        It("exposes Prometheus metrics", func() {
            // 1. Create cluster with dynamic sizing
            // 2. Scrape metrics endpoint
            // 3. Verify cnpg_dynamic_storage_* metrics present
        })
    })
})
```

### 7.2 Azure Test Configuration

```bash
# Azure AKS configuration for E2E tests
export E2E_DEFAULT_STORAGE_CLASS=managed-csi
export E2E_CSI_STORAGE_CLASS=managed-csi

# Controller image used by the AKS runner (required for build/push/deploy)
export CONTROLLER_IMG_BASE=ghcr.io/jmealo/cloudnative-pg-testing
export CONTROLLER_IMG_TAG=dynamic-storage-$(git rev-parse --short HEAD)
export CONTROLLER_IMG="${CONTROLLER_IMG_BASE}:${CONTROLLER_IMG_TAG}"

# Optional runner tuning
export GINKGO_NODES=1
export GINKGO_TIMEOUT=3h
```

### 7.3 Run E2E Tests

```bash
# Run build + push + deploy + dynamic-storage E2E on AKS
/Users/jmealo/repos/cloudnative-pg/hack/e2e/run-aks-e2e.sh

# Iteration mode (reuse existing image/deployment, re-run targeted cases)
/Users/jmealo/repos/cloudnative-pg/hack/e2e/run-aks-e2e.sh --skip-build --skip-deploy --focus "dynamic storage|emergency|maintenance|replica"
```

### Verification Gate

All E2E tests pass:
- [ ] Basic growth
- [ ] Emergency growth
- [ ] Maintenance window queuing
- [ ] Maintenance window execution
- [ ] Rate limiting
- [ ] Limit enforcement
- [ ] New replica provisioning
- [ ] Tablespace support
- [ ] Metrics exposure

---

## Phase 8: Documentation

### 8.1 User Documentation

Create `docs/src/storage_dynamic.md`:
- Feature overview
- Configuration reference with all fields
- Example YAML configurations
- Metrics reference
- Troubleshooting guide
- Migration from auto-resize

### 8.2 Update Existing Docs

Update `docs/src/storage.md` with cross-reference to dynamic storage.

### 8.3 Design RFC

Verify `docs/src/design/dynamic-storage-meta-rfc.md` is complete and accurate.
Verify `docs/src/design/dynamic-storage-e2e-requirements-codex.md` is reflected in E2E coverage and gating.

---

## Phase 9: Final Verification

### 9.1 Code Quality

```bash
make generate manifests fmt
git diff --name-only  # Must be empty

make lint             # Must exit 0
make test             # Must exit 0
make spellcheck       # Must exit 0
make woke             # Must exit 0
```

### 9.2 Build

```bash
make build
```

### 9.3 E2E on Azure

```bash
# Full E2E suite
/Users/jmealo/repos/cloudnative-pg/hack/e2e/run-aks-e2e.sh
```

### 9.4 Commit Structure

Create clean commits following CNPG conventions:

1. `feat(dynamicstorage): add API types for convergent dynamic storage`
2. `feat(dynamicstorage): add disk probing and status collection`
3. `feat(dynamicstorage): add dynamic storage reconciler`
4. `feat(dynamicstorage): add webhook validation`
5. `feat(dynamicstorage): wire reconciler into cluster controller`
6. `feat(dynamicstorage): add kubectl-cnpg storage status command`
7. `test(dynamicstorage): add unit and E2E tests`
8. `docs(dynamicstorage): add design RFC and user documentation`

---

## Iteration Protocol

1. Complete each phase before moving to the next
2. Run verification gates after each phase
3. If a gate fails, fix before proceeding
4. Do not use interactive git commands
5. Network operations are required for image push and AKS execution; use the AKS runner script for all build/push/test flows
6. Paste full terminal output for each verification

If a phase fails after 3 attempts, stop and report what is broken.

---

## Success Criteria

- [ ] All API types defined and generated
- [ ] Webhook validation passes all test cases
- [ ] Dynamic storage reconciler implemented with:
  - [ ] Target size calculation
  - [ ] Emergency growth
  - [ ] Maintenance window scheduling
  - [ ] Rate limit budget tracking
  - [ ] PVC patching
- [ ] New replicas use effectiveSize
- [ ] kubectl plugin displays storage status
- [ ] All E2E tests pass on Azure AKS
- [ ] Documentation complete
- [ ] Clean commit history
- [ ] `make lint test build` all pass
