# KEP Draft: Device Bundles for DRA Extended Resources

**Status:** Proposed. This is a draft upstream Kubernetes enhancement, kept here to refine before filing in kubernetes/enhancements.

- **Target:** alpha in 1.39 at the earliest. The reference tree is at 1.38 beta.
- **Owning SIG:** SIG Scheduling.
- **Participating:** SIG Node (quota and kubelet interplay), WG Device Management.
- **KEP number:** to be assigned.

**Scope:** Let one DRA extended resource stand for a bundle: several device requests, possibly from different DeviceClasses (and so different drivers), with `matchAttribute` constraints and opaque config. A Pod then asks for N topology-aligned bundles in one line:

```yaml
resources:
  limits:
    nodepartition.dra.k8s.io/gpu-local-bundle: 2
```

**Related specs:**

- [003: Webhook feature freeze](003-webhook-feature-freeze.md)
- [004: `dratopoctl bundles`](004-dratopoctl-bundles.md) (the baseline until this lands)
- [005: Controller bundle stamping](005-controller-bundle-stamping.md)

**Upstream code references** are verified against a local Kubernetes tree at `v1.38.0-beta.0-831-g1988b1907f1`.

## Summary

KEP-5004 (DRA extended resources, GA and locked on in 1.37, `pkg/features/kube_features.go:1508`) maps an extended resource name to one DeviceClass. For a Pod requesting `example.com/gpu: N`, the scheduler synthesizes a ResourceClaim with one request per container against that class. That claim has no constraints and no config.

This KEP adds an optional **bundle** to a DeviceClass. The bundle is a small new type built from existing pieces: named `ExactDeviceRequest`s, `DeviceConstraint`s and `DeviceClaimConfiguration`s. For each requested unit, the scheduler stamps the bundle's requests, constraints and config into the synthesized claim. The allocator and the kubelet are unchanged.

## Motivation

With device plugins, users wrote `nvidia.com/gpu: 1`, and the kubelet Topology Manager aligned devices from different resources on the node without being asked. With DRA, aligning devices across drivers has to be written out:

- one request per driver class
- `matchAttribute` constraints on `resource.kubernetes.io/pcieRoot` or `resource.kubernetes.io/numaNode`
- request names wired into containers
- a ResourceClaim or ResourceClaimTemplate in every namespace

Projects keep rebuilding the same "claim macro" out of tree:

- **The node partition topology coordinator** expands claims with a mutating webhook, and is moving to pre-rendered templates (specs 004 and 005).
- **KubeVirt VEP-300 (Managed DRA Claims)** generates claims for VMIs from an admin-defined provisioner object.

Pods have no native way to request an admin-curated, multi-device, aligned bundle by name. The extended-resource path already provides the familiar syntax, a per-Pod claim lifecycle and quota integration. What it lacks is a way to describe several devices and how they relate.

### Goals

- A Pod requests N aligned bundles with one extended resource name.
- Admins define a bundle once, cluster-wide. Users need no namespaced objects.
- A bundle may hold requests to several DeviceClasses, plus constraints and opaque config.
- No change to the allocator, the kubelet or DRA drivers.
- **Fail closed.** A bundle class can never be allocated as if it were a plain device class, under any version skew.

### Non-goals

- Topology discovery, or computing which bundles fit (stays out of tree).
- Fallback between alternative bundle shapes.
- Referencing bundles from `pod.spec.resourceClaims` (see Alternatives).
- KubeVirt VMIs. virt-launcher needs request names in the VMI spec, and extended-resource claims expose them only in Pod status.
- Sharing devices across units. Each unit is an independent aligned group.

## Proposal

### API

Add a field to `DeviceClassSpec` (`staging/src/k8s.io/api/resource/v1/types.go:2637`). It is separate from `extendedResourceRequest` (`:2689`), which is gated by alpha `DRAExtendedResourceCapacity` (`:2687`) and depends on `DRAConsumableCapacity` (`kube_features.go:2586`). With its own field, bundles graduate independently.

```go
type DeviceClassSpec struct {
    // ... existing fields ...

    // extendedResourceBundle, when set, makes this class a bundle definition.
    // A Pod requesting N units of this class's extended resource gets N copies
    // of these requests, constraints and config in the scheduler-synthesized
    // ResourceClaim, with request names rewritten per unit. A bundle class
    // is never allocatable itself: requests that name it directly are
    // unsatisfiable.
    // Requires extendedResourceName (or the implicit name) to be useful.
    // +featureGate=DRAExtendedResourceBundles
    // +optional
    ExtendedResourceBundle *ExtendedResourceBundle `json:"extendedResourceBundle,omitempty"`
}

// ExtendedResourceBundle describes one unit of a bundle.
type ExtendedResourceBundle struct {
    // requests are stamped once per unit. Each names exactly one device class.
    // +listType=atomic
    // +k8s:maxItems=32
    Requests []BundleRequest `json:"requests"`

    // constraints apply within one unit. requests must be non-empty.
    // +optional
    // +listType=atomic
    Constraints []DeviceConstraint `json:"constraints,omitempty"`

    // config applies within one unit. requests must be non-empty.
    // +optional
    // +listType=atomic
    Config []DeviceClaimConfiguration `json:"config,omitempty"`
}

type BundleRequest struct {
    // name is local to the bundle. At most 32 characters, DNS-1123 label.
    Name string `json:"name"`
    // exactly is the existing request type.
    Exactly ExactDeviceRequest `json:"exactly"`
}
```

**Why a dedicated type, not `*DeviceClaim`.** `DeviceClaim` reserves room for future fields (`types.go:1357-1361`, for example scoring). A bundle would inherit them with no per-unit meaning, and "picks up additions automatically" is a liability for an API review. `DeviceRequest` also carries `firstAvailable`, which bundles reject. The dedicated type exposes only the supported subset. It still reuses `ExactDeviceRequest`, `DeviceConstraint` and `DeviceClaimConfiguration` and their validators, so there is no duplicate request type.

**API versions.** The field and types are added to the internal type (`pkg/apis/resource`), `v1`, `v1beta2` and `v1beta1`, with generated conversions alongside the existing `ExtendedResourceRequest` conversions (`zz_generated.conversion.go`). They also get declarative-validation tags where the package uses them, and are added to the round-trip fuzzer tests.

Example. The Pod in the Scope section shows `limits` only. API defaulting copies limits to requests for extended resources, and the scheduler and kubelet read `requests` (`extendeddynamicresources.go:357`, `predicate.go:390`).

```yaml
apiVersion: resource.k8s.io/v1
kind: DeviceClass
metadata:
  name: gpu-local-bundle
spec:
  selectors:
  - cel: {expression: "false"}          # required for bundle classes: fail closed
  extendedResourceName: nodepartition.dra.k8s.io/gpu-local-bundle
  extendedResourceBundle:
    requests:
    - name: gpu
      exactly: {deviceClassName: gpu.example.com, count: 1}
    - name: nic
      exactly: {deviceClassName: sriov.example.com, count: 1}
    - name: cpu
      exactly:
        deviceClassName: dra.cpu
        capacity: {requests: {cpus: "8"}}   # needs DRAConsumableCapacity
    constraints:
    - {requests: [gpu, nic], matchAttribute: resource.kubernetes.io/pcieRoot}
    - {requests: [gpu, cpu], matchAttribute: resource.kubernetes.io/numaNode}
    config:
    - requests: [gpu]
      opaque: {driver: gpu.example.com, parameters: {...}}
```

### Fail-closed semantics

A bundle class must never allocate a device by itself. Without this rule, an empty selector list matches every device:

- the synthesized request targets `class.Name` (`extendeddynamicresources.go:402-409`);
- a plain claim can name any class;
- the implicit name `deviceclass.resource.kubernetes.io/<class>` maps to every class.

The KEP enforces it at three levels:

1. **Validation.** When `extendedResourceBundle` is set, `spec.selectors` must be exactly one selector whose `cel.expression` is the literal string `"false"`. No variants (`"false "`, `!true`) are accepted, so the check is a string compare. Old schedulers and allocators that ignore the new field then find no devices, and the Pod or claim stays unschedulable. It is never wrongly allocated. An older API server that drops the field keeps the selector, so the class stays inert there too. **This is the real safety net.**
2. **Allocator (defense in depth).** A claim request whose `deviceClassName` names a bundle class is unsatisfiable, with an explicit reason ("class X is a bundle and can only be requested as an extended resource"). The class lookup is duplicated in the three allocator implementations (`staging/.../structured/internal/stable/allocator_stable.go:563`, and the same lookup in `incubating/` and `experimental/`). The check goes in all three and returns an unschedulable-class error, not a plain error, so it surfaces as Unschedulable rather than Error. External allocator users, such as the cluster autoscaler, may lag; they are covered by rule 1.
3. **Scheduler.** For bundle classes, the extended-resource path always stamps the bundle and never emits a request against the bundle class itself.

### Validation

Validation lives in `pkg/apis/resource/validation/validation.go`, next to `validateDeviceClassSpec` (`:778`). It reuses `validateExactDeviceRequest` (`:370`), `validateDeviceConstraint` (`:557`) and `validateDeviceClaimConfiguration` (`:576`), with the bundle-local request-name set. No parallel validator is written.

Rules:

- `spec.selectors` must be exactly `[{cel: {expression: "false"}}]` (see above).
- **Bundle class `spec.config`** is rejected when a bundle is set. No request ever names the bundle class, so that config would never apply. Bundle config goes in `extendedResourceBundle.config`.
- **Requests:**
  - `exactly` only (enforced by the type), with `allocationMode: ExactCount`.
  - `adminAccess` is not allowed.
  - `derivedAttributes` (if present in the tree at implementation time) is not allowed in alpha. Claim-level CEL cost (`DeviceClaimDerivedAttributeCELMaxCost`, `validation.go:235`) is summed per claim and would multiply by units, failing late at PreBind.
  - `selectors` and `tolerations` are allowed.
  - `capacity` is allowed only when `DRAConsumableCapacity` is enabled.
  - A request may not name the bundle class itself.
- **Names:** bundle-local request names are at most 32 characters. Stamped names `container-<i>-request-<j>-u<k>-<name>` must be DNS-1123 labels of at most 63 characters (`validation.go:84`). The fixed prefix takes about 27 characters plus digits, which leaves room for 32.
- **References:** `constraints[].requests` and `config[].requests` must be **non-empty**, and every name must refer to a bundle-local request. In a plain claim an empty list means "all requests in the claim" (`validation.go:559`, `:578`). Copied into a multi-unit claim, that would span every unit, and a `matchAttribute` would force all units onto one PCIe root. Requiring explicit names removes the ambiguity.
- **Static size:** one unit fits within the claim limits. Those limits are 32 requests and 32 constraints/config entries (`types.go:1365-1367`), and 32 allocated devices (`AllocationResultsMaxSize`, `types.go:2322`). Sum the member `count`s for the device total.
- **Nesting:** a member that names another bundle class can't be checked by validation. DeviceClass strategy warnings (`registry/resource/deviceclass/strategy.go:57`, `:86`) have no client and cannot look up other classes, so there is no admission warning. Nesting is inert anyway: the member's `cel: "false"` selector matches nothing, and the allocator refuses it. The scheduler reports it at run time with a clear status.
- **Shared extended-resource names:** several classes can map to one name, and the cache picks the newest (`extendedresourcecache.go:173-176`). A plain class and a bundle class on the same name would flip semantics silently. Validation cannot see other classes, so the scheduler records a Pod status note naming the chosen class when more than one class claims the name, and the KEP documents the arbitration.

### Scheduler

Changes are in `pkg/scheduler/framework/plugins/dynamicresources/extendeddynamicresources.go`.

**Stamping.** In `createResourceRequestAndMappings` (`:355`), for a bundle class and a container quantity `q`, create units `0..q-1`. For each unit:

- stamp every bundle request as `container-<i>-request-<j>-u<k>-<name>`;
- copy the constraints and config, with names rewritten to that unit's requests. Validation guarantees the `requests` lists are non-empty, so constraints never span units. Stamping still asserts it, as a guard against stored objects that predate a validation fix;
- add one `ContainerExtendedResourceRequest` mapping per stamped request.

**Claim assembly.** `filterExtendedResources` (`:217`) sets only `Spec.Devices.Requests` today (`:278`). It now also sets `Spec.Devices.Constraints` and `Spec.Devices.Config`.

**Init containers.** Reuse happens per unit, not per device:

- Short-lived init containers today pick the "max" request by `Exactly.Count` and rewrite mappings (`:466-485`). For bundles, "max" means the largest unit count, and reuse takes all requests of the chosen units.
- When a later container needs more units than are available to reuse, it reuses units `0..m-1` under their existing names. New units get the next free `u<k>` index under the new container's prefix.
- The final sort by name (`:501`) is unchanged.

**Size check in Filter.** The claim is created in PreBind (`:563`), so an oversize claim would otherwise fail late. Filter adds up stamped requests, constraints, config entries and **devices**, across every extended resource and container of the Pod (there is one special claim per Pod, `:181`). Over a limit, the Pod is unschedulable, with a message naming the resource and the limit. Stamped requests do not depend on the node, so they are computed once per Pod (in PreFilter state) and reused across nodes.

**Gate check.** `checkExtendedResourceCapacity` (`:285-286`) becomes a per-field check:

- `extendedResourceRequest.capacity` needs `DRAExtendedResourceCapacity`.
- `extendedResourceBundle` needs `DRAExtendedResourceBundles`.
- With the gate off and the field stored, the Pod is unschedulable with an explicit reason. It never falls back to plain-class allocation.

**Device-plugin collision.** "Allocatable == 0 means DRA" is a contract shared by three places: this plugin (`extendeddynamicresources.go:243`), NodeResourcesFit scoring (`resource_allocation.go:219-220`) and the kubelet admission predicate (`predicate.go:395`). Overriding it for bundle names in one place would desync the others. The KEP does not override it. A bundle name that is also advertised by a device plugin is an unsupported admin error and is documented as such. As a guard, Filter rejects a bundle Pod on any node where allocatable for the bundle name is > 0, with a reason naming the collision, so the Pod never lands on the device-plugin path.

**Scoring.** `calculateDRAResourceTotals` (`pkg/scheduler/framework/plugins/noderesources/resource_allocation.go:351-362`) counts devices that match the class's own selectors. That is meaningless for a bundle class. Bundle classes are skipped in NodeResourcesFit and BalancedAllocation scoring for alpha. A beta question is whether to score by the scarcest member.

### Kubelet

No change. `pkg/kubelet/cm/dra/manager.go:575-580` adds every mapping for the container and resource, so the container receives the CDI devices of all its units, without per-request distinction. The kubelet admission predicate (`pkg/kubelet/lifecycle/predicate.go:391`) already strips DRA-backed requests by mapping.

`RequestMappings` has no maximum. The test plan covers status size for large Pods.

### ResourceQuota

`claimEvaluator.addExtendedResourceQuota` (`pkg/quota/v1/evaluator/core/resource_claims.go:138-167`) is unchanged. How it applies to bundles:

- **Bundle units** count against `requests.<bundle-resource>` from the Pod spec. No claim request targets the bundle class, so nothing is double-counted.
- **Member devices** count from the claim, against:
  - `requests.deviceclass.resource.kubernetes.io/<member>` (`:148`)
  - `<member>.deviceclass.resource.kubernetes.io/devices`
  - `requests.<member-ext>` (`:153`), if the member has an extended resource name
- **Per-name subtraction** of Pod usage still runs. If a Pod requests member resource X directly and a bundle also contains X's class, claim usage is 2 and Pod usage is 1, so 1 more is charged. That is correct: two devices in total.

Admins can therefore limit both "bundles" and "GPUs".

**Known limitation: late member quota.** Member-device quota is charged when the scheduler creates the claim in PreBind. A Pod can pass admission and then fail quota at bind time. This is the same as existing extended-resource claims, and the Pod stays pending with the quota error.

### Feature gate

`DRAExtendedResourceBundles`: alpha, off by default. It is independent of `DRAExtendedResourceCapacity`. `DRAExtendedResource` is GA and locked on, so it is not listed as a dependency. Bundle members that use `capacity` also need `DRAConsumableCapacity`.

### Version skew, upgrade and rollback

- **API server:** the DeviceClass strategy drops `extendedResourceBundle` on create when the gate is off, and keeps it on update if it was already set (the standard drop-disabled-fields pattern).
- **Older API servers** (HA mid-upgrade) drop the unknown field on write but keep the `cel: "false"` selector, so a class written through them is inert, never a plain allocatable class. The admin re-applies the class after the upgrade.
- **Older or skewed schedulers** (HA control plane mid-upgrade) ignore the field. They see a class whose selector is `false`, so Pods requesting the bundle stay pending. They never get wrong devices.
- **Rollback, or turning the gate off:** stored classes keep the field. Bundle Pods become unschedulable with an explicit reason, and running Pods keep their allocated claims.
- **Kubelet skew:** none. The kubelet already handles multiple mappings per resource.

### Monitoring

- Metrics:
  - `scheduler_dra_extended_resource_bundle_units_total`
  - `scheduler_dra_extended_resource_bundle_rejections_total{reason=size|nesting|gate|device_plugin}`
- Events on the Pod for size, nesting and device-plugin collision rejections.

### User stories

1. **Platform admin.** Publishes `gpu-local-bundle` once. Users in any namespace request `nodepartition.dra.k8s.io/gpu-local-bundle: 1` and get a GPU, a NIC and CPUs aligned on one PCIe root and NUMA node, without writing templates.
2. **Quota.** The admin limits a team with `requests.nodepartition.dra.k8s.io/gpu-local-bundle: 4`, and member-GPU quota still applies.
3. **Out-of-tree topology tooling.** The coordinator computes which bundles fit and publishes bundle classes, with no webhook.

## Design details: test plan

**Unit: scheduler**
- Single and multiple units, and several containers.
- Init-container reuse: full, partial (more units than are available), and short-lived "max".
- Rewriting of constraint and config names. Two units with a `matchAttribute` constraint may land on different PCIe roots.
- Size limit counted in devices.
- Nesting.
- Gate off with the field stored: unschedulable, never a plain allocation.
- Device-plugin name collision: Filter rejects nodes with allocatable > 0.
- Two classes sharing one extended-resource name: the status note names the chosen class.
- Scoring skips bundle classes.

**Unit: validation**
- The `cel: "false"` selector is required, as the only selector, and variants such as `"false "` are rejected.
- Bundle-class `spec.config` is rejected.
- Dangling references, empty `requests` lists in constraints or config, and names longer than 32 characters.
- `adminAccess` and `derivedAttributes` are rejected.
- Round-trip and conversion for `v1`, `v1beta2` and `v1beta1`.
- `capacity` without `DRAConsumableCapacity`.
- Static size, including device counts.
- Drop-disabled-fields behavior.

**Unit: allocator**
- A plain claim that names a bundle class is unsatisfiable (Unschedulable, not Error), with the stated reason, in all three allocator implementations.

**Unit: quota**
- Bundle units are charged from the Pod, member devices from the claim.
- Member extended resource also requested directly by the Pod.

**Unit: member config**
- A member class's own `spec.config` and bundle config both apply. Precedence is the same as for any claim.

**Integration** (`test/integration/dra`)
- A bundle Pod schedules, and its constraints are satisfied.
- `extendedResourceClaimStatus` lists every stamped request.
- Pod status size with many units and containers.

**Skew and gate flip**
- A scheduler with the gate off, against bundle classes: Pods stay pending.
- Flipping the gate on and then off on a live cluster.

**e2e**
- With the test DRA driver: two pools sharing `pcieRoot`. The allocated devices share the attribute, and the container sees every device.

## Graduation criteria

- **Alpha:**
  - API, validation, fail-closed allocator rule, scheduler stamping, quota rule, drop-disabled-fields behavior, metrics.
  - Unit, integration and skew tests.
- **Beta:**
  - Feedback from at least two out-of-tree users.
  - Scale test (many containers × units, status size).
  - Decisions on scoring, fallback, and `spec.resourceClaims` references.
  - PRR approval.
- **GA:**
  - Two releases at beta with no API change.
  - Conformance-style e2e tests.

## Production readiness (PRR outline)

- **Enable and disable:** with the feature gate. Disabling it leaves bundle Pods pending, not misallocated.
- **Dependencies:** none beyond DRA. `DRAConsumableCapacity` is needed only for members that use capacity.
- **Scalability:**
  - Larger synthesized claims, bounded by the claim limits.
  - Allocation cost equals that of an equivalent hand-written aligned claim.
  - `RequestMappings` grows with units × requests.
- **Troubleshooting:** Pod events and the rejection metric. A pending reason that names the bundle and the limit or problem.

## Risks and likely reviewer objections

- **"DeviceClass is becoming a claim macro."**
  - Mitigation: a bundle class is unallocatable by construction (fail closed), and the field is opt-in.
  - If reviewers prefer a separate object, Alternative A is the fallback.
- **The justification rests on Pods alone.** KubeVirt can't use extended resources. The motivation must stand on the Pod use cases, and does.
- **Per-unit request names leak into Pod status.** Document them as non-parseable, for tooling display only.
- **Claim limits:** about 32 devices per Pod, across all bundles. Raising the limit is out of scope.

## Alternatives

- **A. New cluster-scoped `ResourceClaimProfile` object** holding the `DeviceClaim`, referenced from the class or mapped to the extended resource directly.
  - Pro: cleaner separation, and it can carry status (for example, availability published by out-of-tree tools).
  - Con: a new API kind.
- **B. Reference a bundle or profile from `pod.spec.resourceClaims`**, with kube-controller-manager stamping the claim before scheduling.
  - Pro: stable request names in the spec, which would also serve KubeVirt.
  - Con: touches the core Pod API. A possible follow-up KEP.
- **C. Cluster-scoped ResourceClaimTemplate.**
  - Pro: no scheduler change.
  - Con: no count semantics, no quota by bundle name, and users still write two snippets.
- **D. Stay out of tree** (webhooks, pre-rendered templates, VEP-300-style controllers). This is today's state, and every project reinvents it.
- **E. Fields in `extendedResourceRequest`** (the earlier draft of this KEP). Rejected because it couples bundles to the alpha capacity gate (`types.go:2687`, `extendeddynamicresources.go:286`) and needs a duplicate request type.
- **F. Embed `*DeviceClaim` directly.** Rejected: it inherits `firstAvailable` and future `DeviceClaim` fields that have no per-unit meaning, so validation would be a growing deny list.

## Prototype plan

Work on a branch of the local Kubernetes tree:

1. API field, drop-disabled-fields strategy and validation behind the gate. Run `hack/update-codegen.sh`.
2. Fail-closed allocator rule.
3. Scheduler stamping, init-container unit reuse, Filter size check, per-field gate check, scoring skip.
4. Quota test.
5. Integration and skew tests.
6. Demo: the coordinator publishes one bundle class generated from an offering, and a Pod requests it in one line. Compare against the template baseline from spec 004.

**Before filing:**
- Search kubernetes/enhancements for prior "claim class", "profile" or "bundle" proposals.
- Present at WG Device Management.
- Recruit a SIG Scheduling approver.

## Impact on this repository

When the KEP graduates and a cluster enables the gate:

- **Controller:** publishes bundle classes for curated offerings, using `extendedResourceBundle` and the `cel: "false"` selector. They are derived from the same `PartitionConfig` data.
- **CLI:** `dratopoctl bundles render` gains `--format deviceclass` to preview them.
- **Webhook and stamping:** the webhook (spec 003) and template stamping (spec 005) become unnecessary for Pods. Templates remain for clusters without the gate and for VMIs.

## Open decisions

- **Unit naming scheme.** It is visible in Pod status, so it should be fixed once chosen.
- **The fail-closed marker.** Require `cel: "false"` (simple, and works on old servers), or enforce it in the allocator only (cleaner, but unsafe under skew)? This draft does both, with the selector as the primary safety net.
- **Shared-name arbitration.** Keep "newest class wins" with a status note, or reject Pods when a bundle class and a plain class share a name?
- **Scoring for beta.** Score by the scarcest member, or keep skipping?
