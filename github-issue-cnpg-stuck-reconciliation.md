---
title: "Bug: Cluster reconciliation loop gets stuck when scaling replicas due to failed snapshot recovery jobs"
labels: "bug, operator"
---

### Describe the bug

When scaling up a CloudNativePG cluster, if the snapshot recovery job for a new replica fails, the controller can get stuck in a reconciliation loop. The cluster status remains in "Creating a new replica", and even after manually deleting the failed job, the cluster does not return to a healthy state. This prevents any further scaling operations and can lead to production issues if not manually resolved.

### To Reproduce

Steps to reproduce the behavior:

1. Have a healthy CNPG cluster.
2. Attempt to scale up the number of instances (e.g., from 9 to 10).
3. Induce a failure in the snapshot recovery process for the new replica. This could be due to a missing PVC, an invalid volume snapshot, or other underlying infrastructure issues that make the job unscheduleable.
4. Observe that a `snapshot-recovery` job is created.
5. The job fails and the pod for the job remains in a `Pending` state because the required PVCs are not found.
6. The cluster status becomes `Creating a new replica` and gets stuck there.
7. Manually delete the failed job using `kubectl delete job <job-name>`.
8. Observe that the cluster status does not change and remains stuck.

### Expected behavior

1. After a snapshot recovery job fails a certain number of times, the controller should abandon the attempt for that specific instance and either:
   a. Try creating the replica again from scratch (e.g., via streaming replication).
   b. Mark the instance as failed and allow the cluster to return to a `Ready` state with the existing healthy replicas.
2. Deleting a failed job should immediately trigger the controller to re-evaluate the cluster state and either retry the operation or clear the stuck phase.

### Actual behavior

The controller remains stuck in the `Creating a new replica` phase, even after the failed job is deleted. The only way to recover is to manually patch the cluster's status to remove the `phase` and `phaseReason` fields.

### Workaround

The following manual intervention was required to unstick the cluster:

1. **Delete the stuck snapshot-recovery job:**
   ```bash
   kubectl delete job <cluster-name>-<instance-id>-snapshot-recovery -n <namespace>
   ```

2. **Clear the cluster phase and reason from the status:**
   ```bash
   kubectl patch cluster <cluster-name> -n <namespace> --type='json' -p='[
     {"op": "remove", "path": "/status/phase"},
     {"op": "remove", "path": "/status/phaseReason"}
   ]' --subresource=status
   ```

### Supporting data

- **Cluster Status when stuck:**
  ```yaml
  status:
    phase: Creating a new replica
    phaseReason: Creating replica <cluster-name>-<instance-id>-snapshot-recovery
  ```

- **Job Pod Events:**
  ```
  Events:
    Type     Reason            Age                     From               Message
    ----     ------            ----                    ----               -------
    Warning  FailedScheduling  3m40s (x201 over 7h3m)  default-scheduler  0/90 nodes are available: persistentvolumeclaim "<pvc-name>" not found.
  ```

### System information

- **CloudNativePG version:** 1.26.0
- **Kubernetes version:** 1.31
- **Cloud provider:** Azure (AKS)

### Additional context

This issue was observed in a production environment and caused significant operational difficulty. The use of Jobs for recovery is not immediately obvious to end-users (due to how instance pods are scheduled), making troubleshooting difficult. Improving the controller's resilience to such failures and documenting the recovery process would be highly beneficial.
