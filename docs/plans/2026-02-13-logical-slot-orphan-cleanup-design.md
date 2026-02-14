# Design: Fix Orphaned Logical Slots After Switchover (Issue #9969)

| Field | Value |
|-------|-------|
| **Author** | Claude Code |
| **Status** | Approved |
| **Created** | 2026-02-13 |
| **Issue** | https://github.com/cloudnative-pg/cloudnative-pg/issues/9969 |

---

## Summary

When `synchronizeLogicalDecoding: true` is enabled with PostgreSQL 17+, logical replication slots with `synced=false` on demoted primaries cause WAL accumulation. This design adds automatic cleanup of these orphaned slots during the slot reconciliation loop.

## Problem

1. PostgreSQL 17 introduced native slot synchronization via the `sync_replication_slots` parameter
2. Slots created locally have `synced=false`; slots synchronized from primary have `synced=true`
3. When a primary demotes to replica, its local logical slots retain `synced=false`
4. PostgreSQL's sync worker cannot update slots with `synced=false` (they're read-only)
5. Result: orphaned slots accumulate WAL indefinitely

**Error message in logs:**
```
exiting from slot synchronization because same name slot already exists
```

## Solution

Clean up logical slots with `synced=false` on replicas when `synchronizeLogicalDecoding=true` and PostgreSQL 17+. This allows PostgreSQL's native sync worker to recreate the slots with `synced=true`.

## Architecture

```
+-------------------------------------------------------------+
|                    Instance Reconciliation                   |
+-------------------------------------------------------------+
|  ReconcileReplicationSlots()                                 |
|     |                                                        |
|     +---> Is replica AND synchronizeLogicalDecoding=true     |
|          AND PostgreSQL 17+?                                 |
|               |                                              |
|               +---> CleanupOrphanedLogicalSlots()            |
|                    - Query pg_replication_slots              |
|                      WHERE slot_type='logical'               |
|                      AND synced=false                        |
|                    - Drop each matching slot                 |
|                                                              |
|     +---> (Existing HA slot reconciliation continues...)     |
+-------------------------------------------------------------+
```

## Files to Modify

### 1. `internal/management/controller/slots/infrastructure/replicationslot.go`

Add new struct:

```go
// LogicalReplicationSlot represents a logical replication slot
type LogicalReplicationSlot struct {
    SlotName   string
    Plugin     string
    Active     bool
    RestartLSN string
    Synced     bool  // PG17+: false means locally created, not synced from primary
}
```

### 2. `internal/management/controller/slots/infrastructure/postgresmanager.go`

Add new functions:

```go
// ListLogicalSlotsWithSyncStatus lists logical slots with synced status (PG17+)
func ListLogicalSlotsWithSyncStatus(ctx context.Context, db *sql.DB) ([]LogicalReplicationSlot, error) {
    // Query: SELECT slot_name, plugin, active, restart_lsn, synced
    //        FROM pg_replication_slots
    //        WHERE slot_type = 'logical'
}

// DeleteLogicalSlot drops a logical replication slot by name
func DeleteLogicalSlot(ctx context.Context, db *sql.DB, slotName string) error {
    // Execute: SELECT pg_drop_replication_slot($1)
}
```

### 3. `internal/management/controller/slots/reconciler/replicationslot.go`

Modify `ReconcileReplicationSlots()` to add cleanup call:

```go
func ReconcileReplicationSlots(...) {
    // NEW: Check for orphaned logical slots on replicas with synchronizeLogicalDecoding
    if !isPrimary && isSynchronizeLogicalDecodingEnabled(cluster) {
        pgMajor, _ := cluster.GetPostgresqlMajorVersion()
        if pgMajor >= 17 {
            if err := cleanupOrphanedLogicalSlots(ctx, db); err != nil {
                return reconcile.Result{}, err
            }
        }
    }
    // ... existing code
}

// cleanupOrphanedLogicalSlots removes logical slots with synced=false
func cleanupOrphanedLogicalSlots(ctx context.Context, db *sql.DB) error {
    slots, err := infrastructure.ListLogicalSlotsWithSyncStatus(ctx, db)
    if err != nil {
        return err
    }
    for _, slot := range slots {
        if !slot.Synced && !slot.Active {
            if err := infrastructure.DeleteLogicalSlot(ctx, db, slot.SlotName); err != nil {
                return err
            }
        }
    }
    return nil
}
```

## E2E Test Design

### File: `tests/e2e/logical_slot_switchover_test.go`

```go
var _ = Describe("Logical Slot Switchover",
    Label(tests.LabelPublicationSubscription, tests.LabelSelfHealing),
    func() {

    Context("with synchronizeLogicalDecoding enabled on PG17+", Ordered, func() {
        // Setup: Create cluster with PG17, synchronizeLogicalDecoding=true
        // Setup: Create publication & subscription
        // Setup: Verify logical replication is working

        It("cleans up orphaned logical slots after switchover", func() {
            // 1. Record initial slot state on primary
            // 2. Trigger switchover
            // 3. Wait for old primary (now replica) to rejoin
            // 4. Verify: No slots with synced=false on the demoted primary
            // 5. Verify: WAL accumulation is not growing unbounded
            // 6. Verify: Logical replication continues working
        })
    })
})
```

### Fixture: `tests/e2e/fixtures/logical_slot_switchover/cluster.yaml.template`

```yaml
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: logical-slot-switchover
spec:
  instances: 3
  imageName: ghcr.io/cloudnative-pg/postgresql:17
  replicationSlots:
    highAvailability:
      enabled: true
      synchronizeLogicalDecoding: true
  postgresql:
    parameters:
      hot_standby_feedback: "on"
      sync_replication_slots: "on"
```

### Key Assertions

1. **Before switchover:** Verify logical slots exist on primary
2. **After switchover:** Verify `synced=false` slots are cleaned on demoted primary
3. **Data continuity:** Verify logical replication resumes (data flows through pub/sub)
4. **WAL health:** Verify no unbounded WAL accumulation on replicas

## Design Decisions

### Why cleanup on reconciliation (not just Demote)?

Running during reconciliation covers all scenarios:
- Planned switchovers
- Pod restarts
- Crash recovery
- Any state where an instance becomes a replica

### Why check `!slot.Active` before deletion?

Active slots cannot be dropped. Skipping active slots prevents errors and allows retry on next reconciliation cycle.

### Why only PostgreSQL 17+?

The `synced` column in `pg_replication_slots` was introduced in PostgreSQL 17. Earlier versions use `pg_failover_slots` extension which has different semantics.

## Testing Scope

| Test Type | Coverage |
|-----------|----------|
| Unit tests | `infrastructure/postgresmanager_test.go`, `reconciler/replicationslot_test.go` |
| E2E test | `logical_slot_switchover_test.go` |

## Rollout Considerations

- **Backward compatible:** Only affects clusters with `synchronizeLogicalDecoding=true` AND PostgreSQL 17+
- **No API changes:** Uses existing configuration options
- **Safe to enable:** Worst case is a slot gets dropped and recreated by sync worker

## References

- [Issue #9969](https://github.com/cloudnative-pg/cloudnative-pg/issues/9969)
- [PostgreSQL 17 Slot Sync Documentation](https://www.postgresql.org/docs/17/logicaldecoding-explanation.html)
- [PostgreSQL pg_replication_slots View](https://www.postgresql.org/docs/17/view-pg-replication-slots.html)
