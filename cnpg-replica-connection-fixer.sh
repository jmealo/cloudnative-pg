#!/bin/bash

# CloudNativePG Replica Connection Troubleshooter and Fixer
# This script diagnoses and helps fix replica connection issues

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
FIX=false
VERBOSE=false

# Function to display usage
usage() {
    cat << EOF
Usage: $0 --namespace <namespace> --cluster <cluster-name> [options]

This script diagnoses and helps fix replica connection issues in CloudNativePG clusters.

Required arguments:
    --namespace, -n <namespace>     Kubernetes namespace
    --cluster, -c <cluster-name>    CloudNativePG cluster name

Optional arguments:
    --fix, -f                       Attempt to fix issues (requires confirmation)
    --verbose, -v                   Enable verbose output
    --help, -h                      Show this help message

Examples:
    $0 --namespace db --cluster production-pg-common1
    $0 -n db -c production-pg-common1 --fix
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
            --fix|-f)
                FIX=true
                shift
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

# Function to check pod status
check_pod_status() {
    local pod=$1
    
    echo -e "\n${MAGENTA}=== Pod Status: $pod ===${NC}"
    
    # Get pod details
    kubectl get pod "$pod" -n "$NAMESPACE" -o json | jq -r '
        {
            name: .metadata.name,
            ready: (.status.conditions[] | select(.type=="Ready") | .status),
            phase: .status.phase,
            restarts: (.status.containerStatuses[0].restartCount // 0),
            started: (.status.containerStatuses[0].started // false),
            state: (.status.containerStatuses[0].state | keys[0])
        }
    ' 2>&1
    
    # Check recent events
    info "Recent events for $pod:"
    kubectl get events -n "$NAMESPACE" --field-selector "involvedObject.name=$pod" \
        --sort-by='.lastTimestamp' | tail -5 2>&1
}

# Function to check connection parameters
check_connection_params() {
    local pod=$1
    local role=$2
    
    echo -e "\n${MAGENTA}=== Connection Parameters on $pod ($role) ===${NC}"
    
    # Check replication settings
    if [[ "$role" == "replica" ]]; then
        info "Checking primary_conninfo..."
        kubectl exec -n "$NAMESPACE" "$pod" -c postgres -- psql -U postgres -c "
            SELECT name, setting 
            FROM pg_settings 
            WHERE name IN ('primary_conninfo', 'primary_slot_name', 'hot_standby', 'hot_standby_feedback');
        " 2>&1
        
        # Check recovery status
        info "Checking recovery status..."
        kubectl exec -n "$NAMESPACE" "$pod" -c postgres -- psql -U postgres -c "
            SELECT pg_is_in_recovery(),
                   CASE 
                       WHEN pg_is_in_recovery() THEN 'Replica'
                       ELSE 'Primary'
                   END as role;
        " 2>&1
    fi
}

# Function to check network connectivity
check_network_connectivity() {
    local source_pod=$1
    local target_pod=$2
    
    echo -e "\n${MAGENTA}=== Network Connectivity from $source_pod to $target_pod ===${NC}"
    
    # Get target pod IP
    local target_ip=$(kubectl get pod "$target_pod" -n "$NAMESPACE" -o jsonpath='{.status.podIP}')
    
    if [[ -n "$target_ip" ]]; then
        info "Testing connectivity to $target_pod ($target_ip:5432)..."
        kubectl exec -n "$NAMESPACE" "$source_pod" -c postgres -- bash -c "
            timeout 5 bash -c '</dev/tcp/$target_ip/5432' && echo 'Port 5432 is open' || echo 'Port 5432 is not reachable'
        " 2>&1
    else
        error "Could not get IP for $target_pod"
    fi
}

# Function to check WAL receiver status on replicas
check_wal_receiver() {
    local pod=$1
    
    echo -e "\n${MAGENTA}=== WAL Receiver Status on $pod ===${NC}"
    
    kubectl exec -n "$NAMESPACE" "$pod" -c postgres -- psql -U postgres -c "
        SELECT 
            pid,
            status,
            receive_start_lsn,
            received_lsn,
            latest_end_lsn,
            latest_end_time,
            sender_host,
            sender_port,
            slot_name,
            conninfo
        FROM pg_stat_wal_receiver;
    " 2>&1
}

# Function to check cluster events
check_cluster_events() {
    echo -e "\n${MAGENTA}=== Recent Cluster Events ===${NC}"
    
    kubectl get events -n "$NAMESPACE" --field-selector "involvedObject.kind=Cluster,involvedObject.name=$CLUSTER" \
        --sort-by='.lastTimestamp' | tail -10 2>&1
}

# Function to restart pod
restart_pod() {
    local pod=$1
    
    warning "Restarting pod $pod..."
    kubectl delete pod "$pod" -n "$NAMESPACE" --grace-period=60 2>&1
    
    info "Waiting for pod to be recreated..."
    sleep 10
    
    # Wait for pod to be ready
    kubectl wait --for=condition=Ready pod -l "cnpg.io/instanceName=$pod" -n "$NAMESPACE" --timeout=120s 2>&1 || {
        error "Pod $pod did not become ready within 120 seconds"
        return 1
    }
    
    success "Pod $pod restarted successfully"
}

# Function to force checkpoint
force_checkpoint() {
    local primary_pod=$1
    
    warning "Forcing checkpoint on primary..."
    kubectl exec -n "$NAMESPACE" "$primary_pod" -c postgres -- psql -U postgres -c "CHECKPOINT;" 2>&1
    success "Checkpoint completed"
}

# Function to provide fix recommendations
provide_fix_recommendations() {
    echo -e "\n${MAGENTA}=== Fix Recommendations ===${NC}"
    
    cat << EOF
Based on the analysis, here are the recommended fixes:

1. ${YELLOW}Restart Disconnected Replicas:${NC}
   - Replicas showing no WAL receiver connection should be restarted
   - Use: kubectl delete pod <pod-name> -n $NAMESPACE

2. ${YELLOW}Force Checkpoint on Primary:${NC}
   - This can help recycle WAL files
   - Use: kubectl exec -n $NAMESPACE <primary-pod> -c postgres -- psql -U postgres -c "CHECKPOINT;"

3. ${YELLOW}Check CloudNativePG Operator:${NC}
   - Ensure the operator is running and healthy
   - Check: kubectl get pods -n cnpg-system

4. ${YELLOW}Review Cluster Configuration:${NC}
   - Check if maxSyncReplicas is set correctly
   - Review replication settings in the cluster spec

5. ${YELLOW}Manual WAL Cleanup (Emergency Only):${NC}
   - If WAL space is critical, consider manual cleanup
   - First fix replication, then PostgreSQL will clean up automatically

Run with --fix to attempt automatic fixes (with confirmation).
EOF
}

# Function to attempt fixes
attempt_fixes() {
    local primary_pod=$1
    local disconnected_replicas="$2"
    
    echo -e "\n${MAGENTA}=== Attempting Fixes ===${NC}"
    
    # Ask for confirmation
    echo -e "${YELLOW}The following actions will be performed:${NC}"
    echo "1. Force checkpoint on primary ($primary_pod)"
    if [[ -n "$disconnected_replicas" ]]; then
        echo "2. Restart disconnected replicas:"
        echo "$disconnected_replicas" | while read -r pod; do
            [[ -z "$pod" ]] && continue
            echo "   - $pod"
        done
    fi
    
    read -p "Do you want to proceed? (yes/no): " -r
    if [[ ! "$REPLY" =~ ^[Yy][Ee][Ss]$ ]]; then
        info "Fix operation cancelled"
        return
    fi
    
    # Force checkpoint first
    force_checkpoint "$primary_pod"
    
    # Restart disconnected replicas
    if [[ -n "$disconnected_replicas" ]]; then
        echo "$disconnected_replicas" | while read -r pod; do
            [[ -z "$pod" ]] && continue
            restart_pod "$pod"
        done
    fi
    
    info "Waiting 30 seconds for cluster to stabilize..."
    sleep 30
    
    # Check replication status again
    info "Checking replication status after fixes..."
    kubectl exec -n "$NAMESPACE" "$primary_pod" -c postgres -- psql -U postgres -c "
        SELECT application_name, state, sync_state 
        FROM pg_stat_replication;
    " 2>&1
}

# Main execution
main() {
    parse_args "$@"
    
    echo -e "${GREEN}CloudNativePG Replica Connection Troubleshooter${NC}"
    echo -e "${GREEN}===============================================${NC}"
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
    
    # Get all instance pods
    PODS=$(get_instance_pods)
    if [[ -z "$PODS" ]]; then
        error "No instance pods found for cluster '$CLUSTER'"
        exit 1
    fi
    
    # Check cluster events first
    check_cluster_events
    
    # Check primary replication status
    echo -e "\n${MAGENTA}=== Primary Replication Status ===${NC}"
    kubectl exec -n "$NAMESPACE" "$PRIMARY_POD" -c postgres -- psql -U postgres -c "
        SELECT application_name, client_addr, state, sync_state 
        FROM pg_stat_replication;
    " 2>&1
    
    # Track disconnected replicas
    DISCONNECTED_REPLICAS=""
    
    # Check each pod
    while IFS= read -r pod; do
        [[ -z "$pod" ]] && continue
        
        if [[ "$pod" == "$PRIMARY_POD" ]]; then
            ROLE="primary"
        else
            ROLE="replica"
        fi
        
        check_pod_status "$pod"
        check_connection_params "$pod" "$ROLE"
        
        if [[ "$ROLE" == "replica" ]]; then
            # Check WAL receiver
            WAL_RECEIVER_OUTPUT=$(check_wal_receiver "$pod")
            echo "$WAL_RECEIVER_OUTPUT"
            
            # Check if WAL receiver is active
            if ! echo "$WAL_RECEIVER_OUTPUT" | grep -q "streaming"; then
                warning "Replica $pod is not streaming from primary!"
                DISCONNECTED_REPLICAS+="$pod"$'\n'
            fi
            
            # Check network connectivity to primary
            check_network_connectivity "$pod" "$PRIMARY_POD"
        fi
    done <<< "$PODS"
    
    # Provide recommendations
    provide_fix_recommendations
    
    # Attempt fixes if requested
    if [[ "$FIX" == true ]]; then
        attempt_fixes "$PRIMARY_POD" "$DISCONNECTED_REPLICAS"
    fi
    
    success "Replica connection troubleshooting completed!"
}

# Run main function
main "$@"
