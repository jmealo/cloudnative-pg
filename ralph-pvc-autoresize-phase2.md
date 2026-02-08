You are finishing the PVC Auto-Resize feature for CloudNativePG before PR
submission. Phases 1-3 (status persistence, rate limiting, metrics) are RESOLVED.

**CRITICAL SCOPE RULE:** This PR already changes 55 files with 11,500+ insertions.
Every additional change makes it harder to review. Only implement items marked
"SHIP IN THIS PR" below. Items marked "FOLLOW-UP PR" should NOT be implemented —
they will be separate, focused PRs after the core feature merges.

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

### Verification

1. `make test` passes
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

### E2E 4: Slot Retention Test — Keep as PIt() (Pending)

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

## FOLLOW-UP PR #1: Webhook Validation Warnings

These are safe UX improvements but NOT bugs. Ship separately to keep the
core PR reviewable.

Items:
- Warn when `limit < spec.storage.size` (silent no-op)
- Warn when `minStep`/`maxStep` set with absolute step (silently ignored)
- Warn when `maxActionsPerDay: 0` with `enabled: true` (effectively disabled)
- Warn for `step > 100%` (massive resize)
- Warn when `minAvailable > spec.storage.size` (immediate trigger)
- Warn when `acknowledgeWALRisk` on dual-volume cluster (no-op)
- Warn for `requireArchiveHealthy: true` without backup stanza
- Reject `usageThreshold: 0` (zero-value ambiguity)

## FOLLOW-UP PR #2: Metrics Refactoring

Items:
- Move `cnpg_disk_resize_blocked` to operator metrics endpoint (or populate
  from status)
- Consider moving all resize-action metrics to operator endpoint
- Add `NextActionAt` or `BudgetResetAt` status field for observability

## FOLLOW-UP PR #3: Design Improvements

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

ALL of the following must be true:

### Build & Test
- `make generate && make manifests && make fmt && make lint && make test` exit 0
- All E2E tests pass on AKS (slot test stays as PIt)
- All commits have DCO sign-off

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

### E2E (this PR)
- REQ-12: AutoResizeEvent status verification
- REQ-11: MinAvailable trigger test
- time.Sleep replaced with Eventually/Consistently

### NOT in this PR (follow-up)
- Webhook warnings (limit < size, minStep/maxStep with absolute step, etc.)
- Metrics refactoring (operator endpoint, ResizeBlocked, NextActionAt)
- Pointer field API changes
- resizeInUseVolumes gating
- Slot test stabilization

When ALL "this PR" criteria are met, output: <promise>COMPLETE</promise>
