#!/bin/bash

# cnpg-stuck-job-recovery.sh
#
# This script helps diagnose and recover from a stuck CloudNativePG cluster
# where a snapshot-recovery job is preventing new replicas from being created.
#
# Symptoms:
# - Cluster status is "Creating a new replica" but no new pods appear.
# - A job named "<cluster-name>-<instance-id>-snapshot-recovery" is stuck.
# - The cluster phase doesn't clear even after deleting the job.
#
# Usage:
#   ./cnpg-stuck-job-recovery.sh <cluster-name> <namespace>

set -e
set -o pipefail

CLUSTER_NAME=$1
NAMESPACE=$2

if [ -z "$CLUSTER_NAME" ] || [ -z "$NAMESPACE" ]; then
  echo "Usage: $0 <cluster-name> <namespace>"
  exit 1
fi

echo "🔍 Diagnosing cluster '$CLUSTER_NAME' in namespace '$NAMESPACE'..."

# --- Step 1: Check Cluster Status ---
echo -e "\n--- Cluster Status ---"
kubectl get cluster "$CLUSTER_NAME" -n "$NAMESPACE"
PHASE=$(kubectl get cluster "$CLUSTER_NAME" -n "$NAMESPACE" -o jsonpath='{.status.phase}')
PHASE_REASON=$(kubectl get cluster "$CLUSTER_NAME" -n "$NAMESPACE" -o jsonpath='{.status.phaseReason}')
echo "------------------------"
echo "Phase: $PHASE"
echo "Reason: $PHASE_REASON"
echo "------------------------"

# --- Step 2: Find Stuck Jobs ---
STUCK_JOB=$(kubectl get jobs -n "$NAMESPACE" \
  -l "cnpg.io/cluster=$CLUSTER_NAME,cnpg.io/jobRole=snapshot-recovery" \
  -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)

if [ -z "$STUCK_JOB" ]; then
  echo "✅ No stuck snapshot-recovery jobs found."
else
  echo "🚨 Found stuck snapshot-recovery job: $STUCK_JOB"
  echo "Job details:"
  kubectl get job "$STUCK_JOB" -n "$NAMESPACE"
  
  read -p "Do you want to delete this job? (y/n) " -n 1 -r
  echo
  if [[ $REPLY =~ ^[Yy]$ ]]; then
    echo "Deleting job '$STUCK_JOB'..."
    kubectl delete job "$STUCK_JOB" -n "$NAMESPACE"
    echo "Job deleted."
  fi
fi

# --- Step 3: Clear Cluster Phase ---
if [[ "$PHASE" == "Creating a new replica" ]]; then
  echo "🚨 Cluster phase is stuck in 'Creating a new replica'."
  read -p "Do you want to clear the cluster phase to reset the reconciliation loop? (y/n) " -n 1 -r
  echo
  if [[ $REPLY =~ ^[Yy]$ ]]; then
    echo "Patching cluster to remove phase and phaseReason..."
    kubectl patch cluster "$CLUSTER_NAME" -n "$NAMESPACE" --type='json' -p='[
      {"op": "remove", "path": "/status/phase"},
      {"op": "remove", "path": "/status/phaseReason"}
    ]' --subresource=status
    echo "Cluster phase cleared."
  fi
fi

# --- Step 4: Verify Final State ---
echo -e "\n--- Verifying Final Cluster State ---"
kubectl get cluster "$CLUSTER_NAME" -n "$NAMESPACE"
echo "-------------------------------------"
echo "✅ Recovery script finished. Monitor the cluster to ensure it returns to a healthy state."
echo "If the cluster is still not ready, you may need to restart the controller manager:"
echo "  kubectl rollout restart deployment/cnpg-controller-manager -n cnpg-system"
