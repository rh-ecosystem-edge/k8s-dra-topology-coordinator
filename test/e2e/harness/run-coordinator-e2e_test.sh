#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BRIDGE="$SCRIPT_DIR/run-coordinator-e2e.sh"

failures=0

expect_success() {
  local name=$1
  shift
  if "$@" >/tmp/coordinator-bridge-test.out 2>&1; then
    echo "PASS: $name"
  else
    echo "FAIL: $name"
    cat /tmp/coordinator-bridge-test.out
    failures=$((failures + 1))
  fi
}

expect_failure() {
  local name=$1
  shift
  if "$@" >/tmp/coordinator-bridge-test.out 2>&1; then
    echo "FAIL: $name unexpectedly succeeded"
    cat /tmp/coordinator-bridge-test.out
    failures=$((failures + 1))
  else
    echo "PASS: $name"
  fi
}

run_validation() {
  KUBECONFIG=/tmp/coordinator-test-kubeconfig \
  DRA_HARNESS_NAMESPACE=workload-test \
  "$BRIDGE" \
    --validate-only \
    --chart-dir "$SCRIPT_DIR/../../../deploy/helm/nodepartition" \
    --evidence-dir /tmp/coordinator-bridge-evidence
}

expect_success "valid harness bridge configuration" run_validation

expect_failure "missing harness namespace is rejected" env \
  KUBECONFIG=/tmp/coordinator-test-kubeconfig \
  "$BRIDGE" \
    --validate-only \
    --chart-dir "$SCRIPT_DIR/../../../deploy/helm/nodepartition" \
    --evidence-dir /tmp/coordinator-bridge-evidence

expect_failure "missing chart is rejected" env \
  KUBECONFIG=/tmp/coordinator-test-kubeconfig \
  DRA_HARNESS_NAMESPACE=workload-test \
  "$BRIDGE" \
    --validate-only \
    --chart-dir "$SCRIPT_DIR/../../../does-not-exist" \
    --evidence-dir /tmp/coordinator-bridge-evidence

expect_failure "invalid timeout is rejected" env \
  KUBECONFIG=/tmp/coordinator-test-kubeconfig \
  DRA_HARNESS_NAMESPACE=workload-test \
  "$BRIDGE" \
    --validate-only \
    --chart-dir "$SCRIPT_DIR/../../../deploy/helm/nodepartition" \
    --evidence-dir /tmp/coordinator-bridge-evidence \
    --timeout not-a-number

expect_failure "manual TLS requires certificate inputs" env \
  KUBECONFIG=/tmp/coordinator-test-kubeconfig \
  DRA_HARNESS_NAMESPACE=workload-test \
  "$BRIDGE" \
    --validate-only \
    --chart-dir "$SCRIPT_DIR/../../../deploy/helm/nodepartition" \
    --evidence-dir /tmp/coordinator-bridge-evidence \
    --tls-mode manual

rm -rf /tmp/coordinator-bridge-evidence

if (( failures > 0 )); then
  exit 1
fi
