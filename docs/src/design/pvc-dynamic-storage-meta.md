# RFC: Dynamic Storage Management (Meta-Synthesis)

| Field | Value |
|-------|-------|
| **Author** | Meta-Synthesis (Gemini/Opus/Codex) |
| **Status** | Definitive Design |
| **Created** | 2026-02-08 |
| **Target Release** | TBD |
| **Supersedes** | All previous storage RFCs |

---

## Summary

This RFC defines the unified **Dynamic Storage Management** architecture for CloudNativePG. It synthesizes the **Request/Limit** resource model, the **Status-as-Truth** state management, and the **Convergent Reconciliation** loop into a single feature set. 

This design solves:
1.  **GitOps Drift**: By separating Policy (Spec) from State (Status).
2.  **Replica Heterogeneity**: By using Cluster Status to synchronize new replicas.
3.  **Cost Optimization**: By automating reclamation (shrink) during maintenance windows.
4.  **Uptime Safety**: By enabling immediate emergency expansion.

## 1. API Design: The "Autopilot" Interface

The configuration favors standard Kubernetes terminology (`requests`, `limits`) over complex custom configurations.

```yaml
spec:
  storage:
    # Legacy Bootstrap/Fallback.
    # Acts as the implicit 'request' if dynamic is enabled but request is unset.
    size: 10Gi

    dynamic:
      enabled: true
      
      # 1. The Policy (Desired State)
      request: 100Gi      # Floor: Steady-state target. Operator converges usage DOWN to this.
      limit: 1Ti          # Ceiling: Absolute max. Operator expands UP to this.
      targetBuffer: 20    # Buffer: Desired % free space (Default: 20).

      # 2. The Strategy (Operational Constraints)
      # Controls non-urgent convergence (both minor grow and all shrink)
      maintenanceWindow:
        dayOfWeek: "Sunday"
        startTime: "02:00"
        duration: "4h"
      
      # Optional: Explicit opt-in for destructive shrinking
      shrink:
        enabled: true
        minReclaimThreshold: "50Gi" # Hysteresis: Don't re-clone for < 50Gi gain
```

## 2. Architecture: The Source of Truth

To solve the GitOps/Replica drift problem, we introduce a split-brain model where **Spec is Policy** and **Status is Reality**.

### Cluster Status Updates
```go
type ClusterStatus struct {
    // ... existing fields
    
    // DynamicStorage tracks the operational size of the cluster.
    // This, NOT spec.storage.size, is used when creating new PVCs.
    DynamicStorage *DynamicStorageStatus `json:"dynamicStorage,omitempty"`
}

type DynamicStorageStatus struct {
    // CurrentSize is the active size of the Primary's PVC.
    // All replicas must converge to this size.
    CurrentSize resource.Quantity `json:"currentSize"`
    
    // LastResizeEvent tracks history for cooldown logic.
    LastResizeEvent *metav1.Time `json:"lastResizeEvent,omitempty"`

    // Message explaining current state (e.g. "Waiting for Maintenance Window")
    Message string `json:"message,omitempty"`
}
```

## 3. The Convergent Control Loop

The reconciler runs a two-tier evaluation logic:

### Tier 1: Emergency Check (Run Always)
*Condition:* `AvailableSpace < CriticalThreshold` (e.g., < 10% or < 5GB).
*Action:* **IMMEDIATE EXPANSION**.
- Ignores Maintenance Window.
- Ignores Cooldowns.
- Respects `limit` (unless `AvailableSpace < 1GB`, in which case it may optionally break limit to save data, though standard implementation caps at limit).
- **Mechanism**: CSI Volume Expansion (Patch PVC).

### Tier 2: Convergence Check (Run in Maintenance Window)
*Condition:* `CurrentSize != TargetSize` (where Target is derived from Usage + Buffer, clamped by Request/Limit).

#### Case A: Non-Urgent Growth
- *Scenario:* Buffer is 18% (Target 20%), but not critical.
- *Action:* Expand during window to restore optimal buffer.
- *Mechanism:* CSI Volume Expansion (Patch PVC).

#### Case B: Reclamation (Shrink)
- *Scenario:* `CurrentSize (500Gi) > TargetSize (200Gi) + minReclaimThreshold (50Gi)`.
- *Action:* **Rolling Replacement**.
- *Mechanism:*
    1.  Lock operation (pause other resizes).
    2.  Check Safety: `UsedBytes < TargetSize * 0.8` (Ensure we fit).
    3.  For each Replica:
        -   Delete Replica Pod & PVC.
        -   Create new PVC at `TargetSize`.
        -   Wait for Replica to clone/stream and become Ready.
    4.  Switchover Primary to a shrunk Replica.
    5.  Replace old Primary.
    6.  Update `status.dynamicStorage.currentSize`.

## 4. Implementation Details

### Phase 1: Data & Tablespaces
-   Target `spec.storage` and `spec.tablespaces`.
-   Defer `spec.walStorage` to Phase 2 (due to complex archive safety checks).

### The "Re-clone Tax"
-   Shrink is expensive. The default `minReclaimThreshold` should be high (e.g., 10% of capacity or 50Gi) to prevent "flapping" (shrinking then immediately growing).

## 5. Tooling
-   `kubectl cnpg storage status`: View Policy vs. Reality.
-   `kubectl cnpg storage estimate --target X`: Dry-run a shrink to estimate downtime/re-clone time.

## 6. E2E Testing Plan
1.  **Drift Test**: Scale up, verify new replica gets `CurrentSize` (not Spec size).
2.  **Emergency Test**: Fill disk, verify immediate growth outside window.
3.  **Maintenance Test**: Fill disk slightly, verify wait for window.
4.  **Shrink Test**: Free space, verify shrink during window via rolling replace.
