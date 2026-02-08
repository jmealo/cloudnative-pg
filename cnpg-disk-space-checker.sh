#!/bin/bash

# cnpg-disk-space-checker.sh
#
# A script to check disk space usage on all instances in a CloudNativePG cluster
# and optionally patch the cluster to clear disk space warnings.

set -e
set -o pipefail

# --- Default values ---
CLUSTER_NAME=""
NAMESPACE=""
PATCH_CLUSTER=false
FORCE_PATCH=false

# --- Function to display usage ---
usage() {
  echo "Usage: $0 -c <cluster-name> -n <namespace> [--patch] [--force]"
  echo "  -c, --cluster      The name of the CloudNativePG cluster."
  echo "  -n, --namespace    The namespace where the cluster is running."
  echo "  --patch            Offer to patch the cluster to clear disk space status."
  echo "  --force            Skip confirmation prompts when patching."
  echo "  -h, --help         Display this help message."
  exit 1
}

# --- Parse command-line arguments ---
while [[ "$#" -gt 0 ]]; do
  case $1 in
    -c|--cluster) CLUSTER_NAME="$2"; shift ;;
    -n|--namespace) NAMESPACE="$2"; shift ;;
    --patch) PATCH_CLUSTER=true ;;
    --force) FORCE_PATCH=true ;;
    -h|--help) usage ;;
    *) echo "Unknown parameter passed: $1"; usage ;;
  esac
  shift
done

# --- Validate required parameters ---
if [ -z "$CLUSTER_NAME" ] || [ -z "$NAMESPACE" ]; then
  echo "Error: Cluster name and namespace are required."
  usage
fi

# --- Helper functions ---
info() {
  echo "[INFO] $1" >&2
}

warn() {
  echo "[WARN] $1" >&2
}

error() {
  echo "[ERROR] $1" >&2
  exit 1
}

success() {
  echo "[SUCCESS] $1" >&2
}

# Function to format bytes to human-readable format
format_bytes() {
  local bytes=$1
  if [ -z "$bytes" ] || [ "$bytes" = "null" ]; then
    echo "0 B"
    return
  fi
  
  # Remove any decimal points and convert to integer
  bytes=$(echo "$bytes" | cut -d. -f1)
  
  if [ "$bytes" -lt 1024 ]; then
    echo "${bytes} B"
  elif [ "$bytes" -lt 1048576 ]; then
    echo "$(( bytes / 1024 )) KB"
  elif [ "$bytes" -lt 1073741824 ]; then
    echo "$(( bytes / 1048576 )) MB"
  else
    echo "$(( bytes / 1073741824 )) GB"
  fi
}

# --- Main logic ---
info "Starting disk space check for cluster '$CLUSTER_NAME' in namespace '$NAMESPACE'."

# Check for kubectl
if ! command -v kubectl &> /dev/null; then
  error "kubectl command not found. Please ensure it's installed and in your PATH."
fi

# Check for jq
if ! command -v jq &> /dev/null; then
  error "jq command not found. Please install jq to use this script."
fi

# Get cluster status
info "Fetching cluster status..."
CLUSTER_STATUS=$(kubectl get cluster -n "$NAMESPACE" "$CLUSTER_NAME" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
if [ -z "$CLUSTER_STATUS" ]; then
  error "Cluster '$CLUSTER_NAME' not found in namespace '$NAMESPACE'."
fi

echo ""
echo "Cluster Status: $CLUSTER_STATUS"

# Get cluster conditions
CONDITIONS=$(kubectl get cluster -n "$NAMESPACE" "$CLUSTER_NAME" -o json | jq -r '.status.conditions[]? | select(.type == "HealthyPVC") | "\(.type): \(.status) - \(.message)"' 2>/dev/null || echo "")
if [ -n "$CONDITIONS" ]; then
  echo "PVC Health Conditions:"
  echo "$CONDITIONS"
fi

# Get all pods in the cluster - filter for actual instances only
info "Fetching pods for cluster '$CLUSTER_NAME'..."
PODS=$(kubectl get pods -n "$NAMESPACE" -l "cnpg.io/cluster=$CLUSTER_NAME" -o json | \
  jq '{items: [.items[] | select(.metadata.labels."cnpg.io/instanceName" != null and (.metadata.name | test("pooler|snapshot-recovery") | not))]}')

if [ -z "$(echo "$PODS" | jq -r '.items[]')" ]; then
  error "No instance pods found for cluster '$CLUSTER_NAME' in namespace '$NAMESPACE'."
fi

# Function to check disk usage on a pod
check_disk_on_pod() {
  local pod=$1
  local role=$2
  
  # Check PGDATA disk usage
  local df_output=$(kubectl exec -n "$NAMESPACE" "$pod" -c postgres -- df -h /var/lib/postgresql/data/pgdata 2>/dev/null || echo "")
  
  if [ -n "$df_output" ]; then
    # Parse df output (skip header line)
    echo "$df_output" | tail -n 1 | awk -v pod="$pod" -v role="$role" '{print pod "|" role "|" $2 "|" $3 "|" $4 "|" $5}'
  fi
}

# Check WAL usage on a pod
check_wal_on_pod() {
  local pod=$1
  
  # Count WAL files
  local wal_count=$(kubectl exec -n "$NAMESPACE" "$pod" -c postgres -- bash -c 'find /var/lib/postgresql/data/pgdata/pg_wal -name "*.partial" -o -name "[0-9A-F]*" | grep -E "^[0-9A-F]{24}$" | wc -l' 2>/dev/null || echo "0")
  
  # Get WAL directory size
  local wal_size=$(kubectl exec -n "$NAMESPACE" "$pod" -c postgres -- du -sh /var/lib/postgresql/data/pgdata/pg_wal 2>/dev/null | awk '{print $1}' || echo "Unknown")
  
  echo "$pod|$wal_count|$wal_size"
}

# Display disk usage
echo ""
echo "=== Disk Space Usage ==="
printf "%-30s %-10s %-8s %-8s %-8s %-10s\n" \
  "Instance" "Role" "Size" "Used" "Avail" "Use%"
echo "======================================================================================================"

# Collect disk usage from all pods
while IFS='|' read -r pod_name role labels; do
  disk_info=$(check_disk_on_pod "$pod_name" "$role")
  if [ -n "$disk_info" ]; then
    IFS='|' read -r pod role size used avail use_percent <<< "$disk_info"
    printf "%-30s %-10s %-8s %-8s %-8s %-10s\n" "$pod" "$role" "$size" "$used" "$avail" "$use_percent"
  fi
done < <(echo "$PODS" | jq -r '.items[] | .metadata.name + "|" + (.metadata.labels."cnpg.io/instanceRole" // "unknown") + "|" + (.metadata.labels | tostring)')

# Display WAL usage
echo ""
echo "=== WAL Directory Usage ==="
printf "%-30s %-15s %-15s\n" \
  "Instance" "WAL Files" "WAL Dir Size"
echo "================================================================"

# Collect WAL usage from all pods
while IFS='|' read -r pod_name role labels; do
  wal_info=$(check_wal_on_pod "$pod_name")
  if [ -n "$wal_info" ]; then
    IFS='|' read -r pod wal_count wal_size <<< "$wal_info"
    printf "%-30s %-15s %-15s\n" "$pod" "$wal_count" "$wal_size"
  fi
done < <(echo "$PODS" | jq -r '.items[] | .metadata.name + "|" + (.metadata.labels."cnpg.io/instanceRole" // "unknown") + "|" + (.metadata.labels | tostring)')

# Check if patching is requested
if [ "$PATCH_CLUSTER" = true ]; then
  echo ""
  warn "The cluster may still show disk space warnings even after cleanup."
  
  # Check current cluster annotations
  CURRENT_ANNOTATIONS=$(kubectl get cluster -n "$NAMESPACE" "$CLUSTER_NAME" -o json | jq -r '.metadata.annotations // {}')
  RECONCILE_ANNOTATION=$(echo "$CURRENT_ANNOTATIONS" | jq -r '."cnpg.io/reconcileDiskSpace" // "not set"')
  
  echo ""
  echo "Current reconcileDiskSpace annotation: $RECONCILE_ANNOTATION"
  
  if [ "$FORCE_PATCH" = false ]; then
    echo ""
    echo "Would you like to patch the cluster to trigger disk space reconciliation?"
    echo "This will add/update the 'cnpg.io/reconcileDiskSpace' annotation."
    read -p "Type 'yes' to confirm: " -r
    if [[ ! $REPLY =~ ^[Yy][Ee][Ss]$ ]]; then
      info "Operation cancelled by user."
      exit 0
    fi
  fi
  
  # Generate timestamp for annotation
  TIMESTAMP=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
  
  info "Patching cluster with reconcileDiskSpace annotation..."
  
  # Apply the patch
  if kubectl annotate cluster -n "$NAMESPACE" "$CLUSTER_NAME" \
    "cnpg.io/reconcileDiskSpace=$TIMESTAMP" \
    --overwrite 2>&1; then
    success "Cluster patched successfully!"
    echo ""
    echo "The CloudNativePG operator should now re-evaluate disk space."
    echo "Monitor the cluster status with:"
    echo "  kubectl cnpg status -n $NAMESPACE $CLUSTER_NAME"
    echo ""
    echo "Note: It may take a few minutes for the status to update."
  else
    error "Failed to patch cluster."
  fi
else
  echo ""
  echo "To trigger disk space reconciliation, run with --patch flag:"
  echo "  $0 -c $CLUSTER_NAME -n $NAMESPACE --patch"
fi

exit 0
