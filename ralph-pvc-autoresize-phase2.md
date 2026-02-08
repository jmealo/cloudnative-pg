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

## Phase 1: Fix Status Persistence Bug (CRITICAL)

### The Bug

In `internal/controller/cluster_controller.go`, the autoresize reconciler is
called at line ~815:

```go
if res, err := autoresize.Reconcile(ctx, r.Client, cluster, diskInfoByPod,
    resources.pvcs.Items); err != nil || !res.IsZero() {
    return res, err  // <-- EARLY RETURN
}
```

When a resize occurs, `Reconcile` returns `ctrl.Result{RequeueAfter: 30s}`.
The controller returns early, **skipping** `RegisterPhase` (line 866) and any
status update. `AutoResizeEvents` appended during the resize are LOST.

### The Fix

After `autoresize.Reconcile` returns a non-zero result, patch the cluster
status before returning. Follow the pattern in `scheduledbackup_controller.go`:

```go
// Auto-resize PVCs based on disk usage
origCluster := cluster.DeepCopy()
diskInfoByPod := buildDiskInfoByPod(instancesStatus)
if res, err := autoresize.Reconcile(ctx, r.Client, cluster, diskInfoByPod,
    resources.pvcs.Items); err != nil || !res.IsZero() {
    // Persist status changes (AutoResizeEvents) even on early return
    if statusErr := r.Client.Status().Patch(ctx, cluster,
        client.MergeFrom(origCluster)); statusErr != nil {
        contextLogger.Error(statusErr, "failed to persist auto-resize status")
    }
    return res, err
}
```

### Verification

1. `make test` passes
2. After E2E basic resize test, verify:
   ```bash
   kubectl get cluster -n <ns> -o jsonpath='{.status.autoResizeEvents}'
   ```
   Must show at least one event with `result: success`.

---

## Phase 2: Fix Non-Persistent Rate Limiting (CRITICAL)

### The Bug

`pkg/reconciler/autoresize/ratelimit.go` uses a global in-memory
`BudgetTracker` (line 54: `var GlobalBudgetTracker = NewBudgetTracker()`).
If the operator pod restarts, all resize history is lost. A cluster could
exceed `maxActionsPerDay` immediately after restart.

### The Fix

Derive the budget from `cluster.Status.AutoResizeEvents` instead of the
in-memory tracker. The reconciler already appends `AutoResizeEvent` structs
(reconciler.go line ~283) with timestamps and volume info.

**Option A (preferred): Replace GlobalBudgetTracker entirely**

Add a new function that queries the cluster status:

```go
// HasBudgetFromStatus checks if a volume has remaining resize budget by
// counting successful AutoResizeEvents in the last 24 hours.
func HasBudgetFromStatus(cluster *apiv1.Cluster, volumeKey string,
    maxActionsPerDay int) bool {
    if maxActionsPerDay <= 0 {
        return true
    }
    cutoff := time.Now().Add(-24 * time.Hour)
    count := 0
    for _, event := range cluster.Status.AutoResizeEvents {
        if event.Timestamp.After(cutoff) &&
            event.Result == "success" &&
            matchesVolumeKey(event, volumeKey) {
            count++
        }
    }
    return count < maxActionsPerDay
}
```

Then update `reconcilePVC()` in reconciler.go to call this instead of
`GlobalBudgetTracker.HasBudget()`.

**Option B: Hydrate the in-memory tracker from status on startup**

If you prefer keeping the in-memory tracker for performance:
- Add `HydrateFromEvents(events []AutoResizeEvent)` to BudgetTracker
- Call it at the start of each `Reconcile()` call

**Dependency:** Phase 1 (status persistence) must be done first, or the events
won't exist in the API server to query.

### Verification

1. `make test` passes (update ratelimit_test.go)
2. E2E rate-limit test still passes
3. Manually verify: after a resize, restart the operator pod, check that the
   budget is correctly reflected from status

---

## Phase 3: Implement Resize Metrics (CRITICAL)

### The Bug

Four metrics are registered in `disk.go` but never set:
- `cnpg_disk_at_limit` (GaugeVec) — defined line 92, never `.Set()`
- `cnpg_disk_resize_blocked` (GaugeVec) — defined line 97, never `.Set()`
- `cnpg_disk_resizes_total` (CounterVec) — defined line 102, never `.Inc()`
- `cnpg_disk_resize_budget_remaining` (GaugeVec) — defined line 107, never `.Set()`

PrometheusRules in `prometheusrule.yaml` reference these. Alerts never fire.

### The Fix

These are **operator-level decisions** (resize happens in the controller), but
the metrics are registered in the **instance manager's** metric exporter. Two
approaches:

**Option A (simpler): Set metrics from cluster status in instance manager**

The instance manager already reads disk status from the cluster CR. Add resize
status fields to `ClusterDiskStatus` (or `VolumeDiskStatus`) that the controller
sets, then have the instance manager read and export them:

In `api/v1/cluster_types.go`, add to `VolumeDiskStatus`:
```go
AtLimit         bool   `json:"atLimit,omitempty"`
ResizeBlocked   bool   `json:"resizeBlocked,omitempty"`
BlockReason     string `json:"blockReason,omitempty"`
BudgetRemaining int    `json:"budgetRemaining,omitempty"`
ResizeCount     int    `json:"resizeCount,omitempty"`
```

Then in `disk.go`'s `updateDiskMetrics()`, read these fields and set the gauges.

**Option B (preferred for operator metrics): Add operator-level Prometheus**

Register and set these metrics in the controller manager's existing Prometheus
endpoint. This is more architecturally correct since the controller makes the
resize decisions. Look at how other CNPG controller metrics are registered.

### Verification

1. `make test` passes
2. E2E metrics test: after a resize, verify:
   ```
   cnpg_disk_resizes_total{result="success"} > 0
   cnpg_disk_resize_budget_remaining >= 0
   ```
3. E2E archive-block test: verify `cnpg_disk_resize_blocked == 1`

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
- `make generate && make manifests && make fmt && make lint && make test` exit 0
- Status persistence: AutoResizeEvents are persisted after resize
- Rate limiting: Budget is durable across operator restarts
- Metrics: `cnpg_disk_resizes_total`, `cnpg_disk_at_limit`, etc. are populated
- WAL fail-open emits warning event
- parseQuantityOrDefault logs on invalid input
- All E2E tests pass on AKS (including stabilized slot test if feasible)
- REQ-11 (minAvailable) and REQ-12 (AutoResizeEvent) E2E tests added
- All commits have DCO sign-off

When ALL criteria are met, output: <promise>COMPLETE</promise>
