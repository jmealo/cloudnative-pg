You are implementing the PVC Auto-Resize feature for CloudNativePG, following the RFC at docs/src/design/pvc-autoresize.md in this repo.

## Project Context

This is a Go project using controller-runtime, kubebuilder, Ginkgo/Gomega for tests.
The codebase is cloudnative-pg/cloudnative-pg. You are working on a fork.

## What To Do On Each Iteration

1. Run: git log --oneline -20 && git diff --stat HEAD~1
2. Run: make lint 2>&1 | tail -50
3. Run: make test 2>&1 | tail -100
4. Read this prompt again. Determine which phase you are in based on what exists.
5. Pick the NEXT incomplete task from the current phase.
6. Implement it. Before committing, run: make generate manifests fmt lint test
7. Commit with DCO sign-off (git commit -s). If lint or tests fail, fix before committing.

## Phase 1: Metrics Foundation

Tasks (in order):
- [ ] Add disk.Probe struct in pkg/management/postgres/disk/probe.go
      Uses statfs() on PGDATA, WAL, and tablespace mount points.
      Returns VolumeStats{TotalBytes, UsedBytes, AvailableBytes, PercentUsed, InodesTotal, InodesUsed, InodesFree}.
      Build on existing machinery DiskProbe pattern.
- [ ] Add WAL health checker in pkg/management/postgres/wal/health.go
      Counts .ready files in pg_wal/archive_status/.
      Queries pg_stat_archiver for last_archived_time, last_failed_time, failed_count.
      Queries pg_replication_slots for inactive physical slots + pg_wal_lsn_diff() retention.
      Returns HealthStatus struct.
- [ ] Register Prometheus metrics on :9187 exporter:
      cnpg_disk_total_bytes, cnpg_disk_used_bytes, cnpg_disk_available_bytes,
      cnpg_disk_percent_used, cnpg_disk_inodes_total, cnpg_disk_inodes_used,
      cnpg_disk_inodes_free, cnpg_disk_at_limit, cnpg_disk_resize_blocked,
      cnpg_disk_resizes_total, cnpg_disk_resize_budget_remaining,
      cnpg_wal_archive_healthy, cnpg_wal_pending_archive_files,
      cnpg_wal_inactive_slots, cnpg_wal_slot_retention_bytes.
      Labels: volume_type (data/wal/tablespace), tablespace, reason, result, slot_name.
- [ ] Extend /pg/status endpoint on :8000 to include disk status + WAL health in JSON.
- [ ] Unit tests for disk.Probe (mock statfs), WAL health checker (mock SQL), metrics registration.
- [ ] Verify: make lint && make test pass with zero failures.

Phase 1 gate: ALL metrics registered, status endpoint extended, unit tests green.

## Phase 2: Auto-Resize Core (Behavior-Driven Configuration)

API Types to add in api/v1/cluster_types.go:

type ResizeConfiguration struct {
    Enabled   bool             json:\"enabled\"
    Triggers  *ResizeTriggers  json:\"triggers,omitempty\"
    Expansion *ExpansionPolicy json:\"expansion,omitempty\"
    Strategy  *ResizeStrategy  json:\"strategy,omitempty\"
}

type ResizeTriggers struct {
    UsageThreshold int    json:\"usageThreshold,omitempty\"  // 1-99, default 80
    MinAvailable   string json:\"minAvailable,omitempty\"    // e.g. \"10Gi\"
}

type ExpansionPolicy struct {
    Step    intstr.IntOrString json:\"step,omitempty\"     // \"20%\" or \"10Gi\", default \"20%\"
    MinStep string             json:\"minStep,omitempty\"  // default \"2Gi\"
    MaxStep string             json:\"maxStep,omitempty\"  // default \"500Gi\"
    Limit   string             json:\"limit,omitempty\"
}

type ResizeStrategy struct {
    Mode             ResizeMode       json:\"mode,omitempty\"             // default \"Standard\"
    MaxActionsPerDay int              json:\"maxActionsPerDay,omitempty\" // default 3
    WALSafetyPolicy  *WALSafetyPolicy json:\"walSafetyPolicy,omitempty\"
}

type WALSafetyPolicy struct {
    AcknowledgeWALRisk    bool   json:\"acknowledgeWALRisk,omitempty\"
    RequireArchiveHealthy *bool  json:\"requireArchiveHealthy,omitempty\" // default true
    MaxPendingWALFiles    *int   json:\"maxPendingWALFiles,omitempty\"    // default 100
    MaxSlotRetentionBytes *int64 json:\"maxSlotRetentionBytes,omitempty\"
    AlertOnResize         *bool  json:\"alertOnResize,omitempty\"         // default true
}

Add Resize *ResizeConfiguration to StorageConfiguration.

Tasks (in order):
- [ ] Add all API types above to api/v1/cluster_types.go with kubebuilder markers.
- [ ] Run make generate && make manifests to regenerate CRD and deepcopy.
- [ ] Add ClusterDiskStatus, InstanceDiskStatus, VolumeDiskStatus, WALHealthInfo,
      AutoResizeEvent to cluster status types.
- [ ] Implement expansion clamping logic in pkg/reconciler/autoresize/clamping.go:
      raw_step = current_size * (step_percent / 100)
      clamped_step = max(minStep, min(raw_step, maxStep))
      new_size = min(current_size + clamped_step, limit)
      When step is absolute, ignore minStep/maxStep.
- [ ] Implement trigger evaluation in pkg/reconciler/autoresize/triggers.go:
      Fires when usage > usageThreshold OR available < minAvailable.
      Either condition alone is sufficient. Both set = more protective wins.
- [ ] Implement rate-limit budget tracker in pkg/reconciler/autoresize/ratelimit.go:
      Track resize events per volume in 24h rolling window.
      Block when maxActionsPerDay exhausted. Emit resize_blocked{reason=\"rate_limit\"}.
- [ ] Implement autoresize.Reconciler in pkg/reconciler/autoresize/reconciler.go:
      Called from cluster controller reconciliation loop.
      Decision flow: trigger check -> budget check -> limit check -> WAL safety -> clamp -> patch PVC -> event.
      RequeueAfter 30s.
- [ ] Unit tests for clamping (%, absolute, minStep, maxStep, limit cap),
      trigger evaluation (%, absolute, both, neither),
      rate limiting (budget tracking, exhaustion, 24h window rollover).
- [ ] Verify: make generate && make manifests && make lint && make test pass.

Phase 2 gate: CRD generated, reconciler wired in, all clamping/trigger/ratelimit unit tests green.

## Phase 3: WAL Safety + Webhook Validation

Tasks (in order):
- [ ] Implement WAL safety evaluation in the reconciler:
      If WAL volume or single-volume cluster:
        Block if requireArchiveHealthy && !archiveHealthy.
        Block if pendingFiles > maxPendingWALFiles.
        Block if slotRetention > maxSlotRetentionBytes.
      Emit resize_blocked metric with appropriate reason label.
      Emit Kubernetes warning event.
- [ ] Add webhook validation in internal/webhook/:
      Single-volume: resize.enabled && no walStorage -> acknowledgeWALRisk required.
      UsageThreshold: 1-99.
      MinAvailable: valid resource.Quantity.
      Step: valid % or resource.Quantity (IntOrString).
      MinStep/MaxStep: valid quantities, minStep <= maxStep.
      Limit: valid resource.Quantity.
      MaxActionsPerDay: 0-10.
- [ ] Unit tests for WAL safety blocking (archive unhealthy, pending WAL, slot retention).
- [ ] Unit tests for webhook validation (all valid, all invalid, edge cases).
- [ ] Integration test: single-volume cluster without acknowledgeWALRisk -> webhook rejects.
- [ ] Verify: make lint && make test pass.

Phase 3 gate: WAL safety blocks resize correctly, webhook rejects invalid configs, all tests green.

## Phase 4: E2E Tests + Polish

E2E tests use Ginkgo label filters. All auto-resize E2E test specs MUST use the
Label(\"auto-resize\") decorator so they can be selected via FEATURE_TYPE=auto-resize.

Example spec:
  var _ = Describe(\"PVC auto-resize\", Label(\"auto-resize\"), func() { ... })

### Running E2E Tests

Build and push the controller image, then run the E2E suite on an AKS cluster:

  # Build the controller image
  make docker-build CONTROLLER_IMG=ghcr.io/jmealo/cloudnative-pg-testing:feat-pvc-autoresizing

  # Push to GHCR
  docker push ghcr.io/jmealo/cloudnative-pg-testing:feat-pvc-autoresizing

  # Run only auto-resize E2E tests on AKS
  TEST_CLOUD_VENDOR=aks FEATURE_TYPE=auto-resize \\
    CONTROLLER_IMG=ghcr.io/jmealo/cloudnative-pg-testing:feat-pvc-autoresizing \\
    hack/e2e/run-e2e.sh

FEATURE_TYPE=auto-resize maps to --label-filter \"auto-resize\" in Ginkgo,
so only specs with Label(\"auto-resize\") will run.

NOTE: These E2E tests require real PVC volume expansion, which is not supported
in Kind. You MUST use a cloud provider cluster (AKS) for E2E testing.
Local iteration is limited to unit tests (make test) and linting (make lint).

Tasks (in order):
- [ ] E2E test: basic data volume resize (fill to threshold, verify PVC expands).
- [ ] E2E test: basic WAL volume resize.
- [ ] E2E test: archive health blocks resize (misconfigure backup, fill WAL).
- [ ] E2E test: inactive slot blocks resize.
- [ ] E2E test: single-volume no-ack rejection (webhook).
- [ ] E2E test: single-volume with ack (resize succeeds).
- [ ] E2E test: expansion.limit enforcement.
- [ ] E2E test: rate-limit enforcement (maxActionsPerDay budget).
- [ ] E2E test: minStep clamping (small volume).
- [ ] E2E test: maxStep clamping (large volume).
- [ ] E2E test: tablespace resize.
- [ ] E2E test: metrics accuracy (all cnpg_disk_* and cnpg_wal_* exposed and correct).
- [ ] Build and push image, run E2E on AKS:
      make docker-build CONTROLLER_IMG=ghcr.io/jmealo/cloudnative-pg-testing:feat-pvc-autoresizing
      docker push ghcr.io/jmealo/cloudnative-pg-testing:feat-pvc-autoresizing
      TEST_CLOUD_VENDOR=aks FEATURE_TYPE=auto-resize CONTROLLER_IMG=ghcr.io/jmealo/cloudnative-pg-testing:feat-pvc-autoresizing hack/e2e/run-e2e.sh
- [ ] Verify: make lint && make test pass. E2E green on AKS.

Phase 4 gate: All E2E tests green on AKS. make lint && make test pass locally.

## Commit Convention

This project requires DCO sign-off on every commit. Use the -s flag.
Include a Co-Authored-By trailer.

Format:
  git commit -s -m \"$(cat <<'COMMITEOF'
  feat(component): description here

  Co-Authored-By: Claude Opus 4.5 <noreply@anthropic.com>
  COMMITEOF
  )\"

Examples:
  feat(disk): add statfs-based disk probe
  feat(metrics): register disk usage prometheus metrics
  feat(autoresize): add expansion clamping logic
  feat(webhook): add resize configuration validation
  test(autoresize): add clamping unit tests
  test(e2e): add basic data volume resize test

## Rules

- NEVER skip tests. If a test fails, fix it before moving on.
- NEVER leave linting errors. Before every commit run: make generate manifests fmt lint test
- Run make generate && make manifests after ANY change to api/v1/ types.
- ALWAYS use DCO sign-off: git commit -s. Commits without sign-off will be rejected.
- Follow existing CNPG code patterns. Read neighboring files before writing new ones.
- One logical change per commit. Do not bundle unrelated changes.
- If stuck on the same task for 3+ iterations, add a TODO comment explaining the blocker
  and move to the next task. Come back to it later.
- If ALL phases are complete and ALL tests pass, output: <promise>COMPLETE</promise>

## Completion Criteria

ALL of the following must be true:
- Phase 1: Metrics exposed on :9187, status endpoint extended, unit tests green.
- Phase 2: CRD generated, reconciler wired in, clamping/triggers/ratelimit tested.
- Phase 3: WAL safety blocks correctly, webhook validates all fields, tests green.
- Phase 4: All 12 E2E test scenarios passing on AKS.
- make lint returns zero errors.
- make test returns zero failures.
- you write idiomatic Go code following CNPG patterns.
- All commits have DCO sign-off.
- All commit messages follow the conventional commit format.
- All commit messages include Co-Authored-By trailers.
- Makefile targets: make generate, make manifests, make fmt run without errors.
- Controller image pushed to ghcr.io/jmealo/cloudnative-pg-testing:feat-pvc-autoresizing.
- E2E suite passes: TEST_CLOUD_VENDOR=aks FEATURE_TYPE=auto-resize.
- git log shows clean conventional commit history.

When ALL criteria are met, output: <promise>COMPLETE</promise>