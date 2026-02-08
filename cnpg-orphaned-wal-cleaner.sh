#!/bin/bash

# Script to identify and clean up orphaned WAL files in CloudNativePG replicas
# These are WAL files that have no archive status and are not part of the current replication stream

set -euo pipefail

# Color codes
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Default values
NAMESPACE=""
CLUSTER=""
POD=""
DRY_RUN=true

# Function to display usage
usage() {
    echo "Usage: $0 -n <namespace> -c <cluster-name> -p <pod-name> [--execute]"
    echo "Options:"
    echo "  -n, --namespace    Kubernetes namespace"
    echo "  -c, --cluster      CloudNativePG cluster name"
    echo "  -p, --pod          Specific pod to clean (optional, will prompt if not provided)"
    echo "  --execute          Actually remove files (default is dry-run)"
    echo ""
    echo "Example:"
    echo "  $0 -n db -c production-pg-common1 -p production-pg-common1-16"
    echo "  $0 -n db -c production-pg-common1 -p production-pg-common1-16 --execute"
    exit 1
}

# Parse command line arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        -n|--namespace)
            NAMESPACE="$2"
            shift 2
            ;;
        -c|--cluster)
            CLUSTER="$2"
            shift 2
            ;;
        -p|--pod)
            POD="$2"
            shift 2
            ;;
        --execute)
            DRY_RUN=false
            shift
            ;;
        -h|--help)
            usage
            ;;
        *)
            echo "Unknown option: $1"
            usage
            ;;
    esac
done

# Validate required parameters
if [[ -z "$NAMESPACE" || -z "$CLUSTER" ]]; then
    echo -e "${RED}Error: Namespace and cluster name are required${NC}"
    usage
fi

# Function to check if pod is a replica
is_replica() {
    local pod=$1
    local is_recovery=$(kubectl exec -n "$NAMESPACE" "$pod" -c postgres -- psql -U postgres -t -c "SELECT pg_is_in_recovery();" 2>/dev/null | tr -d ' ')
    [[ "$is_recovery" == "t" ]]
}

# Function to analyze WAL files
analyze_wal_files() {
    local pod=$1
    
    echo -e "${BLUE}Analyzing WAL files on $pod...${NC}" >&2
    
    # Get current timeline and LSN
    local current_info=$(kubectl exec -n "$NAMESPACE" "$pod" -c postgres -- psql -U postgres -t -c "
        SELECT 
            timeline_id,
            pg_current_wal_lsn()::text as current_lsn,
            pg_walfile_name(pg_current_wal_lsn()) as current_wal
        FROM pg_control_checkpoint();" 2>/dev/null | tr -d ' ')
    
    local timeline=$(echo "$current_info" | cut -d'|' -f1)
    local current_wal=$(echo "$current_info" | cut -d'|' -f3)
    
    echo -e "${GREEN}Current timeline: $timeline${NC}" >&2
    echo -e "${GREEN}Current WAL file: $current_wal${NC}" >&2
    
    # Find orphaned WAL files
    echo -e "\n${YELLOW}Checking for orphaned WAL files...${NC}" >&2
    
    kubectl exec -n "$NAMESPACE" "$pod" -c postgres -- bash -c '
        cd /var/lib/postgresql/wal/pg_wal
        
        # Get list of all WAL files
        for wal_file in $(ls -1 | grep -E "^[0-9A-F]{24}$"); do
            # Check if file has any archive status
            if [[ ! -f "archive_status/${wal_file}.done" && ! -f "archive_status/${wal_file}.ready" ]]; then
                # Get file info
                size=$(stat -c "%s" "$wal_file" 2>/dev/null || echo "0")
                mtime=$(stat -c "%Y" "$wal_file" 2>/dev/null || echo "0")
                current_time=$(date +%s)
                age_days=$(( (current_time - mtime) / 86400 ))
                
                # Only report files older than 1 day
                if [[ $age_days -gt 1 ]]; then
                    echo "${wal_file}|${size}|${age_days}"
                fi
            fi
        done
    ' 2>/dev/null
}

# Function to remove orphaned files
remove_orphaned_files() {
    local pod=$1
    local files=("${@:2}")
    
    if [[ ${#files[@]} -eq 0 ]]; then
        echo -e "${GREEN}No orphaned files to remove${NC}"
        return
    fi
    
    echo -e "\n${YELLOW}Removing ${#files[@]} orphaned WAL files...${NC}"
    
    for file in "${files[@]}"; do
        echo -e "Removing: $file"
        kubectl exec -n "$NAMESPACE" "$pod" -c postgres -- rm -f "/var/lib/postgresql/wal/pg_wal/$file"
    done
    
    echo -e "${GREEN}Removed ${#files[@]} orphaned WAL files${NC}"
}

# Main execution
echo -e "${BLUE}CloudNativePG Orphaned WAL File Cleaner${NC}"
echo -e "${BLUE}========================================${NC}"

# If pod not specified, list available pods
if [[ -z "$POD" ]]; then
    echo -e "\n${YELLOW}Available pods in cluster $CLUSTER:${NC}"
    kubectl get pods -n "$NAMESPACE" -l cnpg.io/cluster="$CLUSTER" -o name | sed 's|pod/||' | while read -r pod; do
        if is_replica "$pod"; then
            echo "  $pod (replica)"
        else
            echo "  $pod (primary)"
        fi
    done
    
    echo -e "\n${YELLOW}Please specify a pod with -p <pod-name>${NC}"
    exit 1
fi

# Verify pod exists and is a replica
if ! kubectl get pod -n "$NAMESPACE" "$POD" &>/dev/null; then
    echo -e "${RED}Error: Pod $POD not found in namespace $NAMESPACE${NC}"
    exit 1
fi

if ! is_replica "$POD"; then
    echo -e "${RED}Error: $POD is not a replica. This script is only for cleaning orphaned files on replicas.${NC}"
    exit 1
fi

# Analyze files
echo -e "\n${BLUE}Analyzing orphaned WAL files on $POD...${NC}"
orphaned_files=$(analyze_wal_files "$POD")

if [[ -z "$orphaned_files" ]]; then
    echo -e "${GREEN}No orphaned WAL files found${NC}"
    exit 0
fi

# Display findings
echo -e "\n${YELLOW}Found orphaned WAL files:${NC}"
echo -e "File Name                     Size (MB)  Age (days)"
echo -e "---------------------------------------------------"

total_size=0
file_list=()

while IFS='|' read -r filename size age_days; do
    size_mb=$(( size / 1024 / 1024 ))
    total_size=$(( total_size + size ))
    file_list+=("$filename")
    printf "%-28s %9d  %10d\n" "$filename" "$size_mb" "$age_days"
done <<< "$orphaned_files"

total_size_mb=$(( total_size / 1024 / 1024 ))
echo -e "---------------------------------------------------"
echo -e "Total: ${#file_list[@]} files, ${total_size_mb} MB"

# Check disk usage before
echo -e "\n${BLUE}Current WAL disk usage:${NC}"
kubectl exec -n "$NAMESPACE" "$POD" -c postgres -- df -h /var/lib/postgresql/wal

if [[ "$DRY_RUN" == true ]]; then
    echo -e "\n${YELLOW}DRY RUN MODE - No files will be removed${NC}"
    echo -e "To actually remove these files, run with --execute flag"
else
    echo -e "\n${RED}WARNING: This will permanently remove ${#file_list[@]} WAL files (${total_size_mb} MB)${NC}"
    read -p "Are you sure you want to proceed? (yes/no): " confirm
    
    if [[ "$confirm" == "yes" ]]; then
        remove_orphaned_files "$POD" "${file_list[@]}"
        
        # Show disk usage after
        echo -e "\n${BLUE}WAL disk usage after cleanup:${NC}"
        kubectl exec -n "$NAMESPACE" "$POD" -c postgres -- df -h /var/lib/postgresql/wal
    else
        echo -e "${YELLOW}Operation cancelled${NC}"
    fi
fi
