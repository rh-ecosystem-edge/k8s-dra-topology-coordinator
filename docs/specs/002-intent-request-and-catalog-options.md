# Intent Request and Offering Catalog Options

**Status:** Design options

**Scope:** First-class workload intent requests and discoverable hardware offerings.

## Current state

The coordinator already implements the allocation mechanism beneath an intent layer:

- administrators define groupings in labeled ConfigMaps;
- the controller validates topology, builds aggregate and topology-specific DeviceClasses, and embeds a versioned `PartitionConfig`;
- the webhook expands a selected DeviceClass into driver-specific DRA requests, selectors, constraints, and explicit opaque configuration; and
- native DRA claims that do not reference a managed `PartitionConfig` pass through unchanged.

The current offering metadata is descriptive. `displayName`, `description`, `intentLevel`, `categories`, `workloadTypes`, and `visibility` are published on aggregate DeviceClasses for discovery, but they do not affect claim expansion or scheduling.

The design must preserve:

- direct `ResourceClaim` and `ResourceClaimTemplate` use;
- native driver DeviceClasses and claims;
- existing ConfigMap-based offerings;
- explicit passthrough and KubeVirt behavior; and
- the current topology and `PartitionConfig` implementation.

## First-class intent request options

### Option A: Stable aggregate DeviceClasses

Treat the aggregate DeviceClass name as the workload-facing offering identifier:

```yaml
deviceClassName: gpu-inference
```

The coordinator guarantees that an offering name identifies the aggregate class, while topology-specific classes remain implementation details.

Required work:

- document aggregate DeviceClass names as the stable request target;
- add an explicit `nodepartition.dra.k8s.io/offering=true` label to distinguish curated offerings from fixed and auto-detected classes. Today `IsOffering()` (`internal/controller/grouping.go`) infers offering status from any metadata field, and `applyIntentMetadata` writes only the `intent-level` and `visibility` labels. The new label must come from explicit configuration, not be re-derived from `IsOffering()`;
- add an explicit aggregate marker label. Aggregate classes are currently identified only by the absence of the `numa` and `railIndex` labels, which a resolver should not rely on;
- keep the existing webhook unchanged; and
- provide a CLI/catalog command that resolves metadata to the DeviceClass name.

Advantages:

- smallest implementation;
- no new CRD or admission schema;
- native DRA compatibility; and
- no change to the existing expansion path.

Limitations:

- users still know they are writing a DRA `ResourceClaim`;
- offering selection is still based on a class name; and
- intent metadata remains descriptive unless additional policy is added.

Stable names need collision rules. The aggregate class name is derived with `sanitizeForName(GroupingName)` in `internal/controller/deviceclass_manager.go`, so two groupings can sanitize to the same name. The controller must detect the collision and reject or disambiguate it rather than let one offering shadow another.

### Option B: Offering references on native DRA objects

Allow a `ResourceClaim` or `ResourceClaimTemplate` to carry an offering reference:

```yaml
metadata:
  annotations:
    nodepartition.dra.k8s.io/offering: gpu-inference
```

The coordinator admission path resolves the offering to its aggregate DeviceClass before normal `PartitionConfig` expansion. The original offering reference should remain as an audit annotation, alongside the resolved class name.

For multiple requests, use one structured annotation mapping request names to offerings:

```yaml
nodepartition.dra.k8s.io/offerings: |
  {"gpu":"gpu-inference","network":"low-latency-network"}
```

Resolution behavior:

1. validate the offering reference and namespace scope;
2. resolve it to one aggregate DeviceClass;
3. replace the placeholder `deviceClassName` before expansion;
4. reject unknown, ambiguous, stale, or non-aggregate offerings; and
5. leave ordinary native claims unchanged when no offering reference exists.

This requires webhook coverage for both `resourceclaims` and `resourceclaimtemplates`, and template coverage must mutate, not only validate. Claims generated from a template are intercepted by the claim webhook, but the Pod and VMI handlers read the template's `deviceClassName` directly at Pod CREATE to compute the request renames. A placeholder class there yields no expansion mapping, so container `request:` names would not be rewritten while the generated claim is expanded, and the two would no longer match. Resolving the offering on template CREATE means the template, Pod, and claim paths all see the same resolved class, and the Pod and VMI handlers need no knowledge of offering metadata. ResourceClaim and ResourceClaimTemplate specs are immutable, so resolution is CREATE-only.

Webhook behavior this option depends on:

- **CREATE only.** The current rule also matches UPDATE. The resolver must ignore annotation changes on UPDATE. Separately, a claim created before its class became managed would receive an expansion patch on a later metadata update, which immutability rejects. Dropping UPDATE from the rule avoids both problems.
- **Fail closed, and run first.** The claim path currently fails open: an unmarshal error is allowed, only `managedExpansionError` denies, and a missing DeviceClass is treated as pass-through. The resolver must run before `lookupPartitionConfig` and deny any object that carries an offering annotation but does not resolve.
- **Placeholder class.** `deviceClassName` is required but its existence is not checked at admission. Publish a coordinator-owned sentinel DeviceClass whose CEL selector is `false`, so an unresolved claim can never allocate.
- **`firstAvailable`.** `expandClaim` skips requests with `Exactly == nil`, so placeholders inside `firstAvailable` subrequests would stay unresolved. Reject them unless support is added.
- **Reinvocation and idempotency.** The webhook sets no `reinvocationPolicy`, so it defaults to `Never`. Set `IfNeeded`, and have the resolver skip objects that already carry the audit annotation.
- **Pod and VMI failure policy.** Both use `failurePolicy: Ignore`. If the webhook is down, a Pod that uses an offering-backed template is admitted without the rewrite and fails later.

Advantages:

- users request an intent while retaining native DRA objects;
- direct claims and template-backed claims can share the same resolver; and
- the existing expansion code remains the final resolution step.

Limitations:

- annotations are less strongly typed than a CRD;
- multiple offering references require a small annotation convention;
- the webhook must fail closed if the resolver cannot establish the offering; and
- it adds value over Option A only when an offering name differs from its DeviceClass name (aliases, versioning, per-namespace mapping). If names stay one-to-one with classes, this is Option A with extra indirection.

### Authorization and namespace scope

DeviceClasses are cluster-scoped, and DRA does not restrict which namespaces may reference them. Any namespace can therefore bypass an offering by writing a `deviceClassName` directly, including the underlying driver classes. An offering reference is a convenience, not a boundary.

To enforce a boundary, add an admission rule such as a ValidatingAdmissionPolicy. It runs after mutation, so it sees the resolved driver classes and must key on the audit annotation rather than the offering name. A per-DeviceClass ResourceQuota is the native alternative for limiting use. Either way, `visibility` alone is not authorization.

### Option C: Typed `IntentClaim` or `HardwareClaim` CRD

Introduce a namespaced workload-facing resource:

```yaml
apiVersion: nodepartition.dra.k8s.io/v1alpha1
kind: IntentClaim
metadata:
  name: inference-claim
spec:
  offeringRef:
    name: gpu-inference
  count: 1
```

The controller resolves the reference and publishes status such as:

```yaml
status:
  resolvedDeviceClassName: gpu-inference
  conditions:
  - type: Resolved
    status: "True"
```

The main integration issue is that Pods and VMIs natively reference `ResourceClaim` or `ResourceClaimTemplate`, not arbitrary custom resources. A CRD would therefore also need one of:

- a generated `ResourceClaim` or `ResourceClaimTemplate` with a documented naming contract;
- a client-side compiler that converts `IntentClaim` to DRA objects; or
- an additional Pod/VMI admission flow that creates or references the native DRA object.

Advantages:

- typed, versioned, and strongly discoverable API;
- clear status and failure conditions;
- straightforward RBAC and namespace ownership; and
- a natural source for a catalog UI.

Limitations:

- largest implementation and migration cost;
- another object lifecycle to reconcile; and
- more complex Pod, VMI, and ResourceClaimTemplate integration.

## Catalog options

### Option A: Kubernetes-native catalog and CLI

Use coordinator-managed DeviceClasses as the catalog source and add a read-only CLI:

```sh
nodepartitionctl offerings list
nodepartitionctl offerings get gpu-inference
```

The output should include:

- offering name and resolved DeviceClass name;
- display name and description;
- intent level, categories, and workload types;
- visibility and consumer mode;
- alignment and available topology-specific variants;
- readiness or availability information; and
- the entry kind (see below).

The catalog lists every class the coordinator manages (`managed=true`), not only curated offerings, so users can discover fixed and auto-detected classes too. Each entry has a `kind`, derived from existing labels:

| Kind | Source | Labels |
| --- | --- | --- |
| `offering` | aggregate class of a grouping that carries the explicit offering label | `managed=true`, `grouping`, `offering=true` |
| `grouping` | aggregate class of a grouping without the offering label | `managed=true`, `grouping` |
| `partition` | class the controller builds from detected topology | `managed=true`, `partitionType` |

By default `offerings list` shows `offering` entries only. `--all` adds the other kinds, and `offerings get <name>` works for any kind.

Only `offering` entries carry the stable-name, collision-check, and annotation-resolution guarantees. The names of `grouping` and `partition` classes can change when topology or driver attributes change, so the catalog marks them informational and does not present them as stable request targets. They are not resolvable through the Option B offering annotation.

A DeviceClass has no status. The `count` label gives a static instance count; live availability requires reading ResourceSlices and allocated ResourceClaims. Either name those sources as a requirement or leave availability out of the first version.

Do not expose raw driver opaque configuration in the default human-facing output.

This is the smallest catalog implementation and can be built directly on the current labels and annotations.

### Option B: Read-only HTTP catalog API

Expose a structured API such as:

```http
GET /apis/nodepartition.dra.k8s.io/v1alpha1/offerings
GET /apis/nodepartition.dra.k8s.io/v1alpha1/offerings/gpu-inference
```

A response should contain sanitized offering metadata and resolved class names, not raw `PartitionConfig` or driver-specific opaque parameters by default.

The API can be implemented in either of two ways:

- a lightweight handler backed by a DeviceClass informer/cache; or
- a separate catalog service that watches coordinator-managed DeviceClasses.

The existing health and metrics endpoint is unauthenticated, so a rich catalog API should not be added there without an authentication and authorization design. `visibility: internal` is currently metadata, not an authorization boundary.

### Option C: CRD-backed catalog

Introduce a namespaced `HardwareOffering` resource as the administrator-facing source of truth:

```yaml
apiVersion: nodepartition.dra.k8s.io/v1alpha1
kind: HardwareOffering
metadata:
  name: gpu-inference
spec:
  displayName: GPU inference
  intentLevel: workload
  categories:
  - ai
  - inference
  implementation:
    alignment: pcieRoot
    devices:
    - class: gpu.nvidia.com
      count: 1
    - class: rdma.mellanox.com
      count: 1
status:
  generatedDeviceClasses:
  - gpu-inference
  conditions:
  - type: Available
    status: "True"
```

The Kubernetes API would then provide discovery, watch, RBAC, and status for both a CLI and an external UI.

Existing ConfigMaps should remain supported as a compatibility source. Both ConfigMaps and `HardwareOffering` objects should normalize into one internal offering model before topology resolution.

## Recommended progression

1. Make aggregate offering names stable, collision-checked, and explicitly labeled.
2. Add a catalog CLI using the labeled aggregate DeviceClasses.
3. Add offering-reference resolution to native `ResourceClaim` and `ResourceClaimTemplate` admission, if names that differ from class names are needed.
4. Add a read-only authenticated catalog API only when an external portal needs it.
5. Define a normalized internal offering model shared by ConfigMaps and `HardwareOffering`, then introduce `HardwareOffering` and `IntentClaim` CRDs if typed workload APIs, status, or multi-tenant ownership justify their cost.

The CLI comes after labeling because it selects on the offering label. The normalized model is deferred because `DeviceGrouping` and `IntentMetadata` already serve the ConfigMap source and the admission resolver reads DeviceClass labels the webhook already fetches. A second source does not exist until the CRD step.

This sequence keeps the existing `PartitionConfig` and webhook expansion path intact while progressively removing DeviceClass and topology knowledge from workload authors.

## Acceptance criteria for implementation

- Existing ConfigMap offerings continue to produce the same aggregate and topology-specific DeviceClasses.
- Native claims without offering references pass through unchanged.
- Unknown or ambiguous offering references fail closed with an actionable error.
- Offering resolution targets only aggregate, coordinator-managed DeviceClasses that carry the explicit offering label.
- The catalog lists fixed and auto-detected managed classes with an explicit `kind`, and never presents them as stable offerings.
- Repeated admission of the same object is idempotent.
- An offering resolves identically at template, Pod, and claim admission.
- Catalog responses expose user-facing metadata without leaking opaque driver configuration by default.
- Offering names and resolved DeviceClass names are deterministic across reconciliation and restart.
- Namespace ownership and RBAC are explicit; `visibility` alone is never treated as authorization.
- Direct hardware-specific requests and explicit passthrough offerings remain available.

## Open decisions

- Whether the first workload API should use stable DeviceClass names or native-DRA annotations.
- Whether offering references are namespace-scoped or cluster-scoped.
- Whether an HTTP catalog belongs in this controller or in a separate service.
- Whether recommendation logic is part of the catalog or a separate workload-intelligence component.
- Whether annotation-based resolution is needed at all once aggregate class names are stable.
- How offerings are renamed or versioned without breaking existing claims.
- Whether to mutate templates at admission or teach the Pod and VMI handlers the resolver.
- Whether `firstAvailable` subrequests are supported.
- Whether `grouping` and `partition` entries should be hidden from the default catalog view or shown alongside offerings.
