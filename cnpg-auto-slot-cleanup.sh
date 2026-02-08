#!/bin/bash

# cnpg-auto-slot-cleanup.sh
#
# Automated script to clean up inactive replication slots when WAL space usage
# exceeds a configurable threshold. Designed for non-interactive execution.

set -e
set -o pipefail

# --- Default values ---
CLUSTER_NAME=""
NAMESPACE=""
WAL_THRESHOLD=75  # Default threshold percentage
DRY_RUN=false
LOG_FILE=""
FORCE_DROP=true  # Non-interactive by default

# --- Function to display usage ---
usage() {
  echo "Usage: $0 -c <cluster-name> -n <namespace> [options]"
  echo "  -c, --cluster       The name of the CloudNativePG cluster."
  echo "  -n, --namespace     The namespace where the cluster is running."
  echo "  -t, --threshold     WAL usage percentage threshold to trigger cleanup (default: 75)."
  echo "  -l, --log-file      Log file path for operation logging."
  echo "  --dry-run           Show what would be done without actually dropping slots."
  echo "  -h, --help          Display this help message."
  echo ""
  echo "Example: $0 -c my-cluster -n db -t 80 -l /var/log/cnpg-cleanup.log"
  exit 1
}

# --- Parse command-line arguments ---
while [[ "$#" -gt 0 ]]; do
  case $1 in
    -c|--cluster) CLUSTER_NAME="$2"; shift ;;
    -n|--namespace) NAMESPACE="$2"; shift ;;
    -t|--threshold) WAL_THRESHOLD="$2"; shift ;;
    -l|--log-file) LOG_FILE="$2"; shift ;;
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

# Validate threshold
if ! [[ "$WAL_THRESHOLD" =~ ^[0-9]+$ ]] || [ "$WAL_THRESHOLD" -lt 1 ] || [ "$WAL_THRESHOLD" -gt 100 ]; then
  echo "Error: Threshold must be a number between 1 and 100."
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

# --- Main logic ---
info "Starting automated replication slot cleanup for cluster '$CLUSTER_NAME' in namespace '$NAMESPACE'"
info "WAL usage threshold: $WAL_THRESHOLD%"
if [ "$DRY_RUN" = true ]; then
  info "Running in DRY-RUN mode - no slots will be dropped"
fi

# Check for kubectl
if ! command -v kubectl &> /dev/null; then
  error "kubectl command not found. Please ensure it's installed and in your PATH."
fi

# Check for jq
if ! command -v jq &> /dev/null; then
  error "jq command not found. Please install jq to use this script."
fi

# Get all pods in the cluster - filter for actual instances only
info "Fetching pods for cluster '$CLUSTER_NAME'..."
PODS=$(kubectl get pods -n "$NAMESPACE" -l "cnpg.io/cluster=$CLUSTER_NAME" -o json 2>/dev/null | \
  jq '{items: [.items[] | select(.metadata.labels."cnpg.io/instanceName" != null and (.metadata.name | test("pooler|snapshot-recovery") | not))]}')

if [ -z "$(echo "$PODS" | jq -r '.items[]' 2>/dev/null)" ]; then
  error "No instance pods found for cluster '$CLUSTER_NAME' in namespace '$NAMESPACE'."
fi

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

# Function to check disk usage on a pod
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

# Function to execute SQL query on a pod
exec_sql() {
  local pod=$1
  local query=$2
  kubectl exec -n "$NAMESPACE" "$pod" -c postgres -- psql -U postgres -d postgres -t -A -c "$query" 2>/dev/null || echo ""
}

# Function to get inactive replication slots
get_inactive_slots() {
  local pod=$1
  
  local query="
  WITH slot_info AS (
    SELECT 
      s.slot_name,
      s.slot_type,
      s.active,
      s.active_pid,
      CASE 
        WHEN s.restart_lsn IS NOT NULL THEN 
          pg_wal_lsn_diff(pg_current_wal_lsn(), s.restart_lsn)
        ELSE 0
      END AS retained_bytes,
      CASE
        WHEN s.active_pid IS NOT NULL THEN 't'
        WHEN EXISTS (SELECT 1 FROM pg_stat_replication r WHERE r.slot_name = s.slot_name) THEN 't'
        ELSE s.active::text
      END AS real_active
    FROM pg_replication_slots s
  )
  SELECT 
    slot_name || '|' || retained_bytes::text
  FROM slot_info
  WHERE real_active = 'f'
  ORDER BY retained_bytes DESC;
  "
  
  exec_sql "$pod" "$query"
}

# Function to drop a replication slot
drop_slot() {
  local pod=$1
  local slot=$2
  
  # Double-check slot is inactive before dropping
  local check_query="SELECT active, active_pid FROM pg_replication_slots WHERE slot_name = '$slot';"
  local check_result=$(exec_sql "$pod" "$check_query")
  
  if [ -n "$check_result" ]; then
    IFS='|' read -r is_active active_pid <<< "$check_result"
    if [ "$is_active" = "t" ] || [ -n "$active_pid" ]; then
      warn "Slot '$slot' is currently active (PID: ${active_pid:-unknown}), skipping..."
      return 1
    fi
  fi
  
  if [ "$DRY_RUN" = true ]; then
    info "[DRY-RUN] Would drop slot '$slot' on pod '$pod'"
    return 0
  fi
  
  local drop_query="SELECT pg_drop_replication_slot('$slot');"
  
  if result=$(kubectl exec -n "$NAMESPACE" "$pod" -c postgres -- psql -U postgres -d postgres -t -A -c "$drop_query" 2>&1); then
    success "Dropped slot '$slot' on pod '$pod'"
    return 0
  else
    warn "Failed to drop slot '$slot' on pod '$pod': $result"
    return 1
  fi
}

# Check if cluster uses separate WAL volume
FIRST_POD=$(echo "$PODS" | jq -r '.items[0].metadata.name')
WAL_VOLUME_TYPE=$(check_wal_volume "$FIRST_POD")

info "WAL volume configuration: $WAL_VOLUME_TYPE"

# Determine WAL path based on volume type
if [ "$WAL_VOLUME_TYPE" = "separate" ]; then
  WAL_PATH="/var/lib/postgresql/wal"
else
  WAL_PATH="/var/lib/postgresql/data"
fi

# Track cleanup statistics
TOTAL_SLOTS_DROPPED=0
TOTAL_BYTES_FREED=0
PODS_OVER_THRESHOLD=0

# Check each pod for WAL usage
while IFS='|' read -r pod_name role labels; do
  # Get WAL usage percentage
  wal_usage=$(check_disk_usage "$pod_name" "$WAL_PATH")
  
  info "Pod $pod_name: WAL usage at $wal_usage%"
  
  if [ "$wal_usage" -ge "$WAL_THRESHOLD" ]; then
    PODS_OVER_THRESHOLD=$((PODS_OVER_THRESHOLD + 1))
    warn "Pod $pod_name exceeds threshold ($wal_usage% >= $WAL_THRESHOLD%)"
    
    # Only check primary for replication slots
    if [ "$role" = "primary" ]; then
      info "Checking for inactive replication slots on primary $pod_name..."
      
      # Get inactive slots
      inactive_slots=$(get_inactive_slots "$pod_name")
      
      if [ -n "$inactive_slots" ]; then
        while IFS='|' read -r slot_name retained_bytes; do
          [ -z "$slot_name" ] && continue
          
          info "Found inactive slot '$slot_name' retaining $retained_bytes bytes"
          
          if drop_slot "$pod_name" "$slot_name"; then
            TOTAL_SLOTS_DROPPED=$((TOTAL_SLOTS_DROPPED + 1))
            TOTAL_BYTES_FREED=$((TOTAL_BYTES_FREED + retained_bytes))
          fi
        done <<< "$inactive_slots"
      else
        info "No inactive replication slots found on $pod_name"
      fi
    else
      info "Skipping replica $pod_name (replication slots only exist on primary)"
    fi
  fi
done < <(echo "$PODS" | jq -r '.items[] | .metadata.name + "|" + (.metadata.labels."cnpg.io/instanceRole" // "unknown") + "|" + (.metadata.labels | tostring)')

# Final summary
info "=== Cleanup Summary ==="
info "Pods over threshold: $PODS_OVER_THRESHOLD"
info "Slots dropped: $TOTAL_SLOTS_DROPPED"
info "WAL space freed: $TOTAL_BYTES_FREED bytes"

if [ "$PODS_OVER_THRESHOLD" -eq 0 ]; then
  success "All pods are within WAL usage threshold"
elif [ "$TOTAL_SLOTS_DROPPED" -gt 0 ]; then
  success "Cleanup completed successfully"
else
  warn "WAL usage high but no inactive slots found to clean"
fi

# Exit with appropriate code
if [ "$PODS_OVER_THRESHOLD" -gt 0 ] && [ "$TOTAL_SLOTS_DROPPED" -eq 0 ]; then
  exit 2  # Warning state - high usage but nothing to clean
else
  exit 0  # Success
fi
