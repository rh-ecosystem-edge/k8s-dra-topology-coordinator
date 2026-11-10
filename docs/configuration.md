# Configuring the coordinator

The coordinator is configured with two kinds of labeled ConfigMaps. Both are optional: with none, `auto` mode still publishes fixed partitions for devices whose drivers publish standard topology attributes.

| | Topology rule | Device grouping |
| --- | --- | --- |
| Label | `nodepartition.dra.k8s.io/topology-rule: "true"` | `nodepartition.dra.k8s.io/device-grouping: "true"` |
| Question it answers | How do I read this driver's topology? | Which devices form one named bundle, and at what alignment? |
| Typical author | Cluster admin integrating a DRA driver | Platform admin curating what workloads can request |
| Granularity | One ConfigMap per driver attribute | One ConfigMap per grouping; each `data.name` has exactly one owner |
| Effect | `mapsTo` (`numaNode`, `pcieRoot`, `socket`) feeds the topology model and constraint classification. `mapsTo: numaNode` also drives per-driver NUMA CEL selectors. `partitioning: group` splits fixed partitions. `constraint: match` and `enforcement` add a `matchAttribute`; `fallbackAttribute` loosens it. `deviceClass` overrides the sub-request class for partitions and groupings. | Publishes an aggregate DeviceClass and per-topology DeviceClasses carrying a `PartitionConfig`, plus intent labels and annotations |
| Needed when | The driver does not publish standard `resource.kubernetes.io/numaNode` or `pcieRoot`, or you want extra constraints | You want a named multi-driver offering beyond the automatic partitions |
| Mode interaction | Always read | Ignored with `partitionMode=partitions`; with `partitionMode=groupings`, fixed partitions are skipped instead |
| Invalid input | An invalid add leaves the rule absent. An invalid update keeps the last valid rule. Removing the label removes the rule. | Same. Also: unknown top-level `data` keys are warned about; unknown `devices[]` keys are rejected. |

## Which one do I need?

1. Install with no ConfigMaps and run `kubectl get deviceclasses -l nodepartition.dra.k8s.io/managed=true`.
2. If classes are missing or devices from a driver are not aligned, add **topology rules** for that driver's attributes.
3. If workloads need a named bundle (for example one GPU, one NIC, and some CPU on the same PCIe root), add a **device grouping**. Set `offering: "true"` to make its aggregate class a stable request target.

## Trust and namespace

Rules and groupings control which DeviceClasses the coordinator publishes, so only ConfigMaps in one namespace are read.

- Binary: `--config-namespace` defaults to `$POD_NAMESPACE`. If both are empty, startup fails. Pass `--config-namespace=` explicitly to watch every namespace.
- Helm: defaults to the release namespace. Set `controller.configNamespace` to use another namespace.
- `controller.watchAllNamespaces=true` trusts every namespace. Anyone who can create a labeled ConfigMap can then publish DeviceClasses, including driver opaque configuration. Enable it only when that is intended.

## Lifecycle

Informers watch labeled ConfigMaps in the trusted namespace. A change reloads the in-memory rule or grouping store and queues a debounced reconcile. Every replica keeps its own copy of the stores so the webhook expands equivalent claims identically; only the elected leader publishes DeviceClasses.

## Topology rules

Rules map driver-specific attributes to the coordinator's standard topology model and can add claim constraints.

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: mock-accel-numa-rule
  namespace: nodepartition
  labels:
    nodepartition.dra.k8s.io/topology-rule: "true"
data:
  attribute: mock-accel.example.com/numaNode
  type: int
  driver: mock-accel.example.com
  mapsTo: numaNode
  partitioning: group
  constraint: match
  enforcement: required
```

### Rule fields

| Field | Required | Values | Purpose |
| --- | --- | --- | --- |
| `attribute` | yes | Qualified attribute name | Attribute published by the driver |
| `type` | yes | `int`, `string`, `bool` | Attribute value type |
| `driver` | yes | DRA driver name | Driver that owns the attribute |
| `mapsTo` | no | `numaNode`, `pcieRoot`, `socket` | Standard topology attribute this maps to. `numaNode` also requires `<dns-domain>/<identifier>` form because it builds a CEL selector. |
| `partitioning` | no | `group`, `info` | Whether equal values form separate partition groups; defaults to `info` |
| `constraint` | no | `match`, `none` | Whether matching devices are constrained in expanded claims; defaults to `none` |
| `enforcement` | no | `required`, `preferred` | Whether a match constraint is hard or best-effort; defaults to `required` |
| `fallbackAttribute` | no | Qualified attribute name | Looser attribute to use when the primary match cannot be satisfied |
| `deviceClass` | no | DeviceClass name | Overrides the driver's default class name for generated sub-requests |
| `description` | no | Free-form text | Human-readable rule description |

Standard attributes are `resource.kubernetes.io/numaNode`, `resource.kubernetes.io/pcieRoot`, and `nodepartition.dra.k8s.io/socket`. See [`test/e2e/topology-rules.yaml`](../test/e2e/topology-rules.yaml) for rules covering real drivers.

The coordinator supplies a built-in `resource.kubernetes.io/pcieRoot` match rule with NUMA fallback unless a rule has exactly `attribute: resource.kubernetes.io/pcieRoot` and `constraint: match`. A rule with only `mapsTo: pcieRoot` does not replace it. Driver-specific NUMA rules still generate per-driver CEL selectors, because different drivers may use different attribute names.

`preferred` constraints are emitted only when the topology model can satisfy them. A `required` constraint is retained even when no placement can satisfy it, so the workload remains unschedulable instead of silently losing the requested alignment.

## Device groupings

Device groupings describe a named combination of DRA device classes that should be co-located. Apply them in the trusted namespace; see [`examples/`](../examples/) and the larger real-world set in [`demos/topology-coordinator-admin-debug/configmaps.yaml`](../demos/topology-coordinator-admin-debug/configmaps.yaml), which combines capacity, a rail, and passthrough.

### Fields

`data` keys:

| Key | Required | Values / validation |
| --- | --- | --- |
| `name` | yes | Sanitizes to a DNS-1123 label of at most 56 characters |
| `alignment` | yes | `pcieRoot`, `numaNode`, or `socket` |
| `fallback` | no | Same values as `alignment`; opt-in looser level |
| `devices` | yes | YAML list, non-empty (see below) |
| `mode` | no | `container` (default) or `passthrough` |
| `rail` | no | `true` or `false`; see Rails |
| `displayName`, `description` | no | At most 63 and 1024 Unicode code points |
| `intentLevel` | no | `workload`, `capability`, or `hardware-specific` |
| `categories`, `workloadTypes` | no | Comma-separated; at most 16 unique entries of at most 63 code points |
| `visibility` | no | `public` or `internal` |
| `offering` | no | `true` or `false` |

`devices[]` keys (anything else is rejected, matching is case-insensitive):

| Key | Required | Purpose |
| --- | --- | --- |
| `class` | yes | Topology class used to find devices; unique within the grouping |
| `count` | yes | Positive integer |
| `capacity` | no | Map of consumable DRA capacity requests |
| `requestClass` | no | Driver-owned DeviceClass used in the expanded claim; defaults to `class`; unique within the grouping |
| `requestDriver` | no | Driver that serves `requestClass`; requires `requestClass` |
| `passthrough` | no | Marks a physical device exposed to a VM; needs `mode: passthrough` |
| `selectors` | no | CEL selectors copied to the sub-request |
| `opaque` | no | `driver` plus a JSON-object `parameters`; copied unchanged; needs `mode: passthrough` and `passthrough: true` |

### Example, validation, and upgrades

A minimal grouping:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: gpu-nic-pair
  namespace: nodepartition
  labels:
    nodepartition.dra.k8s.io/device-grouping: "true"
data:
  name: gpu-nic-pair
  rail: "true"
  alignment: pcieRoot
  devices: |
    - class: gpu.example.com
      count: 1
    - class: rdma.example.com
      count: 1
```

`alignment` and `fallback` accept `pcieRoot`, `numaNode`, or `socket`. Device entries require `class` and a positive `count`; each entry may also provide a `capacity` map for consumable DRA capacity. Set `rail: "true"` only for an explicit GPU+NIC offering that must be satisfied on one PCIe root. Rail offerings must use `pcieRoot` alignment without a fallback and exactly two device entries with `count: 1`; the coordinator treats the device classes as the operator-declared GPU and NIC pair and does not infer rail intent from class names.

`alignment` is the preferred co-location level. `fallback` is opt-in: it evaluates remaining devices at a looser level and publishes separate fallback instances; it is never added automatically and it does not stack a NUMA constraint on top of a PCIe-root class. An aggregate grouping preserves one alignment level without pinning a particular value, so `alignment: numaNode` means all expanded requests must share one NUMA node while the scheduler chooses which node. If a grouping produces both preferred and fallback instances, the aggregate does not encode both alternatives; select a topology-specific class when the alignment choice matters.

Keys inside `devices` are matched strictly: an unknown or duplicate key (for example `passthru:` instead of `passthrough:`) rejects the ConfigMap with an error naming the key. Matching stays case-insensitive, so `Class:` is still accepted. Unknown top-level `data` keys are ignored with a warning. A ConfigMap rejected on initial load is absent from the grouping store, so its DeviceClasses are removed on the next reconcile; an invalid update keeps the last valid cached grouping until the update is corrected. Before upgrading, check every `devices:` block for keys outside the supported set: `class`, `requestClass`, `count`, `capacity`, `passthrough`, `selectors`, and `opaque`.

### Published classes and naming

Managed DeviceClasses carry a versioned `PartitionConfig` (`apiVersion: nodepartition.dra.k8s.io/v1alpha1`). The webhook denies claims against a managed class whose `apiVersion` it does not support, or whose config has fields it does not know, instead of expanding the claim without them. Classes published before versioning have no `apiVersion` and are still accepted until the next reconcile rewrites them. Strict decoding only protects webhooks that already include it, so during a rolling update from an older release the old pods still ignore unknown fields.

The aggregate DeviceClass keeps the grouping name. Explicit rail instances are named `<grouping>-rail<N>` and carry the `railIndex` label. Ordinary grouping instances retain their locality meaning in the name, such as `<grouping>-numa<N>`, `<grouping>-pcie-<root>`, or `<grouping>-socket<N>`, and do not carry `railIndex`. Fixed PCIe partition classes use their PCIe-root name directly and also do not use `railIndex`. This is a breaking rename for previously generated non-rail and fixed-partition `-rail<N>` classes. During an upgrade, the coordinator pauses publication while those legacy classes exist; remove the legacy classes (or uninstall/reinstall the release), update claims to the new topology-specific names, then resume reconciliation.

All DeviceClasses are cluster-scoped. A topology-specific name describes a repeatable shape, not a node identity. When matching instances coalesce to one published name, the coordinator adds a `nodepartition.dra.k8s.io/count` label; that label is catalog metadata, not a DRA capacity field. Requesting such a class lets the scheduler choose any matching node; use Pod or VMI node affinity/selection when placement on a particular node is required. The grouping-wide aggregate has no fixed rail or locality value and is scheduler-placed across every satisfiable instance.

For an aggregate `alignment: numaNode` grouping, every participating device must expose the scalar NUMA attribute needed to build one `matchAttribute` constraint. If a driver publishes only list-valued NUMA attributes, the coordinator keeps the topology-specific classes but may omit the grouping-wide NUMA aggregate.

### Passthrough and VFIO

Passthrough devices must declare their VFIO contract explicitly: set `requestClass`, `selectors`, or `opaque` on every device with `passthrough: true`, otherwise the ConfigMap is rejected. The coordinator no longer injects VFIO configuration from driver names, so existing passthrough ConfigMaps must be updated before VMs using them restart. If `requestClass` is served by a different driver than `class`, also set `requestDriver` (see below); otherwise the generated NUMA selector uses the attribute namespace of the driver that publishes `class`.

Grouping offerings default to `mode: container`, which keeps ordinary Pod claims free of VM passthrough configuration. Set `mode: passthrough` and mark only the physical devices that should be exposed to a VM:

```yaml
data:
  name: amd-gim-gpu-nic-vfio
  mode: passthrough
  alignment: pcieRoot
  devices: |
    - class: gpu.amd.com
      requestClass: gpu.amd.com
      selectors:
        - 'device.attributes["gpu.amd.com"].type == "vfio"'
        - 'device.attributes["gpu.amd.com"].isVF == true'
      count: 1
      passthrough: true
    - class: sriovnetwork.k8snetworkplumbingwg.io
      requestClass: sriovnetwork.k8snetworkplumbingwg.io
      count: 1
      passthrough: true
      opaque:
        driver: sriovnetwork.k8snetworkplumbingwg.io
        parameters:
          apiVersion: sriovnetwork.k8snetworkplumbingwg.io/v1alpha1
          kind: VfConfig
          driver: vfio-pci
```

`class` identifies the topology class used to find compatible devices. `requestClass` optionally selects a different driver-owned DeviceClass in the expanded claim; it defaults to `class`. `selectors` are copied to that sub-request, and `opaque` is copied unchanged to the matching request configuration. The coordinator does not interpret opaque parameters or infer VFIO from a class name.

`requestDriver` (optional, requires `requestClass`) names the driver whose devices `requestClass` allocates. Set it when that driver differs from the one that publishes `class`:

- The NUMA selector on the generated request uses `requestDriver`'s NUMA attribute. That attribute comes from a topology rule with `mapsTo: numaNode` for `requestDriver`, and the standard `resource.kubernetes.io/numaNode` is used when there is no such rule. Adding a rule for `requestDriver` can also affect automatic partitions and which DeviceClass is chosen for that driver.
- A request is left out of an alignment unless its attribute is a standard `resource.kubernetes.io/` attribute (which describes the physical device) or belongs to `requestDriver`'s own domain, because `matchAttribute` cannot be satisfied if an allocated device lacks the attribute. If fewer than two requests remain, the alignment is dropped.
- When `opaque` is set, `opaque.driver` must equal `requestDriver`.
- An older controller ignores `requestDriver` and builds the selector from the source driver.

```yaml
devices: |
  - class: gpu.example.com
    requestClass: gpu-vfio.example.com
    requestDriver: vfio.example.com
    count: 1
    passthrough: true
```

For an NVMe passthrough offering, keep topology discovery on `dra.nvme` but request the driver's VFIO class:

```yaml
devices: |
  - class: dra.nvme
    requestClass: dra.nvme-vfio
    count: 1
    passthrough: true
```

The `passthrough` flag controls VM-oriented request splitting and KubeVirt host-device mapping. Actual binding, CDI creation, and cleanup remain responsibilities of the selected DRA driver.

The coordinator's passthrough fields are generic; the selected driver still defines the actual VFIO contract. Common integration shapes are:

| Integration | Coordinator-side declaration | Driver-owned responsibility |
| --- | --- | --- |
| NVIDIA standalone GPU | Request the driver's VFIO DeviceClass (for example `vfio.gpu.nvidia.com`) and any driver-defined selectors | GPU VFIO preparation, CDI/device injection, and restore |
| AMD GIM/VFIO path | Request the VFIO-capable class published by the AMD driver and use its documented selectors (for example a `type: vfio`/VF selector) | GIM/VFIO binding and guest device setup; do not infer this from an `amdgpu` class name |
| SR-IOV | Use the SR-IOV driver's request class and copy its documented `VfConfig` opaque payload, such as `driver: vfio-pci` when that driver requires it | VF configuration, NAD/runtime integration, VFIO binding, and cleanup |
| NVMe passthrough | Discover topology through `dra.nvme` but request the driver's VFIO class (for example `dra.nvme-vfio`) | NVMe rebind/unbind, guest exposure, and return to the normal driver |

These names are examples, not coordinator-wide defaults. Inspect the installed driver's DeviceClasses and claim contract before creating a grouping. If a driver has separate `amdgpu`, GIM, or VFIO offerings, select the exact class and selectors for the desired runtime path.

For a passthrough sub-resource with `count > 1` and no consumable capacity, the webhook creates separate `count: 1` requests so KubeVirt can map each physical device to a unique host device. Capacity-backed sub-resources keep their count and capacity request because they represent shared capacity rather than individually passed-through devices.

For KubeVirt, reference a passthrough grouping from a `ResourceClaimTemplate` in the VMI's `resourceClaims` list. The webhook resolves the template before Kubernetes creates its generated claim, expands the declared requests, creates host-device entries only for explicitly marked passthrough sub-resources, and rewrites existing `requestName` fields when needed. A container-mode offering remains a normal DRA request and never causes implicit VFIO binding.

```yaml
data:
  name: nvidia-gpu-nic-vfio
  mode: passthrough
  alignment: pcieRoot
  devices: |
    - class: gpu.nvidia.com
      requestClass: vfio.gpu.nvidia.com
      count: 2
      passthrough: true
    - class: rdma.mellanox.com
      requestClass: rdma.mellanox.com
      count: 2
      passthrough: true
```

Automatically generated partition classes remain container offerings. Passthrough configuration is copied only when it is explicitly declared on the grouping sub-resource; the webhook never synthesizes a driver configuration from a name.

### Rails

A rail is an explicit GPU+NIC offering that must be satisfied on one PCIe root. Set `rail: "true"` with `alignment: pcieRoot`, no `fallback`, and exactly two devices with `count: 1`. Rail instances are named `<grouping>-rail<N>` and carry the `railIndex` label.

## Intent metadata and offerings


Grouping ConfigMaps may include these optional keys in `data` to describe an offering for discovery:

| Key | Accepted values | Published metadata |
| --- | --- | --- |
| `displayName` | Up to 63 Unicode code points | `nodepartition.dra.k8s.io/display-name` annotation |
| `description` | Up to 1024 Unicode code points | `nodepartition.dra.k8s.io/description` annotation |
| `intentLevel` | `workload`, `capability`, or `hardware-specific` | `nodepartition.dra.k8s.io/intent-level` label |
| `categories` | Comma-separated list | `nodepartition.dra.k8s.io/categories` annotation |
| `workloadTypes` | Comma-separated list | `nodepartition.dra.k8s.io/workload-types` annotation |
| `visibility` | `public` or `internal` | `nodepartition.dra.k8s.io/visibility` label |
| `offering` | `true` or `false` | `nodepartition.dra.k8s.io/offering` label on the aggregate class |

All seven metadata keys are optional, so valid existing grouping ConfigMaps need no metadata changes. The required `data.name` must sanitize to a DNS-1123 label of at most 56 characters; validation may reject older invalid or overlong names. The 63-code-point `displayName` cap and 1024-code-point `description` cap are project policies, not Kubernetes API limits. `categories` and `workloadTypes` are split on commas, trimmed, emptied values dropped, and duplicates removed while retaining first-seen order. Each list accepts at most 16 unique entries of at most 63 Unicode code points; the published annotation joins those entries with commas.

Intent labels and annotations appear on aggregate DeviceClasses for user-defined groupings. Per-instance and auto-detected grouping classes do not receive intent metadata. `visibility` is published only when explicitly set. Discover matching classes with labels:

```sh
kubectl get deviceclasses -l nodepartition.dra.k8s.io/intent-level=workload
kubectl get deviceclasses -l nodepartition.dra.k8s.io/visibility=public
kubectl get deviceclasses -l nodepartition.dra.k8s.io/visibility=public \
  -o custom-columns='NAME:.metadata.name,LEVEL:.metadata.labels.nodepartition\.dra\.k8s\.io/intent-level,DISPLAY:.metadata.annotations.nodepartition\.dra\.k8s\.io/display-name'
```

Every grouping DeviceClass also carries `nodepartition.dra.k8s.io/mode` (`container` or `passthrough`) and `nodepartition.dra.k8s.io/source` (`configmap` or `auto`). Aggregate classes carry `nodepartition.dra.k8s.io/aggregate=true`. The `offering` label is set only from the explicit `offering: "true"` key; it is never inferred from other metadata, and it marks the class as a stable request target.


`offering: "true"` is the only thing that sets the `offering` label. The other metadata keys only publish discovery labels and annotations on aggregate classes. (In code, `IntentMetadata.IsOffering()` means "has any metadata" and is unrelated to the `offering` label.) List published entries with [`dratopoctl`](../README.md#discover-offerings-with-dratopoctl).


`categories` and `workloadTypes` are comma-joined annotations, so Kubernetes label selectors cannot query those values. Intent metadata currently supports discovery and cataloging only; request-level claim rejection based on categories or workload types is deferred, and the webhook expands claims from the DeviceClass scheduling configuration independently of these annotations.

If a grouping ConfigMap update is invalid, the coordinator retains the last valid cached grouping and logs that its metadata may be stale until a valid update arrives. If distinct `data.name` values sanitize to the same DeviceClass name, or their emitted aggregate/per-instance names overlap (for example, an explicit `gpu` rail emits `gpu-rail0` while another grouping's aggregate is `gpu-rail0`), grouping publication pauses to preserve the last successfully published classes; other grouping updates wait until the conflict is corrected. Two ConfigMaps that define the same `data.name` pause publication the same way, so an offering name always has one owner. A grouping class never overwrites a partition class with the same name, or the reverse; the publish fails instead.

Example ConfigMaps:

- [GPU inference workload](../examples/intent-workload-gpu-inference.yaml)
- [Low-latency network capability](../examples/intent-capability-low-latency-network.yaml)
- [GPU and NIC hardware alignment](../examples/intent-hardware-gpu-nic-aligned.yaml)
