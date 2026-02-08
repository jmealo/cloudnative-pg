# Comprehensive PR Review: PVC Auto-Resize with WAL-Aware Safety

**PR:** https://github.com/jmealo/cloudnative-pg/pull/4
**Scope:** 55 files changed, +11,142 additions
**Reviewer perspective:** PostgreSQL and CloudNativePG contributor reviewing AI-generated PR
**Review Date:** 2026-02-07

---

## Overall Assessment

This is a **substantial, well-designed feature** that addresses real-world operational pain documented in numerous GitHub issues (#9927, #9385, #8791, #1808, etc.). The RFC document is thorough and the implementation follows established CNPG patterns.

**Verdict:** Needs revision before merge. Several critical and important issues must be addressed, and the E2E test gaps documented in the requirements file (REQ-11 through REQ-26) should be closed.

---

## Critical Issues (Must Fix)

### 1. Global State in BudgetTracker Creates Test Isolation Problems

**Location:** `pkg/reconciler/autoresize/ratelimit.go:54`

```go
var GlobalBudgetTracker = NewBudgetTracker()
```

This is a package-level global variable that persists across reconciliation cycles. While the intent is correct (track rate limits per volume across cycles), global state causes:

- **Test pollution:** Unit tests that run in parallel can interfere with each other
- **Controller restart amnesia:** Budget state is lost if the operator restarts, allowing more resizes than intended
- **Multi-replica operators:** If running multiple operator replicas for HA, each has its own budget state

**Recommendation:** Store budget state in the Cluster Status (you already have `AutoResizeEvents` there). Query the events slice to count resizes in the 24h window. This makes the rate limit durable and consistent across restarts/replicas.

### 2. "Fail-Open" WAL Health Check May Mask Issues

**Location:** `pkg/reconciler/autoresize/walsafety.go:98-101`

```go
// If no WAL health data is available, allow the resize (fail-open for health data)
if walHealth == nil {
    return WALSafetyResult{Allowed: true}
}
```

This silently allows resize when WAL health can't be determined. In production, a nil `walHealth` could indicate:
- Instance manager not reporting status
- Network issues between operator and instance
- Instance crash

**Recommendation:** Add a configurable `failPolicy` field (`"Open"` vs `"Closed"`) with `"Closed"` as the default for safety-critical deployments. At minimum, emit a warning event when fail-open triggers.

### 3. E2E Test for Inactive Slot Retention is Marked Pending

**Location:** `tests/e2e/auto_resize_test.go:986`

```go
PIt("should block resize when replication slot retains too much WAL", ...
```

The comment explains: *"This test is flaky. The slot exists in PostgreSQL (verified via psql) but isn't being detected by the instance manager's WAL health check."*

This is a P0 requirement (REQ-07) that's not actually passing. The feature claims to block resize when slots retain too much WAL, but this isn't verified end-to-end.

**Recommendation:** Either fix the flaky test before merge, or explicitly document that `maxSlotRetentionBytes` is experimental and may not work reliably in the initial release.

---

## Important Issues (Should Fix)

### 4. minAvailable Trigger Is Untested (REQ-11)

The RFC describes `triggers.minAvailable` as a key feature that addresses the "percentage problem" (80% of 1Gi = only 200Mi free). However, there's no E2E test for this trigger mode.

**Recommendation:** Add `cluster-autoresize-minavailable.yaml.template` fixture and test case.

### 5. AutoResizeEvent Status Recording Is Untested (REQ-12)

The code appends events to `cluster.Status.AutoResizeEvents` but no test verifies this. The kubectl plugin and status endpoint depend on this data.

**Recommendation:** Add assertion to basic resize test:
```go
By("verifying an auto-resize event was recorded", func() {
    // check cluster.Status.AutoResizeEvents is populated
})
```

### 6. MaxStep Runtime Clamping Is Untested (REQ-14)

Test #8 validates the webhook accepts `maxStep` configuration, but doesn't verify that runtime clamping actually limits the step size.

**Recommendation:** Add E2E test with `size: 10Gi`, `step: "50%"`, `maxStep: "2Gi"` and verify PVC grows by at most 2Gi.

### 7. IntOrString Zero-Value Handling May Cause Confusion

**Location:** `pkg/reconciler/autoresize/clamping.go:77-81`

```go
// Default step when zero value (either empty string or zero int)
if (stepVal.Type == intstr.String && stepVal.StrVal == "") ||
    (stepVal.Type == intstr.Int && stepVal.IntVal == 0) {
    stepVal = intstr.FromString(defaultStepPercent)
}
```

`IntOrString{Type: intstr.Int, IntVal: 0}` is treated as "use default" rather than "0 step". This is non-obvious. If someone explicitly sets `step: 0` thinking it means "don't resize", they'll get 20% resizes instead.

**Recommendation:** Either reject `step: 0` in webhook validation, or document this behavior clearly.

### 8. parseQuantityOrDefault Silently Falls Back

**Location:** `pkg/reconciler/autoresize/clamping.go:136-148`

```go
func parseQuantityOrDefault(qtyStr string, defaultStr string) *resource.Quantity {
    if qtyStr == "" {
        qty, _ := resource.ParseQuantity(defaultStr)
        return &qty
    }
    qty, err := resource.ParseQuantity(qtyStr)
    if err != nil {
        fallback, _ := resource.ParseQuantity(defaultStr)
        return &fallback
    }
    return &qty
}
```

If parsing fails, it silently returns the default. This could mask configuration errors that passed webhook validation but still have invalid quantities.

**Recommendation:** Log a warning when falling back due to parse error.

---

## Design Observations

### 9. Disk Probe Accuracy on Directory-Based Provisioners

The RFC correctly documents that `statfs()` returns host filesystem stats for directory-based provisioners like `local-path-provisioner`. However, there's no runtime detection or warning.

**Recommendation:** Add a preflight check that compares device IDs across mount points. If data and WAL volumes report the same device, emit a warning event.

### 10. Multi-Instance Resize Behavior Unclear

All E2E tests use single-instance clusters or only verify pod `-1`. The reconciler loops over all PVCs, but it's unclear:
- Does resize of `-1` PVC affect `-2` PVC decision?
- Are all instances resized simultaneously or sequentially?
- Is rate limit per-PVC or per-cluster?

**Recommendation:** Add E2E test that verifies behavior across a 2+ instance cluster (already marked as REQ-16 gap).

---

## Code Quality Observations

### 11. Good Separation of Concerns

The implementation properly separates:
- `triggers.go` — when to resize
- `clamping.go` — how much to resize
- `walsafety.go` — safety gate logic
- `ratelimit.go` — budget tracking
- `reconciler.go` — orchestration

This makes the code testable and maintainable.

### 12. Comprehensive Webhook Validation

The webhook catches most invalid configurations:
- `minStep > maxStep` rejected
- Invalid quantities rejected
- Single-volume without `acknowledgeWALRisk` rejected
- Usage threshold range enforced

### 13. Good Use of Existing Patterns

The implementation follows CNPG conventions:
- Controller-runtime patterns for reconciliation
- Kubernetes owner references for resources
- Status conditions following K8s conventions
- Structured logging with controller-runtime logger

---

## E2E Test Gap Summary

From `docs/src/design/pvc-autoresize-e2e-requirements.md`:

| Priority | Requirement | Status |
|----------|-------------|--------|
| **P0** | REQ-11: MinAvailable trigger | GAP |
| **P1** | REQ-12: AutoResizeEvent status recording | GAP |
| **P1** | REQ-13: resize_blocked metric verification | GAP |
| **P1** | REQ-14: MaxStep runtime clamping | GAP |
| **P1** | REQ-15: MaxPendingWALFiles explicit test | GAP |
| **P1** | REQ-16: Multi-instance resize verification | GAP |
| P2 | REQ-18-26: Various metrics assertions | GAP |

The requirements document itself (which is excellent) identifies 14 gaps. A P0 feature with a P0 test gap should not merge.

---

## Security Observations

No security concerns identified. The implementation:
- Uses standard Kubernetes RBAC (PVC patch requires existing permissions)
- Doesn't introduce new network endpoints
- Doesn't store sensitive data
- Properly validates all user input in webhooks

---

## Strengths

1. **Excellent RFC documentation** — The design rationale is clear, alternatives are documented, and community issues are cited.

2. **WAL safety is the right approach** — Blocking resize when archiving is failing prevents masking critical failures. This is the key differentiator from generic PVC autoresizers.

3. **Behavior-driven configuration** — The `triggers`, `expansion`, `strategy` model is cleaner than flat fields and allows future extensibility (e.g., predictive mode).

4. **Rate limiting design** — Budget-based `maxActionsPerDay` maps to real cloud provider constraints better than cooldown periods.

5. **Clamping addresses scale problems** — `minStep` and `maxStep` handle both the "thundering herd" (small volumes) and "petabyte problem" (large volumes).

6. **Self-documenting gap analysis** — The E2E requirements document honestly catalogues what's tested and what's not.

---

## Recommended Action Plan

### Before merge:
1. Fix global BudgetTracker (store in status)
2. Add `minAvailable` trigger E2E test (P0 gap)
3. Either fix or explicitly mark `maxSlotRetentionBytes` as experimental
4. Add warning/event when WAL health fails open

### Should have before merge:
5. AutoResizeEvent status verification
6. MaxStep runtime clamping E2E test
7. Multi-instance resize E2E test

### Consider for follow-up:
8. Directory-based provisioner detection
9. Configurable fail-open/fail-closed policy
10. P2 metric assertions

---

## Summary

This is a well-designed feature that addresses real operational pain. The core implementation is solid and follows CNPG patterns. However, the test coverage gaps—particularly the untested `minAvailable` trigger and the flaky `maxSlotRetentionBytes` test—need to be addressed before this is production-ready.

The RFC and requirements documents are exceptionally thorough, which makes reviewing the implementation much easier. The self-documented gaps in `pvc-autoresize-e2e-requirements.md` show intellectual honesty about what's been verified.

**Recommendation:** Request changes to address the 3 critical issues, then iterate on the important issues.

---

## Appendix: Key Files Reviewed

### Core Implementation
- `pkg/reconciler/autoresize/reconciler.go` — Main reconciliation logic
- `pkg/reconciler/autoresize/clamping.go` — Expansion step calculation
- `pkg/reconciler/autoresize/walsafety.go` — WAL health safety gates
- `pkg/reconciler/autoresize/ratelimit.go` — Budget tracking
- `pkg/reconciler/autoresize/triggers.go` — Trigger evaluation

### Webhooks
- `internal/webhook/v1/cluster_webhook.go` — Validation logic
- `internal/webhook/v1/cluster_webhook_autoresize_test.go` — Webhook tests

### Instance Manager
- `pkg/management/postgres/disk/probe_linux.go` — statfs() disk probing
- `pkg/management/postgres/wal/health.go` — WAL health checking

### E2E Tests
- `tests/e2e/auto_resize_test.go` — End-to-end test suite

### Design Documents
- `docs/src/design/pvc-autoresize.md` — RFC
- `docs/src/design/pvc-autoresize-e2e-requirements.md` — Gap analysis
