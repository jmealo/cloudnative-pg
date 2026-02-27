# RFC: Convergent Dynamic Storage Management

| Field | Value |
|-------|-------|
| **Author** | Jeff Mealo / Gemini |
| **Status** | Draft / Proposal |
| **Created** | 2026-02-08 |
| **Target Release** | TBD |

---

## Summary

This RFC proposes a **Convergent Dynamic Storage** model. By defining a target free-space buffer and operational bounds (Request/Limit), the operator autonomously converges the volume size to the most cost-effective and safe state. 

This model distinguishes between **Emergency Growth** (to prevent downtime) and **Scheduled Convergence** (to optimize cost/topology), ensuring the database is always protected while reclamation remains non-disruptive.

## API Design: The Convergent Model

```yaml
spec:
  storage:
    size: 10Gi # Bootstrap size.

    dynamic:
      enabled: true
      request: 10Gi       # Provisioning Floor (Steady State Target)
      limit: 100Gi        # Provisioning Ceiling (Emergency Cap)
      targetBuffer: 20    # % Free space to maintain (Default: 20)

      # Maintenance Window for non-urgent convergence (both Grow and Shrink)
      maintenanceWindow:
        dayOfWeek: "Sunday"
        startTime: "02:00"
        duration: "4h"

      # Optional: Enable shrink via replacement
      shrink:
        enabled: true
```

## Operational Logic: Emergency vs. Convergence

The operator monitors `statfs` and categorizes storage actions into two priority tiers:

### 1. Emergency Grow (High Priority)
- **Trigger**: Available space drops below a "critical" threshold (e.g., 5% or 1/2 of `targetBuffer`).
- **Timing**: **Immediate**. Runs outside maintenance windows.
- **Goal**: Prevent the database from crashing or entering read-only mode.
- **Bound**: Still respects the `limit` unless an emergency override is manually applied.

### 2. Scheduled Convergence (Low Priority)
- **Trigger**: Current size != `TargetSize` (based on `targetBuffer`).
- **Timing**: **Maintenance Window Only**.
- **Actions**:
    - **Grow**: If we are slightly below the buffer but not in "emergency" territory, grow during the window to avoid consuming cloud modification quotas during the day.
    - **Shrink**: If we are over-provisioned (Usage << Target), perform a rolling replacement to reclaim space.

## Scope: Data & Tablespaces (V1) vs. WAL (V2)

### V1: Data and Tablespaces
These volumes are the primary source of storage-related toil. The `targetBuffer` model applies cleanly here as growth is usually proportional to data ingestion.

### V2: WAL Storage
WAL-aware safety is significantly more complex (requiring checks on archive health, replication slots, and pending files). Because of this complexity, **Dynamic WAL Storage is deferred to Phase 2**. 
- In Phase 1, WAL storage remains static or uses the basic Autoresize mechanism.
- This ensures the core Dynamic Management feature can ship without being blocked by the intricacies of PostgreSQL's WAL-safety logic.

## Core Advantages

1. **Idiomatic GitOps**: `spec` defines policy (bounds/buffers); `status` tracks operational state (`currentSize`). No drift.
2. **Reduced API Quota Pressure**: By moving non-urgent growth to maintenance windows, we preserve cloud provider volume modification quotas for emergencies.
3. **Homoegeneity**: New replicas match the cluster's `currentSize` immediately upon creation.
4. **Predictability**: Operators can easily reason about the "Steady State" (Request) and the "Worst Case" (Limit).

## Tooling: `kubectl cnpg storage`
- `status`: Visualize the usage band between request and limit.
- `converge`: Manually trigger a convergence cycle (respecting the maintenance window logic unless forced).