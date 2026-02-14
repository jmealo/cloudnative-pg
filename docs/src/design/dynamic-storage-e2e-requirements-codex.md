# Dynamic Storage E2E Requirements

## Status

Draft requirements for end-to-end validation of dynamic storage behavior in CloudNativePG.

## Purpose

Define the minimum and extended E2E scenarios required before declaring dynamic storage production-ready. These tests cover normal Kubernetes/CNPG operational events while storage growth/shrink reconciliation is in progress.

This is not chaos testing. These are standard lifecycle and control-plane events expected in real clusters.

## Scope

Applies to:

- `spec.storage` dynamic mode (`request`, `limit`, `targetBuffer`, `maintenanceWindow`, `emergencyGrow`)
- Data volume behavior in Phase 1
- Tablespace dynamic behavior in Phase 1 (where supported by fixtures)

WAL dynamic behavior is out of scope for Phase 1 and should be specified in a separate Phase 2 requirement set.

## Core Invariants (Must Hold in Every Test)

1. No data loss: writes committed before disruption remain queryable after reconciliation.
2. Policy bounds respected: effective provisioned size never drops below `request` and never grows above `limit` (unless explicit emergency override is configured and exercised).
3. Convergence resumes after interruption: controller restarts, pod restarts, and leadership changes must not deadlock storage operations.
4. Replica consistency: new or replaced replicas converge to the current effective operational size.
5. Idempotency: repeated reconcile loops do not re-run completed actions unnecessarily.
6. Status coherence: `status.storageSizing` (or final status field name) reflects the active operation, last action, and next step.

## Topology Matrix Requirements

All P0 scenarios below must run in each of these topologies:

- `T1`: `instances = 1` (no replica)
- `T2`: `instances = 2` (single replica)
- `T3`: `instances >= 3` (multiple replicas)

Topology-specific expectations:

1. `T1`: no failover path; operation may be service-impacting during primary replacement; must recover cleanly.
2. `T2`: one promotion candidate; verify promotion/replica replacement ordering is safe.
3. `T3`: verify operation does not cause unnecessary multi-node churn and keeps quorum-safe behavior.

## P0 Gating Scenarios (Required for Merge)

For a grow-only Phase 1 delivery, treat shrink-related scenarios as Phase 2 gating tests and keep all other P0 tests mandatory.

| Scenario | Description | Required Assertions |
|---|---|---|
| Operator restart during growth | Growth operation in progress + operator pod restart | Operation resumes and completes; no duplicate conflicting operations; final size/status correct |
| Primary pod restart during growth | Growth operation in progress + PostgreSQL primary pod restart | Cluster returns Ready; operation resumes or re-plans; data remains intact |
| Failover during growth | Growth operation in progress + failover/switchover event | Correct primary election; operation continues safely; no size divergence across instances |
| Spec mutation during growth | Growth operation in progress + user spec mutation (`targetBuffer`, `limit`, `instances`) | Reconciler re-evaluates plan deterministically; newer spec intent wins; no stuck state |
| Node drain during growth | Growth operation in progress + node cordon/drain affecting active instance | Eviction/re-scheduling does not orphan operation; cluster converges to intended size |
| Backup during growth | Growth operation in progress + backup creation | Backup succeeds or fails with clear retriable status, but must not corrupt cluster or deadlock storage reconciliation |
| Shrink during failover | Shrink/convergence operation in progress + failover/switchover | Replacement sequence remains safe; one primary at all times; operation completes or safely pauses with actionable status |
| Shrink during spec mutation | Shrink/convergence operation in progress + spec mutation (`request` raised/lowered within valid bounds) | Controller applies updated target safely; no unsafe shrink below used space; final size respects updated policy |
| Replica scale-up after resize | New replica scale-up during/after prior dynamic resize | Newly created PVC starts at effective operational size, not stale bootstrap size |
| Rate limit budget boundaries | Daily action-budget or rate-limit boundaries | Planned actions respect budget; emergency reserve behavior is honored; status exposes budget exhaustion clearly |

## P1 Extended Scenarios (Nightly / Non-Blocking Initially)

| Scenario | Description | Required Assertions |
|---|---|---|
| Concurrent disruptions | Concurrent backup + node drain + in-flight growth | No deadlock; eventual convergence; backup observability remains coherent |
| Rolling upgrade during growth | Rolling image upgrade while dynamic sizing operation is active | Upgrade and sizing controllers interoperate without starvation |
| Volume snapshots | Volume snapshot creation/restore around dynamically resized volumes | Snapshot workflows remain valid and deterministic for effective size |
| Threshold oscillation | Repeated oscillation around threshold band | Hysteresis/cooldown prevents flapping and API churn |

## Required Assertions Per Scenario

Each scenario must assert all of:

1. Functional correctness:
   - Cluster reaches expected steady state.
   - `kubectl get cluster` status conditions are healthy or intentionally degraded with explicit reason.
2. Storage correctness:
   - PVC requests match expected post-operation size per instance.
   - No PVC violates request/limit policy.
3. Data correctness:
   - Sentinel table/checksum query validates data consistency before and after event.
4. Controller correctness:
   - Events/conditions indicate operation progression, pause, or retry reason.
   - No infinite reconcile loop or repeated destructive replace attempt.
5. Timing/SLO guardrails:
   - Operation completes within bounded timeout for CI class (medium/slow).

## Test Design Requirements

1. Use deterministic fixtures and explicit storage classes with online expansion support.
2. Mark disruptive scenarios `Serial` where shared-cluster interference is likely.
3. Prefer scenario-specific namespaces and cleanup hooks.
4. Record operation timeline in test logs: trigger time, disruption time, completion time.
5. For Azure CI, keep payload sizes bounded to avoid provider throttling masking logic failures.
6. Avoid asserting only events; always assert final state plus at least one intermediate state signal.

## Mapping to Existing Dynamic Storage E2E File

Current baseline tests exist in:

- `tests/e2e/dynamic_storage_test.go`

Required next additions from this requirements set:

1. In-flight interruption scenarios (restart/failover/spec mutation/drain/backup).
2. Explicit topology matrix execution (`1`, `2`, `>2` instances).
3. Shrink in-progress operational interaction cases (once shrink path is implemented in phase).

## Exit Criteria

Dynamic storage Phase 1 is E2E-complete when:

1. All applicable P0 scenarios pass on target CI providers (including Azure).
2. Failures produce actionable conditions/events (no silent stalls).
3. No unresolved P0 correctness gaps remain for any topology class (`T1`, `T2`, `T3`).
