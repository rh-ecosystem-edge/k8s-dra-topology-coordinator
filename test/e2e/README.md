# E2E tests

These tests exercise the Node Partition Topology Coordinator against a live Kubernetes cluster. They use:

- [mock-device](https://github.com/fabiendupont/mock-device), which is required and must publish `mock-accel.example.com` `ResourceSlice` objects; and
- [dra-driver-cpu](https://github.com/kubernetes-sigs/dra-driver-cpu), which is optional and must run in individual mode when used.

The driver `ResourceSlice` objects are the test inputs. The coordinator consumes them and publishes managed `DeviceClass` objects; it does not publish coordinator `ResourceSlice` objects.

## Quick start: mock-device Vagrant cluster

If the mock-device Vagrant cluster is already running with its DRA driver deployed:

```sh
# From the mock-device checkout
cd ../mock-device/vagrant
vagrant provision --provision-with nodepartition

# Or from this repository
./test/e2e/run-e2e.sh
```

## Manual setup

### Prerequisites

1. A Kubernetes 1.34 or newer cluster with DRA enabled.
2. The mock-device DRA driver deployed and publishing `ResourceSlice` objects for `mock-accel.example.com`.
3. Optionally, `dra-driver-cpu` deployed in individual mode with `--cpu-device-mode=individual`.
4. `kubectl`, Helm, Python 3, and Docker or another image builder.
5. A way to make the coordinator image available to the cluster.
6. A working webhook TLS configuration. The Helm chart defaults to cert-manager and the `selfsigned-issuer` `ClusterIssuer`; configure an existing issuer or select another TLS mode before running the test.

### Verify driver inputs

```sh
kubectl get resourceslices -o json | \
  python3 -c "import sys,json; [print(s['metadata']['name'], s['spec']['driver']) for s in json.load(sys.stdin)['items']]"
```

For the optional CPU driver, verify that it publishes slices under `dra.cpu`:

```sh
kubectl get resourceslices -o json | \
  python3 -c "import sys,json; [print(s['metadata']['name']) for s in json.load(sys.stdin)['items'] if s['spec']['driver']=='dra.cpu']"
```

### Build and load the coordinator image

The chart's default repository is `ghcr.io/rh-ecosystem-edge/nodepartition-controller`; the E2E script uses the `dev` tag.

```sh
make build
docker build -t ghcr.io/rh-ecosystem-edge/nodepartition-controller:dev .
```

For a k3s cluster, import the image on a node:

```sh
docker save ghcr.io/rh-ecosystem-edge/nodepartition-controller:dev | \
  ssh <node> sudo k3s ctr images import -
```

If your cluster uses a different image repository, install the chart manually with matching `controller.image.repository` and `controller.image.tag` values, or adjust the script's Helm command for the local environment.

### Run the tests

```sh
./test/e2e/run-e2e.sh
```

The script:

1. checks for the required mock-accel driver and detects the optional CPU driver;
2. applies the topology-rule ConfigMaps in [`topology-rules.yaml`](topology-rules.yaml);
3. installs the coordinator with Helm and waits for its Deployment;
4. waits for coordinator-managed `DeviceClass` objects;
5. validates partition type labels, selectors, opaque `PartitionConfig`, and mock-device sub-resources;
6. validates cross-driver configuration and per-driver NUMA selectors when `dra-driver-cpu` is present; and
7. removes the test ConfigMaps, Helm release, and managed DeviceClasses on exit.

## What gets validated

### Single-driver checks

| Check | Description |
| --- | --- |
| Driver input | mock-accel `ResourceSlice` objects are present |
| Coordinator output | Managed `DeviceClass` objects are created |
| Partition types | `pcieroot`, `numa`, and `full` types are reported when the topology provides them |
| DeviceClass selector | At least one class has the coordinator CEL selector |
| PartitionConfig | An opaque coordinator configuration exists and references mock-accel sub-resources |

Tier aliases such as `eighth`, `quarter`, or `half` are hardware-dependent and are not required by the script.

### Cross-driver checks

When `dra-driver-cpu` is present, the test also checks:

| Check | Description |
| --- | --- |
| Shared nodes | Both drivers publish `ResourceSlice` objects on at least one node |
| Multi-driver profile | Generated coordinator classes include both driver profiles |
| CPU sub-resource | `PartitionConfig` references `dra.cpu` |
| NUMA selectors | Per-driver NUMA selectors are present for both participating drivers |

## Topology rules

[`topology-rules.yaml`](topology-rules.yaml) maps the test drivers' attributes to standard topology attributes:

| Driver attribute | Standard attribute | Use |
| --- | --- | --- |
| `mock-accel.example.com/numaNode` | `numaNode` | NUMA grouping |
| `mock-accel.example.com/pciAddress` | `pcieRoot` | PCIe grouping |
| `dra.cpu/numaNodeID` | `numaNode` | NUMA grouping |
| `dra.cpu/socketID` | `socket` | Socket grouping |

The rules are ordinary labeled ConfigMaps. They must be applied before the coordinator reconciles so the generated classes contain the expected driver-specific selectors.
