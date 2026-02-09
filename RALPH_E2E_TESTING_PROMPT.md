# Ralph E2E Testing Prompt: Dynamic Storage on Azure AKS

## Objective

Run E2E tests for the dynamic storage feature on Azure AKS and iterate until all tests pass. Follow CloudNativePG contribution guidelines and make necessary fixes to ensure test success.

## Context

- **Branch**: `feat/dynamic-storage-spec`
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
export GINKGO_NODES=${GINKGO_NODES:-1}  # Sequential for dynamic storage
export GINKGO_TIMEOUT=${GINKGO_TIMEOUT:-3h}
export TEST_CLOUD_VENDOR=aks
```

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

## Phase 1: Initial E2E Test Run

### 1.1 Full Test Execution

Run the complete dynamic storage E2E test suite:

```bash
# From repo root
cd /Users/jmealo/repos/cloudnative-pg-gemini

# Run all dynamic storage tests
./hack/e2e/run-aks-e2e.sh
```

**What this does:**
1. Builds the operator Docker image with current code
2. Pushes image to ghcr.io registry
3. Deploys operator to AKS cluster
4. Runs all tests labeled with `dynamic-storage` or `dynamic-storage-p0` or `dynamic-storage-p1`
5. Captures test output and diagnostics

### 1.2 Expected Duration

- Build + Push: 10-15 minutes
- Deploy: 5 minutes
- Test Execution: 2-3 hours (19 tests, some are Serial/Disruptive)

### 1.3 Monitor Progress

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
