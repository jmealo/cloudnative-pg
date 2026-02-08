#!/bin/bash

# CloudNativePG WAL Retention Troubleshooting Script
# This script helps identify why WAL files are being retained

set -euo pipefail

# Color codes for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
MAGENTA='\033[0;35m'
CYAN='\033[0;36m'
NC='\033[0m' # No Color

# Default values
NAMESPACE=""
CLUSTER=""
VERBOSE=false

# Function to display usage
usage() {
    cat << EOF
Usage: $0 --namespace <namespace> --cluster <cluster-name> [options]

This script troubleshoots WAL retention issues in CloudNativePG clusters.

Required arguments:
    --namespace, -n <namespace>     Kubernetes namespace
    --cluster, -c <cluster-name>    CloudNativePG cluster name

Optional arguments:
    --verbose, -v                   Enable verbose output
    --help, -h                      Show this help message

Examples:
    $0 --namespace db --cluster production-pg-common1
    $0 -n db -c production-pg-common1 -v
EOF
}

# Parse command line arguments
parse_args() {
    while [[ $# -gt 0 ]]; do
        case $1 in
            --namespace|-n)
                NAMESPACE="$2"
                shift 2
                ;;
            --cluster|-c)
                CLUSTER="$2"
                shift 2
                ;;
            --verbose|-v)
                VERBOSE=true
                shift
                ;;
            --help|-h)
                usage
                exit 0
                ;;
            *)
                echo -e "${RED}ERROR: Unknown option: $1${NC}" >&2
                usage
                exit 1
                ;;
        esac
    done

    # Validate required arguments
    if [[ -z "$NAMESPACE" || -z "$CLUSTER" ]]; then
        echo -e "${RED}ERROR: Both --namespace and --cluster are required${NC}" >&2
        usage
        exit 1
    fi
}

# Function to print info messages
info() {
    echo -e "${BLUE}INFO:${NC} $1" >&2
}

# Function to print success messages
success() {
    echo -e "${GREEN}SUCCESS:${NC} $1" >&2
}

# Function to print warning messages
warning() {
    echo -e "${YELLOW}WARNING:${NC} $1" >&2
}

# Function to print error messages
error() {
    echo -e "${RED}ERROR:${NC} $1" >&2
}

# Function to print verbose messages
verbose() {
    if [[ "$VERBOSE" == true ]]; then
        echo -e "${CYAN}VERBOSE:${NC} $1" >&2
    fi
}

# Function to get instance pods (excluding poolers and snapshot-recovery)
get_instance_pods() {
    kubectl get pods -n "$NAMESPACE" -l "cnpg.io/cluster=$CLUSTER" -o json | \
        jq -r '.items[] | 
            select(.metadata.labels."cnpg.io/instanceName" != null and 
                   (.metadata.name | test("pooler|snapshot-recovery") | not)) | 
            .metadata.name' 2>/dev/null || echo ""
}

# Function to get primary pod
get_primary_pod() {
    kubectl get pods -n "$NAMESPACE" -l "cnpg.io/cluster=$CLUSTER,cnpg.io/instanceRole=primary" -o name 2>/dev/null | \
        sed 's|pod/||' | head -1
}

# Function to check WAL archiving status
check_wal_archiving() {
    local pod=$1
    local role=$2
    
    echo -e "\n${MAGENTA}=== WAL Archiving Status on $pod ($role) ===${NC}"
    
    # Check pg_stat_archiver
    info "Checking pg_stat_archiver..."
    kubectl exec -n "$NAMESPACE" "$pod" -c postgres -- psql -U postgres -c "
        SELECT 
            archived_count,
            failed_count,
            last_archived_wal,
            last_archived_time,
            last_failed_wal,
            last_failed_time,
            CASE 
                WHEN last_archived_time IS NOT NULL THEN 
                    round(extract(epoch from (now() - last_archived_time))/60, 2) 
                ELSE NULL 
            END as minutes_since_last_archive
        FROM pg_stat_archiver;
    " 2>&1

    # Check archive command
    info "Checking archive command configuration..."
    kubectl exec -n "$NAMESPACE" "$pod" -c postgres -- psql -U postgres -c "
        SHOW archive_command;
        SHOW archive_mode;
        SHOW archive_timeout;
    " 2>&1
}

# Function to check replication lag
check_replication_lag() {
    local primary_pod=$1
    
    echo -e "\n${MAGENTA}=== Replication Lag Status ===${NC}"
    
    info "Checking replication status on primary..."
    kubectl exec -n "$NAMESPACE" "$primary_pod" -c postgres -- psql -U postgres -c "
        SELECT 
            application_name,
            client_addr,
            state,
            sync_state,
            pg_wal_lsn_diff(pg_current_wal_lsn(), sent_lsn) as sent_lag_bytes,
            pg_wal_lsn_diff(sent_lsn, write_lsn) as write_lag_bytes,
            pg_wal_lsn_diff(write_lsn, flush_lsn) as flush_lag_bytes,
            pg_wal_lsn_diff(flush_lsn, replay_lsn) as replay_lag_bytes,
            pg_size_pretty(pg_wal_lsn_diff(pg_current_wal_lsn(), replay_lsn)) as total_lag
        FROM pg_stat_replication
        ORDER BY application_name;
    " 2>&1
}

# Function to check WAL retention settings
check_wal_retention() {
    local pod=$1
    local role=$2
    
    echo -e "\n${MAGENTA}=== WAL Retention Settings on $pod ($role) ===${NC}"
    
    info "Checking WAL retention parameters..."
    kubectl exec -n "$NAMESPACE" "$pod" -c postgres -- psql -U postgres -c "
        SHOW wal_keep_size;
        SHOW max_wal_size;
        SHOW min_wal_size;
        SHOW checkpoint_timeout;
        SHOW checkpoint_completion_target;
    " 2>&1
}

# Function to check WAL files
check_wal_files() {
    local pod=$1
    local role=$2
    
    echo -e "\n${MAGENTA}=== WAL Files on $pod ($role) ===${NC}"
    
    info "Counting WAL files..."
    kubectl exec -n "$NAMESPACE" "$pod" -c postgres -- bash -c '
        WAL_DIR=$(psql -U postgres -tAc "SHOW data_directory")/pg_wal
        echo "WAL Directory: $WAL_DIR"
        echo "Total WAL files: $(ls -1 $WAL_DIR | grep -E "^[0-9A-F]{24}$" | wc -l)"
        echo "WAL directory size: $(du -sh $WAL_DIR 2>/dev/null | cut -f1)"
        echo ""
        echo "Oldest WAL files:"
        ls -la $WAL_DIR | grep -E "^[0-9A-F]{24}$" | head -5
        echo ""
        echo "Newest WAL files:"
        ls -la $WAL_DIR | grep -E "^[0-9A-F]{24}$" | tail -5
    ' 2>&1
}

# Function to check long-running transactions
check_long_transactions() {
    local pod=$1
    local role=$2
    
    echo -e "\n${MAGENTA}=== Long-Running Transactions on $pod ($role) ===${NC}"
    
    info "Checking for long-running transactions..."
    kubectl exec -n "$NAMESPACE" "$pod" -c postgres -- psql -U postgres -c "
        SELECT 
            pid,
            usename,
            application_name,
            state,
            backend_start,
            xact_start,
            query_start,
            state_change,
            EXTRACT(EPOCH FROM (now() - xact_start))::INT as xact_duration_seconds,
            LEFT(query, 100) as query_preview
        FROM pg_stat_activity
        WHERE xact_start IS NOT NULL
        ORDER BY xact_start
        LIMIT 10;
    " 2>&1
}

# Function to check base backup information
check_base_backups() {
    local primary_pod=$1
    
    echo -e "\n${MAGENTA}=== Base Backup Information ===${NC}"
    
    info "Checking for active base backups..."
    kubectl exec -n "$NAMESPACE" "$primary_pod" -c postgres -- psql -U postgres -c "
        SELECT * FROM pg_stat_progress_basebackup;
    " 2>&1
    
    # Check CloudNativePG backup status
    info "Checking CloudNativePG backup objects..."
    kubectl get backups -n "$NAMESPACE" -l "cnpg.io/cluster=$CLUSTER" -o wide 2>&1
}

# Function to check disk space
check_disk_space() {
    local pod=$1
    local role=$2
    
    echo -e "\n${MAGENTA}=== Disk Space on $pod ($role) ===${NC}"
    
    info "Checking disk usage..."
    kubectl exec -n "$NAMESPACE" "$pod" -c postgres -- bash -c '
        df -h | grep -E "(pgdata|pg_wal|Filesystem)"
    ' 2>&1
}

# Function to check CloudNativePG cluster status
check_cluster_status() {
    echo -e "\n${MAGENTA}=== CloudNativePG Cluster Status ===${NC}"
    
    info "Getting cluster resource..."
    kubectl get cluster "$CLUSTER" -n "$NAMESPACE" -o yaml | \
        yq eval '.status.conditions[] | select(.type == "Ready" or .type == "Healthy")' - 2>&1
    
    info "Getting cluster WAL settings..."
    kubectl get cluster "$CLUSTER" -n "$NAMESPACE" -o yaml | \
        yq eval '.spec.postgresql.parameters | to_entries[] | select(.key | test("wal|archive|checkpoint"))' - 2>&1
}

# Function to provide recommendations
provide_recommendations() {
    echo -e "\n${MAGENTA}=== Recommendations ===${NC}"
    
    cat << EOF
Based on the analysis, check the following:

1. ${YELLOW}Archive Failures:${NC}
   - Look for failed_count > 0 in pg_stat_archiver
   - Check last_failed_time for recent failures
   - Verify archive_command is working properly

2. ${YELLOW}Replication Lag:${NC}
   - Check for large lag values in replication status
   - Ensure all replicas are connected and streaming

3. ${YELLOW}Long Transactions:${NC}
   - Look for transactions running for extended periods
   - These can prevent WAL cleanup

4. ${YELLOW}WAL Settings:${NC}
   - Review wal_keep_size setting
   - Check if checkpoint_timeout is too high

5. ${YELLOW}Disk Space:${NC}
   - Verify sufficient space in archive destination
   - Check if pg_wal is on a separate volume

6. ${YELLOW}CloudNativePG Specific:${NC}
   - Check backup retention policies
   - Verify no stuck backup jobs
   - Review cluster events for errors

For immediate relief:
- If archive is failing, fix the archive destination
- If replication is lagged, investigate slow replicas
- If long transactions exist, consider terminating them
- Run CHECKPOINT command to force WAL recycling
EOF
}

# Main execution
main() {
    parse_args "$@"
    
    echo -e "${GREEN}CloudNativePG WAL Retention Troubleshooter${NC}"
    echo -e "${GREEN}===========================================${NC}"
    info "Cluster: $CLUSTER"
    info "Namespace: $NAMESPACE"
    echo ""
    
    # Get primary pod
    PRIMARY_POD=$(get_primary_pod)
    if [[ -z "$PRIMARY_POD" ]]; then
        error "Could not find primary pod for cluster '$CLUSTER'"
        exit 1
    fi
    info "Primary pod: $PRIMARY_POD"
    
    # Check cluster status first
    check_cluster_status
    
    # Get all instance pods
    PODS=$(get_instance_pods)
    if [[ -z "$PODS" ]]; then
        error "No instance pods found for cluster '$CLUSTER'"
        exit 1
    fi
    
    # Check primary-specific information
    check_wal_archiving "$PRIMARY_POD" "primary"
    check_replication_lag "$PRIMARY_POD"
    check_base_backups "$PRIMARY_POD"
    
    # Check each pod
    while IFS= read -r pod; do
        [[ -z "$pod" ]] && continue
        
        if [[ "$pod" == "$PRIMARY_POD" ]]; then
            ROLE="primary"
        else
            ROLE="replica"
        fi
        
        check_wal_retention "$pod" "$ROLE"
        check_wal_files "$pod" "$ROLE"
        check_long_transactions "$pod" "$ROLE"
        check_disk_space "$pod" "$ROLE"
        
        # Only check first few pods unless verbose
        if [[ "$VERBOSE" != true && "$pod" != "$PRIMARY_POD" ]]; then
            info "Run with --verbose to check all replicas"
            break
        fi
    done <<< "$PODS"
    
    # Provide recommendations
    provide_recommendations
    
    success "WAL retention troubleshooting completed!"
}

# Run main function
main "$@"
