# Logical Slot Orphan Cleanup Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Fix WAL accumulation caused by orphaned logical slots after switchover when `synchronizeLogicalDecoding=true` on PostgreSQL 17+.

**Architecture:** Add cleanup of `synced=false` logical slots during slot reconciliation on replicas. The cleanup runs when `synchronizeLogicalDecoding=true` and PostgreSQL 17+, dropping orphaned logical slots so PostgreSQL's native sync worker can recreate them properly.

**Tech Stack:** Go, PostgreSQL 17, Ginkgo/Gomega, sqlmock

---

## Task 1: Add LogicalReplicationSlot Struct

**Files:**
- Modify: `internal/management/controller/slots/infrastructure/replicationslot.go:36`

**Step 1: Write the struct**

Add after the existing `ReplicationSlotList` struct (around line 56):

```go
// LogicalReplicationSlot represents a logical replication slot
type LogicalReplicationSlot struct {
	SlotName   string `json:"slotName,omitempty"`
	Plugin     string `json:"plugin,omitempty"`
	Active     bool   `json:"active"`
	RestartLSN string `json:"restartLSN,omitempty"`
	Synced     bool   `json:"synced"` // PG17+: false if locally created, true if synced from primary
}
```

**Step 2: Run linter to verify**

Run: `go vet ./internal/management/controller/slots/infrastructure/...`
Expected: No errors

**Step 3: Commit**

```bash
git add internal/management/controller/slots/infrastructure/replicationslot.go
git commit -m "refactor(slots): add LogicalReplicationSlot struct for PG17+ synced status

Adds a new struct to represent logical replication slots with the
synced column introduced in PostgreSQL 17.

Ref: #9969

Co-Authored-By: Claude Opus 4.5 <noreply@anthropic.com>"
```

---

## Task 2: Add ListLogicalSlotsWithSyncStatus Function

**Files:**
- Modify: `internal/management/controller/slots/infrastructure/postgresmanager.go`
- Test: `internal/management/controller/slots/infrastructure/postgresmanager_test.go`

**Step 1: Write the failing test**

Add to `postgresmanager_test.go` after the existing `Delete` Context:

```go
Context("ListLogicalSlotsWithSyncStatus", func() {
	const expectedSQL = "^SELECT (.+) FROM pg_catalog.pg_replication_slots WHERE slot_type = 'logical'"

	It("should successfully list logical replication slots with synced status", func(ctx SpecContext) {
		rows := sqlmock.NewRows([]string{"slot_name", "plugin", "active", "restart_lsn", "synced"}).
			AddRow("sub_slot1", "pgoutput", true, "0/1234", true).
			AddRow("sub_slot2", "pgoutput", false, "0/5678", false)

		mock.ExpectQuery(expectedSQL).
			WillReturnRows(rows)

		result, err := ListLogicalSlotsWithSyncStatus(ctx, db)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(HaveLen(2))

		Expect(result[0].SlotName).To(Equal("sub_slot1"))
		Expect(result[0].Plugin).To(Equal("pgoutput"))
		Expect(result[0].Active).To(BeTrue())
		Expect(result[0].RestartLSN).To(Equal("0/1234"))
		Expect(result[0].Synced).To(BeTrue())

		Expect(result[1].SlotName).To(Equal("sub_slot2"))
		Expect(result[1].Synced).To(BeFalse())
	})

	It("should return error when database query fails", func(ctx SpecContext) {
		mock.ExpectQuery(expectedSQL).
			WillReturnError(errors.New("mock error"))

		_, err := ListLogicalSlotsWithSyncStatus(ctx, db)
		Expect(err).To(HaveOccurred())
	})

	It("should return empty slice when no logical slots exist", func(ctx SpecContext) {
		rows := sqlmock.NewRows([]string{"slot_name", "plugin", "active", "restart_lsn", "synced"})

		mock.ExpectQuery(expectedSQL).
			WillReturnRows(rows)

		result, err := ListLogicalSlotsWithSyncStatus(ctx, db)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(BeEmpty())
	})
})
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/management/controller/slots/infrastructure/... -v -run "ListLogicalSlotsWithSyncStatus"`
Expected: FAIL - "undefined: ListLogicalSlotsWithSyncStatus"

**Step 3: Write minimal implementation**

Add to `postgresmanager.go` after the existing `Delete` function:

```go
// ListLogicalSlotsWithSyncStatus lists logical replication slots with their synced status.
// The synced column is only available in PostgreSQL 17+.
// Slots with synced=false were created locally; slots with synced=true were synchronized from the primary.
func ListLogicalSlotsWithSyncStatus(ctx context.Context, db *sql.DB) ([]LogicalReplicationSlot, error) {
	contextLog := log.FromContext(ctx).WithName("listLogicalSlotsWithSyncStatus")

	rows, err := db.QueryContext(
		ctx,
		`SELECT slot_name, plugin, active, coalesce(restart_lsn::TEXT, '') AS restart_lsn, synced
		FROM pg_catalog.pg_replication_slots
		WHERE slot_type = 'logical'`,
	)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = rows.Close()
	}()

	var slots []LogicalReplicationSlot
	for rows.Next() {
		var slot LogicalReplicationSlot
		err := rows.Scan(
			&slot.SlotName,
			&slot.Plugin,
			&slot.Active,
			&slot.RestartLSN,
			&slot.Synced,
		)
		if err != nil {
			return nil, err
		}
		slots = append(slots, slot)
	}

	if rows.Err() != nil {
		return nil, rows.Err()
	}

	contextLog.Trace("Listed logical slots with sync status", "count", len(slots))
	return slots, nil
}
```

**Step 4: Run test to verify it passes**

Run: `go test ./internal/management/controller/slots/infrastructure/... -v -run "ListLogicalSlotsWithSyncStatus"`
Expected: PASS

**Step 5: Commit**

```bash
git add internal/management/controller/slots/infrastructure/postgresmanager.go \
        internal/management/controller/slots/infrastructure/postgresmanager_test.go
git commit -m "feat(slots): add ListLogicalSlotsWithSyncStatus for PG17+ logical slots

Adds function to query logical replication slots with their synced
status. The synced column (PG17+) indicates whether a slot was created
locally (synced=false) or synchronized from the primary (synced=true).

Ref: #9969

Co-Authored-By: Claude Opus 4.5 <noreply@anthropic.com>"
```

---

## Task 3: Add DeleteLogicalSlot Function

**Files:**
- Modify: `internal/management/controller/slots/infrastructure/postgresmanager.go`
- Test: `internal/management/controller/slots/infrastructure/postgresmanager_test.go`

**Step 1: Write the failing test**

Add to `postgresmanager_test.go` after the `ListLogicalSlotsWithSyncStatus` Context:

```go
Context("DeleteLogicalSlot", func() {
	const expectedSQL = "SELECT pg_catalog.pg_drop_replication_slot"

	It("should successfully delete a logical replication slot", func(ctx SpecContext) {
		mock.ExpectExec(expectedSQL).WithArgs("sub_slot1").
			WillReturnResult(sqlmock.NewResult(1, 1))

		err := DeleteLogicalSlot(ctx, db, "sub_slot1")
		Expect(err).NotTo(HaveOccurred())
	})

	It("should return error when the database execution fails", func(ctx SpecContext) {
		mock.ExpectExec(expectedSQL).WithArgs("sub_slot1").
			WillReturnError(errors.New("mock error"))

		err := DeleteLogicalSlot(ctx, db, "sub_slot1")
		Expect(err).To(HaveOccurred())
	})
})
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/management/controller/slots/infrastructure/... -v -run "DeleteLogicalSlot"`
Expected: FAIL - "undefined: DeleteLogicalSlot"

**Step 3: Write minimal implementation**

Add to `postgresmanager.go` after `ListLogicalSlotsWithSyncStatus`:

```go
// DeleteLogicalSlot drops a logical replication slot by name.
// Note: Active slots cannot be dropped - this will return an error from PostgreSQL.
func DeleteLogicalSlot(ctx context.Context, db *sql.DB, slotName string) error {
	contextLog := log.FromContext(ctx).WithName("deleteLogicalSlot")
	contextLog.Info("Dropping logical replication slot", "slotName", slotName)

	_, err := db.ExecContext(ctx, "SELECT pg_catalog.pg_drop_replication_slot($1)", slotName)
	return err
}
```

**Step 4: Run test to verify it passes**

Run: `go test ./internal/management/controller/slots/infrastructure/... -v -run "DeleteLogicalSlot"`
Expected: PASS

**Step 5: Commit**

```bash
git add internal/management/controller/slots/infrastructure/postgresmanager.go \
        internal/management/controller/slots/infrastructure/postgresmanager_test.go
git commit -m "feat(slots): add DeleteLogicalSlot for dropping logical slots

Adds function to drop a logical replication slot by name. Used by
the orphan cleanup logic to remove synced=false slots on replicas.

Ref: #9969

Co-Authored-By: Claude Opus 4.5 <noreply@anthropic.com>"
```

---

## Task 4: Add cleanupOrphanedLogicalSlots Function

**Files:**
- Modify: `internal/management/controller/slots/reconciler/replicationslot.go`
- Test: `internal/management/controller/slots/reconciler/replicationslot_test.go`

**Step 1: Write the failing test**

Add to `replicationslot_test.go`:

```go
var _ = Describe("cleanupOrphanedLogicalSlots", func() {
	var (
		mock sqlmock.Sqlmock
		db   *sql.DB
	)

	BeforeEach(func() {
		var err error
		db, mock, err = sqlmock.New()
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		Expect(mock.ExpectationsWereMet()).To(Succeed())
	})

	It("should drop logical slots with synced=false that are not active", func(ctx SpecContext) {
		rows := sqlmock.NewRows([]string{"slot_name", "plugin", "active", "restart_lsn", "synced"}).
			AddRow("synced_slot", "pgoutput", false, "0/1234", true).     // synced=true, skip
			AddRow("orphan_slot", "pgoutput", false, "0/5678", false).    // synced=false, drop
			AddRow("active_orphan", "pgoutput", true, "0/9ABC", false)    // active, skip

		mock.ExpectQuery("SELECT .+ FROM pg_catalog.pg_replication_slots WHERE slot_type = 'logical'").
			WillReturnRows(rows)

		// Only orphan_slot should be dropped
		mock.ExpectExec("SELECT pg_catalog.pg_drop_replication_slot").
			WithArgs("orphan_slot").
			WillReturnResult(sqlmock.NewResult(1, 1))

		err := cleanupOrphanedLogicalSlots(ctx, db)
		Expect(err).NotTo(HaveOccurred())
	})

	It("should do nothing when no orphaned slots exist", func(ctx SpecContext) {
		rows := sqlmock.NewRows([]string{"slot_name", "plugin", "active", "restart_lsn", "synced"}).
			AddRow("synced_slot", "pgoutput", false, "0/1234", true)

		mock.ExpectQuery("SELECT .+ FROM pg_catalog.pg_replication_slots WHERE slot_type = 'logical'").
			WillReturnRows(rows)

		err := cleanupOrphanedLogicalSlots(ctx, db)
		Expect(err).NotTo(HaveOccurred())
	})

	It("should return error when listing slots fails", func(ctx SpecContext) {
		mock.ExpectQuery("SELECT .+ FROM pg_catalog.pg_replication_slots WHERE slot_type = 'logical'").
			WillReturnError(errors.New("mock error"))

		err := cleanupOrphanedLogicalSlots(ctx, db)
		Expect(err).To(HaveOccurred())
	})
})
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/management/controller/slots/reconciler/... -v -run "cleanupOrphanedLogicalSlots"`
Expected: FAIL - "undefined: cleanupOrphanedLogicalSlots"

**Step 3: Write minimal implementation**

Add to `replicationslot.go`:

```go
// cleanupOrphanedLogicalSlots removes logical replication slots with synced=false.
// On PostgreSQL 17+, slots with synced=false were created locally and cannot be
// updated by the slot sync worker. After a switchover, these orphaned slots must
// be dropped so the sync worker can recreate them with synced=true.
func cleanupOrphanedLogicalSlots(ctx context.Context, db *sql.DB) error {
	contextLog := log.FromContext(ctx).WithName("cleanupOrphanedLogicalSlots")

	slots, err := infrastructure.ListLogicalSlotsWithSyncStatus(ctx, db)
	if err != nil {
		return fmt.Errorf("listing logical slots: %w", err)
	}

	for _, slot := range slots {
		// Only drop slots that are:
		// 1. synced=false (locally created, orphaned after switchover)
		// 2. Not active (active slots cannot be dropped)
		if !slot.Synced && !slot.Active {
			contextLog.Info("Dropping orphaned logical slot",
				"slotName", slot.SlotName,
				"synced", slot.Synced,
				"active", slot.Active)

			if err := infrastructure.DeleteLogicalSlot(ctx, db, slot.SlotName); err != nil {
				return fmt.Errorf("deleting orphaned logical slot %q: %w", slot.SlotName, err)
			}
		}
	}

	return nil
}
```

**Step 4: Run test to verify it passes**

Run: `go test ./internal/management/controller/slots/reconciler/... -v -run "cleanupOrphanedLogicalSlots"`
Expected: PASS

**Step 5: Commit**

```bash
git add internal/management/controller/slots/reconciler/replicationslot.go \
        internal/management/controller/slots/reconciler/replicationslot_test.go
git commit -m "feat(slots): add cleanupOrphanedLogicalSlots for PG17+ replicas

Adds function to clean up logical replication slots with synced=false
on replicas. After switchover, these orphaned slots prevent PostgreSQL's
slot sync worker from recreating them properly.

Ref: #9969

Co-Authored-By: Claude Opus 4.5 <noreply@anthropic.com>"
```

---

## Task 5: Add isSynchronizeLogicalDecodingEnabled Helper

**Files:**
- Modify: `internal/management/controller/slots/reconciler/replicationslot.go`

**Step 1: Add the helper function**

Add to `replicationslot.go`:

```go
// isSynchronizeLogicalDecodingEnabled checks if logical slot synchronization is enabled
func isSynchronizeLogicalDecodingEnabled(cluster *apiv1.Cluster) bool {
	if cluster.Spec.ReplicationSlots == nil {
		return false
	}
	if cluster.Spec.ReplicationSlots.HighAvailability == nil {
		return false
	}
	if !cluster.Spec.ReplicationSlots.HighAvailability.GetEnabled() {
		return false
	}
	return cluster.Spec.ReplicationSlots.HighAvailability.SynchronizeLogicalDecoding
}
```

**Step 2: Run linter to verify**

Run: `go vet ./internal/management/controller/slots/reconciler/...`
Expected: No errors

**Step 3: Commit**

```bash
git add internal/management/controller/slots/reconciler/replicationslot.go
git commit -m "refactor(slots): add isSynchronizeLogicalDecodingEnabled helper

Extracts check for synchronizeLogicalDecoding configuration into a
reusable helper function.

Ref: #9969

Co-Authored-By: Claude Opus 4.5 <noreply@anthropic.com>"
```

---

## Task 6: Integrate Cleanup into ReconcileReplicationSlots

**Files:**
- Modify: `internal/management/controller/slots/reconciler/replicationslot.go`
- Test: `internal/management/controller/slots/reconciler/replicationslot_test.go`

**Step 1: Modify ReconcileReplicationSlots**

Update the `ReconcileReplicationSlots` function to add the cleanup call after the existing checks:

```go
func ReconcileReplicationSlots(
	ctx context.Context,
	instanceName string,
	db *sql.DB,
	cluster *apiv1.Cluster,
) (reconcile.Result, error) {
	if cluster.Spec.ReplicationSlots == nil ||
		cluster.Spec.ReplicationSlots.HighAvailability == nil {
		return reconcile.Result{}, nil
	}

	isPrimary := cluster.Status.CurrentPrimary == instanceName || cluster.Status.TargetPrimary == instanceName

	// NEW: Clean up orphaned logical slots on replicas when synchronizeLogicalDecoding is enabled
	if !isPrimary && isSynchronizeLogicalDecodingEnabled(cluster) {
		pgMajor, err := cluster.GetPostgresqlMajorVersion()
		if err == nil && pgMajor >= 17 {
			if err := cleanupOrphanedLogicalSlots(ctx, db); err != nil {
				return reconcile.Result{}, fmt.Errorf("cleaning up orphaned logical slots: %w", err)
			}
		}
	}

	// ... rest of existing code unchanged
```

**Step 2: Run all reconciler tests**

Run: `go test ./internal/management/controller/slots/reconciler/... -v`
Expected: All tests PASS

**Step 3: Commit**

```bash
git add internal/management/controller/slots/reconciler/replicationslot.go
git commit -m "feat(slots): integrate logical slot cleanup into reconciliation

Adds cleanup of orphaned logical slots (synced=false) on replicas when
synchronizeLogicalDecoding is enabled on PostgreSQL 17+. This fixes
WAL accumulation after switchover.

Fixes: #9969

Co-Authored-By: Claude Opus 4.5 <noreply@anthropic.com>"
```

---

## Task 7: Create E2E Test Fixture

**Files:**
- Create: `tests/e2e/fixtures/logical_slot_switchover/cluster.yaml.template`
- Create: `tests/e2e/fixtures/logical_slot_switchover/source-cluster.yaml.template`
- Create: `tests/e2e/fixtures/logical_slot_switchover/destination-cluster.yaml.template`
- Create: `tests/e2e/fixtures/logical_slot_switchover/source-database.yaml`
- Create: `tests/e2e/fixtures/logical_slot_switchover/destination-database.yaml`
- Create: `tests/e2e/fixtures/logical_slot_switchover/pub.yaml`
- Create: `tests/e2e/fixtures/logical_slot_switchover/sub.yaml`

**Step 1: Create fixture directory**

```bash
mkdir -p tests/e2e/fixtures/logical_slot_switchover
```

**Step 2: Create source-cluster.yaml.template**

```yaml
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: logical-slot-source
spec:
  instances: 3
  imageName: "${POSTGRES_IMG}"
  replicationSlots:
    highAvailability:
      enabled: true
      synchronizeLogicalDecoding: true
  postgresql:
    parameters:
      hot_standby_feedback: "on"
      sync_replication_slots: "on"
      wal_level: logical
  storage:
    size: 1Gi
    storageClass: ${E2E_DEFAULT_STORAGE_CLASS}
```

**Step 3: Create destination-cluster.yaml.template**

```yaml
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: logical-slot-dest
spec:
  instances: 1
  imageName: "${POSTGRES_IMG}"
  postgresql:
    parameters:
      wal_level: logical
  storage:
    size: 1Gi
    storageClass: ${E2E_DEFAULT_STORAGE_CLASS}
```

**Step 4: Create source-database.yaml**

```yaml
apiVersion: postgresql.cnpg.io/v1
kind: Database
metadata:
  name: source-db
spec:
  name: testdb
  owner: app
  cluster:
    name: logical-slot-source
```

**Step 5: Create destination-database.yaml**

```yaml
apiVersion: postgresql.cnpg.io/v1
kind: Database
metadata:
  name: dest-db
spec:
  name: testdb
  owner: app
  cluster:
    name: logical-slot-dest
```

**Step 6: Create pub.yaml**

```yaml
apiVersion: postgresql.cnpg.io/v1
kind: Publication
metadata:
  name: test-pub
spec:
  name: test_pub
  dbname: testdb
  target:
    allTables: true
  cluster:
    name: logical-slot-source
```

**Step 7: Create sub.yaml**

```yaml
apiVersion: postgresql.cnpg.io/v1
kind: Subscription
metadata:
  name: test-sub
spec:
  name: test_sub
  dbname: testdb
  publicationName: test_pub
  externalClusterName: logical-slot-source
  cluster:
    name: logical-slot-dest
```

**Step 8: Commit**

```bash
git add tests/e2e/fixtures/logical_slot_switchover/
git commit -m "test(e2e): add fixtures for logical slot switchover test

Adds YAML fixtures for testing logical slot cleanup after switchover
with synchronizeLogicalDecoding enabled.

Ref: #9969

Co-Authored-By: Claude Opus 4.5 <noreply@anthropic.com>"
```

---

## Task 8: Create E2E Test File

**Files:**
- Create: `tests/e2e/logical_slot_switchover_test.go`

**Step 1: Create the test file**

```go
/*
Copyright The CloudNativePG Contributors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

SPDX-License-Identifier: Apache-2.0
*/

package e2e

import (
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/types"

	apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	"github.com/cloudnative-pg/cloudnative-pg/tests"
	"github.com/cloudnative-pg/cloudnative-pg/tests/utils/clusterutils"
	"github.com/cloudnative-pg/cloudnative-pg/tests/utils/exec"
	"github.com/cloudnative-pg/cloudnative-pg/tests/utils/objects"
	"github.com/cloudnative-pg/cloudnative-pg/tests/utils/postgres"
	"github.com/cloudnative-pg/cloudnative-pg/tests/utils/yaml"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Tests for logical slot cleanup after switchover when synchronizeLogicalDecoding is enabled
var _ = Describe("Logical Slot Switchover", Label(tests.LabelPublicationSubscription, tests.LabelSelfHealing), func() {
	const (
		sourceClusterManifest      = fixturesDir + "/logical_slot_switchover/source-cluster.yaml.template"
		destinationClusterManifest = fixturesDir + "/logical_slot_switchover/destination-cluster.yaml.template"
		sourceDatabaseManifest     = fixturesDir + "/logical_slot_switchover/source-database.yaml"
		destinationDatabaseManifest = fixturesDir + "/logical_slot_switchover/destination-database.yaml"
		pubManifest                = fixturesDir + "/logical_slot_switchover/pub.yaml"
		subManifest                = fixturesDir + "/logical_slot_switchover/sub.yaml"
		level                      = tests.High
	)

	BeforeEach(func() {
		if testLevelEnv.Depth < int(level) {
			Skip("Test depth is lower than the amount requested for this test")
		}
	})

	Context("with synchronizeLogicalDecoding enabled on PG17+", Ordered, func() {
		const (
			namespacePrefix = "logical-slot-switchover"
			dbname          = "testdb"
			tableName       = "test_data"
		)
		var (
			sourceClusterName, destinationClusterName, namespace string
			err                                                  error
		)

		BeforeAll(func() {
			namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix)
			Expect(err).ToNot(HaveOccurred())

			sourceClusterName, err = yaml.GetResourceNameFromYAML(env.Scheme, sourceClusterManifest)
			Expect(err).ToNot(HaveOccurred())

			destinationClusterName, err = yaml.GetResourceNameFromYAML(env.Scheme, destinationClusterManifest)
			Expect(err).ToNot(HaveOccurred())

			By("setting up source cluster with synchronizeLogicalDecoding", func() {
				AssertCreateCluster(namespace, sourceClusterName, sourceClusterManifest, env)
			})

			By("setting up destination cluster", func() {
				AssertCreateCluster(namespace, destinationClusterName, destinationClusterManifest, env)
			})

			By("creating databases", func() {
				CreateResourceFromFile(namespace, sourceDatabaseManifest)
				CreateResourceFromFile(namespace, destinationDatabaseManifest)

				// Wait for databases to be ready
				Eventually(func(g Gomega) {
					db := &apiv1.Database{}
					err := env.Client.Get(env.Ctx, types.NamespacedName{Namespace: namespace, Name: "source-db"}, db)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(db.Status.Applied).Should(HaveValue(BeTrue()))
				}, 300).WithPolling(10 * time.Second).Should(Succeed())
			})

			By("creating test table and data on source", func() {
				query := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (id SERIAL PRIMARY KEY, data TEXT)", tableName)
				_, err = postgres.RunExecOverForward(
					env.Ctx, env.Client, env.Interface, env.RestClientConfig,
					namespace, sourceClusterName, dbname,
					apiv1.ApplicationUserSecretSuffix, query,
				)
				Expect(err).ToNot(HaveOccurred())

				// Insert test data
				_, err = postgres.RunExecOverForward(
					env.Ctx, env.Client, env.Interface, env.RestClientConfig,
					namespace, sourceClusterName, dbname,
					apiv1.ApplicationUserSecretSuffix, "INSERT INTO test_data (data) VALUES ('before_switchover')",
				)
				Expect(err).ToNot(HaveOccurred())
			})

			By("creating table on destination", func() {
				query := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (id SERIAL PRIMARY KEY, data TEXT)", tableName)
				_, err = postgres.RunExecOverForward(
					env.Ctx, env.Client, env.Interface, env.RestClientConfig,
					namespace, destinationClusterName, dbname,
					apiv1.ApplicationUserSecretSuffix, query,
				)
				Expect(err).ToNot(HaveOccurred())
			})

			By("setting up publication and subscription", func() {
				CreateResourceFromFile(namespace, pubManifest)
				CreateResourceFromFile(namespace, subManifest)

				// Wait for pub/sub to be ready
				Eventually(func(g Gomega) {
					pub := &apiv1.Publication{}
					err := env.Client.Get(env.Ctx, types.NamespacedName{Namespace: namespace, Name: "test-pub"}, pub)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(pub.Status.Applied).Should(HaveValue(BeTrue()))
				}, 300).WithPolling(10 * time.Second).Should(Succeed())
			})
		})

		It("cleans up orphaned logical slots after switchover", func() {
			var oldPrimary string

			By("recording initial primary", func() {
				cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, sourceClusterName)
				Expect(err).ToNot(HaveOccurred())
				oldPrimary = cluster.Status.CurrentPrimary
			})

			By("verifying logical slots exist on primary", func() {
				primaryPod, err := clusterutils.GetPrimary(env.Ctx, env.Client, namespace, sourceClusterName)
				Expect(err).ToNot(HaveOccurred())

				query := "SELECT count(*) FROM pg_replication_slots WHERE slot_type = 'logical'"
				Eventually(func(g Gomega) {
					out, _, err := exec.QueryInInstancePod(
						env.Ctx, env.Client, env.Interface, env.RestClientConfig,
						exec.PodLocator{Namespace: primaryPod.Namespace, PodName: primaryPod.Name},
						dbname, query,
					)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(strings.TrimSpace(out)).ToNot(Equal("0"))
				}, 60).Should(Succeed())
			})

			By("triggering switchover", func() {
				AssertSwitchover(namespace, sourceClusterName, env)
			})

			By("verifying no synced=false slots on demoted primary", func() {
				// The old primary is now a replica
				query := "SELECT count(*) FROM pg_replication_slots WHERE slot_type = 'logical' AND synced = false"

				Eventually(func(g Gomega) {
					out, _, err := exec.QueryInInstancePod(
						env.Ctx, env.Client, env.Interface, env.RestClientConfig,
						exec.PodLocator{Namespace: namespace, PodName: oldPrimary},
						postgres.PostgresDBName, query,
					)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(strings.TrimSpace(out)).To(Equal("0"), "Expected no synced=false logical slots on demoted primary")
				}, 120).WithPolling(5 * time.Second).Should(Succeed())
			})

			By("verifying logical replication still works", func() {
				// Insert new data after switchover
				_, err = postgres.RunExecOverForward(
					env.Ctx, env.Client, env.Interface, env.RestClientConfig,
					namespace, sourceClusterName, dbname,
					apiv1.ApplicationUserSecretSuffix, "INSERT INTO test_data (data) VALUES ('after_switchover')",
				)
				Expect(err).ToNot(HaveOccurred())

				// Verify it appears on destination
				Eventually(func(g Gomega) {
					row, err := postgres.RunQueryRowOverForward(
						env.Ctx, env.Client, env.Interface, env.RestClientConfig,
						namespace, destinationClusterName, dbname,
						apiv1.ApplicationUserSecretSuffix,
						"SELECT count(*) FROM test_data WHERE data = 'after_switchover'",
					)
					g.Expect(err).ToNot(HaveOccurred())

					var count int
					err = row.Scan(&count)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(count).To(BeNumerically(">", 0))
				}, 120).WithPolling(5 * time.Second).Should(Succeed())
			})
		})
	})
})
```

**Step 2: Run linter**

Run: `go vet ./tests/e2e/...`
Expected: No errors

**Step 3: Commit**

```bash
git add tests/e2e/logical_slot_switchover_test.go
git commit -m "test(e2e): add logical slot switchover test

Adds E2E test verifying that orphaned logical slots (synced=false) are
cleaned up on demoted primaries after switchover when
synchronizeLogicalDecoding is enabled.

Ref: #9969

Co-Authored-By: Claude Opus 4.5 <noreply@anthropic.com>"
```

---

## Task 9: Run Full Test Suite

**Step 1: Run unit tests**

Run: `go test ./internal/management/controller/slots/... -v`
Expected: All PASS

**Step 2: Run linter**

Run: `make lint`
Expected: No errors

**Step 3: Build**

Run: `go build ./...`
Expected: Success

**Step 4: Commit any fixes if needed**

If any issues found, fix and commit.

---

## Task 10: Final Verification

**Step 1: Review all commits**

Run: `git log --oneline origin/main..HEAD`

**Step 2: Ensure all tests pass**

Run: `go test ./internal/management/controller/slots/... -v`

**Step 3: Ready for PR**

The implementation is complete. Create PR when ready.

---

## Summary of Changes

| File | Change Type | Description |
|------|-------------|-------------|
| `internal/management/controller/slots/infrastructure/replicationslot.go` | Modify | Add `LogicalReplicationSlot` struct |
| `internal/management/controller/slots/infrastructure/postgresmanager.go` | Modify | Add `ListLogicalSlotsWithSyncStatus` and `DeleteLogicalSlot` |
| `internal/management/controller/slots/infrastructure/postgresmanager_test.go` | Modify | Add unit tests for new functions |
| `internal/management/controller/slots/reconciler/replicationslot.go` | Modify | Add `cleanupOrphanedLogicalSlots`, `isSynchronizeLogicalDecodingEnabled`, integrate into reconcile |
| `internal/management/controller/slots/reconciler/replicationslot_test.go` | Modify | Add unit tests for cleanup function |
| `tests/e2e/fixtures/logical_slot_switchover/*` | Create | E2E test fixtures |
| `tests/e2e/logical_slot_switchover_test.go` | Create | E2E test |
