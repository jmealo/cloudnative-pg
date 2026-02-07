# PVC Auto-Resize: E2E Test Requirements

| Field | Value |
|-------|-------|
| **Companion RFC** | [docs/src/design/pvc-autoresize.md](pvc-autoresize.md) |
| **Original E2E Spec** | [docs/src/design/pvc-autoresize-e2e-testing.md](pvc-autoresize-e2e-testing.md) |
| **Test File** | `tests/e2e/auto_resize_test.go` |
| **Fixtures** | `tests/e2e/fixtures/auto_resize/` |
| **Last Audit** | 2026-02-07 |

---

## Purpose

This document captures the result of a gap analysis comparing the original E2E
testing spec, the actual test implementation, and the full feature set. It
serves as the authoritative requirements list for E2E coverage of the PVC
auto-resize feature.

Each requirement has a priority (P0 = must have, P1 = should have, P2 = nice to
have) and current status (COVERED, GAP, PARTIAL).

---

## Current Test Inventory

The following tests exist in `tests/e2e/auto_resize_test.go`:

| # | Context | What It Tests | Fixture |
|---|---------|---------------|---------|
| 1 | `basic auto-resize with single volume` | Fill data PVC past 80%, verify PVC grows | `cluster-autoresize-basic` |
| 2 | `auto-resize with separate WAL volume` | Fill WAL PVC past 80%, verify WAL PVC grows | `cluster-autoresize-wal-runtime` |
| 3 | `auto-resize respects expansion limit` | Fill disk, verify PVC grows to limit but not past it | `cluster-autoresize-limit` |
| 4 | `webhook validation` (reject) | Create single-volume cluster without `acknowledgeWALRisk`, verify rejection | Programmatic |
| 5 | `webhook validation` (accept) | Create single-volume cluster with `acknowledgeWALRisk`, verify acceptance | Programmatic |
| 6 | `rate-limit enforcement` | Trigger first resize, attempt second, verify blocked | `cluster-autoresize-ratelimit` |
| 7 | `minStep clamping` | 5% step on 2Gi (=102Mi) clamped up to 1Gi minStep | `cluster-autoresize-minstep` |
| 8 | `maxStep clamping via webhook` | Create cluster with `maxStep: 10Gi`, verify accepted | Programmatic |
| 9 | `metrics exposure` | Verify `cnpg_disk_{total,used,available}_bytes` and `cnpg_disk_percent_used` exist | `cluster-autoresize-basic` |
| 10 | `tablespace resize` | Fill tablespace PVC, verify it grows | `cluster-autoresize-tablespace` |
| 11 | `archive health blocks resize` | Configure bogus barmanObjectStore, generate WAL to fail archive, fill disk, verify PVC does NOT grow | `cluster-autoresize-archive-block` |
| 12 | `inactive slot blocks resize` | Create inactive physical replication slot, generate >100MB WAL, fill disk, verify PVC does NOT grow | `cluster-autoresize-slot-block` |

---

## Requirements

### REQ-01: Basic Data PVC Resize (P0) — COVERED

A cluster with `resize.enabled: true` on `storageConfiguration` must
automatically expand the data PVC when disk usage exceeds the configured
`usageThreshold`.

**Covered by:** Test #1 (basic auto-resize with single volume).

**Verification:** Fill data volume with `dd`, poll PVCs until
`spec.resources.requests.storage` exceeds original size.

---

### REQ-02: WAL PVC Resize (P0) — COVERED

A cluster with separate WAL storage and `resize.enabled: true` on `walStorage`
must automatically expand the WAL PVC when WAL volume usage exceeds threshold.

**Covered by:** Test #2 (auto-resize with separate WAL volume).

**Verification:** Fill WAL volume at `/var/lib/postgresql/wal/pg_wal/`, verify
WAL PVC grows.

---

### REQ-03: Expansion Limit (P0) — COVERED

PVC growth must never exceed `expansion.limit`. Once the PVC reaches the limit
the reconciler must stop expanding.

**Covered by:** Test #3 (auto-resize respects expansion limit).

**Verification:** Fill disk, verify PVC reaches 3Gi (limit) and does not
exceed it.

---

### REQ-04: Single-Volume WAL Risk Acknowledgment (P0) — COVERED

On a single-volume cluster (no separate `walStorage`), enabling auto-resize
without `acknowledgeWALRisk: true` must be rejected by the webhook.

**Covered by:** Test #4 (webhook rejection) and Test #5 (webhook acceptance).

**Verification:** Programmatic `client.Create()` — expect error without ack,
expect success with ack.

---

### REQ-05: Rate Limiting (P0) — COVERED

`strategy.maxActionsPerDay` must prevent more than the configured number of
resize operations per PVC within a 24-hour rolling window.

**Covered by:** Test #6 (rate-limit enforcement).

**Verification:** Configure `maxActionsPerDay: 1`, trigger first resize, verify
it succeeds, attempt second trigger, verify PVC size is unchanged.

---

### REQ-06: Archive Health Blocks Resize (P0) — COVERED

When `walSafetyPolicy.requireArchiveHealthy` is true (the default) and WAL
archiving is failing, resize must be blocked.

**Covered by:** Test #11 (archive health blocks resize).

**Verification:** Configure barmanObjectStore with non-existent endpoint,
generate WAL to trigger archive failures, fill disk, verify PVC does NOT grow.

---

### REQ-07: Inactive Slot Blocks Resize (P0) — COVERED

When `walSafetyPolicy.maxSlotRetentionBytes` is set and an inactive
replication slot retains more WAL than the threshold, resize must be blocked.

**Covered by:** Test #12 (inactive slot blocks resize).

**Verification:** Create physical slot with `immediately_reserve=true`,
generate >100MB WAL data, fill disk, verify PVC does NOT grow.

---

### REQ-08: MinStep Clamping (P0) — COVERED

When `expansion.step` is a percentage and the computed step is smaller than
`expansion.minStep`, the actual step must be clamped up to `minStep`.

**Covered by:** Test #7 (minStep clamping).

**Verification:** 5% of 2Gi = ~102Mi, but `minStep: 1Gi` means PVC must
grow to at least 3Gi.

---

### REQ-09: Tablespace PVC Resize (P0) — COVERED

Tablespace PVCs with `resize.enabled: true` must be resized when their volume
usage exceeds threshold.

**Covered by:** Test #10 (tablespace resize).

**Verification:** Fill tablespace volume, verify tablespace PVC grows.

---

### REQ-10: Disk Metrics Exposed (P0) — COVERED

Instance manager must expose `cnpg_disk_total_bytes`, `cnpg_disk_used_bytes`,
`cnpg_disk_available_bytes`, and `cnpg_disk_percent_used` Prometheus metrics.

**Covered by:** Test #9 (metrics exposure).

**Verification:** Scrape metrics endpoint, verify metric names present.

---

### REQ-11: MinAvailable Trigger (P0) — GAP

`triggers.minAvailable` is a distinct trigger mode that fires when available
bytes drop below an absolute threshold. This is an OR condition with
`usageThreshold` — either trigger alone is sufficient.

**Status:** NOT TESTED. No fixture or test exercises `minAvailable`.

**Required test:**

1. Create a cluster with `triggers.minAvailable: "300Mi"` (and either no
   `usageThreshold` or a very high one like 99) on a 2Gi volume.
2. Fill disk until fewer than 300Mi remain (~1.7Gi written).
3. Verify PVC is resized.

**Fixture needed:** `cluster-autoresize-minavailable.yaml.template`

---

### REQ-12: AutoResizeEvent Status Recording (P1) — GAP

After every resize, the reconciler appends an `AutoResizeEvent` to
`cluster.status.autoResizeEvents`. This provides audit trail and is consumed
by the kubectl plugin.

**Status:** NOT TESTED. No test verifies `cluster.Status.AutoResizeEvents`
is populated after a resize succeeds.

**Required change:** Add to Test #1 (basic resize) after the "waiting for PVC
to be resized" step:

```go
By("verifying an auto-resize event was recorded in cluster status", func() {
    Eventually(func(g Gomega) {
        cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
        g.Expect(err).ToNot(HaveOccurred())
        g.Expect(cluster.Status.AutoResizeEvents).ToNot(BeEmpty(),
            "at least one auto-resize event should be recorded")

        latest := cluster.Status.AutoResizeEvents[len(cluster.Status.AutoResizeEvents)-1]
        g.Expect(latest.Result).To(Equal("success"))
        g.Expect(latest.VolumeType).To(Equal("data"))
    }, 60*time.Second, 5*time.Second).Should(Succeed())
})
```

---

### REQ-13: resize_blocked Metric (P1) — GAP

When resize is blocked (by archive health, slot retention, rate limit, or
expansion limit), the `cnpg_disk_resize_blocked` metric must be set to 1 with
the appropriate `reason` label.

**Status:** NOT TESTED. The archive-block and slot-block tests verify PVC
didn't grow, but don't verify the metric.

**Required change:** Add to Test #11 (archive block) after the "verifying
resize is blocked" step:

```go
By("verifying resize_blocked metric is exposed", func() {
    cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
    Expect(err).ToNot(HaveOccurred())

    podName := clusterName + "-1"
    pod := &corev1.Pod{}
    err = env.Client.Get(env.Ctx, types.NamespacedName{
        Namespace: namespace, Name: podName,
    }, pod)
    Expect(err).ToNot(HaveOccurred())

    Eventually(func(g Gomega) {
        out, err := proxy.RetrieveMetricsFromInstance(env.Ctx, env.Interface, *pod,
            cluster.IsMetricsTLSEnabled())
        g.Expect(err).ToNot(HaveOccurred())
        g.Expect(out).To(ContainSubstring("cnpg_disk_resize_blocked"))
    }, 60*time.Second, 5*time.Second).Should(Succeed())
})
```

---

### REQ-14: MaxStep Runtime Clamping (P1) — GAP

When `expansion.step` is a percentage and the computed step exceeds
`expansion.maxStep`, the actual step must be capped to `maxStep`. Currently
only the webhook validation is tested (Test #8), not the runtime behavior.

**Status:** PARTIAL. Webhook accepts the config, but no test verifies the
runtime clamping effect.

**Required test:**

1. Create a cluster with `size: 10Gi`, `step: "50%"` (= 5Gi), `maxStep: "2Gi"`.
2. Fill disk past 80%.
3. Verify PVC grows by at most 2Gi (to 12Gi), not 5Gi (to 15Gi).

**Fixture needed:** `cluster-autoresize-maxstep.yaml.template`

---

### REQ-15: MaxPendingWALFiles Block (P1) — GAP

When `walSafetyPolicy.maxPendingWALFiles` is set and the count of pending WAL
files (`.ready` files in `pg_wal/archive_status/`) exceeds the threshold,
resize must be blocked.

**Status:** NOT EXPLICITLY TESTED. The archive-block test (Test #11) may
incidentally trigger this, but the test doesn't target it specifically and
doesn't verify the blocking reason.

**Required test:**

1. Create a cluster with backup configured to a non-existent endpoint AND
   `maxPendingWALFiles: 3`.
2. Generate enough WAL switches to create > 3 pending `.ready` files.
3. Fill disk, verify PVC does NOT grow.
4. Optionally verify `cnpg_disk_resize_blocked{reason="pending_wal"}`.

**Note:** This may be combined with the archive-block test by adding an
additional assertion, since the bogus backup endpoint already causes WAL
files to accumulate.

---

### REQ-16: Multi-Instance Resize (P1) — GAP

All current tests use single-instance clusters. In a multi-instance cluster,
the reconciler should resize PVCs for all instances that exceed the threshold,
not just the primary.

**Status:** NOT TESTED. The basic fixture has `instances: 2` but the test
only checks pod `-1`.

**Required change:** Modify Test #1 or create a new test:

1. Use a cluster with `instances: 2` (already in the basic fixture).
2. After resize triggers on the primary, verify that ALL instance PVCs have
   been resized (check `-1` and `-2`).

---

### REQ-17: MaxStep Webhook Validation (P1) — COVERED

The webhook must accept a cluster with a valid `maxStep` configuration.

**Covered by:** Test #8 (maxStep clamping via webhook).

---

### REQ-18: Metric Value Sanity (P2) — GAP

Disk metrics should have reasonable values — total bytes should reflect the
volume size, used + available should approximate total, and percent used
should be 0-100.

**Status:** NOT TESTED. Test #9 only checks metric names exist.

**Required change:** Add value assertions to Test #9:

```go
// Parse cnpg_disk_total_bytes value and verify > 1GiB (volume is 2Gi)
// Parse cnpg_disk_percent_used and verify 0 < value < 100
```

---

### REQ-19: Tablespace Metrics (P2) — GAP

Tablespace volumes should expose disk metrics with `volume_type="tablespace"`
and `tablespace="<name>"` labels.

**Status:** NOT TESTED. Test #10 tests resize but doesn't check metrics.

**Required change:** Add metric verification to Test #10 after resize succeeds.

---

### REQ-20: WAL Health Metrics (P2) — GAP

The following WAL health metrics should be exposed:
`cnpg_wal_archive_healthy`, `cnpg_wal_pending_archive_files`,
`cnpg_wal_inactive_slots`, `cnpg_wal_slot_retention_bytes`.

**Status:** NOT TESTED.

**Required change:** Add to Test #11 (archive block) or Test #12 (slot block):

```go
g.Expect(out).To(ContainSubstring("cnpg_wal_archive_healthy"))
g.Expect(out).To(ContainSubstring("cnpg_wal_pending_archive_files"))
```

---

### REQ-21: Inode Metrics (P2) — GAP

`cnpg_disk_inodes_total`, `cnpg_disk_inodes_used`, and `cnpg_disk_inodes_free`
should be exposed.

**Status:** NOT TESTED.

**Required change:** Add to Test #9 (metrics exposure).

---

### REQ-22: cnpg_disk_at_limit Metric (P2) — GAP

When a PVC reaches `expansion.limit`, `cnpg_disk_at_limit` should be 1.

**Status:** NOT TESTED.

**Required change:** Add to Test #3 (expansion limit) after PVC reaches limit.

---

### REQ-23: cnpg_disk_resizes_total Counter (P2) — GAP

`cnpg_disk_resizes_total{result="success"}` should increment after a
successful resize.

**Status:** NOT TESTED.

**Required change:** Add to Test #1 (basic resize) after resize succeeds.

---

### REQ-24: cnpg_disk_resize_budget_remaining Metric (P2) — GAP

`cnpg_disk_resize_budget_remaining` should reflect remaining rate-limit budget.

**Status:** NOT TESTED.

**Required change:** Add to Test #6 (rate-limit) — verify budget is 0 after
first resize.

---

### REQ-25: AlertOnResize Warning Event (P2) — GAP

When `walSafetyPolicy.alertOnResize` is true (default) and a WAL volume is
resized, a Kubernetes warning event should be emitted.

**Status:** NOT TESTED.

**Required change:** Add to Test #2 (WAL resize) — check for warning event
on the cluster object after resize.

---

### REQ-26: acknowledgeWALRisk Runtime Resize (P2) — GAP

Test #5 only verifies the cluster is accepted by the webhook. It does not
verify that a single-volume cluster with `acknowledgeWALRisk: true` actually
resizes at runtime.

**Status:** PARTIAL. Webhook acceptance tested, runtime resize not tested.

**Required change:** Extend Test #5 to fill disk and verify resize succeeds,
OR create a dedicated fixture and test.

---

## Structural Differences from Original Spec

| Aspect | Original Spec | Actual |
|--------|--------------|--------|
| Fixture directory | `pvc_autoresize/` | `auto_resize/` |
| Test file name | `pvc_autoresize_test.go` | `auto_resize_test.go` |
| API field | `autoResize` | `resize` (within StorageConfiguration) |
| Cooldown | `cooldownPeriod: 30s` field | Replaced by `maxActionsPerDay` (24h rolling window) |
| Threshold field | `threshold: 70` | `triggers.usageThreshold: 80` |
| Step field | `increase: "200Mi"` | `expansion.step: "20%"` or absolute quantity |
| Helper functions | Dedicated `Assert*` functions | Inline logic per test |

The original spec was written against an earlier API surface. The field names
and semantics evolved during implementation. This requirements document reflects
the CURRENT API.

---

## Priority Summary

### P0 — Must have before merge (1 gap)

- **REQ-11**: MinAvailable trigger — entirely untested trigger mode

### P1 — Should have before merge (5 gaps)

- **REQ-12**: AutoResizeEvent status recording
- **REQ-13**: resize_blocked metric verification
- **REQ-14**: MaxStep runtime clamping
- **REQ-15**: MaxPendingWALFiles explicit test
- **REQ-16**: Multi-instance resize verification

### P2 — Nice to have (8 gaps)

- REQ-18 through REQ-26: Metric value sanity, tablespace metrics, WAL health
  metrics, inode metrics, at_limit metric, resizes_total counter, budget
  remaining metric, alertOnResize event, acknowledgeWALRisk runtime test

### Already covered (10 requirements)

- REQ-01 through REQ-10, REQ-17
