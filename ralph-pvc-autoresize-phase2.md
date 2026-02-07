You are preparing the PVC Auto-Resize feature for CloudNativePG for PR review.
The feature is implemented, tested, and 11/12 E2E tests pass on AKS. Your job
is to clean up the branch for upstream submission.

Ref: docs/src/design/pvc-autoresize.md
E2E Requirements: docs/src/design/pvc-autoresize-e2e-requirements.md

## Project Context
- Repo: cloudnative-pg/cloudnative-pg (Fork by jmealo)
- Stack: Go, Controller Runtime, Kubebuilder, Ginkgo v2
- Constraints: strict linting, DCO sign-off required
- Branch: feat/pvc-autoresizing-wal-safety
- Image base: ghcr.io/jmealo/cloudnative-pg-testing
- Image tag: feat-pvc-autoresizing-<git-short-sha> (unique per build)
- AKS E2E script: `hack/e2e/run-e2e-aks-autoresize.sh`

## Current Test Status (2026-02-07)

### E2E Tests on AKS (11/12 passing)

| # | Test | Status |
|---|------|--------|
| 1 | Basic auto-resize with single volume | PASS |
| 2 | Auto-resize with separate WAL volume | PASS |
| 3 | Expansion limit enforcement | PASS |
| 4 | Webhook validation (reject without acknowledgeWALRisk) | PASS |
| 5 | Webhook validation (accept with acknowledgeWALRisk) | PASS |
| 6 | Rate-limit enforcement | PASS |
| 7 | MinStep clamping | PASS |
| 8 | MaxStep webhook validation | PASS |
| 9 | Metrics exposure | PASS |
| 10 | Tablespace resize | PASS |
| 11 | WAL archive health blocks resize | PASS |
| 12 | Inactive slot blocks resize | PENDING (flaky) |

### Known Issue: Test #12 (Inactive Slot Detection)

The slot retention test is `PIt()` (Ginkgo Pending). The replication slot
exists in PostgreSQL (verified via direct psql query) but the instance
manager's WAL health status reports an empty InactiveSlots array.

Root cause: Timing issue in status propagation pipeline. The slot detection
query (`queryInactiveSlots` in `wal/health.go`) is correct and fully
unit-tested. The flakiness occurs because:
1. `fillWALHealthStatus` runs as part of the instance status probe cycle
2. Under AKS load, the probe may be delayed or the DB connection saturated
3. Query errors are non-fatal (logged, not returned) — `InactiveSlots` stays nil

The WAL safety blocking mechanism is proven by the archive health test (#11)
which exercises the same code path. Ship with `PIt()` and stabilize in a
follow-up (add retry logic or longer probe timeout to the instance manager).

---

## Implementation Inventory

### Core implementation (commit 8f9b2b1b2):
- `pkg/management/postgres/disk/probe.go` — statfs disk probe
- `pkg/management/postgres/wal/health.go` — WAL health checker
- `pkg/management/postgres/webserver/metricserver/disk.go` — Prometheus metrics
- `pkg/management/postgres/disk_status.go` — fillDiskStatus + fillWALHealthStatus
- `pkg/management/postgres/probes.go` — calls fillDiskStatus and fillWALHealthStatus
- `pkg/postgres/status.go` — DiskStatus, WALHealthStatus, WALInactiveSlotInfo types
- `api/v1/cluster_types.go` — ResizeConfiguration, ResizeTriggers, ExpansionPolicy,
  ResizeStrategy, WALSafetyPolicy, ClusterDiskStatus, InstanceDiskStatus,
  VolumeDiskStatus, WALHealthInfo, InactiveSlotInfo, AutoResizeEvent
- `pkg/reconciler/autoresize/` — reconciler.go, clamping.go, triggers.go,
  ratelimit.go, walsafety.go + all test files + suite_test.go
- `internal/webhook/v1/cluster_webhook.go` — validateAutoResize
- `internal/webhook/v1/cluster_webhook_autoresize_test.go`
- `internal/webhook/v1/cluster_webhook_autoresize_conflicts_test.go`
- `internal/controller/cluster_controller.go` — buildDiskInfoByPod, autoresize.Reconcile

### E2E tests:
- `tests/e2e/auto_resize_test.go` — 12 test contexts
- `tests/e2e/fixtures/auto_resize/` — 9 fixture templates
- `tests/labels.go` — LabelAutoResize constant

### User-facing:
- `docs/src/samples/monitoring/prometheusrule.yaml` — cnpg-disk.rules group (10 alerts)
- `docs/src/storage_autoresize.md` — user documentation
- `docs/src/storage.md` — cross-reference added
- `internal/cmd/plugin/disk/` — kubectl cnpg disk status command
- `cmd/kubectl-cnpg/main.go` — disk.NewCmd() registered
- `internal/controller/cluster_status.go` — updateDiskStatus function

---

## CRITICAL: Three-Binary Architecture

CNPG builds THREE different binaries from the same repo:

### 1. Controller Manager (`cmd/manager/main.go`)
- **Runs**: In the `cnpg-controller-manager` pod in `cnpg-system` namespace
- **Platform**: Linux only (runs in a container)
- **Contains**: Reconcilers, webhooks, autoresize logic

### 2. Instance Manager (same binary, different entrypoint)
- **Runs**: Inside every PostgreSQL pod (copied by `bootstrap-controller` init container)
- **Platform**: Linux only
- **Contains**: Disk probe (statfs), WAL health checker, metrics server

### 3. kubectl-cnpg Plugin (`cmd/kubectl-cnpg/main.go`)
- **Runs**: On the user's workstation (darwin, linux, windows)
- **Platform**: Cross-platform
- **Does NOT import**: `pkg/management/postgres/disk/`, `pkg/management/postgres/wal/`

The `probe.go` package uses `syscall.Statfs_t` (Linux-only). This is fine
because the kubectl plugin never imports it. **Do NOT add platform-specific
build tags** — the original single-file `probe.go` is correct.

### Fixed PostgreSQL parameters

CNPG controls `archive_command`, `archive_mode`, and others via
`FixedConfigurationParameters`. To make WAL archiving fail in E2E tests,
configure a `backup.barmanObjectStore` with a non-existent endpoint.

---

## Phase 1: PR Branch Cleanup

The branch has accumulated many fix commits from the development loop. Before
submitting for review, clean up the history:

### 1.1 Revert unnecessary platform-specific build tag split

The earlier loop split `probe.go` into `probe_linux.go` and `probe_other.go`.
This was unnecessary (see Three-Binary Architecture above). Revert to a single
`probe.go` file:

```bash
# Check current state
ls pkg/management/postgres/disk/probe*.go

# If probe_linux.go and probe_other.go exist, merge back to probe.go
# Remove build tags, keep the Linux implementation only
```

### 1.2 Verify local checks pass

```bash
make generate && make manifests && make fmt && make lint && make test
```

All must exit 0.

### 1.3 Rebuild and verify E2E still passes

```bash
hack/e2e/run-e2e-aks-autoresize.sh
```

11/12 tests should pass (Test #12 is `PIt()`).

---

## Phase 2: Commit History Organization

If the user requests, squash the fix commits into logical groups for the PR.
The target structure should be:

1. **feat(autoresize): add PVC auto-resize with WAL safety** — core implementation
2. **feat(autoresize): add auto-resize E2E tests** — E2E test suite
3. **feat(autoresize): add user documentation and monitoring rules** — docs + alerts
4. **feat(autoresize): add kubectl cnpg disk status command** — plugin extension

Only squash if explicitly asked. The current commit history is also acceptable
for PR review — some maintainers prefer seeing the development progression.

---

## Phase 3: PR Description Preparation

Draft a PR description covering:

### Summary
- What: PVC auto-resize with WAL-aware safety mechanisms
- Why: CloudNativePG currently requires manual intervention when disks fill up
- How: New reconciler loop with configurable triggers, expansion policy, and safety checks

### Key Design Decisions
- Percentage-based steps with minStep/maxStep clamping (not flat amounts)
- Budget-based rate limiting (maxActionsPerDay, not cooldown timers)
- WAL safety: archive health, pending WAL files, slot retention checks
- acknowledgeWALRisk for single-volume clusters
- Defense-in-depth: clamping → limit cap → newSize ≤ currentSize guard

### Test Coverage
- Unit tests: 61+ tests across clamping, triggers, rate limiting, WAL safety,
  webhook validation, and cross-field configuration conflicts
- E2E tests: 11/12 passing on AKS (1 pending — flaky slot detection, documented)
- See `docs/src/design/pvc-autoresize-e2e-requirements.md` for full inventory

### Known Limitations
- Directory-based provisioners (local-path-provisioner) share host filesystem
  stats across PVCs — documented, not suitable for this feature
- Test #12 (inactive slot blocks resize) is `PIt()` due to flaky status
  propagation under AKS load — blocking logic is unit-tested

### Breaking Changes
- None. Auto-resize is opt-in (disabled by default)

### Documentation
- User guide: `docs/src/storage_autoresize.md`
- RFC: `docs/src/design/pvc-autoresize.md`
- Prometheus alerts: `docs/src/samples/monitoring/prometheusrule.yaml`

---

## E2E Re-run Quick Reference

```bash
# Full pipeline (build + deploy + test):
hack/e2e/run-e2e-aks-autoresize.sh

# Skip build (redeploy + test):
hack/e2e/run-e2e-aks-autoresize.sh --skip-build

# Test only (operator already deployed):
hack/e2e/run-e2e-aks-autoresize.sh --skip-build --skip-deploy

# Focus on specific test:
hack/e2e/run-e2e-aks-autoresize.sh --focus "archive health" --skip-build --skip-deploy

# Diagnose without running tests:
hack/e2e/run-e2e-aks-autoresize.sh --diagnose-only
```

Test names for `--focus`:
`basic auto-resize`, `separate WAL volume`, `expansion limit`, `webhook`,
`rate-limit`, `minStep`, `maxStep`, `metrics`, `tablespace`, `archive health`,
`inactive slot`

---

## Azure Disk Known Issues

Azure Disk CSI has volume attachment latency. Mitigations in the test config:
- GINKGO_NODES=3 (parallel test execution across 3-node cluster)
- 3h overall Ginkgo timeout
- 5-minute Eventually() timeouts for resize detection
- Namespace cleanup before test runs to free Azure Disks
- Increased ClusterIsReady timeouts (900s/1200s)

If volume attachment issues occur, run `--diagnose-only` and check:
- `kubectl get events --field-selector reason=FailedAttachVolume`
- `kubectl get volumeattachments`
- CSI driver pod health: `kubectl get pods -n kube-system -l app=csi-azuredisk-node`

---

## Commit Convention

DCO sign-off required on every commit:
```
git commit -s -m "$(cat <<'COMMITEOF'
feat(component): description here

Co-Authored-By: Claude Opus 4.5 <noreply@anthropic.com>
COMMITEOF
)"
```

## Completion Criteria

ALL of the following must be true:
- `make generate && make manifests && make fmt && make lint && make test` all exit 0
- Multi-arch Docker image built and pushed with unique SHA-based tag
- 11/12 E2E tests pass on AKS (Test #12 `PIt()` is acceptable)
- PR description drafted and ready for review
- Branch is clean (no untracked dev artifacts committed)
- All commits have DCO sign-off

When ALL criteria are met, output: <promise>COMPLETE</promise>
