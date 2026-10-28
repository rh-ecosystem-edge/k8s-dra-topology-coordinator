# Topology Coordinator Test Plan

## 1. Goal

Validate the topology coordinator end to end with
[`k8s-dra-harness`](../../../devel/k8s-dra-harness) as the driver and workload
environment. The plan covers the coordinator's catalog, grouping, admission,
allocation, webhook high availability, configuration reload, and optional
KubeVirt behavior.

The harness owns driver installation, driver readiness, temporary workload
namespaces, DRA claims/pods, evidence snapshots, and cleanup. Coordinator
tests own coordinator installation, topology/grouping ConfigMaps, managed
DeviceClass assertions, admission probes, leader/follower checks, and
coordinator cleanup.

## 2. Test lanes

| Lane | Environment | Purpose | Run frequency |
| --- | --- | --- | --- |
| Unit | Go test and envtest | Fast deterministic logic and admission coverage | Every PR |
| Harness smoke | Harness `example` adapter, no special hardware | Live webhook, catalog, native DRA allocation, failure and cleanup checks | Every merge |
| Harness topology | Harness `example` plus CPU, or synthetic ResourceSlices | Cross-driver catalog/grouping and topology constraints | Nightly / release |
| Hardware | SR-IOV, GPU, or RoCE-capable nodes | VFIO, real network allocation, device runtime behavior | Release qualification |
| KubeVirt | Existing KubeVirt/OpenShift Virtualization with DRA gates | VMI template expansion and host-device mapping | Release qualification |

The hardware-free lane is meaningful, but it cannot prove that a real NIC or
GPU was rebound to `vfio-pci`. That requires the hardware lane.

## 3. Harness integration

### 3.1 Driver setup

Start with the harness's `example` adapter. It publishes mock
`gpu.example.com` devices and is the first smoke path because it does not need
GPU hardware. Add the harness `cpu` adapter when the cluster has CPU DRA
runtime support; it gives us a second real driver for cross-driver claim
tests without requiring a GPU server.

Run from the harness checkout:

```sh
cd /home/jhull/devel/k8s-dra-harness
make unit-test
make install-ginkgo
KUBECONFIG=/absolute/path/to/kubeconfig \
DRA_HARNESS_CONFIG=/absolute/path/to/coordinator-harness.yaml \
TEST_FEATURES="smoke dra" \
ARTIFACT_DIR=/absolute/path/to/artifacts \
make run-tests
```

Use a dedicated cluster or a unique run namespace. Do not run an install-mode
test against a shared production namespace.

### 3.2 Coordinator bridge

The harness does not install the topology coordinator or inspect coordinator
managed `DeviceClass` objects. Add a thin coordinator test script under
`test/e2e/harness/` in this repository and invoke it as a harness verification
command. This lets the harness keep the driver process alive while the
coordinator script runs, and it preserves one cleanup boundary.

The bridge script must:

1. apply topology rules and grouping ConfigMaps;
2. install the coordinator Helm chart with a unique release name;
3. wait for deployment readiness, webhook endpoints, leader election, and
   managed DeviceClasses;
4. run the coordinator assertions below;
5. collect YAML, webhook responses, events, logs, and metrics under the
   harness evidence directory; and
6. delete claims, pods, ConfigMaps, the coordinator release, and any
   synthetic ResourceSlices in a `trap` before returning.

Because managed `DeviceClass` objects are cluster-scoped, the bridge must also
mark and delete the classes produced by its configured driver name. The Helm
release must use a run-scoped leader-election ID so parallel runs cannot share
the same lease.

The script receives `KUBECONFIG` and `DRA_HARNESS_NAMESPACE` from the harness;
pass the evidence directory explicitly as a command argument. It must derive a
unique coordinator release/run ID, label every object it creates, and use that
label for cleanup. Cleanup must run on success and failure; on failure it
should copy diagnostics before deleting only run-owned objects. Add a
config-validation/smoke test for this script before wiring it into the harness
verification command.

Configure the harness `testPlan.verification` block with the bridge script and
an evidence directory. The command must be inside the configured `scriptsDir`
because the harness intentionally rejects commands outside that checkout.

A minimal install-mode configuration looks like this (replace the checkout and
registry paths):

```yaml
namespace: dra-coordinator-harness
registry: quay.io/example
cleanup: true
drivers:
  - name: example
    sourcePath: /path/to/dra-example-driver
testPlan:
  profile: topology-coordinator
  scenarios: [resource-slices]
  verification:
    scriptsDir: /path/to/k8s-dra-topology-coordinator/test/e2e/harness
    evidenceDir: /tmp/dra-coordinator-evidence
    commands:
      - [run-coordinator-e2e.sh, --evidence-dir, /tmp/dra-coordinator-evidence]
```

For a shared cluster, switch to `existing: true`, name the already-installed
driver namespace, and keep `cleanup: false`; the bridge must then avoid
deleting resources it does not own.

### 3.3 Synthetic topology fixtures

The harness `example` adapter provides allocation, but it does not publish an
arbitrary multi-driver PCIe/NUMA topology. For catalog and admission tests,
the bridge should create uniquely named synthetic `ResourceSlice` objects for
test-only drivers such as `test.gpu`, `test.nic`, and `test.cpu`:

- use the real test node name;
- set `spec.driver`, `spec.nodeName`, pool generation/count, and deterministic
  device names;
- include `resource.kubernetes.io/pcieRoot`, NUMA, and socket attributes;
- put GPU and NIC devices on both matching and non-matching PCIe roots; and
- delete every fixture at the end of the run.

These fixtures prove coordinator publication and webhook mutation only. They
must never be reported as proof of successful device allocation because no
driver is preparing them.

## 4. Preconditions and baseline

Before each live run, record:

- Kubernetes version and `resource.k8s.io/v1` discovery;
- node names, labels, allocatable CPU/memory, and readiness;
- existing coordinator Helm releases, webhook configurations, leases,
  ConfigMaps, managed DeviceClasses, ResourceClaims, and ResourceSlices;
- harness and coordinator Git commits, image digests, chart values, and the
  configured coordinator driver name; and
- whether KubeVirt, cert-manager, NetworkPolicy enforcement, and the selected
  DRA feature gates are present.

Fail preflight if the run would collide with an existing release, webhook,
lease, or managed class. Save the baseline so cleanup can prove that unrelated
objects were not changed.

## 5. Test matrix

### A. Catalog and topology model

| ID | Setup/action | Expected result |
| --- | --- | --- |
| CAT-01 | Publish one driver with devices on two NUMA nodes and two PCIe roots | Managed `pcieroot`, `numa`, and `full` classes appear when `partitionMode=auto`; selectors and opaque `PartitionConfig` are valid |
| CAT-02 | Publish multiple pools and multiple nodes | Classes contain only devices from the correct node/pool; no cross-node partition is emitted |
| CAT-03 | Add topology-rule ConfigMaps for NUMA, PCIe root, and socket | Driver-specific CEL selectors and standard alignment constraints use the configured mappings |
| CAT-04 | Remove a topology rule and reconcile | Output converges to the remaining valid model without stale selectors |
| CAT-05 | Feed overlapping representations of one physical device | Effective counts deduplicate the device; no partition overstates capacity |
| CAT-06 | Configure `partitionMode=partitions` and `partitionMode=groupings` separately | Only the selected output family is published |
| CAT-07 | Restart the coordinator and reconcile from existing slices/config | The same class names, selectors, labels, and opaque config are recreated deterministically |
| CAT-08 | Set a non-default `--driver-name` such as `topology.test.example` | Controller labels/config and webhook lookup use the same configured name; a claim against a managed class expands successfully |

Capture every managed class as JSON and compare normalized fields, not object
resource versions or timestamps.

### B. Grouping and catalog metadata

Apply valid grouping ConfigMaps for:

- container GPU+CPU;
- container GPU+NIC;
- explicit passthrough GPU+NIC; and
- a capacity-bearing device group.

| ID | Setup/action | Expected result |
| --- | --- | --- |
| GRP-01 | Create a valid grouping | Aggregate and per-rail classes appear with expected alignment, fallback, counts, and selectors |
| GRP-02 | Use `mode: container` with GPU/NIC-looking class names | Generated `PartitionConfig` has no passthrough device flags and ordinary Pod claims receive no VFIO config |
| GRP-03 | Use `mode: passthrough` with an explicit opaque config on only the NIC entry | Only the NIC request receives the declared opaque config; the GPU remains a normal request |
| GRP-04 | Update display name, description, intent, categories, workload types, and visibility | Aggregate class labels/annotations update; per-rail classes remain semantically unchanged |
| GRP-05 | Submit malformed YAML, zero/negative counts, unknown mode, overlong metadata, or missing class | Invalid update is rejected and the last valid cached grouping remains published |
| GRP-06 | Create sanitized-name and aggregate/per-rail name collisions | Publication pauses for the conflicting set and resumes after correction; existing valid output is retained |
| GRP-07 | Delete a grouping ConfigMap | Its aggregate and rail classes are removed; unrelated offerings remain |
| GRP-08 | Reapply the same ConfigMap in a different order | Output is byte-for-byte stable after normalization |

### C. ResourceClaim admission

Use direct `ResourceClaim` POSTs through the Kubernetes API and retain the
AdmissionReview response plus the stored object.

| ID | Setup/action | Expected result |
| --- | --- | --- |
| ADM-01 | Claim a coordinator-managed class | Original request expands into the configured underlying requests, count, selectors, constraints, and expansion annotation |
| ADM-02 | Claim a native driver `DeviceClass` | Claim is allowed and unchanged |
| ADM-03 | Claim a missing native class | Native DRA behavior is preserved; the coordinator does not reject an ordinary claim merely because it is unknown to the catalog |
| ADM-04 | Claim a managed class with malformed/missing `PartitionConfig` | Admission fails closed with a useful error |
| ADM-05 | Submit the same claim through CREATE and UPDATE | Expansion is idempotent; requests and constraints are not duplicated |
| ADM-06 | Submit a container grouping that contains a NIC/RDMA-looking class | No VFIO config is injected |
| ADM-07 | Submit an explicit passthrough grouping | Only explicitly declared opaque config is copied to the matching passthrough request; no driver-name heuristic runs |
| ADM-08 | Use a count greater than one | Expanded request naming and count are correct for container resources; passthrough resources are split into unique requests as designed |
| ADM-09a | Create a managed DeviceClass with missing/malformed opaque config | Managed catalog claim is denied; ordinary native claim still passes through |
| ADM-09b | Use an explicit API fault mechanism (proxy or temporary RBAC denial) during DeviceClass lookup | Managed catalog claim is denied; ordinary native claim still passes through; the fault is removed before cleanup |

ADM-06 through ADM-09b are the regression tests for the review findings about
implicit passthrough and fail-open admission.

### D. Pod and ResourceClaimTemplate rewriting

| ID | Setup/action | Expected result |
| --- | --- | --- |
| POD-01 | Create a Pod referencing a directly named expanded claim | Pod reference is preserved or rewritten exactly once according to the expansion annotation |
| POD-02 | Admit a Pod referencing a `ResourceClaimTemplate` whose generated claim name is not the template name | During Pod admission, the webhook expands directly from the template; after Kubernetes creates the generated claim, verify that claim is expanded consistently and that the Pod's effective request mapping is correct |
| POD-03 | Use two containers referencing the same original request | Both references resolve to the intended generated request names without cross-container leakage |
| POD-04 | Create a Pod with an ordinary native claim | Pod is allowed and unchanged |
| POD-05 | Delete or make the template unavailable during admission | Failure policy behavior matches the webhook configuration and no dangling reference is created |

POD-02 is mandatory; the existing direct-claim test is not sufficient evidence
for the template path.

### E. Allocation and scheduler behavior

Use the harness's live `example` workload for real allocation. Every
allocation case must name a coordinator class whose `PartitionConfig` targets
the real `gpu.example.com` DeviceClass published by that harness driver. Do
not submit synthetic `test.*` classes to the scheduler; they are limited to
catalog and admission mutation tests.

| ID | Setup/action | Expected result |
| --- | --- | --- |
| ALLOC-01 | Allocate a single coordinator full/partition offering | Claim allocates and the harness workload completes |
| ALLOC-02 | Allocate two compatible claims | Both allocate until advertised capacity is exhausted |
| ALLOC-03 | Delete the consumer Pod | Allocation is released and a replacement claim can allocate |
| ALLOC-04 | Submit an unsatisfiable topology constraint | Claim/Pod remains pending with an explainable event; no false allocation occurs |
| ALLOC-05 | Allocate a multi-request claim with a same-NUMA/PCIe constraint | Requests land on the same required topology when real driver topology is available |
| ALLOC-06 | Restart the underlying harness driver | ResourceSlices recover and new allocations succeed; existing workloads follow driver semantics |
| ALLOC-07 | Exhaust capacity, then release one allocation | A previously pending claim becomes allocatable without coordinator restart |

The synthetic fixture lane may run ALLOC-04 at admission/scheduler validation
only; ALLOC-01 through ALLOC-03 and ALLOC-06 require a real driver.

### F. KubeVirt and multi-device mapping

Run only when KubeVirt/OpenShift Virtualization and the relevant DRA feature
gates are installed.

| ID | Setup/action | Expected result |
| --- | --- | --- |
| VM-01 | VMI references a coordinator `ResourceClaimTemplate` | VMI admission succeeds and generated host devices reference expanded requests |
| VM-02 | Use a container-mode offering in a VMI | VMI is rejected or remains without passthrough config according to the declared contract; it must not silently bind a device to VFIO |
| VM-03 | Use an explicit passthrough offering | Host devices and VFIO opaque configuration are present only for marked devices |
| VM-04 | Request count > 1 | Each physical passthrough device maps to a unique request/hostDevice; no duplicate requestName is emitted |
| VM-05 | Hold the VMI while the driver restarts | Restart behavior and claim allocation remain consistent with the driver contract |
| VM-06 | Delete the VMI and claim | Host allocation is released and no generated template claim or host-device reference remains |

VM-04 is the validation item for `splitCount`; a single-device VM test does not
cover it.

### G. High availability and leadership

Run the coordinator with two replicas and a short, test-only lease duration.

| ID | Setup/action | Expected result |
| --- | --- | --- |
| HA-01 | Discover both controller Pods, send equivalent claims to each Pod endpoint (via direct endpoint access or port-forward), then send one request through the Service | Direct-replica admission decisions and patches are identical; the Service path is used only for availability |
| HA-02 | Update a grouping ConfigMap while the follower receives webhook traffic | Follower refreshes its rule store and makes the same decision as the leader without waiting for reconciliation |
| HA-03 | Delete the leader Pod | Exactly one new leader publishes; no duplicate or orphaned DeviceClasses appear |
| HA-04 | Force leadership loss during a deliberately slowed reconcile | In-flight work is canceled; the old leader does not publish after lease loss |
| HA-05 | Stop API access briefly and restore it | Webhook health/readiness and controller convergence recover without stale catalog output |
| HA-06 | Restart both replicas one at a time | Webhook remains available within the declared disruption budget and converges to one model |

Compare normalized DeviceClass sets and webhook patches from both replicas.
Do not use only the Service endpoint; direct Pod endpoints are required to
prove replica determinism.

### H. Webhook transport, security, and observability

| ID | Setup/action | Expected result |
| --- | --- | --- |
| OPS-01 | Delete/rotate the webhook certificate or break the Service | ResourceClaim admission fails closed (`failurePolicy: Fail`); Pod/VMI behavior matches their configured best-effort policy |
| OPS-02 | Enable the chart NetworkPolicy | API-server admission traffic remains allowed; unrelated ingress is denied; controller API/DNS egress still works |
| OPS-03 | Remove an RBAC permission in a disposable role | Failure is visible in logs/metrics and does not silently publish partial state |
| OPS-04 | Scrape health and metrics endpoints during reconcile/admission | Health remains accurate; reconcile, admission, and error counters change as expected |
| OPS-05 | Inject invalid configuration and transient API errors | Logs identify the object and reason; no panic, crash loop, or unbounded retry storm |

### I. Upgrade, rollback, and cleanup

| ID | Setup/action | Expected result |
| --- | --- | --- |
| LIF-01 | Upgrade coordinator with unchanged rules and existing claims | Existing claims remain valid; managed classes converge without unnecessary recreation |
| LIF-02 | Treat `driverName` as an immutable installation identity; uninstall the old release and its run-owned managed classes, then install with a new name | No in-place rename is attempted; the new webhook recognizes only the new identity, no old managed class remains, and the migration procedure is documented as a breaking reinstall |
| LIF-03 | Roll back after a grouping update | Last compatible catalog is restored and claims fail safely if the older binary cannot understand new data |
| LIF-04 | Run harness and coordinator cleanup | Only run-owned namespaces, claims, pods, ConfigMaps, releases, fixtures, and managed classes are removed; baseline objects are unchanged |

## 6. Evidence to retain

For every failed or release-qualification run, retain:

- harness evidence directory and verification commit;
- `kubectl get` JSON for ResourceSlices, DeviceClasses, ConfigMaps,
  ResourceClaims, Pods, VMIs, Events, Leases, webhook configurations, and
  NetworkPolicies;
- coordinator and driver logs from all replicas, with timestamps;
- AdmissionReview request/response and JSON patches for every admission case;
- normalized catalog snapshots before/after each ConfigMap or leadership event;
- claim allocation status and Pod/VMI status transitions; and
- test configuration, image digests, chart values, node topology, and feature
  gate settings.

## 7. Pass/fail gates

The run passes only when:

1. all Unit and Harness smoke cases pass;
2. no managed claim is admitted without complete expansion;
3. native claims remain unmodified;
4. container offerings never receive implicit VFIO configuration;
5. explicit passthrough offerings inject VFIO only for marked devices;
6. template-backed Pod and VMI references are proven with generated claim
   names;
7. custom driver-name and two-replica determinism tests pass;
8. no old leader publishes after lease loss;
9. allocation/release cases pass for at least one real driver; and
10. cleanup leaves the recorded baseline unchanged.

Hardware and KubeVirt gates may be marked `not run` only when their documented
preconditions are absent. They must not be reported as passing based on unit
tests or synthetic ResourceSlices.

## 8. Recommended execution order

1. `go test ./...` and existing envtest integration tests.
2. Harness `example` smoke plus coordinator catalog/admission bridge.
3. Add CPU or synthetic slices for cross-driver grouping and HA tests.
4. Run allocation, restart, and failure-policy cases.
5. Run two-replica leader/follower cases.
6. Run KubeVirt and SR-IOV/GPU hardware lanes.
7. Run upgrade/rollback and cleanup checks.

This order catches deterministic catalog and admission regressions before
spending time on driver restarts or hardware-dependent validation.
