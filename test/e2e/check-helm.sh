#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$(dirname "$SCRIPT_DIR")")"
CHART="$PROJECT_DIR/deploy/helm/nodepartition"

assert_rendered() {
    local mode=$1
    local output=$2
    local needle=$3
    if ! grep -Fq -- "$needle" <<<"$output"; then
        echo "Helm mode $mode is missing: $needle" >&2
        exit 1
    fi
}

cert_manager=$(helm template e2e "$CHART" \
    --set controller.webhook.tls.mode=cert-manager \
    --set controller.replicaCount=2 \
    --set driverName=e2e.coordinator.example.com)
assert_rendered cert-manager "$cert_manager" 'kind: Certificate'
assert_rendered cert-manager "$cert_manager" 'cert-manager.io/inject-ca-from'
assert_rendered cert-manager "$cert_manager" '--driver-name=e2e.coordinator.example.com'
assert_rendered cert-manager "$cert_manager" 'replicas: 2'
assert_rendered cert-manager "$cert_manager" 'failurePolicy: Fail'
assert_rendered cert-manager "$cert_manager" 'failurePolicy: Ignore'
assert_rendered cert-manager "$cert_manager" 'resources: ["resourceslices"]'
assert_rendered cert-manager "$cert_manager" 'name: e2e-nodepartition-webhook'
assert_rendered cert-manager "$cert_manager" 'name: claims.nodepartition.dra.k8s.io'
assert_rendered cert-manager "$cert_manager" 'name: vmis.nodepartition.dra.k8s.io'
assert_rendered cert-manager "$cert_manager" 'name: pods.nodepartition.dra.k8s.io'
assert_rendered cert-manager "$cert_manager" 'resources: ["leases"]'

openshift=$(helm template e2e "$CHART" \
    --set controller.webhook.tls.mode=openshift)
assert_rendered openshift "$openshift" 'service.beta.openshift.io/inject-cabundle: "true"'
assert_rendered openshift "$openshift" 'service.beta.openshift.io/serving-cert-secret-name:'

manual=$(helm template e2e "$CHART" \
    --set controller.webhook.tls.mode=manual \
    --set controller.webhook.tls.caBundle=test-ca)
assert_rendered manual "$manual" 'caBundle: dGVzdC1jYQ=='

network_policy=$(helm template e2e "$CHART" \
    --set controller.networkPolicy.enabled=true)
assert_rendered network-policy "$network_policy" 'kind: NetworkPolicy'
assert_rendered network-policy "$network_policy" 'port: 9443'
assert_rendered network-policy "$network_policy" 'port: 6443'

echo "Helm E2E assertions passed"
