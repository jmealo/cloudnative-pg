#!/bin/bash

# Test script to verify color rendering

# Color codes
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

echo "Testing color output methods:"
echo ""

echo "Method 1: Direct echo -e"
echo -e "${RED}This should be red${NC}"
echo -e "${GREEN}This should be green${NC}"
echo -e "${YELLOW}This should be yellow${NC}"
echo -e "${BLUE}This should be blue${NC}"

echo ""
echo "Method 2: Printf with color codes"
printf "${RED}%s${NC}\n" "This should be red"
printf "${GREEN}%s${NC}\n" "This should be green"

echo ""
echo "Method 3: Mixed formatting"
echo -e "Status: ${RED}INACTIVE${NC} | Size: ${YELLOW}51 GB${NC}"

echo ""
echo "Method 4: Table-like output"
echo -e "${YELLOW}Slot Name                      Type       Active   Database${NC}"
echo "=================================================="
echo -e "_cnpg_production_pg_common1_12 physical   ${RED}INACTIVE${NC} N/A"
echo -e "_cnpg_production_pg_common1_15 physical   ${GREEN}ACTIVE${NC}   N/A"
