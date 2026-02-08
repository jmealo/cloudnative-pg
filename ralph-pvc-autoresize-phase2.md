You are finishing the PVC Auto-Resize feature for CloudNativePG before PR
submission. Phases 1-3 (status persistence, rate limiting, metrics derivation)
are RESOLVED. Your job is to apply the remaining fixes, write the missing tests,
and **run the verification gate**.

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

This PR already changes 55 files with 11,500+ lines. Only implement items
listed below. Do NOT implement follow-up items:

- ~~`PatchWithOptimisticLock` for status updates~~ (follow-up)
- ~~Structured logging context~~ (follow-up)
- ~~Pointer fields for zero-value semantics~~ (follow-up)
- ~~Slot test stabilization~~ (follow-up)
- ~~VolumeType/ResizeResult enum types~~ (follow-up — API type change)
- ~~Reconciler integration tests with fake clients~~ (follow-up)
- ~~`Enabled` kubebuilder default marker~~ (follow-up — CRD semantic change)
- ~~Budget metrics per-PVC cardinality~~ (follow-up — metrics rework with `resize_blocked`)

---

## Code Fixes

### Fix 1: Kubebuilder default marker syntax (MUST FIX — breaks CRD generation)

In `api/v1/cluster_types.go`, four markers use `=` instead of `:=`:

| Line | Current | Correct |
|------|---------|---------|
| 2156 | `+kubebuilder:default=Standard` | `+kubebuilder:default:=Standard` |
| 2187 | `+kubebuilder:default=true` | `+kubebuilder:default:=true` |
| 2193 | `+kubebuilder:default=100` | `+kubebuilder:default:=100` |
| 2204 | `+kubebuilder:default=true` | `+kubebuilder:default:=true` |

Every other marker in the file (50+) uses `:=`. Fix these four to match.

### Fix 2: Package name shadowing (SHOULD FIX — lint/clarity)

`pkg/management/postgres/disk_status.go` and `pkg/management/postgres/probes.go`
are in `package postgres` and import `github.com/cloudnative-pg/cloudnative-pg/pkg/postgres`
without an alias. This shadows the package name.

Fix: alias the import as `postgresSpec` (or similar), matching the pattern in
`pkg/management/postgres/webserver/probes/pinger.go:28`.

### Fix 3: Prune stale DiskStatus entries (SHOULD FIX — correctness)

`internal/controller/cluster_status.go:794` (`updateDiskStatus`) only adds
entries to `cluster.Status.DiskStatus.Instances` — it never removes entries
for pods that no longer exist. After scale-down, stale entries persist.

Fix: Reinitialize the map on each update:
```go
cluster.Status.DiskStatus.Instances = make(map[string]*apiv1.InstanceDiskStatus, len(statuses.Items))
```

### Fix 4: Reject int step values in webhook (MUST FIX)

`step: 20` parses as 20 bytes, not 20%. In `validateExpansionStep()`:
```go
if stepVal.Type == intstr.Int && stepVal.IntVal > 0 {
    return field.ErrorList{field.Invalid(fldPath.Child("step"), stepVal,
        "integer step values are ambiguous; use a percentage string like '20%' or an absolute quantity like '5Gi'")}
}
```
Also reject `step: 0` (zero-value ambiguity). Add unit tests.

### Fix 5: WAL health fail-open warning event (MUST FIX)

**Keep fail-open** (disk full → database crash is worse than resizing without
WAL verification). Add a warning event when `walHealth == nil`:
```go
recorder.Eventf(cluster, corev1.EventTypeWarning, "AutoResizeWALHealthUnavailable",
    "Auto-resize permitted without WAL health verification for PVC %s", pvc.Name)
```

### Fix 6: Aggregate errors in PVC loop (SHOULD FIX)

In `reconciler.go`, collect per-PVC errors with `errors.Join`:
```go
var errs []error
for idx := range pvcs {
    pvc := &pvcs[idx]
    resized, err := reconcilePVC(ctx, c, recorder, cluster, diskInfoByPod, pvc)
    if err != nil {
        errs = append(errs, fmt.Errorf("PVC %s: %w", pvc.Name, err))
        continue
    }
    if resized { resizedAny = true }
}
if len(errs) > 0 {
    return ctrl.Result{RequeueAfter: RequeueDelay}, errors.Join(errs...)
}
```

### Fix 7: Emit event for "at expansion limit" (SHOULD FIX)

```go
recorder.Eventf(cluster, corev1.EventTypeWarning, "AutoResizeAtLimit",
    "Volume %s has reached expansion limit %s", pvc.Name, expansion.Limit)
```

### Fix 8: Wire AlertOnResize to event recorder (SHOULD FIX)

`AlertOnResize` (`*bool`, default `true`) exists but is never read. Check it:
```go
if walSafety != nil && (walSafety.AlertOnResize == nil || *walSafety.AlertOnResize) {
    recorder.Eventf(cluster, corev1.EventTypeNormal, "AutoResizeSuccess",
        "Expanded volume %s from %s to %s", pvc.Name, currentSize.String(), newSize.String())
}
```

### Fix 9: Named constant for event cap (STYLE)

```go
const maxAutoResizeEventHistory = 50
```

### Fix 10: Log warnings for parse failures (STYLE)

- `parseQuantityOrDefault`: log warning on invalid input before falling back
- `ShouldResize`: log warning on invalid `minAvailable` before falling back

### Fix 11: Use `%w` for error wrapping (STYLE)

Grep for `fmt.Errorf.*%v` in autoresize package, replace with `%w`.

### Fix 12: Standardize event reason naming (SHOULD FIX — consistency)

Event reasons in `reconciler.go` inconsistently mix two prefixes:

| Line | Current | Should Be |
|------|---------|-----------|
| 203 | `"ResizePVCWALHealthUnknown"` | `"AutoResizeWALHealthUnavailable"` |
| 231 | `"ResizePVC"` | `"AutoResizeSuccess"` |
| 250 | `"ResizePVCWALRisk"` | `"AutoResizeWALRisk"` |

Lines 171, 189, 206 already use `"AutoResizeBlocked"` — correct.

Standardize ALL event reasons to `AutoResize*` prefix. The E2E test at
~line 134 checks `"reason=AutoResizeSuccess"` so make sure to update both
the reconciler and any E2E assertions that reference the old names.

### Fix 13: Enabled field omitempty (STYLE — API consistency)

`api/v1/cluster_types.go:2093`: `Enabled bool \`json:"enabled"\`` lacks
`omitempty`. Every other boolean field in the API uses `json:"...,omitempty"`.

Fix: `Enabled bool \`json:"enabled,omitempty"\``

### Fix 14: WALSafetyResult.BlockReason comment (STYLE — accuracy)

`walsafety.go:59` says "empty if Allowed is true" but line 107 sets
`BlockReason: WALSafetyBlockHealthUnavailable` when `Allowed: true` (fail-open).

Fix comment to: `// BlockReason indicates the reason for blocking or warning.
// May be set even when Allowed is true (e.g., WALSafetyBlockHealthUnavailable).`

### Fix 15: Persist status before error return (MUST FIX — data loss bug)

`cluster_controller.go:825`: when `autoresize.Reconcile` returns a non-nil
error (some PVCs failed), the controller returns early and SKIPS the status
persistence block at line 829. But successful resizes already appended events
to the in-memory `cluster.Status.AutoResizeEvents` (reconciler.go:247+257).
Those events are lost, breaking rate-limiting on the next reconcile.

Fix: move the persistence block ABOVE the error check:
```go
autoResizeRes, err := autoresize.Reconcile(ctx, r.Client, r.Recorder, cluster, diskInfoByPod, resources.pvcs.Items)

// Persist status BEFORE checking error — successful resize events
// must not be lost when other PVCs fail.
if !reflect.DeepEqual(origCluster.Status, cluster.Status) {
    newStatus := cluster.Status
    if patchErr := status.PatchWithOptimisticLock(ctx, r.Client, cluster, func(c *apiv1.Cluster) {
        c.Status = newStatus
    }); patchErr != nil {
        contextLogger.Error(patchErr, "failed to persist auto-resize status changes")
    }
}

if err != nil {
    return autoResizeRes, err
}
```

Note: also return `autoResizeRes` (not `ctrl.Result{}`) so the reconciler's
`RequeueAfter` is preserved on error.

### Fix 16: Reset WAL metrics on collection failure (SHOULD FIX — stale data)

`disk.go:216-219`: when WAL health collection fails, the function returns
early, leaving WAL gauges (`WALArchiveHealthy`, `WALPendingFiles`,
`WALInactiveSlots`) with stale values from the previous scrape. Line 230
already shows the pattern of resetting before re-populating, but it's only
reached on success.

Fix: reset WAL gauges before the early return:
```go
if err != nil {
    contextLogger.Error(err, "failed to check WAL health")
    e.Metrics.DiskMetrics.WALArchiveHealthy.Set(0)
    e.Metrics.DiskMetrics.WALPendingFiles.Set(0)
    e.Metrics.DiskMetrics.WALInactiveSlots.Set(0)
    e.Metrics.DiskMetrics.WALSlotRetentionBytes.Reset()
    return
}
```

Setting `WALArchiveHealthy` to 0 on error is intentionally conservative:
the auto-resize fail-open logic should see "not healthy" rather than a
stale "healthy" from the previous scrape.

### Fix 17: Remove panics in clamping.go (SHOULD FIX — operator safety)

`clamping.go:163` and `clamping.go:176` — `parseQuantityOrDefault()` calls
`panic()` when a hardcoded default string fails to parse. Panics crash the
operator pod. These defaults are compile-time constants (`"2Gi"`, `"500Gi"`)
so the panic should never fire, but the convention is clear: no panics in
controller/instance manager code.

Fix both locations: replace `panic(...)` with log + zero-quantity fallback:
```go
if err != nil {
    autoresizeLog.Error(err, "BUG: invalid hardcoded default quantity", "default", defaultStr)
    zero := resource.MustParse("0")
    return &zero
}
```

The existing unit tests for `parseQuantityOrDefault` will catch any invalid
defaults at CI time, making the runtime panic unnecessary.

### Fix 18: Fix API group in ValidateUpdate (SHOULD FIX — wrong error metadata)

`internal/webhook/v1/cluster_webhook.go:158` uses `"cluster.cnpg.io"` but the
CRD group is `"postgresql.cnpg.io"`. `ValidateCreate` at line 122 already uses
the correct group. This is a pre-existing bug but we're touching this file.

Fix:
```go
return allWarnings, apierrors.NewInvalid(
    schema.GroupKind{Group: "postgresql.cnpg.io", Kind: "Cluster"},
    cluster.Name, allErrs)
```

---

## Unit Tests

### Fix 19: HasBudget unit tests (MUST HAVE)

`HasBudget()` is the rate limiter for the entire feature. It has ZERO tests.

Create `pkg/reconciler/autoresize/hasbudget_test.go` (Ginkgo v2 + Gomega):

**Required test cases:**
1. Empty events → has budget
2. Events within 24h for same PVC → budget exhausted
3. Events within 24h for same PVC → budget remaining
4. Events older than 24h are ignored
5. Events for different PVC don't count
6. Mixed old and new events
7. `maxActions=0` → always false
8. `maxActions` negative → always false
9. Boundary: event exactly at 24h cutoff

### Fix 20: getAutoResizeWarnings unit tests (MUST HAVE)

`getAutoResizeWarnings()` ships in this PR but has ZERO tests.

Create `internal/webhook/v1/cluster_webhook_autoresize_warnings_test.go`:

**Required test cases:**
1. `maxActionsPerDay: 0` warns
2. `minAvailable > size` warns
3. `limit <= size` warns
4. `minStep`/`maxStep` with absolute step warns
5. `acknowledgeWALRisk` on dual-volume warns
6. `requireArchiveHealthy` without backup warns
7. Valid config → no warnings
8. Disabled resize → no warnings

### Fix 21: appendResizeEvent unit tests (SHOULD HAVE)

`appendResizeEvent()` in `reconciler.go:326` manages event history pruning
(25h cutoff + 50-event cap). It has ZERO tests.

Add to `hasbudget_test.go` or a new file:

**Required test cases:**
1. Append to empty events list → event is added
2. Events older than 25h are pruned on append
3. Events with zero timestamps are filtered out
4. History capped at `maxAutoResizeEventHistory` (50) — keeps most recent
5. Mix of old and new events — only recent retained + new appended

---

## E2E Test Fixes

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

### E2E 2: REQ-12 AutoResizeEvent verification

Add to Test #1 (basic resize) after PVC grows:
```go
By("verifying an auto-resize event was recorded in cluster status", func() {
    Eventually(func(g Gomega) {
        cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
        g.Expect(err).ToNot(HaveOccurred())
        g.Expect(cluster.Status.AutoResizeEvents).ToNot(BeEmpty())
        latest := cluster.Status.AutoResizeEvents[len(cluster.Status.AutoResizeEvents)-1]
        g.Expect(latest.Result).To(Equal("success"))
        g.Expect(latest.VolumeType).To(Equal("data"))
    }, 60*time.Second, 5*time.Second).Should(Succeed())
})
```

### E2E 3: REQ-11 MinAvailable trigger

New fixture `cluster-autoresize-minavailable.yaml.template`:
- `triggers.minAvailable: "300Mi"`, `usageThreshold: 99`, `size: 2Gi`

New test: fill disk until <300Mi remain, verify PVC grows.

### E2E 4: Expansion limit second-fill verification

The fixture has `size: 2Gi`, `step: "1Gi"`, `limit: "2.5Gi"`. The first
resize should clamp to exactly 2.5Gi (not 3Gi). After that, verify a second
fill does NOT cause further growth:

```go
By("cleaning up fill file before second fill attempt", func() {
    // rm -f fill_file
})

By("filling disk again to verify limit blocks further resize", func() {
    // Write ~2Gi to the now-2.5Gi volume to exceed 80% again
    // dd if=/dev/zero of=fill_file bs=1M count=2000 || true
})

By("verifying PVC stays at limit and does not grow", func() {
    Consistently(func(g Gomega) {
        // Check all data PVCs for this cluster stay at 2.5Gi
        limitSize := resource.MustParse("2.5Gi")
        g.Expect(currentSize.Cmp(limitSize)).To(BeNumerically("<=", 0))
    }, 2*time.Minute, 10*time.Second).Should(Succeed())
})
```

### E2E 5: Replace time.Sleep with Eventually/Consistently

Any `time.Sleep(2 * time.Minute)` → `Consistently(...)` pattern.

### E2E 6: Rate-limit test time.Sleep at line 748 (SHOULD FIX)

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

### E2E 7: Slot test stays PIt()

Do NOT attempt to fix the slot detection issue. Keep `PIt()`.

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
