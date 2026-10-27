#!/usr/bin/env bash
set -euo pipefail

# Runs the coordinator inside a k8s-dra-harness verification step.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

HARNESS_NAMESPACE="${DRA_HARNESS_NAMESPACE:-}"
CHART_DIR="$REPO_ROOT/deploy/helm/nodepartition"
EVIDENCE_DIR="${COORDINATOR_EVIDENCE_DIR:-}"
COORDINATOR_NAMESPACE="${COORDINATOR_NAMESPACE:-}"
RELEASE="${COORDINATOR_RELEASE:-}"
DRIVER_NAME="${COORDINATOR_DRIVER_NAME:-topology.test.example}"
PARTITION_MODE="${COORDINATOR_PARTITION_MODE:-auto}"
IMAGE_REPOSITORY="${COORDINATOR_IMAGE_REPOSITORY:-}"
IMAGE_TAG="${COORDINATOR_IMAGE_TAG:-}"
TIMEOUT="${COORDINATOR_TIMEOUT:-180}"
TLS_MODE="${COORDINATOR_TLS_MODE:-cert-manager}"
TLS_CA_BUNDLE_FILE="${COORDINATOR_TLS_CA_BUNDLE_FILE:-}"
TLS_CERT_FILE="${COORDINATOR_TLS_CERT_FILE:-}"
TLS_KEY_FILE="${COORDINATOR_TLS_KEY_FILE:-}"
EXPECT_CLASS=""
VALIDATE_ONLY=false
KEEP_RESOURCES=false
declare -a INPUT_FILES=()

usage() {
  cat <<'EOF'
Usage: run-coordinator-e2e.sh [options]

Required environment:
  DRA_HARNESS_NAMESPACE     harness workload namespace

Options:
  --chart-dir DIR           coordinator Helm chart
  --evidence-dir DIR        directory for snapshots and logs
  --namespace NAME          coordinator namespace
  --release NAME            Helm release name
  --driver-name NAME        configured coordinator driver name
  --partition-mode MODE     auto, partitions, or groupings
  --image-repository REPO   controller image repository override
  --image-tag TAG           controller image tag override
  --rules FILE              run-owned topology-rule manifest
  --groupings FILE          run-owned grouping manifest
  --resource-slices FILE    run-owned synthetic ResourceSlice manifest
  --expect-class NAME       managed DeviceClass that must exist
  --timeout SECONDS         rollout/readiness timeout in seconds
  --tls-mode MODE           cert-manager, openshift, or manual
  --tls-ca-bundle FILE      CA bundle for manual TLS mode
  --tls-cert FILE           serving certificate for manual TLS mode
  --tls-key FILE             serving key for manual TLS mode
  --keep-resources          retain coordinator and fixtures
  --validate-only           validate inputs without contacting Kubernetes
  -h, --help                show this help
EOF
}

die() {
  echo "ERROR: $*" >&2
  exit 2
}

short_name() {
  printf '%s' "$1" | tr '[:upper:]' '[:lower:]' | tr -cd 'a-z0-9-' | sed 's/^-*//; s/-*$//' | cut -c1-40
}

while (($# > 0)); do
  case "$1" in
    --chart-dir) CHART_DIR=${2:?missing value for --chart-dir}; shift 2 ;;
    --evidence-dir) EVIDENCE_DIR=${2:?missing value for --evidence-dir}; shift 2 ;;
    --namespace) COORDINATOR_NAMESPACE=${2:?missing value for --namespace}; shift 2 ;;
    --release) RELEASE=${2:?missing value for --release}; shift 2 ;;
    --driver-name) DRIVER_NAME=${2:?missing value for --driver-name}; shift 2 ;;
    --partition-mode) PARTITION_MODE=${2:?missing value for --partition-mode}; shift 2 ;;
    --image-repository) IMAGE_REPOSITORY=${2:?missing value for --image-repository}; shift 2 ;;
    --image-tag) IMAGE_TAG=${2:?missing value for --image-tag}; shift 2 ;;
    --rules|--groupings|--resource-slices)
      [[ $# -ge 2 ]] || die "missing value for $1"
      INPUT_FILES+=("$2")
      shift 2
      ;;
    --expect-class) EXPECT_CLASS=${2:?missing value for --expect-class}; shift 2 ;;
    --timeout) TIMEOUT=${2:?missing value for --timeout}; shift 2 ;;
    --tls-mode) TLS_MODE=${2:?missing value for --tls-mode}; shift 2 ;;
    --tls-ca-bundle) TLS_CA_BUNDLE_FILE=${2:?missing value for --tls-ca-bundle}; shift 2 ;;
    --tls-cert) TLS_CERT_FILE=${2:?missing value for --tls-cert}; shift 2 ;;
    --tls-key) TLS_KEY_FILE=${2:?missing value for --tls-key}; shift 2 ;;
    --keep-resources) KEEP_RESOURCES=true; shift ;;
    --validate-only) VALIDATE_ONLY=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown option $1" ;;
  esac
done

[[ -n "$HARNESS_NAMESPACE" ]] || die "DRA_HARNESS_NAMESPACE is required"
[[ -n "$EVIDENCE_DIR" ]] || die "--evidence-dir or COORDINATOR_EVIDENCE_DIR is required"
[[ -f "$CHART_DIR/Chart.yaml" ]] || die "chart not found: $CHART_DIR/Chart.yaml"
[[ "$TIMEOUT" =~ ^[0-9]+$ && "$TIMEOUT" -gt 0 ]] || die "timeout must be a positive integer number of seconds"
case "$TLS_MODE" in
  cert-manager|openshift) ;;
  manual)
    [[ -f "$TLS_CA_BUNDLE_FILE" ]] || die "manual TLS mode requires --tls-ca-bundle"
    [[ -f "$TLS_CERT_FILE" ]] || die "manual TLS mode requires --tls-cert"
    [[ -f "$TLS_KEY_FILE" ]] || die "manual TLS mode requires --tls-key"
    ;;
  *) die "tls mode must be cert-manager, openshift, or manual" ;;
esac
mkdir -p "$EVIDENCE_DIR"
for file in "${INPUT_FILES[@]}"; do
  [[ -f "$file" ]] || die "fixture not found: $file"
done

if [[ "$VALIDATE_ONLY" == true ]]; then
  echo "coordinator bridge configuration is valid"
  echo "  harness namespace: $HARNESS_NAMESPACE"
  echo "  chart: $CHART_DIR"
  echo "  evidence: $EVIDENCE_DIR"
  exit 0
fi

command -v kubectl >/dev/null 2>&1 || die "kubectl is required"
command -v helm >/dev/null 2>&1 || die "helm is required"
command -v python3 >/dev/null 2>&1 || die "python3 is required"
wait_seconds=$TIMEOUT

RUN_ID="${DRA_HARNESS_RUN_ID:-$(date -u +%Y%m%d%H%M%S)-${RANDOM}}"
RUN_ID=$(short_name "$RUN_ID")
[[ -n "$RUN_ID" ]] || die "could not derive a run ID"
COORDINATOR_NAMESPACE=${COORDINATOR_NAMESPACE:-nodepartition-e2e-$RUN_ID}
RELEASE=${RELEASE:-nodepartition-e2e-$RUN_ID}
COORDINATOR_NAMESPACE=$(short_name "$COORDINATOR_NAMESPACE")
RELEASE=$(short_name "$RELEASE")
[[ -n "$COORDINATOR_NAMESPACE" && -n "$RELEASE" ]] || die "invalid namespace or release name"

RUN_LABEL="nodepartition.dra.k8s.io/e2e-run=$RUN_ID"
CONTROLLER="${RELEASE}-controller"
LEASE_ID="${RELEASE}-leader"
declare -a OWNED_CLASSES=()
declare -A OWNED_CLASS_SET=()

owned_class_names() {
  kubectl get deviceclasses.resource.k8s.io \
    -l nodepartition.dra.k8s.io/managed=true \
    -o json | \
    python3 -c 'import json, sys
wanted=sys.argv[1]
data=json.load(sys.stdin)
for item in data.get("items", []):
    for cfg in item.get("spec", {}).get("config", []):
        opaque=cfg.get("opaque") or {}
        if opaque.get("driver") == wanted:
            print(item["metadata"]["name"])
            break' "$DRIVER_NAME"
}

mark_owned_classes() {
  local class
  while IFS= read -r class; do
    [[ -n "$class" ]] || continue
    if [[ -z "${OWNED_CLASS_SET[$class]+present}" ]]; then
      OWNED_CLASS_SET["$class"]=present
      OWNED_CLASSES+=("$class")
    fi
    kubectl label deviceclasses.resource.k8s.io "$class" "$RUN_LABEL" --overwrite >/dev/null
  done < <(owned_class_names 2>/dev/null || true)
}

if kubectl get namespace "$COORDINATOR_NAMESPACE" >/dev/null 2>&1; then
  die "coordinator namespace already exists: $COORDINATOR_NAMESPACE"
fi
if kubectl -n kube-system get lease "$LEASE_ID" >/dev/null 2>&1; then
  die "leader-election lease already exists: kube-system/$LEASE_ID"
fi
if kubectl get mutatingwebhookconfiguration "$RELEASE" >/dev/null 2>&1; then
  die "webhook configuration already exists: $RELEASE"
fi
existing_classes=$(owned_class_names 2>/dev/null || true)
[[ -z "$existing_classes" ]] || die "managed DeviceClasses already reference driver $DRIVER_NAME: $existing_classes"

for file in "${INPUT_FILES[@]}"; do
  existing=$(kubectl get -f "$file" -o name --ignore-not-found 2>/dev/null || true)
  [[ -z "$existing" ]] || die "fixture already exists; refusing to take ownership: $existing"
done

capture() {
  kubectl get deviceclasses.resource.k8s.io -o yaml >"$EVIDENCE_DIR/deviceclasses.yaml" 2>&1 || true
  kubectl get resourceslices.resource.k8s.io -o yaml >"$EVIDENCE_DIR/resourceslices.yaml" 2>&1 || true
  kubectl get resourceclaims.resource.k8s.io -A -o yaml >"$EVIDENCE_DIR/resourceclaims.yaml" 2>&1 || true
  kubectl get configmaps -A -o yaml >"$EVIDENCE_DIR/configmaps.yaml" 2>&1 || true
  kubectl -n kube-system get lease "$LEASE_ID" -o yaml >"$EVIDENCE_DIR/leader-lease.yaml" 2>&1 || true
  kubectl get events -A --sort-by=.lastTimestamp >"$EVIDENCE_DIR/events.txt" 2>&1 || true
  kubectl -n "$COORDINATOR_NAMESPACE" get all -o yaml >"$EVIDENCE_DIR/coordinator-resources.yaml" 2>&1 || true
  kubectl -n "$COORDINATOR_NAMESPACE" logs "deployment/$CONTROLLER" --all-containers=true >"$EVIDENCE_DIR/coordinator.log" 2>&1 || true
}

cleanup() {
  local rc=$?
  capture
  if [[ "$KEEP_RESOURCES" != true ]]; then
    helm uninstall "$RELEASE" --namespace "$COORDINATOR_NAMESPACE" >/dev/null 2>&1 || true
    for file in "${INPUT_FILES[@]}"; do
      kubectl delete -f "$file" --ignore-not-found >/dev/null 2>&1 || true
    done
    mark_owned_classes
    for class in "${OWNED_CLASSES[@]}"; do
      kubectl delete deviceclasses.resource.k8s.io "$class" --ignore-not-found >/dev/null 2>&1 || true
    done
    kubectl -n kube-system delete lease "$LEASE_ID" --ignore-not-found >/dev/null 2>&1 || true
    kubectl delete namespace "$COORDINATOR_NAMESPACE" --ignore-not-found >/dev/null 2>&1 || true
  fi
  exit "$rc"
}
trap cleanup EXIT

for file in "${INPUT_FILES[@]}"; do
  kubectl apply -f "$file"
  kubectl label -f "$file" "$RUN_LABEL" --overwrite >/dev/null
done

if [[ "$TLS_MODE" == manual ]]; then
  kubectl create namespace "$COORDINATOR_NAMESPACE"
  kubectl -n "$COORDINATOR_NAMESPACE" create secret tls "${RELEASE}-webhook-tls" \
    --cert="$TLS_CERT_FILE" --key="$TLS_KEY_FILE"
fi

helm_args=(
  upgrade --install "$RELEASE" "$CHART_DIR"
  --namespace "$COORDINATOR_NAMESPACE"
  --create-namespace
  --set-string "fullnameOverride=$RELEASE"
  --set-string "driverName=$DRIVER_NAME"
  --set-string "partitionMode=$PARTITION_MODE"
  --set-string "controller.leaderElection.id=$LEASE_ID"
  --set-string "controller.image.pullPolicy=IfNotPresent"
  --set-string "controller.webhook.tls.mode=$TLS_MODE"
)
if [[ "$TLS_MODE" == manual ]]; then
  helm_args+=(--set-file "controller.webhook.tls.caBundle=$TLS_CA_BUNDLE_FILE")
fi
if [[ -n "$IMAGE_REPOSITORY" ]]; then
  helm_args+=(--set-string "controller.image.repository=$IMAGE_REPOSITORY")
fi
if [[ -n "$IMAGE_TAG" ]]; then
  helm_args+=(--set-string "controller.image.tag=$IMAGE_TAG")
fi
helm "${helm_args[@]}"
kubectl label namespace "$COORDINATOR_NAMESPACE" "$RUN_LABEL" --overwrite >/dev/null

kubectl -n "$COORDINATOR_NAMESPACE" rollout status "deployment/$CONTROLLER" --timeout="${TIMEOUT}s"

deadline=$((SECONDS + wait_seconds))
while ((SECONDS < deadline)); do
  if kubectl get deviceclasses.resource.k8s.io \
    -l nodepartition.dra.k8s.io/managed=true \
    -o json 2>/dev/null | \
    python3 -c 'import json, sys
wanted=sys.argv[1]
data=json.load(sys.stdin)
for item in data.get("items", []):
    for cfg in item.get("spec", {}).get("config", []):
        opaque=cfg.get("opaque") or {}
        if opaque.get("driver") == wanted:
            raise SystemExit(0)
raise SystemExit(1)' "$DRIVER_NAME"; then
    break
  fi
  sleep 2
done

if ((SECONDS >= deadline)); then
  die "no managed DeviceClass references configured driver $DRIVER_NAME"
fi
mark_owned_classes

if [[ -n "$EXPECT_CLASS" ]]; then
  kubectl get deviceclasses.resource.k8s.io "$EXPECT_CLASS" >/dev/null
fi

kubectl get deviceclasses.resource.k8s.io \
  -l nodepartition.dra.k8s.io/managed=true \
  -o name
echo "coordinator catalog smoke passed (run=$RUN_ID)"
