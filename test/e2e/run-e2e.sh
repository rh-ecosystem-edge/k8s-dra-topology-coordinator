#!/usr/bin/env bash
# Run the live Node Partition Topology Coordinator E2E profiles.
#
# The suite is intentionally opt-in: it mutates cluster-scoped DRA objects and
# therefore requires an explicitly disposable Kubernetes context.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$(dirname "$SCRIPT_DIR")")"

for command in go kubectl helm; do
    if ! command -v "$command" >/dev/null 2>&1; then
        echo "required command not found: $command" >&2
        exit 2
    fi
done

if [[ -z "${E2E_DISPOSABLE_CLUSTER:-}" ||
      ("$E2E_DISPOSABLE_CLUSTER" != "1" && "$E2E_DISPOSABLE_CLUSTER" != "true" && "$E2E_DISPOSABLE_CLUSTER" != "yes") ]]; then
    echo "set E2E_DISPOSABLE_CLUSTER=1 before running live E2E" >&2
    exit 2
fi

if [[ -z "${E2E_KUBECONFIG:-}" && -n "${KUBECONFIG:-}" ]]; then
    export E2E_KUBECONFIG="$KUBECONFIG"
fi
if [[ -z "${E2E_KUBECONFIG:-}" ]]; then
    echo "set E2E_KUBECONFIG to the disposable cluster kubeconfig" >&2
    exit 2
fi
export KUBECONFIG="$E2E_KUBECONFIG"

export E2E_PROFILE="${E2E_PROFILE:-full}"
export E2E_RUN_ID="${E2E_RUN_ID:-e2e-$(date -u +%Y%m%d%H%M%S)}"
export E2E_NAMESPACE="${E2E_NAMESPACE:-nodepartition-e2e-$E2E_RUN_ID}"
export E2E_RELEASE="${E2E_RELEASE:-nodepartition-e2e-$E2E_RUN_ID}"
export E2E_CHART_PATH="${E2E_CHART_PATH:-$PROJECT_DIR/deploy/helm/nodepartition}"

echo "Running E2E profile=$E2E_PROFILE namespace=$E2E_NAMESPACE release=$E2E_RELEASE run=$E2E_RUN_ID"

cd "$PROJECT_DIR"
exec go test -tags=e2e ./test/e2e -run '^TestE2E' -count=1 -v
