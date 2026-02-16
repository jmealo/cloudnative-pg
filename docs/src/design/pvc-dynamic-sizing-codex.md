# RFC: Dynamic Storage Sizing with Maintenance Windows for CloudNativePG

| Field | Value |
|-------|-------|
| **Author** | Jeff Mealo (drafted with Codex support) |
| **Status** | Draft / Request for Comments |
| **Created** | 2026-02-08 |
| **Target Release** | TBD |
| **Supersedes** | none |
| **Related RFCs** | `docs/src/design/pvc-autoresize.md`, `docs/src/design/pvc-declarative-shrink.md` |

---

## Summary

This RFC proposes a **dynamic sizing policy** for CNPG volumes that can both
grow and shrink while preserving GitOps semantics and predictable operations.

Core idea:

1. Keep `spec` as declarative **policy**, not mutable observed state.
2. Maintain an operator-managed **effective provisioned size** per logical
   volume class.
3. Start with a straightforward policy contract:
   - `request` (floor)
   - `limit` (ceiling)
   - `targetBuffer` (desired free-space band)
   - `maintenanceWindow` (non-urgent convergence)
   - `emergencyGrow` (outside-window safety behavior)
4. Perform non-urgent sizing actions in **maintenance windows**.
5. Allow emergency growth outside maintenance windows to prevent disk-full
   failure.

This model addresses known rough edges in the auto-resize-only approach:

1. spec/PVC drift when patching PVC directly
2. new replicas starting from stale declarative size
3. snapshot restore mismatch risks when source PVCs are larger than declared
   size

---

## Motivation

The current auto-resize direction solves "out of disk" growth but leaves
important lifecycle gaps:

1. No first-class reclaim/downsize path.
2. Temporary pressure events can permanently over-provision storage.
3. If only one PVC is patched, volume sizes can diverge across instances.
4. New replicas may be created at a smaller declarative size and require
   immediate follow-up resizing.
5. GitOps users reasonably prefer not to have operator-written size changes in
   spec.

Dynamic sizing aims to unify emergency growth and planned reclamation behind one
policy contract.

---

## Goals

1. Provide a single policy model for grow + shrink behavior.
2. Keep user-declared spec GitOps-stable (no mandatory writeback).
3. Guarantee safe emergency growth paths outside maintenance windows.
4. Run non-urgent right-sizing during maintenance windows.
5. Make replica provisioning use an effective size to avoid immediate-resize and
   snapshot-size mismatch traps.
6. Preserve deterministic interaction with WAL safety and cloud action budgets.
7. Keep V1 configuration surface small and easy to reason about.

## Non-Goals

1. In-place PVC/filesystem shrinking.
2. Cross-storage-class migration.
3. Perfectly equal PVC sizes at every instant during emergency handling.
4. New operation CRD as the default control surface.

---

## Design Principles

1. **Policy in spec, state in status**
   - spec defines intent and constraints.
   - status reports effective size and action history.

2. **Logical volume identity over PVC identity**
   - budgeting and policy decisions must survive PVC replacement and pod
     re-creation.

3. **Urgency-aware orchestration**
   - emergency actions prioritize availability.
   - non-urgent optimization actions obey windows and hysteresis.

4. **GitOps-first defaults**
   - no required spec mutation by controller.

---

## Proposed API Direction (V1 Minimal)

For V1, the dynamic policy surface is intentionally minimal.

The eventual policy shape applies to:

1. `spec.storage`
2. `spec.walStorage`
3. `spec.tablespaces[*].storage`

V1 scope applies this to `storage` and tablespaces first; WAL follows in Phase 2.

```yaml
spec:
  storage:
    request: 10Gi   # floor: never shrink below
    limit: 100Gi    # ceiling: never grow beyond
    targetBuffer:
      minFreePercent: 20  # default: 20
    maintenanceWindow:
      enabled: true
      timezone: UTC
      days: [Mon, Tue, Wed, Thu, Fri]
      startTime: "01:00"
      duration: 2h
    emergencyGrow:
      enabled: true
      allowOutsideWindow: true
      allowLimitOverride: false
```

### Field semantics

1. `request`
   - minimum effective size for that logical volume class.
2. `limit`
   - maximum effective size for that logical volume class.
3. `targetBuffer`
   - desired free-space band the controller attempts to maintain between
     `request` and `limit`.
4. `maintenanceWindow`
   - scheduling gate for non-urgent grow/shrink convergence actions.
5. `emergencyGrow`
   - controls emergency behavior outside maintenance windows; defaults should
     prioritize safety while still honoring `limit` unless explicit override is
     enabled.

### Static compatibility

Clusters that keep existing `size` configuration remain static behavior.
Dynamic behavior is enabled only when the new policy fields are present.

### Why this is simpler than the prior auto-resize surface

V1 replaces a large tunable set (triggers, step controls, expansion limits,
strategy knobs) with one policy set:

1. floor (`request`)
2. ceiling (`limit`)
3. target free-space band (`targetBuffer`)
4. scheduling window (`maintenanceWindow`)
5. emergency policy (`emergencyGrow`)

Advanced step tuning and policy controls remain possible as future
enhancements if needed, but are not required for the first release.

---

## Status Model (Strawman)

Add status fields for effective sizing state:

```yaml
status:
  storageSizing:
    data:
      policy:
        request: 10Gi
        limit: 100Gi
        targetBuffer:
          minFreePercent: 20
        emergencyGrow:
          enabled: true
          allowOutsideWindow: true
          allowLimitOverride: false
      effectiveSize: 180Gi
      targetSize: 180Gi
      lastAction:
        kind: EmergencyGrow # EmergencyGrow | ScheduledGrow | ScheduledShrink
        from: 150Gi
        to: 180Gi
        time: "2026-02-08T11:30:00Z"
      budget:
        actionsLast24h: 2
        reserveRemaining: 1
      conditions:
        - type: MaintenanceWindowBlocked
          status: "False"
```

Equivalent blocks for WAL and each tablespace identity.

---

## Control-Loop Behavior

### Logical identities

Budgeting and policy are keyed by logical identity:

1. cluster + volume class (`data`)
2. cluster + volume class (`wal`)
3. cluster + volume class + tablespace name

Not keyed by PVC name.

### Decision flow

1. Collect disk and WAL safety status.
2. Compute candidate desired effective size from
   `request/limit/targetBuffer`.
3. Classify action:
   - `EmergencyGrow`
   - `ScheduledGrow`
   - `ScheduledShrink`
   - `NoOp`
4. Evaluate gates:
   - WAL safety
   - budget
   - maintenance window (skip only for emergency)
   - built-in anti-oscillation defaults (cooldown, hysteresis, min delta)
5. Execute action.
6. Update effective size/status/events.

### Emergency behavior

Emergency growth can bypass maintenance window but must:

1. respect hard `limit` bounds by default
2. consume from rate-limit budget with reserved emergency allowance
3. emit high-signal events/conditions
4. only exceed `limit` when `emergencyGrow.allowLimitOverride=true`

### Scheduled behavior

Scheduled grow/shrink runs only inside maintenance window and obeys
built-in hysteresis/cooldown defaults to avoid oscillation.

### V1 defaulted strategy behavior

To keep API surface small, V1 keeps strategy knobs mostly internal defaults:

1. safe step sizing/clamping
2. per-logical-volume daily action budgets
3. cooldown/hysteresis defaults
4. WAL safety integration for WAL-relevant scopes

These can be exposed later if strong operational need appears.

---

## Replica Provisioning and Snapshot Implications

In dynamic mode, new PVC provisioning must use **effective size**, not only the
raw declarative floor (`request`, or legacy `size` where applicable).

Benefits:

1. avoids immediate "new replica then instant resize" loop
2. avoids snapshot restore into too-small target when source snapshot was taken
   from a larger effective PVC

When effective size is smaller than snapshot source constraints allow, controller
must choose a compatible bootstrap path (for example streaming/basebackup) or
delay action with explicit status.

---

## Interaction with Existing Auto-Resize Logic

Dynamic mode replaces the need for separate "auto grow only" semantics in the
same scope.

Required invariants:

1. one active sizing owner per logical scope
2. shrink/grow lock to prevent competing actions
3. immediate-regrow preflight check before scheduled shrink
4. budget accounting continuity across PVC replacement

---

## Maintenance Window Semantics

Maintenance window applies to:

1. scheduled growth
2. scheduled shrink
3. convergence actions that are not emergency-critical

Maintenance window does **not** block:

1. emergency growth needed to avoid disk-full risk

If outside window and non-urgent action is due, report pending condition and
next eligible window in status.

---

## GitOps and Spec Semantics

Default policy:

1. controller patches PVCs/replacements and status
2. controller does not rewrite user size policy fields in spec

Spec remains stable and reviewable; status shows current effective runtime size.

Optional future enhancement (not part of this RFC):

1. explicit opt-in "spec writeback" for non-GitOps users

---

## Safety and Failure Handling

1. Any scheduled shrink must pass:
   - data-fit + reserve
   - immediate-regrow prediction
   - WAL safety checks
2. If replacement fails mid-flight:
   - pause further actions for that scope
   - keep cluster available
   - surface resumable failure state
3. If budget exhausted:
   - reserve emergency slot if configured
   - defer non-urgent actions

---

## Migration and Compatibility

1. Existing clusters without dynamic policy fields continue current behavior.
2. Existing `resize` block remains supported.
3. Validation should reject conflicting config where both legacy and dynamic
   policy attempt to control the same scope simultaneously.
4. A migration guide can map legacy intent to dynamic policy where possible:
   - floor from current declarative size
   - ceiling from existing expansion limit (if set)
   - target buffer from threshold defaults (or explicit policy choice)

## Configuration Surface Reduction (V1)

Compared to the prior auto-resize RFC surface, V1 intentionally collapses
policy to:

1. `request` (floor)
2. `limit` (ceiling)
3. `targetBuffer` (target free-space policy)
4. `maintenanceWindow` (for non-urgent actions)
5. `emergencyGrow` (outside-window emergency behavior)

Fields from the prior model such as trigger thresholds, explicit step/min/max
step, and multiple strategy knobs are treated as controller defaults in V1.
They can be reintroduced later only if operational evidence shows they are
necessary.

---

## Alternatives Considered

### Alternative A: Keep original auto-resize RFC model as primary (`pvc-autoresize.md`)

Summary:

1. patch PVC directly on threshold breach
2. do not patch cluster spec
3. growth only, no integrated reclaim
4. higher user-facing config surface (triggers, expansion strategy, step
   clamping, WAL strategy knobs)

Strengths:

1. lower initial implementation scope
2. solves urgent disk-full growth

Limitations that motivated this RFC:

1. no first-class shrink lifecycle
2. spec/runtime divergence without unified policy semantics
3. replica creation and snapshot restore friction when declarative size lags
   runtime effective size

Conclusion:

valuable foundational work, but not a full lifecycle policy model.

### Alternative B: Static sizing + explicit declarative shrink only

Strength:

1. minimal conceptual change from current behavior

Limitation:

1. does not unify ongoing buffer management and maintenance-window scheduling
   as a single policy.

### Alternative C: Separate operation CRD for shrink/grow actions

Strength:

1. explicit operational objects and action audit trail

Limitation:

1. adds API surface and shifts away from "spec as desired state" for common
   lifecycle operations.

---

## Open Questions

1. Should dynamic mode be GA directly, or alpha behind feature gate?
2. Which maintenance-window syntax best aligns with CNPG conventions?
3. Should V1 expose strategy knobs (step size, budgets, hysteresis), or keep
   them as internal defaults behind the minimal
   `request/limit/targetBuffer` API?
4. Should emergency actions ever exceed `limit` with explicit override policy?
5. How should volume-level effective sizes converge across instances after an
   emergency one-off grow?

---

## Proposed Phasing

### Phase 1

1. Dynamic mode for data + tablespaces with
   `request/limit/targetBuffer/maintenanceWindow/emergencyGrow`.
2. Maintenance-window scheduling for non-urgent actions.
3. Emergency grow bypass outside window.
4. Effective-size status model and logical-identity budget tracking.
5. Keep WAL on existing model in this phase.

### Phase 2

1. WAL dynamic policy support with WAL safety integration.
2. Replica provisioning path hardening for snapshot/source constraints.
3. Optional exposure of additional strategy knobs (if justified).

### Phase 3

1. Policy tuning and defaults from production telemetry.
2. Optional enhanced convergence policies.

---

## References

1. `docs/src/design/pvc-autoresize.md`
2. `docs/src/design/pvc-declarative-shrink.md`
3. `docs/src/storage_autoresize.md`
4. `docs/src/storage.md`
5. [Kubernetes PVC expansion docs](https://kubernetes.io/docs/concepts/storage/persistent-volumes/#expanding-persistent-volumes-claims)
6. [Kubernetes expansion failure recovery docs](https://kubernetes.io/docs/concepts/storage/persistent-volumes/#recovering-from-failure-when-expanding-volumes)
7. [CSI Volume Snapshot API](https://kubernetes-csi.github.io/docs/api/volume-snapshot.html)
