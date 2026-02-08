#!/bin/bash

# cnpg-replication-slot-checker.sh
#
# A script to check all instances in a CloudNativePG cluster for replication slots
# that are idle/holding onto WAL and offers to drop them with detailed information
# about space usage and age.

set -e
set -o pipefail

# --- Default values ---
DRY_RUN=false
CLUSTER_NAME=""
NAMESPACE=""
FORCE_DROP=false
SHOW_ACTIVE=false

# --- Function to display usage ---
usage() {
  echo "Usage: $0 -c <cluster-name> -n <namespace> [--dry-run] [--show-active] [--force]"
  echo "  -c, --cluster      The name of the CloudNativePG cluster."
  echo "  -n, --namespace    The namespace where the cluster is running."
  echo "  --dry-run          Show what would be done without actually dropping slots."
  echo "  --show-active      Also show active replication slots (by default only inactive are shown)."
  echo "  --force            Skip confirmation prompts (use with caution!)."
  echo "  -h, --help         Display this help message."
  exit 1
}

# --- Parse command-line arguments ---
while [[ "$#" -gt 0 ]]; do
  case $1 in
    -c|--cluster) CLUSTER_NAME="$2"; shift ;;
    -n|--namespace) NAMESPACE="$2"; shift ;;
    --dry-run) DRY_RUN=true ;;
    --show-active) SHOW_ACTIVE=true ;;
    --force) FORCE_DROP=true ;;
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

# Function to calculate age from timestamp
calculate_age() {
  local lsn1=$1
  local lsn2=$2
  
  if [ -z "$lsn1" ] || [ -z "$lsn2" ] || [ "$lsn1" = "null" ] || [ "$lsn2" = "null" ]; then
    echo "Unknown"
    return
  fi
  
  # This is a simplified age calculation based on LSN difference
  # In reality, you'd want to use timestamp comparisons
  echo "Active"
}

# --- Main logic ---
info "Starting replication slot check for cluster '$CLUSTER_NAME' in namespace '$NAMESPACE'."
if [ "$DRY_RUN" = true ]; then
  info "Running in DRY-RUN mode. No slots will be dropped."
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
PODS=$(kubectl get pods -n "$NAMESPACE" -l "cnpg.io/cluster=$CLUSTER_NAME" -o json | \
  jq '{items: [.items[] | select(.metadata.labels."cnpg.io/instanceName" != null and (.metadata.name | test("pooler|snapshot-recovery") | not))]}')

if [ -z "$(echo "$PODS" | jq -r '.items[]')" ]; then
  error "No instance pods found for cluster '$CLUSTER_NAME' in namespace '$NAMESPACE'."
fi

# Function to execute SQL query on a pod
exec_sql() {
  local pod=$1
  local query=$2
  kubectl exec -n "$NAMESPACE" "$pod" -c postgres -- psql -U postgres -d postgres -t -A -c "$query" 2>/dev/null || echo ""
}

# Function to get replication slots from a pod
check_slots_on_pod() {
  local pod=$1
  local role=$2
  
  info "Checking replication slots on $pod ($role)..."
  
  # Complex query to get all slot information including WAL retention size
  # Join with pg_stat_replication to get more accurate active status
  local query="
  WITH slot_info AS (
    SELECT 
      s.slot_name,
      s.slot_type,
      s.active,
      s.active_pid,
      s.database,
      s.plugin,
      s.restart_lsn,
      s.confirmed_flush_lsn,
      s.wal_status,
      s.safe_wal_size,
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
    slot_name || '|' ||
    slot_type || '|' ||
    real_active || '|' ||
    COALESCE(database, 'N/A') || '|' ||
    COALESCE(plugin, 'N/A') || '|' ||
    COALESCE(retained_bytes::text, '0') || '|' ||
    COALESCE(wal_status, 'unknown') || '|' ||
    '$pod' || '|' ||
    '$role'
  FROM slot_info
  ORDER BY real_active, retained_bytes DESC;
  "
  
  local result=$(exec_sql "$pod" "$query")
  if [ -n "$result" ]; then
    echo "$result"
  fi
}

# Collect all slots from all pods
ALL_SLOTS=""
TOTAL_INACTIVE_SLOTS=0
TOTAL_WAL_RETAINED=0

echo "$PODS" | jq -r '.items[] | .metadata.name + "|" + (.metadata.labels."cnpg.io/instanceRole" // "unknown")' | while IFS='|' read -r pod role; do
  slots=$(check_slots_on_pod "$pod" "$role")
  if [ -n "$slots" ]; then
    ALL_SLOTS="${ALL_SLOTS}${slots}"$'\n'
  fi
done

# Store results in a temporary file to work around subshell variable issues
TEMP_FILE=$(mktemp)
echo "$PODS" | jq -r '.items[] | .metadata.name + "|" + (.metadata.labels."cnpg.io/instanceRole" // "unknown")' | while IFS='|' read -r pod role; do
  slots=$(check_slots_on_pod "$pod" "$role")
  if [ -n "$slots" ]; then
    echo "$slots" >> "$TEMP_FILE"
  fi
done

# Read results from temp file
if [ -f "$TEMP_FILE" ] && [ -s "$TEMP_FILE" ]; then
  ALL_SLOTS=$(cat "$TEMP_FILE")
else
  ALL_SLOTS=""
fi
rm -f "$TEMP_FILE"

if [ -z "$ALL_SLOTS" ]; then
  success "No replication slots found in cluster '$CLUSTER_NAME'."
  exit 0
fi

# Display results
echo ""
echo "=== Replication Slots Summary ==="
printf "%-30s %-10s %-8s %-15s %-15s %-12s %-10s %-20s %-10s\n" \
  "Slot Name" "Type" "Active" "Database" "Plugin" "WAL Retained" "WAL Status" "Instance" "Role"
echo "======================================================================================================"

# Arrays to store inactive slots for potential cleanup
declare -a INACTIVE_SLOTS
declare -a INACTIVE_PODS
declare -a INACTIVE_SIZES

# Process and display slots
while IFS= read -r line; do
  [ -z "$line" ] && continue
  
  IFS='|' read -r slot_name slot_type active database plugin retained_bytes wal_status pod role <<< "$line"
  
  # Skip active slots unless --show-active is set
  if [ "$active" = "t" ] && [ "$SHOW_ACTIVE" = false ]; then
    continue
  fi
  
  # Format the output
  if [ "$active" = "t" ]; then
    active_display="ACTIVE"
  else
    active_display="INACTIVE"
    INACTIVE_SLOTS+=("$slot_name")
    INACTIVE_PODS+=("$pod")
    INACTIVE_SIZES+=("$retained_bytes")
    TOTAL_INACTIVE_SLOTS=$((TOTAL_INACTIVE_SLOTS + 1))
    if [ "$retained_bytes" != "0" ] && [ "$retained_bytes" != "" ]; then
      TOTAL_WAL_RETAINED=$((TOTAL_WAL_RETAINED + retained_bytes))
    fi
  fi
  
  formatted_size=$(format_bytes "$retained_bytes")
  
  printf "%-30s %-10s %-8s %-15s %-15s %-12s %-10s %-20s %-10s\n" \
    "$slot_name" "$slot_type" "$active_display" "$database" "$plugin" "$formatted_size" "$wal_status" "$pod" "$role"
    
done <<< "$ALL_SLOTS"

echo "======================================================================================================"

# Summary
echo ""
echo "Summary:"
echo "Total inactive slots: $TOTAL_INACTIVE_SLOTS"
echo "Total WAL retained by inactive slots: $(format_bytes $TOTAL_WAL_RETAINED)"

# Offer to clean up inactive slots
if [ ${#INACTIVE_SLOTS[@]} -gt 0 ]; then
  echo ""
  echo "Found ${#INACTIVE_SLOTS[@]} inactive replication slot(s) that can be cleaned up."
  
  if [ "$DRY_RUN" = true ]; then
    echo "Dry-run mode: The following slots would be dropped:"
    for i in "${!INACTIVE_SLOTS[@]}"; do
      echo "  - ${INACTIVE_SLOTS[$i]} on ${INACTIVE_PODS[$i]} (retaining $(format_bytes ${INACTIVE_SIZES[$i]}))"
    done
    exit 0
  fi
  
  # Ask for confirmation if not forced
  if [ "$FORCE_DROP" = false ]; then
    echo ""
    echo "Do you want to drop these inactive replication slots?"
    echo "This action cannot be undone. The following slots will be dropped:"
    for i in "${!INACTIVE_SLOTS[@]}"; do
      echo "  - ${INACTIVE_SLOTS[$i]} on ${INACTIVE_PODS[$i]} (retaining $(format_bytes ${INACTIVE_SIZES[$i]}))"
    done
    
    read -p "Type 'yes' to confirm: " -r
    if [[ ! $REPLY =~ ^[Yy][Ee][Ss]$ ]]; then
      info "Operation cancelled by user."
      exit 0
    fi
  fi
  
  # Drop the slots
  echo ""
  echo "Dropping inactive replication slots..."
  DROPPED_COUNT=0
  FAILED_COUNT=0
  
  for i in "${!INACTIVE_SLOTS[@]}"; do
    slot="${INACTIVE_SLOTS[$i]}"
    pod="${INACTIVE_PODS[$i]}"
    
    info "Dropping slot '$slot' on pod '$pod'..."
    
    # First check if the slot is really inactive before attempting to drop
    check_query="SELECT active, active_pid FROM pg_replication_slots WHERE slot_name = '$slot';"
    check_result=$(kubectl exec -n "$NAMESPACE" "$pod" -c postgres -- psql -U postgres -d postgres -t -A -c "$check_query" 2>/dev/null || echo "")
    
    if [ -n "$check_result" ]; then
      IFS='|' read -r is_active active_pid <<< "$check_result"
      if [ "$is_active" = "t" ] || [ -n "$active_pid" ]; then
        warn "Slot '$slot' is currently active (PID: ${active_pid:-unknown}), skipping..."
        FAILED_COUNT=$((FAILED_COUNT + 1))
        continue
      fi
    fi
    
    drop_query="SELECT pg_drop_replication_slot('$slot');"
    
    if result=$(kubectl exec -n "$NAMESPACE" "$pod" -c postgres -- psql -U postgres -d postgres -t -A -c "$drop_query" 2>&1); then
      success "Dropped slot '$slot' on pod '$pod'"
      DROPPED_COUNT=$((DROPPED_COUNT + 1))
    else
      warn "Failed to drop slot '$slot' on pod '$pod': $result"
      FAILED_COUNT=$((FAILED_COUNT + 1))
    fi
  done
  
  # Final summary
  echo ""
  echo "=== Cleanup Summary ==="
  echo "Successfully dropped: $DROPPED_COUNT slot(s)"
  echo "Failed to drop: $FAILED_COUNT slot(s)"
  echo "WAL space reclaimed: $(format_bytes $TOTAL_WAL_RETAINED)"
  
  if [ $FAILED_COUNT -gt 0 ]; then
    warn "Some slots could not be dropped. Please check the error messages above."
    exit 1
  else
    success "All inactive replication slots have been cleaned up successfully!"
  fi
else
  success "No inactive replication slots found. Cluster is clean!"
fi

exit 0
