# Review-Readiness Prompt: PVC Auto-Resize with WAL Safety

## Objective

Prepare the `feat/pvc-autoresizing-wal-safety` branch for submission as a PR to
`cloudnative-pg/cloudnative-pg`. The branch implements automatic PVC resizing
with WAL-aware safety checks. The code is functionally complete and E2E tested.

This prompt performs a **final audit, cleanup, and history rewrite** to ensure
the PR meets all CNPG project standards and will survive maintainer review.

---

## Prerequisites (User Must Complete Before Running This Prompt)

The following steps require network access and must be done manually:

```bash
git fetch upstream
```

If upstream is not configured:
```bash
git remote add upstream git@github.com:cloudnative-pg/cloudnative-pg.git
git fetch upstream
```

Verify `upstream/main` is available:
```bash
git rev-parse upstream/main
```

---

## Branch State

- **Branch:** `feat/pvc-autoresizing-wal-safety`
- **Base:** `main`
- **Commits:** ~62
- **Files changed:** ~75 (13,914 insertions, 213 deletions)
- **E2E status:** 11 passed, 0 failed, 2 pending (slot retention, archive health)

---

## Phase 1: Code Pattern Audit

Verify the implementation follows all CNPG conventions. Fix any issues found.
Do all fixes on the current branch BEFORE history rewrite (Phase 4).

### 1.1 Copyright Headers

Every new `.go` file must have the CNPG Apache 2.0 copyright header:

```go
/*
Copyright © contributors to CloudNativePG, established as
CloudNativePG a Series of LF Projects, LLC.
...
SPDX-License-Identifier: Apache-2.0
*/
```

Check:

```bash
for f in $(git diff --name-only upstream/main..HEAD -- '*.go'); do
  if [ -f "$f" ] && ! head -1 "$f" | grep -q "Copyright"; then
    echo "MISSING HEADER: $f"
  fi
done
```

Fix any missing headers by copying the header from an existing file in the same
package.

### 1.2 Import Organization

CNPG uses `gci` for import ordering (stdlib → external → internal). Run:

```bash
make fmt
```

If `git diff --name-only` shows changes after `make fmt`, those files had
formatting issues. Commit the fix:

```bash
git add -A && git commit -s -m "chore(autoresize): fix formatting"
```

### 1.3 Logging

All logging must use `github.com/cloudnative-pg/machinery/pkg/log`. Verify no
prohibited logging in non-test code:

```bash
# No fmt.Println/Printf
git diff upstream/main..HEAD -- '*.go' ':!*_test.go' | grep '^+' | grep 'fmt\.Print'

# No direct logr usage
git diff upstream/main..HEAD -- '*.go' ':!*_test.go' | grep '^+' | grep '"github.com/go-logr/logr"'

# No klog
git diff upstream/main..HEAD -- '*.go' | grep '^+' | grep '"k8s.io/klog'
```

All should return nothing. If they return matches, fix the code.

### 1.4 Error Handling

Errors must be wrapped with context. Check for bare returns:

```bash
git diff upstream/main..HEAD -- '*.go' ':!*_test.go' | grep '^+.*return err$'
```

Each match should be reviewed. Errors should generally be wrapped as
`fmt.Errorf("context: %w", err)` unless returning from a trivial one-line
delegation.

### 1.5 Generated Files

Verify generated files are up to date:

```bash
make generate
make manifests
```

If `git diff --name-only` shows changes, the generated files were stale.
Commit the update:

```bash
git add api/v1/zz_generated.deepcopy.go config/crd/ && \
git commit -s -m "chore(autoresize): regenerate deepcopy and CRDs"
```

### 1.6 Linter

```bash
make lint
```

Must pass with zero errors. The project uses golangci-lint with strict rules
including: `gocognit`, `gocyclo`, `gocritic`, `gosec`, `lll` (120 char line
limit), `nestif`, `dupl`, and many more.

If `make lint` reports issues, fix them and commit:

```bash
git add -A && git commit -s -m "chore(autoresize): fix lint issues"
```

### 1.7 Spell Check and Inclusive Language

```bash
make spellcheck
make woke
```

Fix any findings. These are enforced in CI.

### Verification Gate

```bash
make generate manifests fmt
git diff --name-only  # must be empty (no unstaged changes)

make lint             # must exit 0
make test             # must exit 0
make spellcheck       # must exit 0 (or note if unavailable)
make woke             # must exit 0 (or note if unavailable)
```

Paste full output.

---

## Phase 2: Content Review

### 2.1 Review Docs for Internal References

Search for references to AI tools, internal working documents, or debug markers:

```bash
grep -rinE 'TODO|FIXME|HACK|ralph|codex|claude|prompt|working doc' \
  docs/src/design/pvc-autoresize.md \
  docs/src/storage_autoresize.md \
  docs/src/storage.md
```

If any matches are found, remove them. `TODO` comments in code are acceptable
only if they reference a GitHub issue number (e.g., `// TODO(#5083): support shrink`).

### 2.2 Review Docs for Completeness

Read `docs/src/storage_autoresize.md` and verify it covers:
- Feature overview and what it does
- How to enable (`resize.enabled: true`)
- Configuration reference (triggers, expansion, strategy, WAL safety)
- Default values for all configurable fields
- Example YAML showing a complete configuration
- Metrics and monitoring
- Known limitations
- Interaction with manual resize

If any section is missing or incomplete, add it.

### 2.3 Review Code for Stale Debug Artifacts

```bash
git diff upstream/main..HEAD -- '*.go' | grep '^+' | \
  grep -iE 'debug|println|printf.*debug|log\.Info.*debug|temporary|TEMP[^L]|XXX'
```

Remove any debug logging. Production code should use appropriate log levels:
- `Info` for operational events
- `Debug` for troubleshooting detail
- `Warning` for degraded conditions
- `Error` for failures

### 2.4 Verify Pending E2E Tests Have Explanatory Comments

The 2 pending tests must have clear comments explaining why:

```bash
grep -A10 -B2 'Skip\|Pending' tests/e2e/auto_resize_test.go
```

Each `Skip`/`Pending` should have a comment explaining:
1. WHY the test is pending
2. WHERE the underlying logic is tested instead (unit tests)
3. WHAT would need to change to enable it

If comments are missing or unclear, add them.

### 2.5 Review Prometheus Rules

Read `docs/src/samples/monitoring/prometheusrule.yaml` and verify:
- Alert names follow existing naming conventions in the file
- Thresholds are reasonable
- Labels and annotations are consistent with existing alerts in the same file

### 2.6 Remove .gitignore Changes

The branch added `.mcp.json` and `*mcp*` to `.gitignore`. These are
development-environment-specific and must not be in this PR:

```bash
git checkout upstream/main -- .gitignore
git add .gitignore && git commit -s -m "chore: revert .gitignore changes"
```

### 2.7 Remove Internal Working Docs from Diff

Check if any internal development documents are in the diff:

```bash
git diff --name-only upstream/main..HEAD | grep -E \
  'e2e-requirements|e2e-testing|declarative-shrink|feature-request|ralph|finalize|polish|variant'
```

If any appear, remove them from the branch:

```bash
# For files that exist on upstream/main, restore them:
git checkout upstream/main -- <file>

# For files that are NEW (not on main), delete them:
git rm <file>

git commit -s -m "chore: remove internal working documents"
```

### 2.8 Remove Upstream Noise from Diff

Check for files that changed only because of upstream divergence, not the feature:

```bash
git diff --name-only upstream/main..HEAD | grep -E \
  '\.github/workflows/codeql|\.github/workflows/ossf|\.github/workflows/snyk'
```

If any appear, restore them to upstream state:

```bash
git checkout upstream/main -- \
  .github/workflows/codeql-analysis.yml \
  .github/workflows/ossf_scorecard.yml \
  .github/workflows/snyk.yml \
  2>/dev/null
git add -A && git diff --cached --quiet || \
  git commit -s -m "chore: revert unrelated workflow changes"
```

### Verification Gate

```bash
# No stale artifacts in docs
grep -rinE 'TODO|FIXME|HACK|ralph|codex|claude|prompt|working doc' \
  docs/src/design/pvc-autoresize.md docs/src/storage_autoresize.md 2>/dev/null
# Should return nothing (or only issue-referenced TODOs)

# No files outside standard source tree
git diff --name-only upstream/main..HEAD | \
  grep -vE '^(api/|config/|docs/|hack/|internal/|pkg/|tests/|cmd/)' | grep -v '^$'
# Should return nothing
```

Paste full output.

---

## Phase 3: Test Verification

Run the full test and build suite to confirm everything is green.

### 3.1 Unit Tests

```bash
make test
```

### 3.2 Build

```bash
make build
```

Must succeed for all three binaries (manager, instance-manager, kubectl-cnpg).

### 3.3 Full Checks (if available)

```bash
make checks
```

This runs: `go-mod-check`, `generate`, `manifests`, `apidoc`, `fmt`,
`spellcheck`, `woke`, `vet`, `lint`, `govulncheck`.

If `make checks` is not available or has infrastructure dependencies that are
not met, run individually:

```bash
make generate manifests fmt vet lint test
```

### Verification Gate

All commands exit 0. Paste full output.

---

## Phase 4: History Rewrite (Scripted Squash)

The branch has many iterative commits. CNPG uses **linear history** and
reviewers will read every commit. Squash into clean, logical units.

**IMPORTANT:** This phase uses `git reset --soft` (not interactive rebase).
All operations are non-interactive.

### 4.1 Save Current State

```bash
# Record the current tip for safety
CURRENT_TIP=$(git rev-parse HEAD)
echo "Saved tip: $CURRENT_TIP"

# Verify all changes are committed
git status --porcelain
# Must be empty
```

### 4.2 Soft Reset to upstream/main

This collapses all feature commits into a single set of staged changes:

```bash
git reset --soft upstream/main
```

After this, `git status` will show all feature files as staged. The working
tree is untouched — no code is lost.

### 4.3 Create Clean Commits

Unstage everything, then selectively stage and commit in logical groups.

```bash
git reset HEAD
```

Now all changes are unstaged. Create commits in this order:

**Commit 1: API types**

```bash
git add \
  api/v1/cluster_types.go \
  api/v1/zz_generated.deepcopy.go \
  config/crd/

git commit -s -m "$(cat <<'EOF'
feat(autoresize): add API types for automatic PVC resizing

Define the StorageConfiguration.Resize field and all supporting types:
ResizeConfiguration, ResizeTriggers, ExpansionPolicy, ResizeStrategy,
WALSafetyPolicy, ClusterDiskStatus, and AutoResizeEvent.

The API supports threshold-based triggers (usageThreshold and minAvailable),
configurable expansion policy with percentage/absolute steps and min/max
clamping, rate limiting via maxActionsPerDay, and WAL safety checks for
archive health and replication slot retention.

Signed-off-by: Jeff Mealo <jmealo@protonmail.com>
EOF
)"
```

**Commit 2: Disk probing and WAL health monitoring**

```bash
git add \
  pkg/management/postgres/disk/ \
  pkg/management/postgres/wal/ \
  pkg/management/postgres/disk_status.go \
  pkg/management/postgres/instance.go \
  pkg/management/postgres/probes.go \
  pkg/management/postgres/webserver/metricserver/disk.go \
  pkg/management/postgres/webserver/metricserver/pg_collector.go \
  pkg/management/postgres/webserver/remote.go \
  pkg/postgres/status.go

git commit -s -m "$(cat <<'EOF'
feat(autoresize): add disk probing and WAL health monitoring

Add instance-level disk usage probing that collects filesystem statistics
(total, used, available, percent used) for data, WAL, and tablespace
volumes. Report disk status and WAL health (archive health, pending WAL
files, inactive replication slot retention) through the instance status
endpoint.

Expose Prometheus metrics for disk usage per volume type so operators can
alert on storage pressure independently of auto-resize thresholds.

Signed-off-by: Jeff Mealo <jmealo@protonmail.com>
EOF
)"
```

**Commit 3: Auto-resize reconciler**

```bash
git add \
  pkg/reconciler/autoresize/

git commit -s -m "$(cat <<'EOF'
feat(autoresize): add resize reconciler with WAL safety and rate limiting

Implement the core auto-resize reconciler that evaluates each PVC for
expansion eligibility on every reconciliation cycle.

- Evaluate usageThreshold and minAvailable triggers per PVC
- Calculate expansion step with percentage and absolute value support
- Clamp expansions to minStep/maxStep bounds and enforce expansion limit
- Rate-limit resize operations per 24h rolling window (maxActionsPerDay)
- Check WAL archive health and replication slot retention before resize
- Require explicit acknowledgeWALRisk for single-volume clusters
- Record resize events in cluster status for budget tracking and audit

The reconciler is fail-open when WAL health data is unavailable (with a
warning event) and fail-closed when health data indicates problems.

Signed-off-by: Jeff Mealo <jmealo@protonmail.com>
EOF
)"
```

**Commit 4: Webhook validation and controller wiring**

```bash
git add \
  internal/webhook/v1/cluster_webhook.go \
  internal/controller/cluster_controller.go \
  internal/controller/cluster_status.go

git commit -s -m "$(cat <<'EOF'
feat(autoresize): add webhook validation and controller wiring

Add webhook validation for the ResizeConfiguration fields: mutually
valid trigger combinations, parseable quantity values, step/limit
consistency, and WAL safety policy constraints.

Wire the auto-resize reconciler into the main cluster reconciliation
loop. Disk status from instance manager reports is passed through to
the reconciler. Status mutations (resize events) are persisted via
optimistic-lock patch after each cycle.

Signed-off-by: Jeff Mealo <jmealo@protonmail.com>
EOF
)"
```

**Commit 5: kubectl-cnpg disk status command**

```bash
git add \
  cmd/kubectl-cnpg/main.go \
  internal/cmd/plugin/disk/

git commit -s -m "$(cat <<'EOF'
feat(autoresize): add kubectl-cnpg disk status command

Add 'kubectl cnpg disk status' subcommand that displays per-instance
disk usage for data, WAL, and tablespace volumes. Shows total size,
used bytes, available bytes, and percent used in a tabular format.

Signed-off-by: Jeff Mealo <jmealo@protonmail.com>
EOF
)"
```

**Commit 6: Tests**

```bash
git add \
  internal/webhook/v1/cluster_webhook_autoresize_conflicts_test.go \
  internal/webhook/v1/cluster_webhook_autoresize_test.go \
  tests/ \
  hack/e2e/

git commit -s -m "$(cat <<'EOF'
test(autoresize): add unit and E2E tests for PVC auto-resize

Add comprehensive test coverage for the auto-resize feature:

Unit tests:
- Trigger evaluation (usageThreshold, minAvailable, edge cases)
- Expansion calculation and clamping (minStep, maxStep, limit)
- WAL safety policy evaluation (archive health, slot retention)
- Rate-limit budget tracking (HasBudget, rolling 24h window)
- Webhook validation (conflicts, configuration constraints)

E2E tests (11 passing, 2 pending):
- Basic auto-resize with usage threshold trigger
- minAvailable trigger
- Separate WAL volume resize
- Runtime WAL volume configuration
- Expansion limit enforcement
- Rate-limit enforcement
- minStep/maxStep clamping
- Prometheus metrics exposure
- Tablespace resize
- Webhook validation
- Pending: slot retention and archive health (covered by unit tests)

Signed-off-by: Jeff Mealo <jmealo@protonmail.com>
EOF
)"
```

**Commit 7: Documentation**

```bash
git add \
  docs/

git commit -s -m "$(cat <<'EOF'
docs(autoresize): add design RFC and user documentation

Add the design RFC (docs/src/design/pvc-autoresize.md) documenting the
auto-resize architecture, API design decisions, WAL safety model, and
interaction with manual resize and future declarative shrink.

Add user-facing documentation (docs/src/storage_autoresize.md) with
configuration reference, example YAML, default values, metrics, and
known limitations.

Update storage.md with cross-reference to auto-resize documentation.
Add sample Prometheus alerting rules for disk usage monitoring.

Signed-off-by: Jeff Mealo <jmealo@protonmail.com>
EOF
)"
```

### 4.4 Verify Nothing Was Lost

```bash
# Compare working tree to the saved tip
git diff $CURRENT_TIP --stat
# Should show NO differences (or only the cleanup commits from Phases 1-2)

# If there are unexpected differences, something went wrong.
# Recovery: git reset --hard $CURRENT_TIP
```

### 4.5 Handle Remaining Unstaged Files

After all commits, check if anything was missed:

```bash
git status --porcelain
```

If files remain:
- Feature-related files: add to the appropriate commit via `git add <file> && git commit --amend --no-edit -s`
- Non-feature files (working docs, prompts, etc.): discard with `git checkout -- <file>` or ignore (untracked)

### Verification Gate

```bash
# Exactly 7 commits
git rev-list --count upstream/main..HEAD
# Should be 7

# All commits have DCO
git log --format="%H %s" upstream/main..HEAD | while read hash msg; do
  if ! git log -1 --format="%B" "$hash" | grep -q "Signed-off-by:"; then
    echo "MISSING DCO: $hash $msg"
  fi
done
# Should return nothing

# All commits follow Conventional Commits
git log --oneline upstream/main..HEAD
# All should match: type(autoresize): description

# No debug/WIP in messages
git log --oneline upstream/main..HEAD | grep -iE 'debug|WIP|TODO|pending|fix.*fix'
# Should return nothing

# Build still works after rewrite
make build
make test
```

Paste full output.

---

## Phase 5: Final Diff Review

Produce the final diff and verify cleanliness:

```bash
git diff --stat upstream/main..HEAD
```

### Checks

```bash
# No binary files in diff
git diff upstream/main..HEAD --numstat | awk '$1 == "-" && $2 == "-" {print $3}'
# Should return nothing

# No files outside standard source tree
git diff --name-only upstream/main..HEAD | \
  grep -vE '^(api/|config/|docs/|hack/|internal/|pkg/|tests/|cmd/)' | grep -v '^$'
# Should return nothing

# File count is reasonable
git diff --name-only upstream/main..HEAD | wc -l
# Should be ~55-65

# No go.mod/go.sum changes (should be clean after reset to upstream/main)
git diff --name-only upstream/main..HEAD | grep -E '^go\.(mod|sum)$'
# Should return nothing (if it does, the dependency was actually needed — verify)
```

### Verification Gate

All checks pass. Paste full output.

---

## Phase 6: PR Description

Generate the PR title and description. Output these so they can be copy-pasted.

### PR Title

```
feat(autoresize): automatic PVC resizing with WAL-aware safety checks
```

### PR Description

```markdown
## Summary

Implements automatic PVC resizing for CloudNativePG clusters. When enabled,
the operator monitors disk usage and triggers PVC expansion when configured
thresholds are reached, with safety checks for WAL archiving and replication
slot health.

### Key capabilities

- **Threshold-based triggers**: usageThreshold (percentage) and minAvailable (absolute)
- **Configurable expansion**: percentage or absolute step, with min/max clamping and limit cap
- **Rate limiting**: maxActionsPerDay per volume (default 3, matching cloud provider limits)
- **WAL safety**: blocks resize when archiving is unhealthy or inactive replication slots retain excessive WAL
- **Single-volume safety**: requires explicit acknowledgeWALRisk for clusters without separate WAL volume
- **Observability**: Prometheus metrics, Kubernetes events, cluster status with resize history
- **kubectl plugin**: `kubectl cnpg disk status` for disk usage visibility

### Configuration example

    spec:
      storage:
        size: 10Gi
        resize:
          enabled: true
          triggers:
            usageThreshold: 80
            minAvailable: 2Gi
          expansion:
            step: "20%"
            minStep: 2Gi
            maxStep: 500Gi
            limit: 1Ti
          strategy:
            maxActionsPerDay: 3
            walSafetyPolicy:
              requireArchiveHealthy: true
              maxPendingWALFiles: 100

### Design decisions

- Auto-resize does NOT update `spec.storage.size` — PVCs grow but the
  declarative spec remains unchanged, preserving GitOps compatibility
- WAL safety is fail-open when health data is unavailable (with warning event)
  and fail-closed when health data indicates problems
- Single-volume clusters (data+WAL on same PVC) require explicit
  `acknowledgeWALRisk: true` because resize can mask WAL-related issues

### Test plan

- [x] Unit tests for all reconciler logic (triggers, clamping, WAL safety, budget)
- [x] Webhook validation tests (conflicts, configuration validation)
- [x] E2E: basic auto-resize with usage threshold trigger
- [x] E2E: minAvailable trigger
- [x] E2E: separate WAL volume resize
- [x] E2E: runtime WAL volume configuration
- [x] E2E: expansion limit enforcement
- [x] E2E: rate-limit enforcement
- [x] E2E: minStep/maxStep clamping
- [x] E2E: Prometheus metrics exposure
- [x] E2E: tablespace resize
- [x] E2E: webhook validation

### Related issues

- Closes #9928
- Closes #9927
- Related: #1808, #5083
```

---

## Iteration Protocol

After completing each phase:

1. Run the phase's verification gate commands.
2. Paste the **full terminal output** here.
3. If any check fails, fix it before moving to the next phase.
4. Do not skip phases.
5. Do NOT use interactive commands (`git rebase -i`, editors, `less`, etc.).
   All operations must be non-interactive.
6. Do NOT attempt network operations (`git fetch`, `git push`, `git pull`).
   The user will handle these.

If a phase fails after 3 attempts, stop and report what is broken.
