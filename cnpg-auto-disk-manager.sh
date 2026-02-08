#!/bin/bash

# cnpg-auto-disk-manager.sh
#
# Automated disk management script for CloudNativePG clusters
# Non-interactive version for automated PVC resizing and monitoring

set -e
set -o pipefail

# --- Default values ---
CLUSTER_NAME=""
NAMESPACE=""
RESIZE_THRESHOLD=95  # Default threshold for triggering PVC resize
RESIZE_INCREMENT=20  # Percentage to increase PVC size by
DRY_RUN=false
LOG_FILE=""
RECONCILE_AFTER_RESIZE=true  # Auto-reconcile disk space after resize

# --- Function to display usage ---
usage() {
  echo "Usage: $0 -c <cluster-name> -n <namespace> [options]"
  echo "  -c, --cluster         The name of the CloudNativePG cluster."
  echo "  -n, --namespace       The namespace where the cluster is running."
  echo "  -r, --resize-threshold  Disk usage percentage to trigger resize (default: 95)."
  echo "  -i, --increment       Percentage to increase PVC size by (default: 20)."
  echo "  -l, --log-file        Log file path for operation logging."
  echo "  --dry-run             Show what would be done without making changes."
  echo "  --no-reconcile        Skip disk space reconciliation after resize."
  echo "  -h, --help            Display this help message."
  echo ""
  echo "Example: $0 -c my-cluster -n db -r 90 -l /var/log/cnpg-disk.log"
  exit 1
}

# --- Parse command-line arguments ---
while [[ "$#" -gt 0 ]]; do
  case $1 in
    -c|--cluster) CLUSTER_NAME="$2"; shift ;;
    -n|--namespace) NAMESPACE="$2"; shift ;;
    -r|--resize-threshold) RESIZE_THRESHOLD="$2"; shift ;;
    -i|--increment) RESIZE_INCREMENT="$2"; shift ;;
    -l|--log-file) LOG_FILE="$2"; shift ;;
    --dry-run) DRY_RUN=true ;;
    --no-reconcile) RECONCILE_AFTER_RESIZE=false ;;
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

# Validate thresholds
if ! [[ "$RESIZE_THRESHOLD" =~ ^[0-9]+$ ]] || [ "$RESIZE_THRESHOLD" -lt 50 ] || [ "$RESIZE_THRESHOLD" -gt 100 ]; then
  echo "Error: Resize threshold must be a number between 50 and 100."
  exit 1
fi

if ! [[ "$RESIZE_INCREMENT" =~ ^[0-9]+$ ]] || [ "$RESIZE_INCREMENT" -lt 5 ] || [ "$RESIZE_INCREMENT" -gt 100 ]; then
  echo "Error: Resize increment must be a number between 5 and 100."
  exit 1
fi

# --- Logging functions ---
log() {
  local level=$1
  shift
  local message="$@"
  local timestamp=$(date -u +"%Y-%m-%d %H:%M:%S UTC")
  local log_entry="[$timestamp] [$level] $message"
  
  if [ -n "$LOG_FILE" ]; then
    echo "$log_entry" >> "$LOG_FILE"
  fi
  
  # Also output to stderr for visibility
  echo "$log_entry" >&2
}

info() {
  log "INFO" "$@"
}

warn() {
  log "WARN" "$@"
}

error() {
  log "ERROR" "$@"
  exit 1
}

success() {
  log "SUCCESS" "$@"
}

# Function to calculate new size with increment
calculate_new_size() {
  local current_size=$1
  local increment=$2
  
  # Extract number from size (e.g., "100Gi" -> "100")
  local number=$(echo "$current_size" | sed 's/[^0-9.]//g')
  
  # Calculate new size (round up)
  local new_number=$(echo "scale=0; ($number * (100 + $increment) / 100 + 0.5)/1" | bc)
  
  echo "${new_number}Gi"
}

# --- Main logic ---
info "Starting automated disk management for cluster '$CLUSTER_NAME' in namespace '$NAMESPACE'"
info "Resize threshold: $RESIZE_THRESHOLD%, Increment: $RESIZE_INCREMENT%"
if [ "$DRY_RUN" = true ]; then
  info "Running in DRY-RUN mode - no changes will be made"
fi

# Check for required tools
for tool in kubectl jq bc; do
  if ! command -v "$tool" &> /dev/null; then
    error "$tool command not found. Please ensure it's installed and in your PATH."
  fi
done

# Get cluster status
info "Fetching cluster configuration..."
CLUSTER_JSON=$(kubectl get cluster -n "$NAMESPACE" "$CLUSTER_NAME" -o json 2>/dev/null || echo "")
if [ -z "$CLUSTER_JSON" ]; then
  error "Cluster '$CLUSTER_NAME' not found in namespace '$NAMESPACE'."
fi

CLUSTER_STATUS=$(echo "$CLUSTER_JSON" | jq -r '.status.phase')
info "Cluster status: $CLUSTER_STATUS"

# Get current storage configuration
STORAGE_SIZE=$(echo "$CLUSTER_JSON" | jq -r '.spec.storage.size // "unknown"')
WAL_STORAGE_SIZE=$(echo "$CLUSTER_JSON" | jq -r '.spec.walStorage.size // "none"')

info "Current configuration - Storage: $STORAGE_SIZE, WAL: $WAL_STORAGE_SIZE"

# Get all pods in the cluster
PODS=$(kubectl get pods -n "$NAMESPACE" -l "cnpg.io/cluster=$CLUSTER_NAME" -o json | \
  jq '{items: [.items[] | select(.metadata.labels."cnpg.io/instanceName" != null and (.metadata.name | test("pooler|snapshot-recovery") | not))]}')

if [ -z "$(echo "$PODS" | jq -r '.items[]' 2>/dev/null)" ]; then
  error "No instance pods found for cluster '$CLUSTER_NAME' in namespace '$NAMESPACE'."
fi

# Function to check disk usage
check_disk_usage() {
  local pod=$1
  local path=$2
  
  # Get disk usage percentage (remove % sign)
  local usage=$(kubectl exec -n "$NAMESPACE" "$pod" -c postgres -- df -h "$path" 2>/dev/null | tail -n 1 | awk '{print $5}' | sed 's/%//')
  
  if [ -n "$usage" ] && [[ "$usage" =~ ^[0-9]+$ ]]; then
    echo "$usage"
  else
    echo "0"
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

# Check if cluster uses separate WAL volume
FIRST_POD=$(echo "$PODS" | jq -r '.items[0].metadata.name')
WAL_VOLUME_TYPE=$(check_wal_volume "$FIRST_POD")
info "WAL volume configuration: $WAL_VOLUME_TYPE"

# Track maximum usage percentages
MAX_STORAGE_USAGE=0
MAX_WAL_USAGE=0

# Check storage usage on all pods
info "Checking disk usage across all instances..."
while IFS='|' read -r pod_name role labels; do
  # Check storage PVC usage
  storage_usage=$(check_disk_usage "$pod_name" "/var/lib/postgresql/data/pgdata")
  if [ "$storage_usage" -gt "$MAX_STORAGE_USAGE" ]; then
    MAX_STORAGE_USAGE=$storage_usage
  fi
  
  # Check WAL PVC usage if separate
  if [ "$WAL_VOLUME_TYPE" = "separate" ]; then
    wal_usage=$(check_disk_usage "$pod_name" "/var/lib/postgresql/wal")
    if [ "$wal_usage" -gt "$MAX_WAL_USAGE" ]; then
      MAX_WAL_USAGE=$wal_usage
    fi
  fi
done < <(echo "$PODS" | jq -r '.items[] | .metadata.name + "|" + (.metadata.labels."cnpg.io/instanceRole" // "unknown") + "|" + (.metadata.labels | tostring)')

info "Maximum disk usage - Storage: $MAX_STORAGE_USAGE%, WAL: $MAX_WAL_USAGE%"

# Determine if resize is needed
NEED_STORAGE_RESIZE=false
NEED_WAL_RESIZE=false
RESIZE_PERFORMED=false

if [ "$MAX_STORAGE_USAGE" -ge "$RESIZE_THRESHOLD" ]; then
  NEED_STORAGE_RESIZE=true
  warn "Storage PVC usage ($MAX_STORAGE_USAGE%) exceeds threshold ($RESIZE_THRESHOLD%)"
fi

if [ "$WAL_VOLUME_TYPE" = "separate" ] && [ "$MAX_WAL_USAGE" -ge "$RESIZE_THRESHOLD" ]; then
  NEED_WAL_RESIZE=true
  warn "WAL PVC usage ($MAX_WAL_USAGE%) exceeds threshold ($RESIZE_THRESHOLD%)"
fi

# Perform resize if needed
if [ "$NEED_STORAGE_RESIZE" = true ] || [ "$NEED_WAL_RESIZE" = true ]; then
  info "PVC resize required"
  
  # Calculate new sizes
  if [ "$NEED_STORAGE_RESIZE" = true ]; then
    NEW_STORAGE_SIZE=$(calculate_new_size "$STORAGE_SIZE" "$RESIZE_INCREMENT")
    info "New storage size will be: $NEW_STORAGE_SIZE (from $STORAGE_SIZE)"
  fi
  
  if [ "$NEED_WAL_RESIZE" = true ] && [ "$WAL_STORAGE_SIZE" != "none" ]; then
    NEW_WAL_SIZE=$(calculate_new_size "$WAL_STORAGE_SIZE" "$RESIZE_INCREMENT")
    info "New WAL size will be: $NEW_WAL_SIZE (from $WAL_STORAGE_SIZE)"
  fi
  
  if [ "$DRY_RUN" = true ]; then
    info "[DRY-RUN] Would resize PVCs but skipping due to dry-run mode"
  else
    # Directly patch PVCs instead of cluster spec (works even if cluster is in bad state)
    info "Resizing PVCs directly..."
    
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
          RESIZE_PERFORMED=true
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
          RESIZE_PERFORMED=true
        else
          warn "Failed to patch PVC $pvc"
        fi
      done
    fi
    
    if [ "$RESIZE_PERFORMED" = true ]; then
      success "PVC resize requests submitted!"
      
      # Wait a bit for storage provider to process
      info "Waiting 30 seconds for storage provider to process resize..."
      sleep 30
      
      # Also update cluster spec for consistency (this may fail if cluster is in bad state)
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
      
      # CloudNativePG will automatically detect the PVC resize
      if [ "$RECONCILE_AFTER_RESIZE" = true ]; then
        info "CloudNativePG will automatically detect the PVC resize and update disk space status"
        info "This typically happens within a few minutes"
      fi
    else
      error "Failed to resize any PVCs"
    fi
  fi
else
  success "All PVCs are within acceptable usage thresholds"
fi

# Final summary
info "=== Disk Management Summary ==="
info "Cluster: $CLUSTER_NAME (namespace: $NAMESPACE)"
info "Storage usage: $MAX_STORAGE_USAGE% (threshold: $RESIZE_THRESHOLD%)"
if [ "$WAL_VOLUME_TYPE" = "separate" ]; then
  info "WAL usage: $MAX_WAL_USAGE% (threshold: $RESIZE_THRESHOLD%)"
fi

if [ "$RESIZE_PERFORMED" = true ]; then
  success "PVC resize completed successfully"
  echo ""
  echo "Monitor progress with:"
  echo "  kubectl cnpg status -n $NAMESPACE $CLUSTER_NAME"
  echo "  kubectl get pvc -n $NAMESPACE -l cnpg.io/cluster=$CLUSTER_NAME"
  exit 0
elif [ "$NEED_STORAGE_RESIZE" = true ] || [ "$NEED_WAL_RESIZE" = true ]; then
  if [ "$DRY_RUN" = true ]; then
    warn "Resize needed but skipped due to dry-run mode"
    exit 0
  else
    exit 2  # Warning state
  fi
else
  exit 0  # Success - no action needed
fi
