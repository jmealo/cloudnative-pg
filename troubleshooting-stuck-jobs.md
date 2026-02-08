# Troubleshooting Stuck Cluster Reconciliation

In some cases, the CloudNativePG cluster might get stuck in a "Creating a new replica" phase during scaling operations. This can happen if the underlying job responsible for creating the replica fails and the controller doesn't recover gracefully.

## Symptoms

- The cluster status shows `Creating a new replica` for an extended period.
- `kubectl get pods -l cnpg.io/cluster=<cluster-name>` does not show any new pods being created.
- A Kubernetes `Job` object for snapshot recovery is stuck in a failing state.

## Identifying the Stuck Job

The CloudNativePG operator uses Kubernetes Jobs to perform certain operations, such as recovering a replica from a volume snapshot. Unlike regular instance pods, these jobs are meant to run to completion and then be removed. If they fail, they can block the controller's reconciliation loop.

1.  **Check for running jobs for your cluster:**
    ```bash
    kubectl get jobs -n <namespace> -l cnpg.io/cluster=<cluster-name>
    ```

2.  **Look for jobs with a role of `snapshot-recovery`:**
    ```bash
    kubectl get jobs -n <namespace> -l cnpg.io/cluster=<cluster-name>,cnpg.io/jobRole=snapshot-recovery
    ```

3.  **Inspect the job's pod to find the failure reason:**
    First, get the pod created by the job:
    ```bash
    kubectl get pods -n <namespace> -l job-name=<job-name>
    ```
    Then, describe the pod to see the events. A common failure is a `FailedScheduling` event because the required PVCs were never created.
    ```bash
    kubectl describe pod <pod-name> -n <namespace>

    # Example Event
    # Events:
    #   Type     Reason            Age                     From               Message
    #   ----     ------            ----                    ----               -------
    #   Warning  FailedScheduling  3m40s (x201 over 7h3m)  default-scheduler  0/90 nodes are available: persistentvolumeclaim "<pvc-name>" not found.
    ```

## Recovery Procedure

If you have identified a stuck job that is blocking cluster operations, follow these steps to recover.

### Step 1: Delete the Stuck Job

The first step is to delete the failing job. This will remove the stuck pod and signal to the controller that the operation has ended.

```bash
kubectl delete job <job-name> -n <namespace>
```

### Step 2: Clear the Cluster Phase

After deleting the job, the controller might still be stuck in the `Creating a new replica` phase. You need to manually clear this status to force the controller to re-evaluate the cluster's state from scratch.

**This is a powerful operation and should be done with care.**

```bash
kubectl patch cluster <cluster-name> -n <namespace> --type='json' -p='[
  {"op": "remove", "path": "/status/phase"},
  {"op": "remove", "path": "/status/phaseReason"}
]' --subresource=status
```

### Step 3: Monitor the Cluster

After clearing the phase, the cluster should return to a `Ready` state (assuming the existing replicas are healthy). The controller will then re-attempt any required scaling operations.

Monitor the cluster's status:
```bash
watch kubectl get cluster <cluster-name> -n <namespace>
```

And check the controller logs for any further errors:
```bash
kubectl logs -n cnpg-system deployment/cnpg-controller-manager -f
```

## Prevention

- **Scale Incrementally:** When scaling up a cluster, do so in smaller increments (e.g., one or two instances at a time) to reduce the likelihood of hitting complex race conditions.
- **Ensure Storage is Available:** Before scaling, ensure your storage class is functioning correctly and has enough capacity.
- **Check Volume Snapshots:** If you rely on volume snapshots for recovery, ensure they are valid and accessible before scaling.
