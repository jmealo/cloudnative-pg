# Ralph E2E Testing Prompt: Dynamic Storage on Azure AKS

## Objective

Run E2E tests for the dynamic storage feature on Azure AKS and iterate until all tests pass. Follow CloudNativePG contribution guidelines and make necessary fixes to ensure test success.

## Context

- **Branch**: `feat/dynamic-storage-complete`
- **Feature**: Convergent Dynamic Storage Management
- **Test Suite**: `tests/e2e/dynamic_storage_test.go` (19 test cases)
- **Infrastructure**: Azure AKS with Premium SSD (supports online volume expansion)
- **Test Runner**: `hack/e2e/run-aks-e2e.sh`

## Prerequisites (User Completes Before Starting)

### 1. Azure Authentication

```bash
# Login to Azure
az login

# Set subscription
az account set --subscription <your-subscription-id>

# Verify
az account show
```

### 2. Environment Variables

```bash
# Required: Image registry (should already be configured from previous work)
export CONTROLLER_IMG_BASE=${CONTROLLER_IMG_BASE:-ghcr.io/jmealo/cloudnative-pg-testing}

# Optional: Customize test run
export GINKGO_NODES=${GINKGO_NODES:-6}  # Parallel execution (capped by node count)
export GINKGO_TIMEOUT=${GINKGO_TIMEOUT:-3h}
export TEST_CLOUD_VENDOR=aks

# Namespace isolation (for running both repos on same cluster)
# This repo (gemini) uses separate namespaces to avoid conflicts
export OPERATOR_NAMESPACE=${OPERATOR_NAMESPACE:-cnpg-system-gemini}
export MINIO_NAMESPACE=${MINIO_NAMESPACE:-minio-gemini}
export TEST_NAMESPACE_PREFIX=${TEST_NAMESPACE_PREFIX:-ds-gemini}
```

### 2.1 Running Both Repos Simultaneously

To run tests from both `cloudnative-pg` and `cloudnative-pg-gemini` on the same AKS cluster:

| Repo | OPERATOR_NAMESPACE | MINIO_NAMESPACE | TEST_NAMESPACE_PREFIX |
|------|-------------------|-----------------|----------------------|
| cloudnative-pg | `cnpg-system` (default) | `minio` | `dynamic-storage` |
| cloudnative-pg-gemini | `cnpg-system-gemini` | `minio-gemini` | `ds-gemini` |

**Important**: Each repo deploys its own operator. Both can run simultaneously without conflicts.

### 3. Git Status Verification

```bash
# Ensure we're on the correct branch
git status
git log --oneline -5

# Expected latest commits:
# - docs: add design documents and user documentation for dynamic storage
# - test(dynamicstorage): add E2E tests and test infrastructure
# - feat(dynamicstorage): implement convergent dynamic storage management
# - test(dynamicstorage): add test for requeue behavior when disk status unavailable
# - fix(dynamicstorage): add retry logic when disk status unavailable
```

---

## Phase 1: Initial E2E Test Run (Parallel Mode)

### Strategy: Fix While Tests Run

E2E tests take 2-3 hours. **Do NOT wait for completion.** Run tests in background and fix failures as they appear. This maximizes efficiency by parallelizing test execution with debugging.

### 1.1 Start Tests in Background

```bash
# From repo root
cd /Users/jmealo/repos/cloudnative-pg-gemini

# Start tests in background with live output to file
LOG_FILE="/tmp/e2e-run-$(date +%Y%m%d-%H%M%S).log"
./hack/e2e/run-aks-e2e.sh 2>&1 | tee "$LOG_FILE" &
E2E_PID=$!
echo "Tests running as PID $E2E_PID, logging to $LOG_FILE"
```

**What this does:**
1. Builds the operator Docker image with current code
2. Pushes image to ghcr.io registry
3. Deploys operator to AKS cluster
4. Runs all tests labeled with `dynamic-storage` or `dynamic-storage-p0` or `dynamic-storage-p1`
5. Captures test output and diagnostics

### 1.2 Monitor for Failures (Parallel Fixing)

While tests run, monitor for failures and spawn sub-agents to investigate:

```bash
# Check for failures in real-time (run periodically)
tail -100 "$LOG_FILE" | grep -E "(FAIL|Error|panic|\[FAILED\])"

# Check JSON report if available (updated after each spec)
jq -r '.[] | select(.SpecReports != null) | .SpecReports[] | select(.State == "failed") | .LeafNodeText' tests/e2e/out/dynamic_storage_report.json 2>/dev/null
```

**When a failure is detected:**

1. **Identify the failing test** - Note the test name, error message, stack trace
2. **Spawn a sub-agent** using the Task tool to investigate:

```
Use Task tool with:
- subagent_type: "debugger" or "golang-pro"
- prompt: Include:
  - The failing test name and error message
  - Instructions to:
    1. Read AI_CONTRIBUTING.md for CNPG standards
    2. Analyze the failure (test code in tests/e2e/, operator code)
    3. Propose a minimal fix following CNPG patterns
    4. Do NOT commit - just propose the fix and explain why
```

3. **Review sub-agent output** and apply fix if correct
4. **Continue monitoring** for more failures

### 1.3 Parallel Sub-Agent Guidelines

| Sub-Agent Task | Agent Type | Key Instructions |
|----------------|------------|------------------|
| Analyze test failure | `debugger` | Read test, find root cause, suggest fix |
| Fix Go code bug | `golang-pro` | Follow CNPG error/logging patterns |
| Fix timing issue | `debugger` | Analyze timeouts, suggest retry logic |
| Gather K8s context | `Bash` agent | Get pod logs, events, PVC status |

**Critical Rules:**
- Sub-agents **analyze and propose**, not commit
- Main agent integrates fixes and commits with proper sign-off
- One sub-agent per distinct failure to parallelize
- Share context: test name, error, relevant file paths

### 1.4 Apply Fixes Without Blocking Tests

While tests continue running:

```bash
# Apply proposed fixes (edit files as needed)

# Run quick validation
make fmt
make lint

# Stage but don't commit yet - batch commits after test run
git add -A
```

### 1.5 Wait for Test Completion

```bash
# Wait for background test to finish
wait $E2E_PID
EXIT_CODE=$?
echo "Tests completed with exit code: $EXIT_CODE"
```

### 1.6 Expected Duration

- Build + Push: 10-15 minutes
- Deploy: 5 minutes
- Test Execution: 2-3 hours (19 tests, some are Serial/Disruptive)
- **With parallel fixing**: Fixes are ready when tests complete

### 1.7 Sequential Fallback Mode

If parallel fixing is too complex or you prefer sequential:

```bash
# Run full suite (wait for completion)
./hack/e2e/run-aks-e2e.sh 2>&1 | tee /tmp/e2e-run-$(date +%Y%m%d-%H%M%S).log
echo "Exit code: $?"
```

Then proceed to Phase 2 for analysis.

### 1.8 Monitor Progress

Watch for output sections:
- `=== Build Stage ===`
- `=== Push Stage ===`
- `=== Deploy Stage ===`
- `=== Pre-flight Checks ===`
- `=== Test Execution ===`
- Test results with ✓ (pass) or ✗ (fail)

---

## Phase 2: Analyze Test Results

### 2.1 Capture Test Output

The script should produce a summary like:

```
Tests Run: 19
Passed: X
Failed: Y

Failed Tests:
- Test name 1: <reason>
- Test name 2: <reason>
```

### 2.2 Diagnostic Commands (if tests fail)

The script includes diagnostic helpers. If tests fail, run:

```bash
# Diagnose volume attachments and cluster state
./hack/e2e/run-aks-e2e.sh --diagnose-only

# Check operator logs
kubectl logs -n cnpg-system -l app.kubernetes.io/name=cloudnative-pg --tail=200

# Check test namespace resources
kubectl get all,pvc,clusters -n <test-namespace>

# Check cluster status
kubectl get clusters.postgresql.cnpg.io -n <test-namespace> -o yaml
```

### 2.3 Common Failure Patterns

| Symptom | Likely Cause | Investigation Steps |
|---------|--------------|---------------------|
| Timeout waiting for PVC growth | Disk status still not available, or reconciler not running | Check operator logs, verify reconciler is called, check instance status |
| PVC stuck in "Resizing" | Azure CSI driver issue or node attachment problem | Check VolumeAttachment, node logs, Azure portal |
| Test setup fails | AKS cluster or storage class issue | Verify pre-flight checks, check storage class allows expansion |
| Instance not starting | Image pull or resource issue | Check pod events, describe pod |

---

## Phase 3: Iteration Loop (If Tests Fail)

### 3.0 Parallel vs Sequential Fixing

**Parallel Mode (Recommended):** If using parallel mode from Phase 1, you've already been fixing tests as they fail. After test completion:
1. Commit all staged fixes with proper sign-off
2. Re-run failed tests with `--focus` to verify fixes
3. Skip to Phase 4 if all tests pass

**Sequential Mode:** If you waited for tests to complete, follow the workflow below.

### 3.1 Debug Methodology

Follow this debugging workflow:

1. **Identify the failing test**
   - Read test failure output carefully
   - Note the test name and failure reason

2. **Examine logs**
   ```bash
   # Operator logs
   kubectl logs -n cnpg-system deployment/cloudnative-pg-controller-manager -c manager --tail=500 | grep -i "dynamic\|storage\|resize"

   # Instance logs
   kubectl logs -n <namespace> <pod-name> -c postgres --tail=100
   ```

3. **Check cluster status**
   ```bash
   kubectl get cluster -n <namespace> <cluster-name> -o jsonpath='{.status.storageSizing}' | jq
   ```

4. **Verify disk status**
   ```bash
   # Check if disk status is being reported
   kubectl get cluster -n <namespace> <cluster-name> -o yaml | grep -A10 "storageSizing"
   ```

### 3.2 Make Fixes

When you identify an issue:

1. **Edit the code** (most likely locations):
   - `pkg/reconciler/dynamicstorage/reconciler.go` - Main logic
   - `pkg/management/postgres/probes.go` - Disk status collection
   - `tests/e2e/dynamic_storage_test.go` - Test expectations
   - `tests/utils/timeouts/timeouts.go` - Timeout configuration

2. **Test locally if possible**:
   ```bash
   # Run unit tests
   make test

   # Run specific package tests
   go test ./pkg/reconciler/dynamicstorage/... -v
   ```

3. **Commit the fix**:
   ```bash
   git add <files>
   git commit -m "fix(dynamicstorage): <description>

   <detailed explanation>

   Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>"
   ```

### 3.3 Re-run Tests

After making fixes, re-run tests:

```bash
# Quick iteration: skip build if only test code changed
./hack/e2e/run-aks-e2e.sh --skip-build --skip-deploy

# Full rebuild if operator code changed
./hack/e2e/run-aks-e2e.sh

# Run specific failing test only (faster iteration)
./hack/e2e/run-aks-e2e.sh --skip-build --skip-deploy --focus "test name pattern"
```

**Examples:**
```bash
# Re-run just the normal growth test
./hack/e2e/run-aks-e2e.sh --skip-build --skip-deploy --focus "grows storage when usage exceeds"

# Re-run emergency and maintenance tests
./hack/e2e/run-aks-e2e.sh --skip-build --skip-deploy --focus "emergency|maintenance"
```

### 3.4 Iteration Guidelines

- **Maximum 10 iterations**: If tests don't pass after 10 attempts, stop and report blockers
- **Document each fix**: Every code change needs a clear commit message
- **Test incrementally**: Focus on failing tests, don't re-run everything if unnecessary
- **Check Azure quotas**: Some failures might be infrastructure-related (quota exhausted, node capacity, etc.)

---

## Phase 4: Success Criteria

### 4.1 All Tests Must Pass

```
Tests Run: 19
Passed: 19
Failed: 0
```

### 4.2 Test Coverage Matrix

All these must pass:

**Basic Tests (P0):**
- ✓ rejects invalid configurations
- ✓ provisions PVC at request size
- ✓ grows storage when usage exceeds target buffer
- ✓ respects limit and does not grow beyond it
- ✓ creates new replicas at effective size

**Maintenance & Emergency (P0):**
- ✓ queues growth when outside maintenance window
- ✓ grows immediately when critical threshold is reached

**Operational Tests (P1):**
- ✓ respects max actions per day budget
- ✓ resumes growth operation after operator pod restart
- ✓ resumes growth operation after primary pod restart
- ✓ continues growth safely after failover
- ✓ re-evaluates plan deterministically when spec changes
- ✓ recovers growth operation after node drain
- ✓ backup succeeds without deadlocking storage reconciliation
- ✓ creates new replica at effective operational size
- ✓ respects planned/emergency action budget and exposes exhaustion in status

**Topology Tests (P1):**
- ✓ handles dynamic sizing with no replicas (T1: instances=1)
- ✓ handles dynamic sizing with single replica (T2: instances=2)
- ✓ handles dynamic sizing with multiple replicas (T3: instances>=3)

### 4.3 Clean State

- No failing tests
- No timeout errors
- All clusters reach Ready state
- All PVCs successfully resized when expected
- Operator logs show no errors

---

## Phase 5: Final Verification

### 5.1 Full Test Suite

Once all tests pass individually, run the complete suite one final time:

```bash
# Final verification run
./hack/e2e/run-aks-e2e.sh
```

### 5.2 Documentation Update

If you made significant changes during iteration, update:

```bash
# Add any new troubleshooting sections
vim docs/src/storage_dynamic.md

# Update design docs if behavior changed
vim docs/src/design/dynamic-storage-meta-rfc.md

# Commit documentation updates
git add docs/
git commit -m "docs(dynamicstorage): update based on E2E testing insights"
```

### 5.3 Create Summary

Create a final summary document:

```bash
cat > E2E_TEST_SUMMARY.md <<'EOF'
# Dynamic Storage E2E Test Results

## Test Execution
- Date: $(date)
- Branch: feat/dynamic-storage-spec
- Commit: $(git rev-parse --short HEAD)
- AKS Cluster: [cluster details]

## Results
- Total Tests: 19
- Passed: 19
- Failed: 0
- Duration: [X hours Y minutes]

## Issues Found and Fixed
1. [Issue 1]: [Description and fix commit]
2. [Issue 2]: [Description and fix commit]
...

## Test Coverage
[List all passing tests with checkmarks]

## Conclusion
All E2E tests pass successfully. The dynamic storage feature is ready for review.
EOF

git add E2E_TEST_SUMMARY.md
git commit -m "test: add E2E test execution summary for dynamic storage"
```

---

## Important Notes

### CloudNativePG Contribution Guidelines

From `CONTRIBUTING.md`:
- Follow existing code patterns and style
- Write clear commit messages
- Include unit tests for all new code
- Document new features
- External contributors: maintainers handle comprehensive cloud E2E testing, but we're doing this with user permission

### Test Script Features

The `run-aks-e2e.sh` script provides:
- `--skip-build`: Skip Docker build (use when only test code changed)
- `--skip-deploy`: Skip operator deployment (use when only test code changed)
- `--focus <pattern>`: Run specific tests matching regex pattern
- `--diagnose-only`: Just show cluster state, don't run tests
- Automatic pre-flight checks (storage class validation, node availability)
- VictoriaLogs integration hints for log analysis

### Debugging Tools

If stuck, use:
```bash
# Describe all resources in test namespace
kubectl describe all -n <namespace>

# Get operator logs with timestamps
kubectl logs -n cnpg-system deployment/cloudnative-pg-controller-manager -c manager --timestamps --tail=500

# Check Azure Disk CSI driver
kubectl get csidrivers
kubectl get storageclasses -o yaml

# Check node status
kubectl get nodes -o wide
kubectl describe node <node-name>

# Check volume attachments
kubectl get volumeattachments
```

---

## Success Promise

When ALL tests pass (19/19), output:

<promise>ALL E2E TESTS PASS - DYNAMIC STORAGE FEATURE COMPLETE</promise>

Only output this promise when:
1. Full test suite runs successfully
2. All 19 tests pass
3. No timeout or infrastructure errors
4. Clean git state with all fixes committed
5. Summary document created

Do NOT output the promise if any tests fail or if you're unsure about the results.

---

## Emergency Stop Conditions

Stop immediately and report if:
1. **Azure quota exhausted**: Cannot create more resources
2. **Authentication failures**: Cannot push images or access AKS
3. **Infrastructure down**: AKS cluster unreachable
4. **Fundamental bug**: Issue requires major architectural change
5. **10 iterations exceeded**: Tests still failing after 10 fix attempts

In these cases, document the blocker clearly and ask for user intervention.

---

## Execution Checklist

Before starting:
- [ ] Azure authentication verified (`az account show`)
- [ ] On correct branch (`git status` shows `feat/dynamic-storage-spec`)
- [ ] Latest commits include all dynamic storage implementation
- [ ] Environment variables set (`CONTROLLER_IMG_BASE`, etc.)
- [ ] AKS cluster accessible (`kubectl cluster-info`)

During execution:
- [ ] Monitor build stage for errors
- [ ] Monitor push stage for authentication issues
- [ ] Monitor deploy stage for operator startup
- [ ] Monitor test execution for failures
- [ ] Capture full output to file for analysis

After each iteration:
- [ ] Analyze failure reasons carefully
- [ ] Make targeted fixes, not shotgun changes
- [ ] Test locally before rebuilding
- [ ] Commit with clear messages
- [ ] Document what was fixed and why

Final verification:
- [ ] All 19 tests pass
- [ ] No errors in operator logs
- [ ] All clusters reach Ready state
- [ ] Summary document created
- [ ] All fixes committed with good messages

---

Begin execution when user confirms readiness.
