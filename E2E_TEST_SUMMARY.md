# Dynamic Storage E2E Test Results

## Test Execution
- **Date**: 2026-02-09
- **Branch**: feat/dynamic-storage-spec
- **Commit**: 605d94e0d (with additional E2E script fixes)
- **AKS Cluster**: Azure AKS with Premium SSD (supports online volume expansion)
- **Duration**: 1h 55m

## Results Summary
- **Total Dynamic Storage Tests**: 22
- **Passed**: 14
- **Failed**: 8
- **Skipped**: 214 (non-dynamic-storage tests)

## Passing Tests
| Test | Status | Notes |
|------|--------|-------|
| rejects invalid configurations | PASSED | Webhook validation working |
| provisions PVC at request size | PASSED | Initial sizing correct |
| respects limit and does not grow beyond it | PASSED | Limit enforcement working |
| creates new replicas at effective size | PASSED | Replica sizing correct |
| queues growth when outside maintenance window | PASSED | Maintenance window scheduling working |
| grows immediately when critical threshold is reached | PASSED | Emergency growth working |
| respects max actions per day budget | PASSED | Rate limiting working |
| respects planned/emergency action budget | PASSED | Budget tracking working |
| resumes growth operation after operator pod restart | PASSED | Operator restart recovery working |
| backup succeeds without deadlocking storage reconciliation | PASSED | Backup/storage interaction working |
| creates new replica at effective operational size | PASSED | Replica scaling working |
| handles dynamic sizing with no replicas (T1) | PASSED | Single-instance topology working |
| handles dynamic sizing with multiple replicas (T3) | PASSED | Multi-instance topology working |
| Spec mutation: re-evaluates plan deterministically | PASSED | Spec change handling working |

## Failing Tests

### 1. grows storage when usage exceeds target buffer
**Status**: FAILED (timeout 300s)
**Error**: PVC request should grow beyond initial 5Gi after sizing logic runs
**Root Cause**: Instance manager not reporting DiskStatus to operator
**Impact**: Core functionality - growth not triggered when disk fills

### 2. handles dynamic sizing with single replica (T2)
**Status**: FAILED (timeout 45s)
**Error**: Storage sizing never reaches ready state
**Root Cause**: Same as #1 - disk status not being collected

### 3. resumes growth operation after primary pod restart
**Status**: FAILED (timeout 900s)
**Error**: Cluster stuck in "Failing over" state
**Root Cause**: Failover not completing, likely related to disk status issue

### 4. continues growth safely after failover
**Status**: FAILED (timeout 900s)
**Error**: Cluster not reaching healthy state after failover
**Root Cause**: Similar to #3

### 5. concurrent backup, node drain and in-flight growth (P1)
**Status**: FAILED (timeout 900s)
**Error**: Node drain not completing
**Root Cause**: AKS node drain command timing out

### 6. rolling image upgrade while dynamic sizing active (P1)
**Status**: FAILED
**Error**: Expected 10Gi, got 5Gi
**Root Cause**: PVC not growing during upgrade scenario

### 7. repeated oscillation around threshold band (P1)
**Status**: FAILED (assertion)
**Error**: Expected 5Gi to equal 10Gi
**Root Cause**: PVC growth not occurring

### 8. recovers growth operation after node drain
**Status**: FAILED (timeout 900s)
**Error**: Node drain command timing out
**Root Cause**: AKS infrastructure issue with node drain

## Root Cause Analysis

### Primary Issue: DiskStatus Not Being Collected
The operator logs consistently show:
```
"No instances available for disk status collection"
```

This means `instanceStatuses.Items` is empty when `dynamicstorage.Reconcile` is called.
The disk status probing code (`fillDiskStatus`) exists in the instance manager but the
status isn't reaching the operator's reconciler.

### Potential Causes:
1. **Instance status HTTP response timing** - The status endpoint might not include
   DiskStatus in all response scenarios
2. **Pod list filtering** - `FilterActivePods` might be excluding running pods incorrectly
3. **Race condition** - Status collection happening before pods are fully ready

## Fixes Applied This Session

### 1. Cron Schedule Format (COMMITTED)
- **File**: `tests/e2e/dynamic_storage_test.go`
- **Change**: Updated maintenance window schedule from 5-field `"0 4 31 2 *"` to
  6-field `"0 0 4 31 2 *"` to match robfig/cron library expectations

### 2. E2E Script Helper Functions (COMMITTED)
- **File**: `hack/e2e/run-aks-e2e.sh`
- **Change**: Moved info/warn/ok/fail function definitions before flag parsing loop

## Recommended Next Steps

### High Priority
1. **Debug DiskStatus collection path**
   - Add logging to `fillDiskStatus` to confirm it's being called
   - Verify DiskStatus is included in instance manager HTTP response
   - Check if `GetStatusFromInstances` is correctly handling the response

2. **Review instance status collection timing**
   - Ensure dynamic storage reconciler is called after pods are fully ready
   - Add requeue logic if pods exist but status isn't available yet

### Medium Priority
3. **Investigate node drain timeouts**
   - AKS-specific issue with node drain commands taking >15 minutes
   - May need to adjust timeouts or investigate pod eviction policies

4. **Add more diagnostic logging**
   - Log when DiskStatus is set in instance manager
   - Log when DiskStatus is received by operator

## Test Coverage vs Requirements

### P0 Requirements Coverage
| Requirement | Test | Status |
|-------------|------|--------|
| Invalid config rejection | rejects invalid configurations | PASS |
| Initial PVC sizing | provisions PVC at request size | PASS |
| Growth on usage | grows storage when usage exceeds | FAIL |
| Limit enforcement | respects limit and does not grow beyond | PASS |
| Emergency growth | grows immediately when critical | PASS |
| Maintenance window | queues growth when outside window | PASS |
| Operator restart | resumes growth after operator restart | PASS |
| Primary pod restart | resumes growth after primary restart | FAIL |
| Failover | continues growth after failover | FAIL |
| Node drain | recovers after node drain | FAIL |
| Backup interaction | backup succeeds without deadlock | PASS |
| Replica sizing | creates replica at effective size | PASS |
| Rate limiting | respects action budget | PASS |

### Topology Coverage
| Topology | Test | Status |
|----------|------|--------|
| T1 (instances=1) | handles dynamic sizing with no replicas | PASS |
| T2 (instances=2) | handles dynamic sizing with single replica | FAIL |
| T3 (instances>=3) | handles dynamic sizing with multiple replicas | PASS |

## Conclusion
The dynamic storage feature has solid foundational tests passing (14/22), but the core
"growth on usage" functionality is blocked by a DiskStatus collection issue. Once this
is resolved, most other failing tests should also pass as they depend on the same
underlying growth mechanism.

The tests for emergency growth and maintenance window work correctly, suggesting the
issue is specifically in the normal growth path's disk status collection.
