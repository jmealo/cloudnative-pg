#!/bin/bash

# cnpg-disk-manager.sh
#
# Comprehensive disk management script for CloudNativePG clusters
# Checks disk usage, offers to resize PVCs, and manages disk space warnings

set -e
set -o pipefail

# --- Default values ---
CLUSTER_NAME=""
NAMESPACE=""
RESIZE_THRESHOLD=95  # Default threshold for offering PVC resize
PATCH_CLUSTER=false
FORCE_PATCH=false
RESIZE_PVCS=false
AUTO_MODE=false
RESIZE_INCREMENT=20  # Percentage to increase PVC size by

# --- Function to display usage ---
usage() {
  echo "Usage: $0 -c <cluster-name> -n <namespace> [options]"
  echo "  -c, --cluster         The name of the CloudNativePG cluster."
  echo "  -n, --namespace       The namespace where the cluster is running."
  echo "  -r, --resize-threshold  Disk usage percentage to trigger resize offer (default: 95)."
  echo "  -i, --increment       Percentage to increase PVC size by (default: 20)."
  echo "  --patch              Offer to patch the cluster to clear disk space status."
  echo "  --resize             Offer to resize PVCs when threshold is exceeded."
  echo "  --auto               Non-interactive mode (requires --resize)."
  echo "  --force              Skip confirmation prompts."
  echo "  -h, --help           Display this help message."
  exit 1
}

# --- Parse command-line arguments ---
while [[ "$#" -gt 0 ]]; do
  case $1 in
    -c|--cluster) CLUSTER_NAME="$2"; shift ;;
    -n|--namespace) NAMESPACE="$2"; shift ;;
    -r|--resize-threshold) RESIZE_THRESHOLD="$2"; shift ;;
    -i|--increment) RESIZE_INCREMENT="$2"; shift ;;
    --patch) PATCH_CLUSTER=true ;;
    --resize) RESIZE_PVCS=true ;;
    --auto) AUTO_MODE=true ;;
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

if [ "$AUTO_MODE" = true ] && [ "$RESIZE_PVCS" = false ]; then
  echo "Error: --auto mode requires --resize flag."
  usage
fi

# Validate thresholds
if ! [[ "$RESIZE_THRESHOLD" =~ ^[0-9]+$ ]] || [ "$RESIZE_THRESHOLD" -lt 50 ] || [ "$RESIZE_THRESHOLD" -gt 100 ]; then
  echo "Error: Resize threshold must be a number between 50 and 100."
  exit 1
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

# Function to parse storage size and convert to Gi
parse_storage_size() {
  local size=$1
  local number=$(echo "$size" | sed 's/[^0-9.]//g')
  local unit=$(echo "$size" | sed 's/[0-9.]//g')
  
  case "$unit" in
    "G"|"Gi") echo "${number}Gi" ;;
    "M"|"Mi") echo "$(( number / 1024 ))Gi" ;;
    "T"|"Ti") echo "$(( number * 1024 ))Gi" ;;
    *) echo "${number}Gi" ;;
  esac
}

# Function to calculate new size with increment
calculate_new_size() {
  local current_size=$1
  local increment=$2
  
  # Extract number from size (e.g., "100Gi" -> "100")
  local number=$(echo "$current_size" | sed 's/[^0-9.]//g')
  
  # Calculate new size
  local new_number=$(echo "$number * (100 + $increment) / 100" | bc)
  
  echo "${new_number}Gi"
}

# --- Main logic ---
info "Starting disk management for cluster '$CLUSTER_NAME' in namespace '$NAMESPACE'."

# Check for required tools
for tool in kubectl jq bc; do
  if ! command -v "$tool" &> /dev/null; then
    error "$tool command not found. Please ensure it's installed and in your PATH."
  fi
done

# Get cluster status
info "Fetching cluster status..."
CLUSTER_JSON=$(kubectl get cluster -n "$NAMESPACE" "$CLUSTER_NAME" -o json 2>/dev/null || echo "")
if [ -z "$CLUSTER_JSON" ]; then
  error "Cluster '$CLUSTER_NAME' not found in namespace '$NAMESPACE'."
fi

CLUSTER_STATUS=$(echo "$CLUSTER_JSON" | jq -r '.status.phase')
echo ""
echo "Cluster Status: $CLUSTER_STATUS"

# Get cluster storage configuration
STORAGE_SIZE=$(echo "$CLUSTER_JSON" | jq -r '.spec.storage.size // "unknown"')
WAL_STORAGE_SIZE=$(echo "$CLUSTER_JSON" | jq -r '.spec.walStorage.size // "none"')

info "Current storage configuration:"
info "  Storage size: $STORAGE_SIZE"
info "  WAL storage size: $WAL_STORAGE_SIZE"

# Get cluster conditions
CONDITIONS=$(echo "$CLUSTER_JSON" | jq -r '.status.conditions[]? | select(.type == "HealthyPVC") | "\(.type): \(.status) - \(.message)"' 2>/dev/null || echo "")
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
  local path=$3
  
  # Check disk usage for specified path
  local df_output=$(kubectl exec -n "$NAMESPACE" "$pod" -c postgres -- df -h "$path" 2>/dev/null || echo "")
  
  if [ -n "$df_output" ]; then
    # Parse df output (skip header line)
    echo "$df_output" | tail -n 1 | awk -v pod="$pod" -v role="$role" '{print pod "|" role "|" $2 "|" $3 "|" $4 "|" $5}'
  fi
}

# Function to check if WAL is on separate volume
check_wal_volume() {
  local pod=$1
  
  # Check if pg_wal is a symlink (indicating separate volume)
  local wal_link=$(kubectl exec -n "$NAMESPACE" "$pod" -c postgres -- bash -c 'readlink -f /var/lib/postgresql/data/pgdata/pg_wal' 2>/dev/null || echo "")
  
  if [[ "$wal_link" == "/var/lib/postgresql/wal/"* ]]; then
    echo "separate"
  else
    echo "shared"
  fi
}

# Check WAL usage on a pod
check_wal_files() {
  local pod=$1
  local wal_path=$2
  
  # Count WAL files
  local wal_count=$(kubectl exec -n "$NAMESPACE" "$pod" -c postgres -- bash -c "find $wal_path -name '*.partial' -o -name '[0-9A-F]*' | wc -l" 2>/dev/null || echo "0")
  
  # Get WAL directory size
  local wal_size=$(kubectl exec -n "$NAMESPACE" "$pod" -c postgres -- du -sh "$wal_path" 2>/dev/null | awk '{print $1}' || echo "Unknown")
  
  echo "$pod|$wal_count|$wal_size"
}

# Check if cluster uses separate WAL volume
FIRST_POD=$(echo "$PODS" | jq -r '.items[0].metadata.name')
WAL_VOLUME_TYPE=$(check_wal_volume "$FIRST_POD")

info "WAL volume configuration: $WAL_VOLUME_TYPE"

# Track maximum usage percentages
MAX_STORAGE_USAGE=0
MAX_WAL_USAGE=0

# Display storage disk usage
echo ""
echo "=== Storage PVC Usage (PGDATA) ==="
printf "%-30s %-10s %-8s %-8s %-8s %-10s\n" \
  "Instance" "Role" "Size" "Used" "Avail" "Use%"
echo "======================================================================================================"

# Collect storage disk usage from all pods
while IFS='|' read -r pod_name role labels; do
  disk_info=$(check_disk_on_pod "$pod_name" "$role" "/var/lib/postgresql/data/pgdata")
  if [ -n "$disk_info" ]; then
    IFS='|' read -r pod role size used avail use_percent <<< "$disk_info"
    printf "%-30s %-10s %-8s %-8s %-8s %-10s\n" "$pod" "$role" "$size" "$used" "$avail" "$use_percent"
    
    # Track maximum usage
    usage_num=$(echo "$use_percent" | sed 's/%//')
    if [[ "$usage_num" =~ ^[0-9]+$ ]] && [ "$usage_num" -gt "$MAX_STORAGE_USAGE" ]; then
      MAX_STORAGE_USAGE=$usage_num
    fi
  fi
done < <(echo "$PODS" | jq -r '.items[] | .metadata.name + "|" + (.metadata.labels."cnpg.io/instanceRole" // "unknown") + "|" + (.metadata.labels | tostring)')

# If separate WAL volume, show WAL PVC usage
if [ "$WAL_VOLUME_TYPE" = "separate" ]; then
  echo ""
  echo "=== WAL PVC Usage ==="
  printf "%-30s %-10s %-8s %-8s %-8s %-10s\n" \
    "Instance" "Role" "Size" "Used" "Avail" "Use%"
  echo "======================================================================================================"
  
  while IFS='|' read -r pod_name role labels; do
    disk_info=$(check_disk_on_pod "$pod_name" "$role" "/var/lib/postgresql/wal")
    if [ -n "$disk_info" ]; then
      IFS='|' read -r pod role size used avail use_percent <<< "$disk_info"
      printf "%-30s %-10s %-8s %-8s %-8s %-10s\n" "$pod" "$role" "$size" "$used" "$avail" "$use_percent"
      
      # Track maximum usage
      usage_num=$(echo "$use_percent" | sed 's/%//')
      if [[ "$usage_num" =~ ^[0-9]+$ ]] && [ "$usage_num" -gt "$MAX_WAL_USAGE" ]; then
        MAX_WAL_USAGE=$usage_num
      fi
    fi
  done < <(echo "$PODS" | jq -r '.items[] | .metadata.name + "|" + (.metadata.labels."cnpg.io/instanceRole" // "unknown") + "|" + (.metadata.labels | tostring)')
fi

# Display WAL file information
echo ""
echo "=== WAL Directory File Count ==="
printf "%-30s %-15s %-15s\n" \
  "Instance" "WAL Files" "WAL Dir Size"
echo "================================================================"

# Determine WAL path based on volume type
if [ "$WAL_VOLUME_TYPE" = "separate" ]; then
  WAL_PATH="/var/lib/postgresql/wal"
else
  WAL_PATH="/var/lib/postgresql/data/pgdata/pg_wal"
fi

# Collect WAL usage from all pods
while IFS='|' read -r pod_name role labels; do
  wal_info=$(check_wal_files "$pod_name" "$WAL_PATH")
  if [ -n "$wal_info" ]; then
    IFS='|' read -r pod wal_count wal_size <<< "$wal_info"
    printf "%-30s %-15s %-15s\n" "$pod" "$wal_count" "$wal_size"
  fi
done < <(echo "$PODS" | jq -r '.items[] | .metadata.name + "|" + (.metadata.labels."cnpg.io/instanceRole" // "unknown") + "|" + (.metadata.labels | tostring)')

echo ""
info "Maximum storage usage: $MAX_STORAGE_USAGE%"
if [ "$WAL_VOLUME_TYPE" = "separate" ]; then
  info "Maximum WAL usage: $MAX_WAL_USAGE%"
fi

# Check if resize is needed
NEED_STORAGE_RESIZE=false
NEED_WAL_RESIZE=false

if [ "$MAX_STORAGE_USAGE" -ge "$RESIZE_THRESHOLD" ]; then
  NEED_STORAGE_RESIZE=true
  warn "Storage PVC usage ($MAX_STORAGE_USAGE%) exceeds threshold ($RESIZE_THRESHOLD%)"
fi

if [ "$WAL_VOLUME_TYPE" = "separate" ] && [ "$MAX_WAL_USAGE" -ge "$RESIZE_THRESHOLD" ]; then
  NEED_WAL_RESIZE=true
  warn "WAL PVC usage ($MAX_WAL_USAGE%) exceeds threshold ($RESIZE_THRESHOLD%)"
fi

# Handle PVC resizing if needed
if [ "$RESIZE_PVCS" = true ] && ([ "$NEED_STORAGE_RESIZE" = true ] || [ "$NEED_WAL_RESIZE" = true ]); then
  echo ""
  warn "PVC resize is recommended!"
  
  # Calculate new sizes
  if [ "$NEED_STORAGE_RESIZE" = true ]; then
    NEW_STORAGE_SIZE=$(calculate_new_size "$STORAGE_SIZE" "$RESIZE_INCREMENT")
    info "Recommended new storage size: $NEW_STORAGE_SIZE (current: $STORAGE_SIZE)"
  fi
  
  if [ "$NEED_WAL_RESIZE" = true ] && [ "$WAL_STORAGE_SIZE" != "none" ]; then
    NEW_WAL_SIZE=$(calculate_new_size "$WAL_STORAGE_SIZE" "$RESIZE_INCREMENT")
    info "Recommended new WAL size: $NEW_WAL_SIZE (current: $WAL_STORAGE_SIZE)"
  fi
  
  # Confirm resize
  if [ "$AUTO_MODE" = false ] && [ "$FORCE_PATCH" = false ]; then
    echo ""
    echo "Would you like to resize the PVCs?"
    echo "This will update the cluster specification with new sizes."
    read -p "Type 'yes' to confirm: " -r
    if [[ ! $REPLY =~ ^[Yy][Ee][Ss]$ ]]; then
      info "PVC resize cancelled by user."
      RESIZE_PVCS=false
    fi
  fi
  
  if [ "$RESIZE_PVCS" = true ]; then
    info "Preparing to resize PVCs directly..."
    
    # Get all PVCs for the cluster
    info "Fetching PVCs for cluster '$CLUSTER_NAME'..."
    
    # Resize storage PVCs if needed
    if [ "$NEED_STORAGE_RESIZE" = true ]; then
      info "Resizing storage PVCs to $NEW_STORAGE_SIZE..."
      
      # Get storage PVCs (data PVCs)
      STORAGE_PVCS=$(kubectl get pvc -n "$NAMESPACE" -l "cnpg.io/cluster=$CLUSTER_NAME,cnpg.io/instanceRole" -o json | \
        jq -r '.items[] | select(.metadata.name | test("data")) | .metadata.name')
      
      if [ -z "$STORAGE_PVCS" ]; then
        # Try alternative label selector
        STORAGE_PVCS=$(kubectl get pvc -n "$NAMESPACE" -l "cnpg.io/cluster=$CLUSTER_NAME" -o json | \
          jq -r '.items[] | select(.metadata.name | test("pgdata")) | .metadata.name')
      fi
      
      for pvc in $STORAGE_PVCS; do
        info "Patching PVC: $pvc"
        if kubectl patch pvc -n "$NAMESPACE" "$pvc" --type=merge \
          -p "{\"spec\":{\"resources\":{\"requests\":{\"storage\":\"$NEW_STORAGE_SIZE\"}}}}" 2>&1; then
          success "PVC $pvc resize requested"
        else
          warn "Failed to patch PVC $pvc"
        fi
      done
    fi
    
    # Resize WAL PVCs if needed
    if [ "$NEED_WAL_RESIZE" = true ] && [ "$WAL_STORAGE_SIZE" != "none" ]; then
      info "Resizing WAL PVCs to $NEW_WAL_SIZE..."
      
      # Get WAL PVCs
      WAL_PVCS=$(kubectl get pvc -n "$NAMESPACE" -l "cnpg.io/cluster=$CLUSTER_NAME" -o json | \
        jq -r '.items[] | select(.metadata.name | test("wal")) | .metadata.name')
      
      for pvc in $WAL_PVCS; do
        info "Patching PVC: $pvc"
        if kubectl patch pvc -n "$NAMESPACE" "$pvc" --type=merge \
          -p "{\"spec\":{\"resources\":{\"requests\":{\"storage\":\"$NEW_WAL_SIZE\"}}}}" 2>&1; then
          success "PVC $pvc resize requested"
        else
          warn "Failed to patch PVC $pvc"
        fi
      done
    fi
    
    echo ""
    success "PVC resize requests submitted!"
    echo ""
    echo "The storage provider will now resize the PVCs."
    echo "This process may take several minutes depending on your storage class."
    echo ""
    echo "Monitor progress with:"
    echo "  kubectl get pvc -n $NAMESPACE -l cnpg.io/cluster=$CLUSTER_NAME -w"
    echo ""
    echo "Once PVCs are resized, CloudNativePG should automatically detect the new space."
    
    # Also update cluster spec for consistency
    info "Updating cluster spec to match new PVC sizes..."
    CLUSTER_PATCH='{"spec":{'
    
    if [ "$NEED_STORAGE_RESIZE" = true ]; then
      CLUSTER_PATCH="${CLUSTER_PATCH}\"storage\":{\"size\":\"$NEW_STORAGE_SIZE\"}"
    fi
    
    if [ "$NEED_WAL_RESIZE" = true ] && [ "$WAL_STORAGE_SIZE" != "none" ]; then
      if [ "$NEED_STORAGE_RESIZE" = true ]; then
        CLUSTER_PATCH="${CLUSTER_PATCH},"
      fi
      CLUSTER_PATCH="${CLUSTER_PATCH}\"walStorage\":{\"size\":\"$NEW_WAL_SIZE\"}"
    fi
    
    CLUSTER_PATCH="${CLUSTER_PATCH}}}"
    
    if kubectl patch cluster -n "$NAMESPACE" "$CLUSTER_NAME" --type=merge -p "$CLUSTER_PATCH" 2>&1; then
      success "Cluster spec updated to match new PVC sizes"
    else
      warn "Failed to update cluster spec (PVCs will still resize)"
    fi
  fi
fi

# Check if patching is requested to clear disk space warnings
if [ "$PATCH_CLUSTER" = true ]; then
  echo ""
  warn "CloudNativePG automatically detects disk space changes."
  warn "After resizing PVCs or cleaning up space, the cluster status should update automatically."
  
  if [ "$FORCE_PATCH" = false ] && [ "$AUTO_MODE" = false ]; then
    echo ""
    echo "Would you like to trigger a cluster reconciliation by restarting the primary pod?"
    echo "This will force CloudNativePG to re-check disk space immediately."
    read -p "Type 'yes' to confirm: " -r
    if [[ ! $REPLY =~ ^[Yy][Ee][Ss]$ ]]; then
      info "Operation cancelled by user."
      exit 0
    fi
  fi
  
  # Find the primary pod
  PRIMARY_POD=$(echo "$PODS" | jq -r '.items[] | select(.metadata.labels."cnpg.io/instanceRole" == "primary") | .metadata.name' | head -1)
  
  if [ -z "$PRIMARY_POD" ]; then
    error "Could not find primary pod"
  fi
  
  info "Deleting primary pod $PRIMARY_POD to trigger reconciliation..."
  
  if kubectl delete pod -n "$NAMESPACE" "$PRIMARY_POD" --wait=false 2>&1; then
    success "Primary pod deletion initiated!"
    echo ""
    echo "CloudNativePG will recreate the pod and re-check disk space."
    echo "Monitor the cluster status with:"
    echo "  kubectl cnpg status -n $NAMESPACE $CLUSTER_NAME"
    echo ""
    echo "Note: The cluster will perform a failover. This may take a few minutes."
  else
    error "Failed to delete primary pod."
  fi
else
  echo ""
  if [ "$NEED_STORAGE_RESIZE" = true ] || [ "$NEED_WAL_RESIZE" = true ]; then
    echo "To resize PVCs, run with --resize flag:"
    echo "  $0 -c $CLUSTER_NAME -n $NAMESPACE --resize"
  fi
  echo ""
  echo "CloudNativePG automatically detects disk space changes."
  echo "The cluster status should update within a few minutes after any changes."
fi

exit 0
