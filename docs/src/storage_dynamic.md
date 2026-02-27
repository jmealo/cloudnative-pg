# Dynamic Storage Sizing

This section describes CloudNativePG's dynamic storage sizing feature, which
enables automatic storage management within user-defined bounds while
maintaining a target free-space buffer.

## Overview

Dynamic storage sizing provides convergent storage management that
automatically grows your PostgreSQL storage as needed, within operator-defined
bounds. This removes the burden of manual storage management while preventing
runaway growth through hard limits.

Key features:

- **Request/Limit bounds**: Define minimum and maximum storage sizes
- **Target buffer**: Maintain a percentage of free space
- **Maintenance windows**: Schedule non-urgent growth operations
- **Emergency growth**: Immediate growth when critical thresholds are reached
- **Rate limiting**: Budget controls to prevent excessive resize operations

## Enabling Dynamic Sizing

To enable dynamic sizing, replace the `size` field with `request` and `limit`:

```yaml
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: my-cluster
spec:
  instances: 3
  storage:
    request: "10Gi"    # Minimum size (floor)
    limit: "100Gi"     # Maximum size (ceiling)
    targetBuffer: 20   # Target 20% free space
```

!!! Important
    The `size` field is mutually exclusive with `request` and `limit`.
    You cannot use both approaches simultaneously.

## Configuration Reference

### StorageConfiguration Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `request` | string | - | Minimum provisioned size (floor) |
| `limit` | string | - | Maximum provisioned size (ceiling) |
| `targetBuffer` | int | 20 | Desired free space percentage (5-50%) |
| `maintenanceWindow` | object | nil | When non-urgent sizing operations occur |
| `emergencyGrow` | object | nil | Controls growth outside maintenance windows |

### MaintenanceWindow Configuration

```yaml
storage:
  request: "10Gi"
  limit: "100Gi"
  maintenanceWindow:
    schedule: "0 3 * * *"   # Cron syntax: 3 AM daily
    duration: "2h"          # 2-hour window
    timezone: "UTC"         # Timezone for schedule
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `schedule` | string | "0 3 * * *" | Cron syntax schedule |
| `duration` | string | "2h" | Window duration |
| `timezone` | string | "UTC" | Timezone for the schedule |

### EmergencyGrow Configuration

```yaml
storage:
  request: "10Gi"
  limit: "100Gi"
  emergencyGrow:
    enabled: true
    criticalThreshold: 95     # Grow immediately at 95% usage
    criticalMinimumFree: "1Gi" # Or when free space drops below 1Gi
    maxActionsPerDay: 4       # Max resize operations in 24h
    reservedActionsForEmergency: 1  # Actions reserved for emergencies
    exceedLimitOnEmergency: false   # Allow exceeding limit as last resort
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | true | Enable emergency growth |
| `criticalThreshold` | int | 95 | Usage % triggering emergency growth |
| `criticalMinimumFree` | string | "1Gi" | Minimum free space threshold |
| `maxActionsPerDay` | int | 4 | Max resize operations per 24h |
| `reservedActionsForEmergency` | int | 1 | Actions reserved for emergency |
| `exceedLimitOnEmergency` | bool | false | Allow exceeding limit in emergency |

## How It Works

### Size Calculation

The operator calculates the target size using the formula:

```
targetSize = usedBytes / (1 - targetBuffer%)
```

For example, with 8 GiB used and 20% target buffer:

```
targetSize = 8 GiB / 0.80 = 10 GiB
```

The target is then clamped to the request/limit bounds.

### Growth Behavior

1. **Balanced State**: Free space is at or above target buffer. No action needed.

2. **NeedsGrow State**: Free space is below target buffer but not critical.
   Growth is scheduled for the next maintenance window.

3. **Emergency State**: Usage exceeds critical threshold or free space drops
   below critical minimum. Growth happens immediately regardless of
   maintenance window.

4. **PendingGrowth State**: Growth is needed but waiting for maintenance window.

### Rate Limiting

The operator tracks resize operations in a rolling 24-hour window:

- `maxActionsPerDay`: Total allowed operations
- `reservedActionsForEmergency`: Actions held back for emergencies
- `availableForPlanned`: Remaining actions for scheduled growth

This prevents excessive resizing during unusual workload spikes.

## Example Configurations

### Development Cluster

Minimal configuration with generous growth room:

```yaml
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: dev-cluster
spec:
  instances: 1
  storage:
    request: "5Gi"
    limit: "50Gi"
    targetBuffer: 25
```

### Production Cluster

Full configuration with maintenance windows and rate limiting:

```yaml
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: prod-cluster
spec:
  instances: 3
  storage:
    request: "100Gi"
    limit: "1Ti"
    targetBuffer: 20
    storageClass: premium-ssd
    maintenanceWindow:
      schedule: "0 3 * * 0"  # Sunday at 3 AM
      duration: "4h"
      timezone: "America/New_York"
    emergencyGrow:
      enabled: true
      criticalThreshold: 95
      criticalMinimumFree: "10Gi"
      maxActionsPerDay: 4
      reservedActionsForEmergency: 2
```

### Tablespace with Dynamic Sizing

```yaml
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: cluster-with-tablespace
spec:
  instances: 3
  storage:
    request: "50Gi"
    limit: "200Gi"
  tablespaces:
    - name: archive_data
      storage:
        request: "100Gi"
        limit: "1Ti"
        targetBuffer: 30
```

## Monitoring

### Status

Check dynamic sizing status via kubectl:

```bash
kubectl cnpg storage status my-cluster
```

Output example:

```
Cluster: my-cluster
Dynamic Sizing: Enabled

Data Volume Configuration:
  Request:        10Gi
  Limit:          100Gi
  Target Buffer:  20%

Data Volume Status:
  Effective Size: 25Gi
  Target Size:    25Gi
  State:          Balanced

Budget:
  Max Actions/Day:      4
  Used (24h):           1
  Available (Planned):  2
  Available (Emergency): 1
  Resets At:            2026-02-09T12:30:00Z

PVCs:
NAME                    ROLE        SIZE    STATUS
my-cluster-1            PGData      25Gi    Bound
my-cluster-2            PGData      25Gi    Bound
my-cluster-3            PGData      25Gi    Bound
```

### Prometheus Metrics

Dynamic storage exposes the following metrics:

| Metric | Description |
|--------|-------------|
| `cnpg_disk_total_bytes` | Total filesystem size |
| `cnpg_disk_used_bytes` | Used bytes on filesystem |
| `cnpg_disk_available_bytes` | Available bytes |
| `cnpg_disk_percent_used` | Percentage used |
| `cnpg_dynamic_storage_effective_size_bytes` | Current effective size |
| `cnpg_dynamic_storage_budget_remaining` | Remaining daily budget |

### Cluster Status

The cluster status includes storage sizing information:

```yaml
status:
  storageSizing:
    data:
      effectiveSize: "25Gi"
      targetSize: "25Gi"
      state: "Balanced"
      budget:
        actionsLast24h: 1
        availableForPlanned: 2
        availableForEmergency: 1
      lastAction:
        kind: "ScheduledGrow"
        from: "20Gi"
        to: "25Gi"
        timestamp: "2026-02-08T03:15:00Z"
        instance: "my-cluster-1"
        result: "Success"
```

## Requirements

### Storage Class

Your storage class must support volume expansion:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: expandable-storage
allowVolumeExpansion: true  # Required
provisioner: ...
```

### Cluster Configuration

The cluster must have `resizeInUseVolumes` enabled (default is true):

```yaml
spec:
  storage:
    resizeInUseVolumes: true  # Default
```

## Migration from Static Sizing

To migrate from static (`size`) to dynamic (`request`/`limit`) sizing:

1. Ensure your storage class supports volume expansion
2. Update the cluster spec:

```yaml
# Before
storage:
  size: "50Gi"

# After
storage:
  request: "50Gi"   # Current size becomes the request
  limit: "200Gi"    # Set appropriate limit
  targetBuffer: 20
```

3. Apply the change. Existing PVCs will not be shrunk.

## Limitations

- **No shrinking**: Storage can only grow, never shrink
- **CSI support required**: Underlying storage must support online expansion
- **Single storage class**: All instances use the same storage class
- **Pod disruption**: Some cloud providers require pod restart for expansion

## Troubleshooting

### PVC Not Growing

1. Check if maintenance window is open or emergency conditions are met
2. Verify budget has available actions
3. Check storage class supports expansion:
   ```bash
   kubectl get storageclass <name> -o yaml | grep allowVolumeExpansion
   ```

### Growth Pending

If state shows "PendingGrowth":

1. Wait for maintenance window, or
2. Reduce usage below emergency threshold, or
3. Manually trigger by adjusting thresholds

### Budget Exhausted

If no actions are available:

1. Wait for 24h rolling window to reset
2. Check `budgetResetsAt` in status
3. Consider increasing `maxActionsPerDay` if legitimate
