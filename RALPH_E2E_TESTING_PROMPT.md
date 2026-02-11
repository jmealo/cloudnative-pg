# Ralph E2E Testing Prompt: Dynamic Storage on Azure AKS

Use with `/ralph-loop:ralph-loop` for deterministic, fast-feedback iterations.

## Objective

Get dynamic-storage E2E to green on AKS with minimal loop latency, then run one final full-suite verification.

## Completion Promise

Output this exact line only when done:

`ALL E2E TESTS PASS - DYNAMIC STORAGE FEATURE COMPLETE`

Do not output it early.

## Operating Mode

- Optimize for loop speed: one failing behavior at a time.
- Prefer `--fail-fast` + `--focus` in development loops.
- Do not run long background workflows as the primary strategy.
- Do not wait for a 2-3 hour full suite unless focused gates are already green.

## Preconditions

Verify before first loop:

```bash
az account show
kubectl cluster-info
git status --short
```

Set/confirm env:

```bash
export CONTROLLER_IMG_BASE=${CONTROLLER_IMG_BASE:-ghcr.io/jmealo/cloudnative-pg-testing}
export TEST_CLOUD_VENDOR=aks
export OPERATOR_NAMESPACE=${OPERATOR_NAMESPACE:-cnpg-system-gemini}
export MINIO_NAMESPACE=${MINIO_NAMESPACE:-minio-gemini}
export TEST_NAMESPACE_PREFIX=${TEST_NAMESPACE_PREFIX:-ds-gemini}
```

### Running Both Repos on Same Cluster

| Repo | OPERATOR_NAMESPACE | MINIO_NAMESPACE | TEST_NAMESPACE_PREFIX |
|------|-------------------|-----------------|----------------------|
| cloudnative-pg | `cnpg-system` (default) | `minio` | `dynamic-storage` |
| cloudnative-pg-gemini | `cnpg-system-gemini` | `minio-gemini` | `ds-gemini` |

Each repo deploys its own operator. Both run simultaneously without conflicts.

---

## Loop Algorithm

### Iteration 0 (Bootstrap)

Run one broad, fail-fast gate to surface the first blocker:

```bash
./hack/e2e/run-aks-e2e.sh --fail-fast --focus "dynamic storage|grows|maintenance|emergency|replica|failover|node drain"
```

### Iteration N (Primary Loop)

1. Parse first failing spec from `tests/e2e/out/dynamic_storage_report.json`.
2. Fix only that failure (smallest safe patch).
3. Re-run focused fail-fast:

```bash
./hack/e2e/run-aks-e2e.sh --skip-build --skip-deploy --fail-fast --focus "<failing test regex>"
```

4. If focused gate passes, widen focus to next gate.
5. Repeat until all gates pass.

**When operator code changes** (not just test code), drop `--skip-build --skip-deploy`.

### Gate Order

Advance only when current gate is green.

1. Validation + basic growth
2. Maintenance + emergency
3. Operational disruptions (restart/failover/node-drain/backup)
4. Topology (T1/T2/T3)
5. Final full suite (no focus)

---

## Test Inventory (20 tests)

| # | Test Name | Gate | Priority |
|---|-----------|------|----------|
| 1 | rejects invalid configurations | 1 | P0 |
| 2 | provisions PVC at request size | 1 | P0 |
| 3 | grows storage when usage exceeds target buffer | 1 | P0 |
| 4 | respects limit and does not grow beyond it | 1 | P0 |
| 5 | creates new replicas at effective size | 1 | P0 |
| 6 | grows tablespace storage when usage exceeds target buffer | 1 | P0 |
| 7 | queues growth when outside maintenance window | 2 | P0 |
| 8 | grows immediately when critical threshold is reached | 2 | P0 |
| 9 | initializes max-actions budget counters | 2 | P1 |
| 10 | resumes growth operation after operator pod restart | 3 | P1 |
| 11 | resumes growth operation after primary pod restart | 3 | P1 |
| 12 | continues growth safely after failover | 3 | P1 |
| 13 | accepts storage spec mutations during growth without losing data | 3 | P1 |
| 14 | recovers growth operation after node drain | 3 | P1 |
| 15 | backup succeeds or fails clearly without deadlocking storage reconciliation | 3 | P1 |
| 16 | creates new replica at effective operational size | 3 | P1 |
| 17 | exposes planned/emergency budget split in status | 3 | P1 |
| 18 | handles dynamic sizing with no replicas | 4 | P1 |
| 19 | handles dynamic sizing with single replica | 4 | P1 |
| 20 | handles dynamic sizing with multiple replicas without unnecessary churn | 4 | P1 |

---

## Script Reference

### `hack/e2e/run-aks-e2e.sh`

| Flag | Effect |
|------|--------|
| `--skip-build` | Skip Docker build (use when only test code changed) |
| `--skip-deploy` | Skip operator deployment |
| `--fail-fast` | Stop on first failing spec |
| `--focus <regex>` | Run tests matching pattern |
| `--diagnose-only` | Show cluster state, don't run tests |

### `hack/e2e/monitor-dynamic-storage.sh`

For long/full runs only (not the primary dev loop):

```bash
LOG_FILE="/tmp/e2e-run-$(date +%Y%m%d-%H%M%S).log"
./hack/e2e/run-aks-e2e.sh 2>&1 | tee "$LOG_FILE" &
E2E_PID=$!
./hack/e2e/monitor-dynamic-storage.sh --log-file "$LOG_FILE" --pid "$E2E_PID"
```

---

## Diagnostics (When Failing)

### Quick Triage

```bash
# Operator logs
kubectl logs -n $OPERATOR_NAMESPACE deployment/cnpg-controller-manager -c manager --tail=500

# Resources in test namespace
kubectl get all,pvc,clusters -n <test-namespace>

# Cluster status (storageSizing)
kubectl get cluster -n <test-namespace> <cluster-name> -o yaml

# Volume attachments and cluster state
./hack/e2e/run-aks-e2e.sh --diagnose-only
```

### Extracting Failed Tests from Report

```bash
jq -r '.[] | select(.SpecReports) | .SpecReports[] | select(.State == "failed") | .LeafNodeText' \
  tests/e2e/out/dynamic_storage_report.json
```

### Common Failure Patterns

| Symptom | Likely Cause | Action |
|---------|-------------|--------|
| Timeout waiting for PVC growth | DiskStatus not collected, reconciler not running | Check operator logs for "No instances available for disk status" |
| PVC stuck in "Resizing" | Azure CSI driver or node attachment issue | Check VolumeAttachment, Azure portal |
| Test setup fails | AKS cluster or storage class misconfigured | Run `--diagnose-only`, verify `allowVolumeExpansion` |
| Instance not starting | Image pull or resource issue | `kubectl describe pod`, check events |
| Cluster stuck "Failing over" | Failover not completing, often related to disk fill | Check disk usage, pg_rewind space |
| Node drain timeout | AKS eviction policies or PDB issues | Adjust drain timeout, check PDBs |

### Key Source Files

| File | Purpose |
|------|---------|
| `pkg/reconciler/dynamicstorage/reconciler.go` | Main reconciler logic |
| `pkg/management/postgres/probes.go` | Disk status collection |
| `tests/e2e/dynamic_storage_test.go` | E2E test definitions |
| `tests/utils/timeouts/timeouts.go` | Timeout configuration |

---

## Regression Ledger (Required)

After every E2E run, append to `E2E_TEST_SUMMARY.md`:

```markdown
## Run: <YYYY-MM-DD HH:MM UTC>
- Branch: <branch>
- Commit: <sha>
- Command: `<exact command>`
- Totals: Total=<n> Passed=<n> Failed=<n> Skipped=<n>

### Deltas vs previous run
- REGRESSION: <tests or none>
- FIXED: <tests or none>
- UNCHANGED FAILING: <tests or none>
```

---

## Final Verification (Only After All Gates Green)

```bash
./hack/e2e/run-aks-e2e.sh
```

Success criteria:

- Tests Run: 20
- Passed: 20
- Failed: 0
- No infra/auth errors blocking results
- `E2E_TEST_SUMMARY.md` updated with final run and deltas

---

## Per-Iteration Output Contract

At the end of each loop, report:

1. Command run
2. Pass/fail counts
3. First failing test (or `none`)
4. Root cause hypothesis
5. Exact files changed
6. Next command

If complete, output only:

`ALL E2E TESTS PASS - DYNAMIC STORAGE FEATURE COMPLETE`

---

## Emergency Stop Conditions

Stop immediately and report if:

1. **Azure quota exhausted**: Cannot create more resources
2. **Authentication failures**: Cannot push images or access AKS
3. **Infrastructure down**: AKS cluster unreachable
4. **Fundamental bug**: Issue requires major architectural change
5. **10 iterations exceeded**: Tests still failing after 10 fix attempts
