You are finishing the PVC Auto-Resize feature for CloudNativePG before PR
submission. Phases 1-3 (status persistence, rate limiting, metrics) are RESOLVED.

**CRITICAL SCOPE RULE:** This PR already changes 55 files with 11,500+ insertions.
Every additional change makes it harder to review. Only implement items marked
"SHIP IN THIS PR" below. Items marked "FOLLOW-UP PR" should NOT be implemented —
they will be separate, focused PRs after the core feature merges.

**MANDATORY VERIFICATION GATE:** You are NOT done until you have actually RUN the
tests and they PASS. Saying "the code looks correct" is NOT the same as running
the tests. You MUST execute these commands and see them succeed before claiming
completion:

```bash
make generate && make manifests   # CRD + deepcopy regeneration
make fmt                          # gofmt
make lint                         # golangci-lint
make test                         # unit tests
```

If any command fails, FIX THE ISSUE and re-run. Do not skip. Do not say "this
should work." Do not say "I believe this will pass." RUN IT.

After unit tests pass, run E2E tests on AKS:
```bash
hack/e2e/run-e2e-aks-autoresize.sh
```

**YOU MUST PASTE THE ACTUAL COMMAND OUTPUT** showing success. If you have not
pasted `make test` output showing PASS, you are not done.

Ref: docs/src/design/pvc-autoresize.md (see "Pre-Merge Implementation Issues"
     and "Configuration Conflicts & Validation Gaps")
E2E Requirements: docs/src/design/pvc-autoresize-e2e-requirements.md

## Project Context
- Repo: cloudnative-pg/cloudnative-pg (Fork by jmealo)
- Stack: Go, Controller Runtime, Kubebuilder, Ginkgo v2
- Constraints: strict linting, DCO sign-off required
- Branch: feat/pvc-autoresizing-wal-safety
- Image base: ghcr.io/jmealo/cloudnative-pg-testing
- AKS E2E script: `hack/e2e/run-e2e-aks-autoresize.sh`

## Three-Binary Architecture

CNPG builds THREE binaries:
1. **Controller Manager** (Linux): reconcilers, webhooks, autoresize decisions
2. **Instance Manager** (Linux): disk probe, WAL health, metrics — runs inside
   every PostgreSQL pod, copied by the `bootstrap-controller` init container
3. **kubectl-cnpg Plugin** (cross-platform): CLI commands, does NOT import
   `pkg/management/postgres/disk/` or `pkg/management/postgres/wal/`

`probe.go` uses `syscall.Statfs_t` (Linux-only). This is fine — the plugin
never imports it. **Do NOT add platform build tags.**

WAL archiving is controlled via `archive_command` (a fixed parameter). To make
archiving fail in E2E tests, use a bogus `barmanObjectStore` endpoint.

---

## Phases 1-3: RESOLVED

Phase 1 (Status Persistence), Phase 2 (Persistent Rate Limiting), and
Phase 3 (Resize Metrics) have been implemented:

- `cluster_controller.go`: `origCluster.DeepCopy()` → `Reconcile()` →
  `Status().Patch()` before returning requeue result
- `GlobalBudgetTracker` removed. Stateless `HasBudget()` derives budget
  from `cluster.Status.AutoResizeEvents`. `PVCName` field added to events.
- `ratelimit.go` and `ratelimit_test.go` deleted
- `deriveDecisionMetrics()` in `disk.go` populates `ResizesTotal`,
  `ResizeBudgetRemain`, and `AtLimit` from cluster status
- Reconciler now accepts `record.EventRecorder` and emits K8s events
  for rate limit blocks and WAL safety blocks

**Remaining metrics gap:** `cnpg_disk_resize_blocked` is still not populated.
See follow-up items below.

---

## SHIP IN THIS PR: Code Fixes

### Fix 1: Reject int step values in webhook (Must Fix)

If `step` is provided as an int (e.g., `step: 20`), `resource.ParseQuantity("20")`
returns 20 bytes, not 20%. This is almost never the user's intent.

In `internal/webhook/v1/cluster_webhook.go`, `validateExpansionStep()`:
```go
if stepVal.Type == intstr.Int && stepVal.IntVal > 0 {
    return field.ErrorList{field.Invalid(fldPath.Child("step"), stepVal,
        "integer step values are ambiguous; use a percentage string like '20%' or an absolute quantity like '5Gi'")}
}
```

Also reject `step: 0`:
```go
if stepVal.Type == intstr.Int && stepVal.IntVal == 0 {
    return field.ErrorList{field.Invalid(fldPath.Child("step"), stepVal,
        "step must be a positive quantity or percentage, not 0")}
}
```

Add unit tests to `cluster_webhook_autoresize_conflicts_test.go`.

### Fix 2: WAL health fail-open warning event (Must Fix)

**IMPORTANT: Keep fail-open.** The primary threat is disk full → database crash.
If WAL health data is unavailable, blocking the resize is MORE dangerous than
allowing it. The RFC design is correct. We just need a warning event.

In `walsafety.go`:
```go
if walHealth == nil {
    return WALSafetyResult{
        Allowed: true,
        Reason:  "wal_health_unavailable",
    }
}
```

In `reconcilePVC()`:
```go
if walSafetyResult.Reason == "wal_health_unavailable" {
    recorder.Eventf(cluster, corev1.EventTypeWarning, "AutoResizeWALHealthUnavailable",
        "Auto-resize permitted without WAL health verification for PVC %s", pvc.Name)
}
```

### Fix 3: Aggregate errors in PVC loop (Should Fix)

In `reconciler.go`, partial PVC failures are silently swallowed:
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

### Fix 4: Emit event for "at expansion limit" (Should Fix)

Rate limit and WAL safety blocks emit events, but expansion limit does not:
```go
if currentSize.Cmp(limit) >= 0 {
    contextLogger.Info("auto-resize blocked: at expansion limit", "pvc", pvc.Name)
    recorder.Eventf(cluster, corev1.EventTypeWarning, "AutoResizeAtLimit",
        "Volume %s has reached expansion limit %s", pvc.Name, expansion.Limit)
    return false, nil
}
```

### Fix 5: Named constant for event history cap (Style)

```go
const maxAutoResizeEventHistory = 50
```

### Fix 6: Log warning for parseQuantityOrDefault (Style)

```go
func parseQuantityOrDefault(qtyStr string, defaultStr string) *resource.Quantity {
    if qtyStr == "" {
        qty, _ := resource.ParseQuantity(defaultStr)
        return &qty
    }
    qty, err := resource.ParseQuantity(qtyStr)
    if err != nil {
        log.Warning("invalid quantity in auto-resize config, using default",
            "provided", qtyStr, "default", defaultStr, "error", err)
        fallback, _ := resource.ParseQuantity(defaultStr)
        return &fallback
    }
    return &qty
}
```

### Fix 7: Log warning for invalid minAvailable in ShouldResize (Style)

```go
minAvailableQty, err := resource.ParseQuantity(triggers.MinAvailable)
if err != nil {
    contextLogger.Warn("invalid minAvailable, using percentage trigger only",
        "minAvailable", triggers.MinAvailable, "error", err)
    return usedPercent > float64(usageThreshold)
}
```

### Fix 8: Wire AlertOnResize to event recorder (Should Fix)

`AlertOnResize` (`*bool`, default `true`) exists in `WALSafetyPolicy` but is
never read. In `reconcilePVC()`, check it before emitting resize events:

```go
if walSafety != nil && (walSafety.AlertOnResize == nil || *walSafety.AlertOnResize) {
    recorder.Eventf(cluster, corev1.EventTypeNormal, "AutoResizeSuccess",
        "Expanded volume %s from %s to %s", pvc.Name, currentSize.String(), newSize.String())
}
```

### Fix 9: Use %w for error wrapping (Style)

Grep for `fmt.Errorf.*%v` in the autoresize package and replace with `%w`.

### Fix 10: HasBudget unit tests (Must Have — coverage gap)

`HasBudget()` in `reconciler.go` is the rate limiter for the entire feature.
It replaced the old `GlobalBudgetTracker` (deleted `ratelimit.go` +
`ratelimit_test.go`) but the replacement has ZERO unit tests.

Create `pkg/reconciler/autoresize/hasbudget_test.go` following the same
Ginkgo v2 pattern as `walsafety_test.go`:

```go
package autoresize

import (
    "time"

    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

    apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"

    . "github.com/onsi/ginkgo/v2"
    . "github.com/onsi/gomega"
)

var _ = Describe("HasBudget", func() {
    // Test cases needed:
})
```

**Required test cases:**

1. **Empty events list → has budget**: Cluster with no `AutoResizeEvents`,
   `maxActions=3` → returns `true`.

2. **Events within 24h for same PVC → budget exhausted**: 3 events for
   PVC "cluster-1" all with `Timestamp` within last hour, `maxActions=3`
   → returns `false`.

3. **Events within 24h for same PVC → budget remaining**: 2 events for
   PVC "cluster-1" within last hour, `maxActions=3` → returns `true`.

4. **Events older than 24h are ignored**: 3 events for PVC "cluster-1"
   all with `Timestamp` 25 hours ago, `maxActions=3` → returns `true`.

5. **Events for DIFFERENT PVC don't count**: 3 events for PVC "cluster-2"
   within last hour, checking budget for PVC "cluster-1", `maxActions=3`
   → returns `true`.

6. **Mixed old and new events**: 2 events 25h ago + 2 events 1h ago for
   same PVC, `maxActions=3` → returns `true` (only 2 within window).

7. **maxActions=0 → always false**: Any events, `maxActions=0` → `false`.

8. **maxActions negative → always false**: `maxActions=-1` → `false`.

9. **Boundary: event exactly at 24h cutoff**: Event with `Timestamp`
   exactly `time.Now().Add(-24 * time.Hour)` — verify consistent behavior.

Build events with:
```go
apiv1.AutoResizeEvent{
    Timestamp:    metav1.NewTime(time.Now().Add(-1 * time.Hour)),
    PVCName:      "cluster-1",
    InstanceName: "cluster-1",
    VolumeType:   "data",
    Result:       "success",
}
```

### Fix 11: getAutoResizeWarnings unit tests (Must Have — untested shipping code)

`getAutoResizeWarnings()` and `getAutoResizeWarningsForStorage()` in
`cluster_webhook.go` are already implemented and wired into the admission
pipeline (line 2544). This code runs on every cluster create/update.
It has ZERO test coverage. Shipping untested webhook code is not acceptable.

Add tests to `cluster_webhook_autoresize_test.go` (or a new file
`cluster_webhook_autoresize_warnings_test.go`) following the existing
pattern:

```go
package v1

import (
    "k8s.io/apimachinery/pkg/util/intstr"
    "k8s.io/utils/ptr"

    apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"

    . "github.com/onsi/ginkgo/v2"
    . "github.com/onsi/gomega"
)
```

**Required test cases for each warning condition:**

1. **`maxActionsPerDay: 0` warns**: Cluster with `resize.enabled: true`,
   `strategy.maxActionsPerDay: 0` → warnings include substring
   `"effectively disables auto-resize"`.

2. **`minAvailable > size` warns**: Cluster with `size: "1Gi"`,
   `triggers.minAvailable: "5Gi"` → warnings include substring
   `"resize will trigger immediately"`.

3. **`limit <= size` warns**: Cluster with `size: "10Gi"`,
   `expansion.limit: "5Gi"` → warnings include substring
   `"auto-resize will never increase"`.

4. **`minStep`/`maxStep` with absolute step warns**: Cluster with
   `step: "10Gi"`, `minStep: "2Gi"` → warnings include substring
   `"apply only to percentage-based steps"`.

5. **`acknowledgeWALRisk` on dual-volume warns**: Cluster with
   `walStorage` configured AND `acknowledgeWALRisk: true` on data →
   warnings include substring `"has no effect"`.

6. **`requireArchiveHealthy` without backup warns**: Cluster with
   `requireArchiveHealthy: true` and NO `barmanObjectStore` → warnings
   include substring `"no backup configuration"`.

7. **No warnings for valid config**: Cluster with sensible configuration
   → `getAutoResizeWarnings()` returns empty.

8. **Disabled resize produces no warnings**: Cluster with
   `resize.enabled: false` → no warnings.

Use the `makeCluster` / `makeMultiVolumeCluster` helpers from
`cluster_webhook_autoresize_conflicts_test.go` if they work, or build
minimal cluster objects inline.

### Verification

1. `make test` passes (including new HasBudget and warning tests)
2. `make lint` passes
3. `make vet` passes

---

## SHIP IN THIS PR: E2E Test Fixes

### E2E 1: REQ-12 AutoResizeEvent Verification (Critical)

After fixing status persistence (Phase 1), add to Test #1:
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

### E2E 2: REQ-11 MinAvailable Trigger (P0)

New fixture `cluster-autoresize-minavailable.yaml.template`:
- `triggers.minAvailable: "300Mi"` (no usageThreshold or set to 99)
- `size: 2Gi`

New test: fill disk until <300Mi remain, verify PVC grows.

### E2E 3: Replace time.Sleep with Eventually/Consistently (P1)

Several tests use `time.Sleep(2 * time.Minute)`. Replace with:
```go
Consistently(func(g Gomega) {
    // verify PVC size has NOT changed
}, 2*time.Minute, 10*time.Second).Should(Succeed())
```

### E2E 4: Expansion Limit Test — verify limit BLOCKS second resize (Should Fix)

The current expansion limit test (Test #3) only proves one resize happened
and landed at the limit. It does NOT verify that the limit blocks further
growth. The test starts at 2Gi with `step: 1Gi` and `limit: 3Gi` — one
resize lands exactly at 3Gi by construction. This proves "resize happened"
but not "limit works."

**Fix:** After the first resize reaches 3Gi, clean up the fill file, then
fill again to exceed 80% of the now-3Gi volume. Wait with `Consistently`
to verify the PVC stays at 3Gi and does NOT grow to 4Gi. This proves the
limit is actually enforced, not just that the math happened to land there.

Add after the "verifying PVC does not exceed the limit" step:

```go
By("cleaning up fill file before second fill attempt", func() {
    // ... rm -f fill_file
})

By("filling disk again to verify limit blocks further resize", func() {
    // Write ~2.5Gi to the now-3Gi volume to exceed 80% again
    commandTimeout := time.Second * 120
    _, _, err = env.EventuallyExecCommand(
        env.Ctx, *pod, specs.PostgresContainerName, &commandTimeout,
        "sh", "-c",
        "dd if=/dev/zero of=/var/lib/postgresql/data/pgdata/fill_file bs=1M count=2500",
    )
    Expect(err).ToNot(HaveOccurred())
})

By("verifying PVC stays at limit and does not grow", func() {
    Consistently(func(g Gomega) {
        pvcList, err := storage.GetPVCList(env.Ctx, env.Client, namespace)
        g.Expect(err).ToNot(HaveOccurred())
        for idx := range pvcList.Items {
            pvc := &pvcList.Items[idx]
            if pvc.Labels[utils.ClusterLabelName] != clusterName {
                continue
            }
            if pvc.Labels[utils.PvcRoleLabelName] != string(utils.PVCRolePgData) {
                continue
            }
            currentSize := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
            limitSize := resource.MustParse("3Gi")
            g.Expect(currentSize.Cmp(limitSize)).To(BeNumerically("<=", 0),
                "PVC should remain at limit, not grow further")
        }
    }, 2*time.Minute, 10*time.Second).Should(Succeed())
})
```

### E2E 5: Slot Retention Test — Keep as PIt() (Pending)

The slot detection issue has multiple root cause candidates (isPrimary gating,
error swallowing, missing WAL health serialization). Do NOT attempt to fix this
in the current PR. Keep as `PIt()` with a clear comment explaining the issue.
Stabilization is a follow-up.

---

## SHIP IN THIS PR: Branch Cleanup

### Cleanup 1: Revert build tag split

If `probe_linux.go` and `probe_other.go` exist, merge back to `probe.go`.

### Cleanup 2: Remove dev artifacts

Do NOT commit: `.claude/`, `*.pdf`, `ralph-*.md`, `*SUMMARY.md`, etc.

### Cleanup 3: Final verification

```bash
make generate && make manifests && make fmt && make lint && make test
```

---

## FOLLOW-UP PR #1: Status Patch & Metrics Refactoring

Items:
- Switch status persistence from raw `r.Status().Patch()` to CNPG's
  `status.PatchWithOptimisticLock()` (in `pkg/resources/status/patch.go`).
  This provides conflict retry via `retry.RetryOnConflict`. Requires
  restructuring autoresize status updates to use `Transaction` closures.
- Move `cnpg_disk_resize_blocked` to operator metrics endpoint (or populate
  from status)
- Consider moving all resize-action metrics to operator endpoint
- Add `NextActionAt` or `BudgetResetAt` status field for observability
- Structured logging context (`log.WithValues("cluster", ...)`) for the
  autoresize reconciler

## FOLLOW-UP PR #2: Design Improvements (was #3)

Items:
- Pointer fields for `UsageThreshold` and `MaxActionsPerDay` (API change)
- `resizeInUseVolumes` flag gating (needs design discussion)
- `requireArchiveHealthy` behavior when no backup configured
- `maxActionsPerDay: 0` semantics (reject vs "unlimited")
- Event history cap sizing (ensure 50 is sufficient for budget window)
- Cross-volume rate limit documentation
- OR-trigger semantics documentation
- Slot retention test stabilization (after WAL health investigation)

---

## Commit Convention

DCO sign-off required:
```
git commit -s -m "$(cat <<'COMMITEOF'
fix(autoresize): description here

Co-Authored-By: Claude Opus 4.5 <noreply@anthropic.com>
COMMITEOF
)"
```

## Completion Criteria

**STOP. READ THIS CAREFULLY.**

You may ONLY output `<promise>COMPLETE</promise>` after ALL of the following
are true AND you have pasted the actual terminal output proving it:

### Build & Test — MUST RUN, NOT JUST READ CODE
- [ ] You ran `make generate` and it exited 0 (paste output)
- [ ] You ran `make manifests` and it exited 0 (paste output)
- [ ] You ran `make fmt` and it exited 0 (paste output)
- [ ] You ran `make lint` and it exited 0 (paste output)
- [ ] You ran `make test` and ALL tests passed (paste output showing PASS)
- [ ] You ran `hack/e2e/run-e2e-aks-autoresize.sh` and E2E tests passed
      (slot test stays as PIt; use `--focus` to rerun failing tests)
- [ ] All commits have DCO sign-off

If you say COMPLETE without having run `make test` and pasted the output,
you are lying. Do not do this.

### How to run E2E tests on AKS

The E2E runner script handles build, deploy, and test execution:

```bash
# Full run (build image, deploy operator, run tests):
hack/e2e/run-e2e-aks-autoresize.sh

# Skip build/deploy when iterating on test code:
hack/e2e/run-e2e-aks-autoresize.sh --skip-build --skip-deploy

# Re-run a single failing test by name:
hack/e2e/run-e2e-aks-autoresize.sh --focus "archive health" --skip-build --skip-deploy

# Diagnose a stuck cluster without running tests:
hack/e2e/run-e2e-aks-autoresize.sh --diagnose-only
```

Test names for `--focus`: `basic auto-resize`, `separate WAL volume`,
`expansion limit`, `webhook`, `rate-limit`, `minStep`, `maxStep`,
`metrics`, `tablespace`, `archive health`, `inactive slot`, `minAvailable`.

If a test fails, fix the code, re-run with `--focus "failing test name"`
until it passes, then run the full suite without `--focus` to confirm
no regressions.

### Critical Bugs (RESOLVED)
- ~~Status persistence~~ ✅
- ~~Durable rate limiting~~ ✅
- ~~Metrics population~~ ✅

### Code Fixes (this PR)
- Int step values rejected in webhook
- WAL fail-open emits warning event (STAYS fail-open)
- Error aggregation in PVC loop
- Event for "at expansion limit"
- AlertOnResize wired to recorder
- Named constant for event cap
- parseQuantityOrDefault logs on invalid input
- ShouldResize logs on invalid minAvailable
- %w error wrapping

### Unit Tests (this PR)
- HasBudget unit tests (Fix 10) — rate limiter has ZERO unit tests currently
- getAutoResizeWarnings unit tests (Fix 11) — webhook warnings ship in this
  PR but have ZERO test coverage

### E2E (this PR)
- REQ-12: AutoResizeEvent status verification
- REQ-11: MinAvailable trigger test
- Expansion limit test verifies second resize is blocked (not just first lands at limit)
- time.Sleep replaced with Eventually/Consistently
- ALL E2E tests pass on AKS (run `hack/e2e/run-e2e-aks-autoresize.sh`)

### NOT in this PR (follow-up)
- Metrics refactoring (operator endpoint, ResizeBlocked, NextActionAt)
- Pointer field API changes
- resizeInUseVolumes gating
- Slot test stabilization
- E2E maxStep runtime clamping test (REQ-14, unit tests prove the math)
- E2E multi-instance test (REQ-16)
- E2E metric value assertions (REQ-18)

When ALL "this PR" criteria are met, output: <promise>COMPLETE</promise>
