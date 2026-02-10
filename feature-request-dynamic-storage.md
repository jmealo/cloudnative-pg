---
name: Feature request
title: "[Feature]: Convergent Dynamic Storage Management (request/limit/targetBuffer)"
labels: ["triage", "enhancement", "storage"]
---

### Is there an existing issue already for this feature request/idea?
- [x] I have searched for an existing issue, and could not find anything. I believe this is a new feature request.

### What problem is this feature going to solve? Why should it be added?

CNPG storage management has three rough edges:

1. **Out of disk**: Manual intervention required, risking downtime and data loss
2. **Spec drift after growth**: Auto-resize patches PVCs but not spec, breaking GitOps and causing new replicas to start at wrong size
3. **Over-provisioned storage**: No path to shrink, temporary spikes cause permanent cost overhead

The existing auto-resize RFC solves #1 but creates #2 and doesn't address #3.

### Describe the solution you'd like

**Convergent Dynamic Storage Management** using familiar Kubernetes semantics:

```yaml
storage:
  request: 10Gi           # Floor (minimum provisioned)
  limit: 100Gi            # Ceiling (maximum provisioned)
  targetBuffer: 20        # % free space to maintain
  maintenanceWindow:
    schedule: "0 3 * * 0" # Sundays 3 AM
    duration: "4h"
  emergencyGrow:
    criticalThreshold: 95
```

**How it works:**

| Action | Trigger | Timing |
|--------|---------|--------|
| **EmergencyGrow** | Free space critical (<5%) | Immediate (outside window) |
| **ScheduledGrow** | Free space below buffer | Maintenance window |
| **ScheduledShrink** | Free space significantly above buffer | Maintenance window |

**Key design decisions:**

1. **Policy in spec, state in status**: User declares bounds, controller reports `effectiveSize` in status. No spec mutation = no GitOps drift.
2. **Logical volume identity**: Budget tracking survives PVC replacement.
3. **New replicas use effectiveSize**: No more "create small, immediately resize."
4. **Shrink is intentional**: Via `kubectl cnpg storage shrink`, not automatic (it's always a major operation).

### Describe alternatives you've considered

| Alternative | Rejected Because |
|-------------|-----------------|
| Auto-resize only (current RFC) | Creates spec/PVC drift, no shrink path, 15+ config fields |
| Manual shrink via backup/restore | High toil, error-prone |
| External volume manager | Adds operational complexity |
| Threshold + hysteresis | Less intuitive than request/limit model |

### Additional context

**Phasing:**

- **Phase 1**: Dynamic sizing for data + tablespaces (`request`/`limit`/`targetBuffer`, maintenance windows, emergency growth)
- **Phase 2**: WAL safety checks, `kubectl cnpg storage shrink`, WAL volume support
- **Phase 3**: Grafana dashboards, PrometheusRule alerts, recovery time estimation

**Configuration comparison:**

Auto-resize RFC (15+ fields):
```yaml
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
    walSafetyPolicy: ...
```

Dynamic sizing (5 core fields):
```yaml
request: 100Gi
limit: 500Gi
targetBuffer: 20
maintenanceWindow: ...
emergencyGrow: ...
```

**Related RFCs:**
- `docs/src/design/dynamic-storage-meta-rfc.md` (synthesized from 3 proposals)
- `docs/src/design/pvc-autoresize.md` (superseded)

**Related issues:**
- #9928, #9927, #1808, #5083
