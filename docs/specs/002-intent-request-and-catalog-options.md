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
- add an explicit offering label to distinguish curated offerings from fixed and auto-detected classes;
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

This requires webhook coverage for both `resourceclaims` and `resourceclaimtemplates`. The Pod and VMI handlers should continue to consume the resolved native DRA objects rather than understand offering metadata themselves.

Advantages:

- users request an intent while retaining native DRA objects;
- direct claims and template-backed claims can share the same resolver; and
- the existing expansion code remains the final resolution step.

Limitations:

- annotations are less strongly typed than a CRD;
- multiple offering references require a small annotation convention; and
- the webhook must fail closed if the resolver cannot establish the offering.

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

Use aggregate managed DeviceClasses as the catalog source and add a read-only CLI:

```sh
nodepartitionctl offerings list
nodepartitionctl offerings get gpu-inference
```

The output should include:

- offering name and resolved DeviceClass name;
- display name and description;
- intent level, categories, and workload types;
- visibility and consumer mode;
- alignment and available topology-specific variants; and
- readiness or availability information.

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

1. Define a normalized internal offering model shared by ConfigMaps and future APIs.
2. Add a catalog CLI using aggregate DeviceClasses.
3. Make aggregate offering names stable and explicitly labeled.
4. Add offering-reference resolution to native `ResourceClaim` and `ResourceClaimTemplate` admission.
5. Add a read-only authenticated catalog API only when an external portal needs it.
6. Introduce `HardwareOffering` and `IntentClaim` CRDs if typed workload APIs, status, or multi-tenant ownership justify their cost.

This sequence keeps the existing `PartitionConfig` and webhook expansion path intact while progressively removing DeviceClass and topology knowledge from workload authors.

## Acceptance criteria for implementation

- Existing ConfigMap offerings continue to produce the same aggregate and topology-specific DeviceClasses.
- Native claims without offering references pass through unchanged.
- Unknown or ambiguous offering references fail closed with an actionable error.
- Offering resolution targets only aggregate, coordinator-managed DeviceClasses.
- Repeated admission of the same object is idempotent.
- Catalog responses expose user-facing metadata without leaking opaque driver configuration by default.
- Offering names and resolved DeviceClass names are deterministic across reconciliation and restart.
- Namespace ownership and RBAC are explicit; `visibility` alone is never treated as authorization.
- Direct hardware-specific requests and explicit passthrough offerings remain available.

## Open decisions

- Whether the first workload API should use stable DeviceClass names or native-DRA annotations.
- Whether offering references are namespace-scoped or cluster-scoped.
- Whether an HTTP catalog belongs in this controller or in a separate service.
- Whether recommendation logic is part of the catalog or a separate workload-intelligence component.
