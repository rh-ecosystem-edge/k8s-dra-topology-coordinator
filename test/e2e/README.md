# End-to-end tests

The live suite exercises the coordinator against a real Kubernetes API server. It is intentionally separate from ordinary unit and envtest runs because it mutates cluster-scoped DRA resources.

## Requirements

- Kubernetes 1.34 or newer with DRA enabled;
- a disposable cluster context in `E2E_KUBECONFIG`;
- the mock-device DRA driver publishing `mock-accel.example.com` `ResourceSlice` objects;
- `kubectl`, Helm, and Go;
- a locally available `dev` coordinator image; and
- `dra-driver-cpu` in individual mode for the required cross-driver profile;
- a working webhook TLS provider (the default is cert-manager with the
  `selfsigned-issuer` `ClusterIssuer`; use `E2E_TLS_*` to select another one).

The suite refuses to run unless `E2E_DISPOSABLE_CLUSTER=1` (or `true`/`yes`) is set. This is required because `DeviceClass` and admission configurations are cluster-scoped.

## Run

```sh
E2E_KUBECONFIG=/path/to/disposable/kubeconfig \
E2E_DISPOSABLE_CLUSTER=1 \
E2E_REQUIRE_CPU=1 \
E2E_PROFILE=full \
make e2e
```

`test/e2e/run-e2e.sh` creates unique namespace, release, and run identifiers when they are not supplied. It rejects existing managed coordinator `DeviceClass` objects and cleans only resources created by that run.

Useful variables:

| Variable | Default | Meaning |
| --- | --- | --- |
| `E2E_PROFILE` | `full` | `controller`, `cross-driver`, `admission`, `ha`, `full`, or `kubevirt` |
| `E2E_NAMESPACE` | unique | Namespace owned by the run |
| `E2E_RELEASE` | unique | Helm release owned by the run |
| `E2E_RUN_ID` | unique | Owner label value |
| `E2E_DRIVER_NAME` | `nodepartition.dra.k8s.io` | Coordinator identity used by the chart and webhook |
| `E2E_TLS_MODE` | `cert-manager` | `cert-manager`, `openshift`, or `manual` |
| `E2E_TLS_ISSUER_NAME` | `selfsigned-issuer` | cert-manager issuer name |
| `E2E_TLS_ISSUER_KIND` | `ClusterIssuer` | cert-manager issuer kind |
| `E2E_TLS_CA_BUNDLE` | empty | Required when `E2E_TLS_MODE=manual` |
| `E2E_REQUIRE_CPU` | `false` | Require `dra.cpu` slices for cross-driver checks |
| `E2E_REQUIRE_KUBEVIRT` | `false` | Require KubeVirt for the optional VMI profile |
| `E2E_CHART_PATH` | repository chart | Chart path used by Helm |

The tagged package can be compiled without a cluster:

```sh
go test -tags=e2e ./test/e2e -count=1
```

Without `E2E_KUBECONFIG`, live tests skip instead of mutating a developer cluster.

## Coverage profiles

### `controller`

Validates ResourceSlice discovery, single- and cross-driver ResourceSlice intersection,
topology rules, fixed partitions, generated selectors and opaque `PartitionConfig`,
grouping modes, intent metadata, and stale-output cleanup.

Use `E2E_PROFILE=cross-driver E2E_REQUIRE_CPU=1` to run the required shared-node
mock-accelerator/CPU catalog checks without the admission and HA profiles.

### `admission`

Creates ResourceClaims through the API server and verifies expansion, native pass-through,
malformed managed-claim rejection, update idempotence, direct and template-backed Pod
rewriting, custom driver identity, and webhook TLS/service wiring.

### `ha`

Runs two controller replicas and verifies leader ownership, follower cache readiness, deterministic admission, leader deletion, lease handoff, and publication continuity.

### `kubevirt`

Optional live profile for VMI host-device generation, existing-device rewrites, explicit passthrough/VFIO behavior, count splitting, and capacity-backed requests. Fast unit/envtest coverage remains required even when this profile is unavailable.

## CI

The normal CI workflow compiles the tagged package. `.github/workflows/e2e.yml` runs the live `full` profile manually or nightly on a dedicated self-hosted runner that supplies the disposable cluster, drivers, image, and `E2E_KUBECONFIG`; CI deliberately uses a non-default `E2E_DRIVER_NAME` to exercise configured identity end to end.
