# Fixes: #7793 Resolve stuck reconciliation loop for failed snapshot recovery jobs

## Problem

This PR addresses a critical issue where CloudNativePG clusters get stuck in an infinite reconciliation loop when scaling operations fail due to snapshot recovery job failures. The cluster remains stuck in the "Creating a new replica" phase indefinitely, preventing any further scaling operations and requiring manual intervention to recover.

### Issue Details
- When scaling up a cluster, if the snapshot recovery job fails (e.g., due to missing PVCs/unscheduleable), the controller gets stuck
- The cluster status remains in "Creating a new replica" even after manually deleting the failed job
- **Even when the cluster already has the correct number of replicas**, it remains stuck in the scaling phase
- Manual patching of the cluster status is required to recover
- This can cause significant operational issues in production environments

## Solution

This implementation adds intelligent stuck job detection and recovery mechanisms to the CloudNativePG operator:

### Key Features

#### 1. **Instance Count Validation**
- **Checks if the cluster already has the correct number of replicas before continuing stuck scaling operations**
- Automatically clears stuck scaling phases when the desired instance count is already met
- Prevents unnecessary scaling attempts when the cluster is already at the target size

#### 2. **Automatic Stuck Job Detection**
- Detects jobs stuck for more than 10 minutes without progress
- Identifies missing PVCs that prevent job execution
- Monitors job conditions and pod states with 5-second recheck intervals

#### 3. **Automatic Recovery**
- Safely deletes stuck jobs with proper owner references
- **Clears stuck "Creating a new replica" phases when cluster already has correct number of instances**
- Resets scaling phases after job deletion to allow operations to retry
- Returns appropriate reconcile results to trigger job recreation

#### 4. **Enhanced Observability**
- Detailed logging for troubleshooting with job metadata (age, active/succeeded/failed counts)
- Clear identification of stuck states and recovery actions
- Kubernetes events for failed/stuck job detection
- Informative phase reasons showing recovery status

## Implementation Details

### Core Changes

#### **New Job Management Utilities** (`pkg/utils/jobs.go`)
```go
// Core job state detection functions
- IsJobFailed() - Detects jobs with failed conditions
- IsJobStuck() - Identifies jobs pending for >10 minutes with no pods created  
- IsJobComplete() - Checks for successful job completion
- IsJobFailedOrStuck() - Combined detection for problematic jobs
```

#### **Smart Scaling Phase Management** (`internal/controller/cluster_controller.go`)
```go
- checkAndClearStuckScalingPhase() - Detects when cluster already has correct replica count but remains stuck in scaling phase, automatically clears the phase
- clearStuckScalingPhaseAfterJobDeletion() - Resets phases after job cleanup to enable retries
- isInScalingPhase() - Helper to identify scaling-related phases (PhaseCreatingReplica, PhaseScalingUp, PhaseScalingDown)
```

#### **Enhanced Reconciliation Loop** 
The main `reconcileResources()` function now includes:
- Pre-reconciliation stuck scaling phase checks
- Failed/stuck job detection during running job monitoring
- Automatic job deletion with detailed event logging
- Phase clearing and immediate reconciliation retry

#### **Equilibrium State Detection**
```go
// Long-term stuck state detection
- checkForEquilibriumState() - Detects long-running jobs (>15 min) making no progress
- checkForMissingPVCs() - Identifies missing PVCs that prevent job execution
```

### Configuration

#### **Timeout Constants**
- **Stuck Job Timeout**: `10 minutes` (configurable via `defaultStuckJobTimeout`)
- **Equilibrium Detection**: `15 minutes` (configurable via `equilibriumStateTimeout`) 
- **Job Recheck Interval**: `5 seconds` (`runningJobRecheckInterval`)

### Recovery Scenario Example

```bash
# Scenario: Cluster scaling from 9 to 10 instances
# 1. Snapshot recovery job fails due to missing PVC
# 2. Meanwhile, instance was created through streaming replication
# 3. Cluster now has 10 healthy instances but stuck in "Creating a new replica" phase

# The fix automatically:
1. ✅ Detects stuck scaling phase (PhaseCreatingReplica)
2. ✅ Verifies cluster has 10 instances (matching desired count)
3. ✅ Clears stuck scaling phase → PhaseHealthy
4. ✅ Deletes failed job with detailed logging
5. ✅ Returns cluster to normal operation

# Result: Zero manual intervention required
```

### Enhanced Error Handling & Logging

#### **Detailed Job Failure Logging**
```go
// Example log output for failed jobs
phaseReason := fmt.Sprintf("Recovering from failed job %s (active:%d succeeded:%d failed:%d age:%s)",
    job.Name, job.Status.Active, job.Status.Succeeded, job.Status.Failed,
    jobAge.Truncate(time.Second))
```

#### **Missing PVC Detection**
```go
// Specific error identification for troubleshooting
if err := r.checkForMissingPVCs(ctx, cluster, &job); err != nil {
    phaseReason = fmt.Sprintf("Recovering from stuck job %s - %s (age:%s)",
        job.Name, err.Error(), jobAge.Truncate(time.Second))
}
```

### Testing Coverage

#### **Comprehensive Unit Tests** (`pkg/utils/jobs_test.go`)
- ✅ All job utility functions with edge cases
- ✅ Time-based testing for stuck job detection  
- ✅ Scenarios: recent jobs, active jobs, completed jobs, failed jobs
- ✅ Timeout boundary testing

#### **Integration Test Structure** (`cluster_controller_stuck_reconciliation_test.go`)
- Framework established for integration testing
- Uses Ginkgo/Gomega testing patterns
- Fake Kubernetes client for controlled testing

## Benefits

### **Operational Excellence**
1. **🎯 Smart Recovery**: Detects when scaling operations are unnecessary (correct replica count already achieved)
2. **🔄 Automatic Recovery**: No manual intervention required for stuck reconciliation loops  
3. **🛡️ Improved Reliability**: Prevents production outages due to stuck scaling operations
4. **👁️ Better Observability**: Enhanced logging provides clear insights into issues and recovery actions
5. **⚡ Resource Efficiency**: Prevents accumulation of stuck jobs in cluster
6. **📈 Reduced MTTR**: Eliminates need for manual status patching in most cases

### **Safety & Reliability**
- **Conservative approach**: Only takes corrective action when cluster state is definitively determined
- **Maintains data safety**: Uses proper Kubernetes owner references for job deletion
- **Idempotent operations**: Safe to run multiple times without side effects
- **Preserves existing functionality**: No breaking changes to normal operation

## Future Enhancements

- [ ] Prometheus metrics for stuck job detection and recovery
- [ ] Configurable timeouts via cluster spec
- [ ] Automatic PVC recovery mechanisms  
- [ ] Exponential backoff for job recreation
- [ ] Kubernetes events for equilibrium state detection

## Related Issues

Fixes #7793 - Cluster reconciliation loop gets stuck when scaling replicas due to failed snapshot recovery jobs

---

**Testing Instructions:**
1. Create a cluster and scale it up
2. Simulate PVC deletion to cause snapshot recovery job failure  
3. Observe automatic detection and recovery
4. Verify cluster returns to healthy state without manual intervention