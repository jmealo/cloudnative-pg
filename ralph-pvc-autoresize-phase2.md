You are finishing the PVC Auto-Resize feature for CloudNativePG before PR
submission. Most fixes are DONE. You have **4 remaining items** and then
the **verification gate**.

## MANDATORY VERIFICATION GATE

You are NOT done until you have actually RUN the tests and they PASS:

```bash
make generate && make manifests && make fmt && make lint && make test
```

If any command fails, FIX THE ISSUE and re-run. After unit tests pass:

```bash
hack/e2e/run-e2e-aks-autoresize.sh
```

**YOU MUST PASTE THE ACTUAL COMMAND OUTPUT.** If you have not pasted `make test`
output showing PASS, you are not done.

---

## Project Context

- **Repo:** cloudnative-pg/cloudnative-pg (Fork by jmealo)
- **Branch:** feat/pvc-autoresizing-wal-safety
- **Stack:** Go 1.25, Controller Runtime, Kubebuilder, Ginkgo v2 + Gomega
- **Linting:** `.golangci.yml` — 40+ linters, 120-char line limit
- **Logger:** `log.FromContext(ctx)` from `github.com/cloudnative-pg/machinery/pkg/log`
- **Imports:** stdlib → external → `github.com/cloudnative-pg/cloudnative-pg` → blank → dot
- **Import aliases:** `apiv1`, `corev1`, `metav1` (enforced by `.golangci.yml`)
- **Image base:** ghcr.io/jmealo/cloudnative-pg-testing
- **Commits:** DCO sign-off required (`git commit -s`)

### Three-Binary Architecture

1. **Controller Manager** (Linux): reconcilers, webhooks, autoresize decisions
2. **Instance Manager** (Linux): disk probe, WAL health, metrics — runs inside
   every PostgreSQL pod
3. **kubectl-cnpg Plugin** (cross-platform): CLI commands

`probe.go` uses `syscall.Statfs_t` (Linux-only). The plugin never imports it.
The `probe_linux.go` / `probe_other.go` build tag split is correct — do NOT
merge them.

---

## SCOPE RULE

This PR already changes 55 files with 11,500+ lines. Only implement the 4
items listed below. Do NOT implement follow-up items:

- ~~`PatchWithOptimisticLock` for status updates~~ (follow-up)
- ~~Structured logging context~~ (follow-up)
- ~~Pointer fields for zero-value semantics~~ (follow-up)
- ~~Slot test stabilization~~ (follow-up)
- ~~VolumeType/ResizeResult enum types~~ (follow-up — API type change)
- ~~Reconciler integration tests with fake clients~~ (follow-up)
- ~~`Enabled` kubebuilder default marker~~ (follow-up — CRD semantic change)
- ~~Budget metrics per-PVC cardinality~~ (follow-up — metrics rework with `resize_blocked`)

---

## Already Implemented (DO NOT REDO)

The following 26 items are already in the codebase and passing. Do not modify
or revert them. They are listed here for context only.

| # | Fix | Status |
|---|-----|--------|
| 1 | Kubebuilder default marker `:=` syntax | ✅ Done |
| 2 | Package name shadowing → `postgresSpec` alias | ✅ Done |
| 3 | Prune stale DiskStatus entries (map reinit) | ✅ Done |
| 4 | Reject int step values in webhook | ✅ Done |
| 5 | WAL health fail-open warning event | ✅ Done |
| 6 | Aggregate errors in PVC loop (`errors.Join`) | ✅ Done |
| 7 | Emit event for "at expansion limit" | ✅ Done |
| 8 | Wire AlertOnResize to event recorder | ✅ Done |
| 9 | Named constant `maxAutoResizeEventHistory` | ✅ Done |
| 10 | Log warnings for parse failures | ✅ Done |
| 11 | Use `%w` for error wrapping | ✅ Done |
| 12 | Standardize event reasons to `AutoResize*` | ✅ Done |
| 13 | Enabled field `omitempty` | ✅ Done |
| 14 | WALSafetyResult.BlockReason comment | ✅ Done |
| 15 | Persist status before error return | ✅ Done |
| 16 | Reset WAL metrics on collection failure | ✅ Done |
| 17 | Remove panics in clamping.go | ✅ Done |
| 18 | Fix API group in ValidateUpdate | ✅ Done |
| 21 | HasBudget unit tests | ✅ Done |
| 22 | getAutoResizeWarnings unit tests | ✅ Done |
| 23 | appendResizeEvent unit tests | ✅ Done |
| E2E 2 | AutoResizeEvent verification (REQ-12) | ✅ Done |
| E2E 3 | MinAvailable trigger test (REQ-11) | ✅ Done |
| E2E 4 | Expansion limit second-fill verification | ✅ Done |
| E2E 5 | Replace time.Sleep(2min) with Consistently | ✅ Done |
| E2E 7 | Slot test stays PIt() | ✅ Done |

---

## Remaining Fixes (4 items)

### Fix 1: Use local pod name in decision metrics (SHOULD FIX — wrong labels)

`pkg/management/postgres/webserver/metricserver/disk.go` — the
`deriveDecisionMetrics` function (around line 289) currently uses
`cluster.Status.CurrentPrimary` to derive PVC names for budget/resize metrics.
This means every pod (including replicas) emits metrics labeled with the
PRIMARY's pod/PVC name, hiding standby disk activity.

The `Exporter` struct has `instance *postgres.Instance` (pg_collector.go:51)
and `instance.GetPodName()` returns the local pod name.

**Fix:** add `localPodName string` parameter to `deriveDecisionMetrics`:
```go
func (dm *DiskMetrics) deriveDecisionMetrics(cluster *apiv1.Cluster, probe *disk.VolumeProbeResult, localPodName string) {
```

Remove the `CurrentPrimary` lookup and `cluster.Name + "-1"` fallback.
Use `localPodName` to derive PVC names and as the `instance` metric label.

Update call sites in `collectDiskUsageMetrics` to pass `e.instance.GetPodName()`:
```go
e.Metrics.DiskMetrics.deriveDecisionMetrics(cluster, dataResult, e.instance.GetPodName())
```

### Fix 2: Reset disk metrics on probe failure (SHOULD FIX — stale data)

`disk.go:249-283`: when a volume probe fails, the function logs and continues
but the gauge values from the previous scrape persist. This is the same issue
already fixed for WAL metrics (WALArchiveHealthy/WALPendingFiles/WALInactiveSlots
are reset to 0 on error — see ~line 218). The disk probe has three error paths
(PGDATA, WAL, tablespace) that all need gauge resets.

Add a helper and call it on probe error:
```go
func (dm *DiskMetrics) resetVolumeStats(volType string, ts string) {
    dm.TotalBytes.WithLabelValues(volType, ts).Set(0)
    dm.UsedBytes.WithLabelValues(volType, ts).Set(0)
    dm.AvailableBytes.WithLabelValues(volType, ts).Set(0)
    dm.PercentUsed.WithLabelValues(volType, ts).Set(0)
    dm.InodesTotal.WithLabelValues(volType, ts).Set(0)
    dm.InodesUsed.WithLabelValues(volType, ts).Set(0)
    dm.InodesFree.WithLabelValues(volType, ts).Set(0)
}
```

Call in each error branch, e.g.:
```go
if err != nil {
    contextLogger.Error(err, "failed to probe PGDATA volume")
    e.Metrics.DiskMetrics.resetVolumeStats(string(disk.VolumeTypeData), "")
}
```

### E2E 1: Verify `cnpg_disk_resize_blocked` assertions match implementation

`cnpg_disk_resize_blocked` is now populated in `deriveDecisionMetrics`
(disk.go:360-374) with reasons `"rate_limit"` and `"expansion_limit"`.

Verify the E2E assertions match the actual label values:
- **Rate-limit test** (~line 762): should assert `reason="rate_limit"` ✓
- **Archive-block test** (~line 1079): currently asserts `reason="wal_health_unavailable"`
  but the metric only populates `"rate_limit"` and `"expansion_limit"` — NOT
  `"wal_health_unavailable"`. Either add a `"wal_health_unavailable"` reason to
  `deriveDecisionMetrics` or change the E2E assertion to verify blocking via PVC
  size unchanged instead.

### E2E 2: Replace time.Sleep(30s) at line 748 with Consistently

`auto_resize_test.go:748` still has `time.Sleep(30 * time.Second)`. Replace
with `Consistently` to verify PVC size stays unchanged:
```go
Consistently(func(g Gomega) {
    pvcList, err := storage.GetPVCList(env.Ctx, env.Client, namespace)
    g.Expect(err).ToNot(HaveOccurred())
    for idx := range pvcList.Items {
        pvc := &pvcList.Items[idx]
        if pvc.Labels[utils.ClusterLabelName] == clusterName &&
            pvc.Labels[utils.PvcRoleLabelName] == string(utils.PVCRolePgData) {
            g.Expect(pvc.Spec.Resources.Requests.Storage().Cmp(sizeAfterFirstResize)).To(Equal(0),
                "PVC should not grow past rate limit")
        }
    }
}, 30*time.Second, 5*time.Second).Should(Succeed())
```

---

## Branch Cleanup

- Do NOT commit: `.claude/`, `*.pdf`, `ralph-*.md`, `*SUMMARY.md`, `review-*.md`
- Do NOT merge `probe_linux.go`/`probe_other.go` — the build tag split is correct

---

## Completion Criteria

You may ONLY claim completion after ALL of the following are true AND you have
pasted the actual terminal output proving it:

- [ ] `make generate` exited 0 (paste output)
- [ ] `make manifests` exited 0 (paste output)
- [ ] `make fmt` exited 0 (paste output)
- [ ] `make lint` exited 0 (paste output)
- [ ] `make test` ALL tests passed (paste output showing PASS)
- [ ] `hack/e2e/run-e2e-aks-autoresize.sh` E2E tests passed (paste output)
- [ ] All commits have DCO sign-off (`git commit -s`)

### E2E Runner Quick Reference

```bash
# Full run (build + deploy + test):
hack/e2e/run-e2e-aks-autoresize.sh

# Skip build/deploy when iterating on test code:
hack/e2e/run-e2e-aks-autoresize.sh --skip-build --skip-deploy

# Re-run a single failing test:
hack/e2e/run-e2e-aks-autoresize.sh --focus "archive health" --skip-build --skip-deploy
```

Test names for `--focus`: `basic auto-resize`, `separate WAL volume`,
`expansion limit`, `webhook`, `rate-limit`, `minStep`, `maxStep`,
`metrics`, `tablespace`, `archive health`, `inactive slot`, `minAvailable`.

When ALL criteria are met, output: <promise>COMPLETE</promise>
