You are fixing critical bugs in the PVC Auto-Resize feature for CloudNativePG
before PR submission. The feature is implemented and 11/12 E2E tests pass on
AKS, but internal review found critical persistence and metrics bugs.

Ref: docs/src/design/pvc-autoresize.md (see "Pre-Merge Implementation Issues")
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
The blocked condition is decided by the operator but the metric lives in the
instance manager. Address in Phase 3.5 below.

---

## Phase 3.5: Remaining Code Issues from CNPG-Idiomatic Review

### 3.5.1 Aggregate Errors in PVC Loop (Important)

In `reconciler.go`, if 3 out of 5 PVCs fail, the function returns `nil` error.

**Fix:**
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

### 3.5.2 Emit Event for "At Expansion Limit" (Important)

Rate limit and WAL safety blocks now emit events. Expansion limit blocks
do not. Add:

```go
if currentSize.Cmp(limit) >= 0 {
    contextLogger.Info("auto-resize blocked: at expansion limit", "pvc", pvc.Name)
    recorder.Eventf(cluster, corev1.EventTypeWarning, "AutoResizeAtLimit",
        "Volume %s has reached expansion limit %s", pvc.Name, expansion.Limit)
    return false, nil
}
```

### 3.5.3 Named Constant for Event History Cap (Style)

Replace magic number `50` in `reconciler.go`:
```go
const maxAutoResizeEventHistory = 50
```

### 3.5.4 Log Warning for Invalid minAvailable (Style)

In `triggers.go`, `ShouldResize` silently ignores invalid `minAvailable`:
```go
minAvailableQty, err := resource.ParseQuantity(triggers.MinAvailable)
if err != nil {
    contextLogger.Warn("invalid minAvailable, using percentage trigger only",
        "minAvailable", triggers.MinAvailable, "error", err)
    return usedPercent > float64(usageThreshold)
}
```

### 3.5.5 Fix ResizeBlocked Metric (Important)

`cnpg_disk_resize_blocked` is still not populated. To keep it in the instance
manager, the controller must write a blocked reason to cluster status that the
instance manager can read. Alternatively, skip this metric for now and document
it as a follow-up (move to operator metrics endpoint).

### 3.5.6 Use %w for All Error Wrapping (Style)

Grep for `fmt.Errorf.*%v` in the autoresize package and replace with `%w`.

### Verification

1. `make test` passes
2. `make lint` passes
3. `make vet` passes

---

## Phase 4: Add WAL Health Fail-Open Warning (IMPORTANT)

### The Issue

In `pkg/reconciler/autoresize/walsafety.go:98`:
```go
if walHealth == nil {
    return WALSafetyResult{Allowed: true}
}
```

Resize proceeds silently when WAL health data is unavailable.

### The Fix

Emit a Kubernetes warning event before returning:

```go
if walHealth == nil {
    contextLogger.Info("WAL health data unavailable, allowing resize (fail-open)")
    return WALSafetyResult{
        Allowed: true,
        Reason:  "wal_health_unavailable",
    }
}
```

Then in `reconcilePVC()` (reconciler.go), when the WAL safety result has
`Reason == "wal_health_unavailable"`, record a warning event:

```go
if walResult.Reason == "wal_health_unavailable" {
    r.Recorder.Eventf(cluster, corev1.EventTypeWarning, "AutoResizeWALHealthUnavailable",
        "Auto-resize permitted without WAL health verification for PVC %s", pvc.Name)
}
```

### Verification

1. `make test` passes (update walsafety_test.go to check Reason field)
2. No regression in E2E tests

---

## Phase 5: Add parseQuantityOrDefault Warning Log (IMPORTANT)

### The Issue

`pkg/reconciler/autoresize/clamping.go:136` silently falls back to defaults
when user quantities fail to parse. A typo like `minStep: "2XGi"` silently
becomes `minStep: "2Gi"`.

### The Fix

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

### Verification

1. `make test` passes
2. `make lint` passes (may need to add `log` import)

---

## Phase 5.5: Reject or Document `step: 0` Zero-Value (IMPORTANT)

### The Issue

In `pkg/reconciler/autoresize/clamping.go:77-81`:
```go
if (stepVal.Type == intstr.String && stepVal.StrVal == "") ||
    (stepVal.Type == intstr.Int && stepVal.IntVal == 0) {
    stepVal = intstr.FromString(defaultStepPercent)
}
```

`IntOrString{Type: intstr.Int, IntVal: 0}` (i.e. `step: 0`) is treated as
"use default 20%" rather than "0 step = don't resize". If someone explicitly
sets `step: 0` thinking it disables resize, they get 20% resizes instead.

### The Fix (choose one)

**Option A: Reject `step: 0` in webhook validation** (preferred)

In `internal/webhook/v1/cluster_webhook.go`, `validateExpansionStep()`:
```go
if stepVal.Type == intstr.Int && stepVal.IntVal == 0 {
    return field.ErrorList{field.Invalid(fldPath.Child("step"), stepVal,
        "step must be a positive quantity or percentage, not 0")}
}
```

**Option B: Document the behavior**

If `step: 0` should mean "use default", document this explicitly in
`docs/src/storage_autoresize.md` in the expansion section. Add a note:
"If `step` is omitted or set to `0`, the default step of 20% is used."

### Verification

1. `make test` passes
2. If Option A: add webhook unit test for `step: 0` rejection

---

## Phase 5.75: Webhook Validation Warnings (IMPORTANT)

A comprehensive review identified multiple configurations that the webhook
accepts but lead to surprising or broken runtime behavior. Implement webhook
warnings for the highest-impact cases.

### Context

The full analysis is in `docs/src/design/pvc-autoresize.md` section
"Configuration Conflicts & Validation Gaps". The summary table there lists
all cases by severity.

### Must-Have Validations (implement in this phase)

**1. Reject `usageThreshold: 0`** — same zero-value ambiguity as `step: 0`.

In `internal/webhook/v1/cluster_webhook.go`, `validateResizeTriggers()`:
```go
if triggers.UsageThreshold != nil && *triggers.UsageThreshold == 0 {
    allErrs = append(allErrs, field.Invalid(fldPath.Child("usageThreshold"),
        *triggers.UsageThreshold,
        "usageThreshold must be between 1 and 99, not 0"))
}
```

**2. Warn when `limit < spec.storage.size`** — silent no-op.

In `validateExpansionPolicy()`:
```go
if limit != nil && currentSize != nil && limit.Cmp(*currentSize) < 0 {
    // Emit warning (not error) — limit could be raised later
    contextLogger.Warn("expansion.limit is less than current storage size; auto-resize will not trigger",
        "limit", limit.String(), "currentSize", currentSize.String())
}
```

**3. Warn when `minStep`/`maxStep` set with absolute `step`** — silently ignored.

In `validateExpansionPolicy()`:
```go
if isAbsoluteStep && (minStep != nil || maxStep != nil) {
    contextLogger.Warn("minStep and maxStep are only applied to percentage-based steps; ignored for absolute step",
        "step", step.String())
}
```

**4. Warn when `maxActionsPerDay: 0` with `enabled: true`** — effectively disabled.

In `validateResizeStrategy()`:
```go
if strategy.MaxActionsPerDay != nil && *strategy.MaxActionsPerDay == 0 {
    contextLogger.Warn("maxActionsPerDay: 0 effectively disables auto-resize despite enabled: true")
}
```

**5. Reject bare integer step values** (e.g., `step: 20` → 20 bytes) — unit ambiguity.

A user writing `step: 20` likely means 20%, but IntOrString integer values are
parsed as `resource.ParseQuantity("20")` which yields 20 bytes. This is almost
never the user's intent.

In `validateExpansionStep()`:
```go
if stepVal.Type == intstr.Int && stepVal.IntVal > 0 {
    return field.ErrorList{field.Invalid(fldPath.Child("step"), stepVal,
        "integer step values are ambiguous; use a percentage string like '20%' or an absolute quantity like '5Gi'")}
}
```

### Nice-to-Have Validations (implement if time permits)

6. Warn for `step > 100%` (each resize more than doubles the volume)
7. Warn when `minAvailable > spec.storage.size` (immediate trigger)
8. Warn when `acknowledgeWALRisk` set on dual-volume cluster (no-op)
9. Warn for `requireArchiveHealthy: true` without backup stanza

### Unit Tests

Add tests for each validation to
`internal/webhook/v1/cluster_webhook_autoresize_conflicts_test.go`:

```go
It("should reject usageThreshold: 0", func() { ... })
It("should warn when limit < current size", func() { ... })
It("should warn when minStep/maxStep set with absolute step", func() { ... })
It("should warn when maxActionsPerDay is 0", func() { ... })
```

### Verification

1. `make test` passes
2. Existing webhook tests still pass (no regressions)
3. New conflict unit tests pass

---

## Phase 6: E2E Test Gaps (P0 + P1)

Read `docs/src/design/pvc-autoresize-e2e-requirements.md` in full. Address
these gaps IN ORDER of priority:

### 6.1 REQ-12: AutoResizeEvent Verification (P1, now CRITICAL)

After fixing status persistence (Phase 1), add verification to Test #1:

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

This test validates Phase 1 actually works.

### 6.2 REQ-11: MinAvailable Trigger (P0)

New fixture `cluster-autoresize-minavailable.yaml.template`:
- `triggers.minAvailable: "300Mi"` (no usageThreshold or set to 99)
- `size: 2Gi`

New test: fill disk until <300Mi remain, verify PVC grows.

### 6.3 REQ-14: MaxStep Runtime Clamping (P1)

New fixture `cluster-autoresize-maxstep-runtime.yaml.template`:
- `size: 10Gi`, `step: "50%"`, `maxStep: "2Gi"`

New test: fill disk, verify PVC grows by at most 2Gi (to 12Gi, not 15Gi).

### 6.4 REQ-16: Multi-Instance Resize (P1)

The basic fixture already has `instances: 2`. Add assertions for pod -2:

```go
By("verifying ALL instance PVCs were resized", func() {
    for i := 1; i <= 2; i++ {
        pvcName := fmt.Sprintf("%s-%d", clusterName, i)
        // ... check PVC size > original
    }
})
```

### 6.5 Replace time.Sleep with Eventually (P1)

Several tests use `time.Sleep(2 * time.Minute)`. Replace with:
```go
Consistently(func(g Gomega) {
    // verify PVC size has NOT changed
}, 2*time.Minute, 10*time.Second).Should(Succeed())
```

This speeds up passing tests and makes assertions explicit.

---

## Phase 7: Stabilize Slot Retention Test (IMPORTANT)

### VictoriaLogs Findings

The test creates the slot and verifies it via psql (`128MB retention`), but
cluster status shows `InactiveSlotCount=0`. VictoriaLogs shows disk status
fields but NO WAL health fields are being propagated.

### Root Cause Candidates

1. **isPrimary gating**: `queryInactiveSlots` only runs when `isPrimary == true`
   (`wal/health.go:110`). If the instance is transiently in recovery during the
   status probe, slot checks are silently skipped.

2. **Non-fatal error swallowing**: Query errors at `wal/health.go:113` are
   logged but not returned. If `queryInactiveSlots` fails, `InactiveSlots` = nil.

3. **Missing WAL health in status**: VictoriaLogs shows NO WAL health fields.
   Check if `fillWALHealthStatus` is silently failing (line 106-109 in
   `disk_status.go` returns early on error).

### Fix Strategy

1. Add structured logging to `queryInactiveSlots`:
   ```go
   contextLogger.Info("querying inactive replication slots",
       "isPrimary", isPrimary, "resultCount", len(status.InactiveSlots))
   ```

2. In `fillWALHealthStatus`, add logging on success too (not just error):
   ```go
   contextLogger.Debug("WAL health check complete",
       "archiveHealthy", healthStatus.ArchiveHealthy,
       "inactiveSlots", len(healthStatus.InactiveSlots))
   ```

3. Make the slot query timeout explicit (5s context deadline).

4. After fixes, re-enable the test by changing `PIt` → `It` and re-run on AKS.

---

## Phase 8: Build and Verify

### Local verification

```bash
make generate && make manifests && make fmt && make lint && make test
```

### E2E on AKS

```bash
hack/e2e/run-e2e-aks-autoresize.sh
```

All 12 tests should pass (including the stabilized slot test).

### Fast iteration with --focus

```bash
hack/e2e/run-e2e-aks-autoresize.sh --focus "basic auto-resize" --skip-build --skip-deploy
hack/e2e/run-e2e-aks-autoresize.sh --focus "inactive slot" --skip-build --skip-deploy
```

---

## Phase 9: Branch Cleanup

### 9.1 Revert unnecessary platform-specific build tag split

If `probe_linux.go` and `probe_other.go` exist, merge back to `probe.go`.

### 9.2 Remove untracked dev artifacts

Do NOT commit: `.claude/`, `*.pdf`, `ralph-*.md`, `*SUMMARY.md`, etc.

### 9.3 Final local verification

```bash
make generate && make manifests && make fmt && make lint && make test
```

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
- All E2E tests pass on AKS (including stabilized slot test if feasible)
- All commits have DCO sign-off

### Critical Bugs (RESOLVED)
- ~~Status persistence: AutoResizeEvents are persisted after resize~~ ✅
- ~~Rate limiting: Budget is durable across operator restarts~~ ✅
- ~~Metrics: `cnpg_disk_resizes_total`, `cnpg_disk_at_limit` populated~~ ✅

### Remaining Code Fixes
- Error aggregation in PVC loop (`errors.Join`)
- Event for "at expansion limit"
- Named constant for event history cap (50)
- WAL fail-open emits warning event
- parseQuantityOrDefault logs on invalid input
- `step: 0` and `usageThreshold: 0` rejected or documented
- Webhook warnings for `limit < size`, `minStep`/`maxStep` with absolute step,
  `maxActionsPerDay: 0`

### E2E Test Gaps
- REQ-11 (minAvailable) and REQ-12 (AutoResizeEvent) E2E tests added
- REQ-14 (maxStep runtime clamping) E2E test added
- REQ-16 (multi-instance resize verification) assertions added

When ALL criteria are met, output: <promise>COMPLETE</promise>
