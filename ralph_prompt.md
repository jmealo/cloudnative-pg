You are implementing the "Automatic PVC Resizing" feature for CloudNativePG.
Ref: docs/src/design/pvc-autoresize.md

## Project Context
- Repo: cloudnative-pg/cloudnative-pg (Fork)
- Stack: Go, Controller Runtime, Kubebuilder, Ginkgo
- Constraints: strict linting, DCO sign-off required

## The Loop (What you do on every iteration)
1. Git Check: `git log --oneline -5 && git diff --stat HEAD~1`
2. Quality Check: `make lint` and `make test`.
3. Plan: Read the Phase list below. Identify the current phase. Pick the NEXT single task.
4. Execute: Implement the task.
5. Verify: Run `make generate manifests fmt lint test` BEFORE committing.
6. Commit: `git commit -s -m "feat(scope): message"`.
   * If tests fail, DO NOT COMMIT. Fix them first.

## Phase 1: Metrics Foundation
1. [ ] Create `pkg/management/postgres/disk/probe.go`:
   - Implement `disk.Probe` using `statfs`.
   - Return `VolumeStats` (Total, Used, Available, Inodes).
2. [ ] Update `pkg/management/postgres/webserver/metric.go`:
   - Register Prometheus metrics: `cnpg_disk_*` and `cnpg_wal_*`.
3. [ ] Update `pkg/management/postgres/webserver/status.go`:
   - Add `DiskStatus` to `/pg/status` JSON response.
4. [ ] Test: Unit tests for Probe (mock statfs) and Metrics.

## Phase 2: Core Logic (Behavior-Driven)
1. [ ] API: Add structs to `api/v1/cluster_types.go`:
   - `ResizeConfiguration`, `ResizeTriggers`, `ExpansionPolicy`, `ResizeStrategy`.
   - Run `make generate && make manifests`.
2. [ ] Clamping Logic (`pkg/reconciler/autoresize/clamping.go`):
   - Func `CalculateNewSize(current, step, minStep, maxStep, limit)`.
   - Rule: If step is %, clamp result between minStep and maxStep.
   - Rule: If step is absolute, ignore min/max clamps.
   - Rule: Never exceed Limit.
3. [ ] Trigger Logic (`pkg/reconciler/autoresize/triggers.go`):
   - Func `ShouldResize(usage, threshold, available, minAvailable)`.
   - Rule: Resize if (Usage > Threshold) OR (Available < MinAvailable).
4. [ ] Rate Limiting (`pkg/reconciler/autoresize/ratelimit.go`):
   - Track actions per volume in 24h window. Block if budget exhausted.
5. [ ] Reconciler (`pkg/reconciler/autoresize/reconciler.go`):
   - Wire it all together. RequeueAfter 30s.

## Phase 3: WAL Safety
1. [ ] Safety Checks:
   - Block if Archive Unhealthy (`RequireArchiveHealthy`).
   - Block if Pending WAL > `MaxPendingWALFiles`.
   - Block if Slot Retention > `MaxSlotRetentionBytes`.
2. [ ] Webhook (`internal/webhook/`):
   - Validation: `Resize.Enabled=true` on single-volume requires `AcknowledgeWALRisk=true`.

## Completion
Output exactly: <promise>COMPLETE</promise> only when all phases are done and tests pass.
