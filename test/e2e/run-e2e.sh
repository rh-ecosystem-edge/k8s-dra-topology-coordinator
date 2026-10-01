#!/bin/bash
# E2E test for the Node Partition Topology Coordinator
#
# Tests cross-driver topology partitioning using mock-accel (required)
# and dra-driver-cpu (optional, tested if present). When both drivers
# are available, validates that generated DeviceClasses contain sub-resources
# from both drivers grouped by shared NUMA topology.
#
# Usage:
#   ./test/e2e/run-e2e.sh                    # uses current kubectl context
#   KUBECONFIG=/path/to/kubeconfig ./test/e2e/run-e2e.sh
#
# For use with mock-device Vagrant cluster:
#   export KUBECONFIG=$(vagrant ssh mock-cluster-node1 -c \
#     "sudo cat /etc/rancher/k3s/k3s.yaml" 2>/dev/null | \
#     sed "s/127.0.0.1/$(vagrant ssh mock-cluster-node1 -c \
#     'hostname -I' 2>/dev/null | awk '{print $2}')/")
#   ./test/e2e/run-e2e.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$(dirname "$SCRIPT_DIR")")"

# Colors
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'

COORDINATOR_DRIVER="nodepartition.dra.k8s.io"
MOCK_ACCEL_DRIVER="mock-accel.example.com"
CPU_DRIVER="dra.cpu"
LABEL_MANAGED="${COORDINATOR_DRIVER}/managed=true"
TIMEOUT=120

# Track which drivers are available
HAS_MOCK_ACCEL=false
HAS_CPU_DRIVER=false

pass=0
fail=0
skip=0

check() {
    local desc=$1
    shift
    if "$@" >/dev/null 2>&1; then
        echo -e "  ${GREEN}✓ $desc${NC}"
        pass=$((pass + 1))
    else
        echo -e "  ${RED}✗ $desc${NC}"
        fail=$((fail + 1))
    fi
}

check_skip() {
    local desc=$1
    shift
    if "$@" >/dev/null 2>&1; then
        echo -e "  ${GREEN}✓ $desc${NC}"
        pass=$((pass + 1))
    else
        echo -e "  ${YELLOW}⊘ $desc (skipped — driver not present)${NC}"
        skip=$((skip + 1))
    fi
}

check_optional() {
    local desc=$1
    shift
    if "$@" >/dev/null 2>&1; then
        echo -e "  ${GREEN}✓ $desc${NC}"
        pass=$((pass + 1))
    else
        echo -e "  ${YELLOW}⊘ $desc (not present in this topology)${NC}"
        skip=$((skip + 1))
    fi
}

cleanup() {
    echo -e "\n${YELLOW}Cleaning up...${NC}"
    kubectl delete -f "$SCRIPT_DIR/topology-rules.yaml" --ignore-not-found >/dev/null 2>&1 || true
    helm uninstall nodepartition --namespace default >/dev/null 2>&1 || true
    kubectl delete deviceclasses -l "$LABEL_MANAGED" --ignore-not-found >/dev/null 2>&1 || true
    echo -e "${GREEN}Cleanup complete${NC}"
}
trap cleanup EXIT

# count_driver_slices returns the number of ResourceSlices for a given driver
count_driver_slices() {
    local driver=$1
    kubectl get resourceslices -o json 2>/dev/null | \
        python3 -c "
import sys, json
data = json.load(sys.stdin)
print(len([s for s in data['items'] if s['spec']['driver'] == '$driver']))
" 2>/dev/null || echo "0"
}

echo -e "${GREEN}=== Node Partition Topology Coordinator E2E Test ===${NC}"
echo

# --- Pre-checks ---
echo -e "${YELLOW}Pre-checks...${NC}"

check "kubectl is available" command -v kubectl
check "helm is available" command -v helm

# Detect available DRA drivers
MOCK_SLICE_COUNT=$(count_driver_slices "$MOCK_ACCEL_DRIVER")
CPU_SLICE_COUNT=$(count_driver_slices "$CPU_DRIVER")

if [ "$MOCK_SLICE_COUNT" -gt 0 ]; then
    HAS_MOCK_ACCEL=true
fi
if [ "$CPU_SLICE_COUNT" -gt 0 ]; then
    HAS_CPU_DRIVER=true
fi

# mock-accel is required
if [ "$HAS_MOCK_ACCEL" = false ]; then
    echo -e "  ${RED}✗ No mock-accel ResourceSlices found — is the DRA driver deployed?${NC}"
    exit 1
fi
check "mock-accel ResourceSlices present ($MOCK_SLICE_COUNT)" [ "$MOCK_SLICE_COUNT" -gt 0 ]

# dra-driver-cpu is optional
if [ "$HAS_CPU_DRIVER" = true ]; then
    echo -e "  ${GREEN}✓ dra-driver-cpu ResourceSlices present ($CPU_SLICE_COUNT)${NC}"
    pass=$((pass + 1))
else
    echo -e "  ${YELLOW}⊘ dra-driver-cpu not detected — cross-driver tests will be skipped${NC}"
    skip=$((skip + 1))
fi

DRIVER_SUMMARY="mock-accel"
if [ "$HAS_CPU_DRIVER" = true ]; then
    DRIVER_SUMMARY="mock-accel + dra-driver-cpu"
fi
echo -e "  Drivers under test: ${GREEN}$DRIVER_SUMMARY${NC}"
echo

# --- Deploy topology rules ---
echo -e "${YELLOW}Deploying topology rules...${NC}"
kubectl apply -f "$SCRIPT_DIR/topology-rules.yaml"
check "mock-accel topology rules created" kubectl get configmap mock-accel-numa-rule
check "mock-accel PCIe topology rule created" kubectl get configmap mock-accel-pci-rule
if [ "$HAS_CPU_DRIVER" = true ]; then
    check "cpu NUMA topology rule created" kubectl get configmap cpu-numa-rule
    check "cpu socket topology rule created" kubectl get configmap cpu-socket-rule
else
    check_skip "cpu NUMA topology rule created" kubectl get configmap cpu-numa-rule
    check_skip "cpu socket topology rule created" kubectl get configmap cpu-socket-rule
fi
echo

# --- Deploy coordinator ---
echo -e "${YELLOW}Deploying coordinator...${NC}"

# Build image if not already available
if ! kubectl get pods -l app.kubernetes.io/name=nodepartition >/dev/null 2>&1; then
    helm install nodepartition "$PROJECT_DIR/deploy/helm/nodepartition" \
        --set controller.image.tag=dev \
        --set controller.image.pullPolicy=IfNotPresent \
        --wait --timeout 60s 2>/dev/null || {
        # If helm install fails (image not available), try building locally
        echo -e "${YELLOW}  Helm install may need a locally available image.${NC}"
        echo -e "${YELLOW}  Build with: make build && docker build -t ghcr.io/rh-ecosystem-edge/nodepartition-controller:dev .${NC}"
    }
fi

# Wait for coordinator deployment to be ready
echo -e "${YELLOW}Waiting for coordinator to be ready...${NC}"
kubectl rollout status deployment -l app.kubernetes.io/component=controller --timeout=60s 2>/dev/null || true
check "coordinator deployment ready" kubectl get deployment -l app.kubernetes.io/component=controller -o jsonpath='{.items[0].status.readyReplicas}' 2>/dev/null
echo

# --- Wait for coordinator DeviceClasses ---
echo -e "${YELLOW}Waiting for coordinator to publish DeviceClasses...${NC}"
ELAPSED=0
DC_COUNT=0
while [ $ELAPSED -lt $TIMEOUT ]; do
    DC_COUNT=$(kubectl get deviceclasses -l "$LABEL_MANAGED" --no-headers 2>/dev/null | wc -l)
    if [ "$DC_COUNT" -gt 0 ]; then
        break
    fi
    sleep 5
    ELAPSED=$((ELAPSED + 5))
    echo -e "  Waiting... ($ELAPSED/${TIMEOUT}s)"
done

check "coordinator DeviceClasses published ($DC_COUNT)" [ "$DC_COUNT" -gt 0 ]
echo

if [ "$DC_COUNT" -eq 0 ]; then
    echo -e "${RED}No coordinator DeviceClasses found — aborting validation${NC}"
    echo -e "${YELLOW}Check coordinator logs:${NC}"
    kubectl logs -l app.kubernetes.io/component=controller --tail=50 2>/dev/null || true
    exit 1
fi

# --- Validate DeviceClasses ---
echo -e "${YELLOW}Validating coordinator DeviceClasses...${NC}"
DC_JSON=$(kubectl get deviceclasses -l "$LABEL_MANAGED" -o json 2>/dev/null)
DC_NAMES=$(echo "$DC_JSON" | python3 -c "import sys,json; print('\\n'.join(sorted(dc['metadata']['name'] for dc in json.load(sys.stdin)['items'])))")
echo -e "  DeviceClasses: ${GREEN}$DC_NAMES${NC}"

PARTITION_TYPES=$(echo "$DC_JSON" | python3 -c "import sys,json; print(' '.join(sorted({dc.get('metadata', {}).get('labels', {}).get('${COORDINATOR_DRIVER}/partitionType', '') for dc in json.load(sys.stdin)['items']} - {''})))")
echo -e "  Partition types found: ${GREEN}$PARTITION_TYPES${NC}"
check "pcieroot partition type exists" grep -q "pcieroot" <<< "$PARTITION_TYPES"
check "full partition type exists" grep -q "full" <<< "$PARTITION_TYPES"
check_optional "numa partition type exists" grep -q "numa" <<< "$PARTITION_TYPES"

DC_SELECTORS=$(echo "$DC_JSON" | python3 -c "import sys,json; print('yes' if any(dc.get('spec', {}).get('selectors') for dc in json.load(sys.stdin)['items']) else 'no')")
check "DeviceClass has a CEL selector" [ "$DC_SELECTORS" = "yes" ]

PARTITION_CONFIG_HAS_MOCK=$(echo "$DC_JSON" | python3 -c "
import json, sys
for dc in json.load(sys.stdin)['items']:
    for cfg in dc.get('spec', {}).get('config', []):
        opaque = cfg.get('opaque') or {}
        if opaque.get('driver') != '$COORDINATOR_DRIVER':
            continue
        params = opaque.get('parameters', {})
        if isinstance(params, str):
            params = json.loads(params)
        if any('mock-accel' in sr.get('deviceClass', '') for sr in params.get('subResources', [])):
            print('yes')
            sys.exit(0)
print('no')
" 2>/dev/null)
check "PartitionConfig references mock-accel sub-resources" [ "$PARTITION_CONFIG_HAS_MOCK" = "yes" ]

echo

# --- Cross-driver validation (mock-accel + CPU) ---
if [ "$HAS_CPU_DRIVER" = true ]; then
    echo -e "${YELLOW}Validating cross-driver partitioning (mock-accel + dra-driver-cpu)...${NC}"

    PARTITION_CONFIG_HAS_CPU=$(echo "$DC_JSON" | python3 -c "
import json, sys
for dc in json.load(sys.stdin)['items']:
    for cfg in dc.get('spec', {}).get('config', []):
        opaque = cfg.get('opaque') or {}
        if opaque.get('driver') != '$COORDINATOR_DRIVER':
            continue
        params = opaque.get('parameters', {})
        if isinstance(params, str):
            params = json.loads(params)
        if any('dra.cpu' in sr.get('deviceClass', '') for sr in params.get('subResources', [])):
            print('yes')
            sys.exit(0)
print('no')
" 2>/dev/null)
    check "PartitionConfig references dra-driver-cpu sub-resources" [ "$PARTITION_CONFIG_HAS_CPU" = "yes" ]

    # Check that both drivers' input slices overlap on at least one node.
    SHARED_NODES=$(kubectl get resourceslices -o json 2>/dev/null | \
        python3 -c "
import sys, json
data = json.load(sys.stdin)
mock_nodes = set()
cpu_nodes = set()
for s in data['items']:
    node = s['spec'].get('nodeName', '')
    if not node:
        continue
    if s['spec']['driver'] == '$MOCK_ACCEL_DRIVER':
        mock_nodes.add(node)
    elif s['spec']['driver'] == '$CPU_DRIVER':
        cpu_nodes.add(node)
print(len(mock_nodes & cpu_nodes))
" 2>/dev/null)
    check "drivers share nodes ($SHARED_NODES nodes with both mock-accel + cpu)" [ "$SHARED_NODES" -gt 0 ]

    PARTITION_PROFILES=$(echo "$DC_JSON" | python3 -c "
import sys, json
profiles = {dc.get('metadata', {}).get('labels', {}).get('${COORDINATOR_DRIVER}/profile', '') for dc in json.load(sys.stdin)['items']}
for profile in sorted(profiles - {''}):
    print(profile)
" 2>/dev/null)
    echo -e "  Partition profiles: ${GREEN}${PARTITION_PROFILES}${NC}"
    MULTI_DRIVER_PROFILE=$(echo "$PARTITION_PROFILES" | grep -c "mock-accel.*dra.cpu\|dra.cpu.*mock-accel" || true)
    check "partition profile reflects multiple drivers" [ "$MULTI_DRIVER_PROFILE" -gt 0 ]

    NUMA_SELECTORS_HAVE_BOTH_DRIVERS=$(echo "$DC_JSON" | python3 -c "
import json, sys
drivers = set()
for dc in json.load(sys.stdin)['items']:
    for cfg in dc.get('spec', {}).get('config', []):
        opaque = cfg.get('opaque') or {}
        if opaque.get('driver') != '$COORDINATOR_DRIVER':
            continue
        params = opaque.get('parameters', {})
        if isinstance(params, str):
            params = json.loads(params)
        for sr in params.get('subResources', []):
            if any('numaNode' in selector for selector in sr.get('selectors', [])):
                if 'mock-accel' in sr.get('deviceClass', ''):
                    drivers.add('mock')
                if 'dra.cpu' in sr.get('deviceClass', ''):
                    drivers.add('cpu')
print('yes' if drivers == {'mock', 'cpu'} else 'no')
" 2>/dev/null)
    check "PartitionConfig has per-driver NUMA selectors" [ "$NUMA_SELECTORS_HAVE_BOTH_DRIVERS" = "yes" ]

    echo
fi

echo

# --- Summary ---
total=$((pass + fail + skip))
echo -e "${GREEN}=== E2E Test Results ===${NC}"
echo -e "  Passed:  ${GREEN}$pass${NC}"
echo -e "  Failed:  ${RED}$fail${NC}"
echo -e "  Skipped: ${YELLOW}$skip${NC}"
echo -e "  Total:   $total"
echo

if [ "$fail" -gt 0 ]; then
    echo -e "${RED}E2E FAILED${NC}"
    exit 1
fi

echo -e "${GREEN}E2E PASSED${NC}"
