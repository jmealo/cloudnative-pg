#!/bin/bash

# cnpg-pvc-resizer.sh
#
# A script to safely resize PersistentVolumeClaims (PVCs) for a CloudNativePG cluster.
# It can resize both data and WAL volumes and includes safety checks.

set -e
set -o pipefail

# --- Default values ---
DRY_RUN=false
CLUSTER_NAME=""
NAMESPACE=""
WAL_SIZE=""
STORAGE_SIZE=""

# --- Function to display usage ---
usage() {
  echo "Usage: $0 -c <cluster-name> -n <namespace> [-w <wal-size>] [-s <storage-size>] [--dry-run]"
  echo "  -c, --cluster    The name of the CloudNativePG cluster."
  echo "  -n, --namespace  The namespace where the cluster is running."
  echo "  -w, --wal-size   The new size for the WAL PVCs (e.g., 50Gi)."
  echo "  -s, --storage-size The new size for the main data storage PVCs (e.g., 200Gi)."
  echo "  --dry-run        Show what would be done without actually resizing the PVCs."
  echo "  -h, --help       Display this help message."
  exit 1
}

# --- Parse command-line arguments ---
while [[ "$#" -gt 0 ]]; do
  case $1 in
    -c|--cluster) CLUSTER_NAME="$2"; shift ;;
    -n|--namespace) NAMESPACE="$2"; shift ;;
    -w|--wal-size) WAL_SIZE="$2"; shift ;;
    -s|--storage-size) STORAGE_SIZE="$2"; shift ;;
    --dry-run) DRY_RUN=true ;;
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

if [ -z "$WAL_SIZE" ] && [ -z "$STORAGE_SIZE" ]; then
  echo "Error: You must specify at least one size to change (--wal-size or --storage-size)."
  usage
fi

# --- Helper functions ---
info() {
  echo "INFO: $1"
}

warn() {
  echo "WARN: $1"
}

error() {
  echo "ERROR: $1" >&2
  exit 1
}

# --- Main logic ---
info "Starting PVC resize process for cluster '$CLUSTER_NAME' in namespace '$NAMESPACE'."
if [ "$DRY_RUN" = true ]; then
  info "Running in DRY-RUN mode. No changes will be made."
fi

# Check for kubectl
if ! command -v kubectl &> /dev/null; then
  error "kubectl command not found. Please ensure it's installed and in your PATH."
fi

info "Fetching PVCs for cluster '$CLUSTER_NAME'..."
PVC_LIST=$(kubectl get pvc -n "$NAMESPACE" -l "cnpg.io/cluster=$CLUSTER_NAME" -o json)

if [ -z "$(echo "$PVC_LIST" | jq '.items[]')" ]; then
  error "No PVCs found for cluster '$CLUSTER_NAME' in namespace '$NAMESPACE'."
fi

# Check if storage class allows volume expansion
STORAGE_CLASS_NAME=$(echo "$PVC_LIST" | jq -r '.items[0].spec.storageClassName')
if [ -z "$STORAGE_CLASS_NAME" ]; then
  error "Could not determine storage class for the PVCs."
fi

info "Verifying that storage class '$STORAGE_CLASS_NAME' allows volume expansion..."
ALLOW_EXPANSION=$(kubectl get sc "$STORAGE_CLASS_NAME" -o jsonpath='{.allowVolumeExpansion}')
if [ "$ALLOW_EXPANSION" != "true" ]; then
  error "Storage class '$STORAGE_CLASS_NAME' does not allow volume expansion. Cannot proceed."
fi
info "Storage class '$STORAGE_CLASS_NAME' supports volume expansion."

# --- Prepare resize operations ---
RESIZE_PLAN=()
while IFS= read -r pvc_json; do
  pvc_name=$(echo "$pvc_json" | jq -r '.metadata.name')
  current_size=$(echo "$pvc_json" | jq -r '.spec.resources.requests.storage')
  target_size=""

  if [[ "$pvc_name" == *"-wal" ]] && [ -n "$WAL_SIZE" ]; then
    target_size=$WAL_SIZE
  elif [[ "$pvc_name" != *"-wal" ]] && [ -n "$STORAGE_SIZE" ]; then
    target_size=$STORAGE_SIZE
  fi

  if [ -n "$target_size" ] && [ "$current_size" != "$target_size" ]; then
    RESIZE_PLAN+=("$pvc_name $current_size $target_size")
  fi
done < <(echo "$PVC_LIST" | jq -c '.items[]')

if [ ${#RESIZE_PLAN[@]} -eq 0 ]; then
  info "All relevant PVCs are already at the desired size. No action needed."
  exit 0
fi

# --- Display plan and ask for confirmation ---
echo -e "\n--- Resize Plan ---"
printf "%-40s %-15s %-15s\n" "PVC Name" "Current Size" "Target Size"
echo "---------------------------------------------------------------------"
for plan in "${RESIZE_PLAN[@]}"; do
  read -r name current target <<< "$plan"
  printf "%-40s %-15s %-15s\n" "$name" "$current" "$target"
done
echo "---------------------------------------------------------------------"

if [ "$DRY_RUN" = true ]; then
  info "Dry-run complete. Exiting."
  exit 0
fi

read -p "Do you want to apply this resize plan? (y/n) " -n 1 -r
echo
if [[ ! $REPLY =~ ^[Yy]$ ]]; then
  info "Aborted by user."
  exit 1
fi

# --- Execute resize ---
info "Applying resize plan..."
for plan in "${RESIZE_PLAN[@]}"; do
  read -r pvc_name _ target_size <<< "$plan"
  info "Resizing '$pvc_name' to '$target_size'..."
  if ! kubectl patch pvc "$pvc_name" -n "$NAMESPACE" -p "{\"spec\":{\"resources\":{\"requests\":{\"storage\":\"$target_size\"}}}}"; then
    warn "Failed to patch PVC '$pvc_name'. It might have been resized by another process. Please check manually."
  fi
done

info "All patch commands have been sent."
info "Please monitor the PVCs to ensure the resize completes successfully."
info "You can check the status with: kubectl get pvc -n $NAMESPACE -l cnpg.io/cluster=$CLUSTER_NAME -w"
warn "Note: You may need to restart the corresponding pods for the filesystem to be resized."
info "For CloudNativePG, this is typically handled by the operator, but a manual rollout may be required if issues arise."
info "You can trigger a restart with: kubectl cnpg restart cluster $CLUSTER_NAME -n $NAMESPACE"

exit 0
