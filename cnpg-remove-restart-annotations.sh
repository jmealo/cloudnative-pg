#!/bin/bash

# Script to remove restart annotations from CloudNativePG cluster and pods
# This removes the kubectl.kubernetes.io/restartedAt annotation that forces rolling restarts

set -euo pipefail

# Color codes
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Configuration
NAMESPACE="db"
CLUSTER="production-pg-common1"
ANNOTATION="kubectl.kubernetes.io/restartedAt"

echo -e "${BLUE}CloudNativePG Restart Annotation Remover${NC}"
echo -e "${BLUE}========================================${NC}"
echo ""

# Check current annotations
echo -e "${YELLOW}Checking current restart annotations...${NC}"

# Check cluster annotation
echo -e "\n${BLUE}Cluster annotation:${NC}"
cluster_annotation=$(kubectl get cluster -n "$NAMESPACE" "$CLUSTER" -o jsonpath="{.metadata.annotations['kubectl\.kubernetes\.io/restartedAt']}" 2>/dev/null || echo "")
if [ -n "$cluster_annotation" ]; then
    echo -e "  ${CLUSTER}: ${cluster_annotation}"
else
    echo -e "  ${CLUSTER}: No restart annotation found"
fi

# Check pod annotations
echo -e "\n${BLUE}Pod annotations:${NC}"
pods_with_annotation=$(kubectl get pods -n "$NAMESPACE" -l cnpg.io/cluster="$CLUSTER" -o json | \
    jq -r '.items[] | select(.metadata.annotations."kubectl.kubernetes.io/restartedAt" != null) | .metadata.name')

if [ -z "$pods_with_annotation" ]; then
    echo -e "  No pods with restart annotations found"
else
    echo "$pods_with_annotation" | while read -r pod; do
        annotation_value=$(kubectl get pod -n "$NAMESPACE" "$pod" -o jsonpath="{.metadata.annotations['kubectl\.kubernetes\.io/restartedAt']}" 2>/dev/null)
        echo -e "  ${pod}: ${annotation_value}"
    done
fi

# Ask for confirmation
echo -e "\n${YELLOW}This will remove the restart annotations from:${NC}"
echo -e "  - Cluster: ${CLUSTER}"
if [ -n "$pods_with_annotation" ]; then
    echo -e "  - Pods:"
    echo "$pods_with_annotation" | while read -r pod; do
        echo -e "    - ${pod}"
    done
fi

echo -e "\n${RED}Note: Removing these annotations will NOT restart the pods.${NC}"
read -p "Do you want to proceed? (yes/no): " confirm

if [[ "$confirm" != "yes" ]]; then
    echo -e "${YELLOW}Operation cancelled${NC}"
    exit 0
fi

# Remove annotations
echo -e "\n${BLUE}Removing annotations...${NC}"

# Remove from cluster
if [ -n "$cluster_annotation" ]; then
    echo -e "Removing annotation from cluster ${CLUSTER}..."
    if kubectl annotate cluster -n "$NAMESPACE" "$CLUSTER" "${ANNOTATION}-" 2>&1; then
        echo -e "${GREEN}✓ Removed annotation from cluster${NC}"
    else
        echo -e "${RED}✗ Failed to remove annotation from cluster${NC}"
    fi
else
    echo -e "${GREEN}✓ Cluster has no restart annotation${NC}"
fi

# Remove from pods
if [ -n "$pods_with_annotation" ]; then
    echo -e "\nRemoving annotations from pods..."
    while read -r pod; do
        echo -e "  Processing ${pod}..."
        if kubectl annotate pod -n "$NAMESPACE" "$pod" "${ANNOTATION}-" 2>&1; then
            echo -e "  ${GREEN}✓ Removed annotation from ${pod}${NC}"
        else
            echo -e "  ${RED}✗ Failed to remove annotation from ${pod}${NC}"
        fi
    done <<< "$pods_with_annotation"
else
    echo -e "${GREEN}✓ No pods have restart annotations${NC}"
fi

# Verify removal
echo -e "\n${BLUE}Verifying annotation removal...${NC}"

# Check cluster
remaining_cluster_annotation=$(kubectl get cluster -n "$NAMESPACE" "$CLUSTER" -o jsonpath="{.metadata.annotations['kubectl\.kubernetes\.io/restartedAt']}" 2>/dev/null || echo "")
if [ -z "$remaining_cluster_annotation" ]; then
    echo -e "${GREEN}✓ Cluster annotation successfully removed${NC}"
else
    echo -e "${RED}✗ Cluster still has annotation: ${remaining_cluster_annotation}${NC}"
fi

# Check pods
remaining_pods=$(kubectl get pods -n "$NAMESPACE" -l cnpg.io/cluster="$CLUSTER" -o json | \
    jq -r '.items[] | select(.metadata.annotations."kubectl.kubernetes.io/restartedAt" != null) | .metadata.name')

if [ -z "$remaining_pods" ]; then
    echo -e "${GREEN}✓ All pod annotations successfully removed${NC}"
else
    echo -e "${RED}✗ Some pods still have annotations:${NC}"
    echo "$remaining_pods" | while read -r pod; do
        echo -e "  - ${pod}"
    done
fi

echo -e "\n${GREEN}Operation completed!${NC}"
