# Webhook Feature Freeze

**Status:** Proposed

**Scope:** Stop adding features to the mutating webhook, publish a deprecation path, and wire up usage metrics. Later decisions about removing the webhook and about controller stamping depend on those metrics.

**Related specs:**

- [002: Intent request and catalog options](002-intent-request-and-catalog-options.md): this work extends Option A (stable aggregate DeviceClasses).
- [004: `dratopoctl bundles`](004-dratopoctl-bundles.md): the replacement delivery path.
- [005: Controller bundle stamping](005-controller-bundle-stamping.md): optional in-cluster delivery.
- [006: KEP draft, extended-resource device bundles](006-kep-extended-resource-device-bundles.md): the long-term upstream path.

## Context

Today the coordinator delivers curated bundles in two parts:

- The controller publishes managed DeviceClasses that carry an opaque `PartitionConfig`.
- A mutating webhook expands any `ResourceClaim` naming such a class into per-driver requests, `matchAttribute` constraints and opaque config. It also rewrites Pod and KubeVirt VMI request names to match.

Analysis of the code and of upstream Kubernetes found three things:

- **Expansion does not depend on the claim's workload.** It depends only on the request and the class's `PartitionConfig` (`expandRequest`, `internal/webhook/webhook.go:389`). It can therefore run when the bundle is generated rather than when a claim is admitted. Spec 004 does this.
- **A one-line request needs an upstream change.** DRA extended resources (KEP-5004) allocate inside the scheduler before any webhook sees the claim, and the claim the scheduler synthesizes carries no constraints or config. Spec 006 drafts that change.
- **Admission is the riskiest place for this logic.** Every claim, Pod and VMI create depends on the webhook being available and on it seeing the cluster the same way the controller does. Webhook outages and failure-policy settings turn into scheduling failures.

So the webhook stops growing. It stays supported for existing users, and the generation-time path in spec 004 becomes the recommended way to consume bundles.

## Decisions

- **Freeze, don't remove.** No new webhook features. Bug and security fixes continue. Behavior does not change.
- **Measure before removing.** Removal needs evidence of how the webhook is actually used, so this spec wires up the existing metrics. Removal is a later decision, made against the criteria below.
- **Hand-written claims stay valid.** A `ResourceClaim` naming a managed class is still expanded for as long as the webhook exists.

## Current code (verified at `ba64fff`)

- **Routing.** `handleAdmission` (`internal/webhook/webhook.go:143`) dispatches to:
  - the claim path, for CREATE and UPDATE (`:165`)
  - `handlePodAdmission` (`:836`)
  - `handleVMIAdmission` (`:976`)
- **Exit points that return a patch:**
  - claim: `:190`, after the empty check at `:186`
  - Pod: `:946`, after `:936`
  - VMI: `:1252`, after `:1245`
- **`patchResponse` fallback.** If marshalling fails, `patchResponse` returns an unchanged allow (`:1384-1389`).
- **What each path patches:**
  - **Pod:** only when a container claim reference has a non-empty `request` that matches the mapping (`:905-909`).
  - **VMI:** two different features. It generates `hostDevices` (`:1115`, `:1122`) and rewrites `requestName` on `gpus` and `hostDevices` (`:1205`, `:1241`).
  - **Denials** happen at `:181`, `:875`, `:882`, `:1009`, `:1013` and `:1041`.
- **Existing metrics.**
  - `internal/metrics/metrics.go:48-53` already defines `WebhookExpansions` as `nodepartition_webhook_expansions_total`, with no labels, registered at `:70`. **Nothing increments it.**
  - `WebhookErrors` (`:55`) is likewise unused.
- **HA.** Every replica starts the webhook (`cmd/nodepartition-controller/main.go:159`, before leader election at `:185`), and every replica serves `/metrics` (`:139`).
- **Chart.** Metrics can be disabled with `controller.metrics.enabled=false`. The chart ships no ServiceMonitor. `deploy/helm/nodepartition/templates/NOTES.txt` already mentions the webhook (`:9`, `:29`).

## Implementation steps

### Step 1: Wire up the metrics

Convert the existing `WebhookExpansions` counter to a `CounterVec` with one `kind` label. The metric name stays the same, and no code calls `.Inc()` on the old var today.

| `kind` | Incremented when |
|---|---|
| `claim` | the claim path returns a non-empty patch (a managed-class expansion) |
| `pod` | the Pod path returns a non-empty patch (a container request-name rewrite) |
| `vmi_generate` | the VMI patch includes an `add` under `/hostDevices` (generated host devices) |
| `vmi_rewrite` | the VMI patch includes a `replace` of a `requestName` |

- **Where to increment.** At the three call sites listed above, **after** `patchResponse` returns, and only if the response carries a patch. Use a small helper so the rule lives in one place:

  ```go
  func countedPatch(kind string, patches []jsonPatch) admissionv1.AdmissionResponse {
      resp := patchResponse(patches)
      if resp.PatchType != nil {
          metrics.WebhookExpansions.WithLabelValues(kind).Inc()
      }
      return resp
  }
  ```

  The marshalling fallback in `patchResponse` returns an allow with no patch, so it is never counted.
- **VMI kinds are counted independently.** One VMI patch can hold both kinds. For example, a VMI with two claims where only one already has `hostDevices` (the `hasDevices` check at `:1028` is per claim) gets generation for one claim and rewrites for the other. Increment `vmi_generate` if any op is an `add` under `/hostDevices`, and `vmi_rewrite` if any op is a `replace` of `requestName`. A mixed VMI increments both. The VMI path therefore uses its own variant of the helper that inspects the ops.
- **Two VMI kinds.** They let the open KubeVirt question (whether `hostDevices` generation is still used) be answered from data.
- **Denials.** Convert `WebhookErrors` to a `CounterVec` with the same `kind` label, and increment it at each deny site:

  | Deny site | `kind` | `reason` |
  |---|---|---|
  | `:181` | `claim` | `expansion` |
  | `:875` | `pod` | `lookup` |
  | `:882` | `pod` | `expansion` |
  | `:1009` | `vmi` | `lookup` |
  | `:1013`, `:1041` | `vmi` | `expansion` |

  `lookup` denials fire on any template Get failure, including a missing template or a transient API error with no partition class involved. `expansion` denials come from `managedExpansionError` and do involve a managed class.
- **Fail-open errors are not counted.** A non-managed claim expansion error returns an allow at `:183`. It stays invisible to this metric. That is intended: it is not usage of the webhook.
- **Zero values.** In `internal/metrics/metrics.go`'s `init()`, right after `MustRegister`, pre-initialize every label combination with `WithLabelValues(...)`, so a zero is exported. Without that, "stays at zero" looks the same as "series missing".
- **Logging.** No per-request logging.

### Step 2: Deprecation document

Add `docs/webhook-deprecation.md` covering:

- **Freeze.** What is frozen (features) and what continues (bug and security fixes).
- **Migration.** Admins keep defining offerings, render them with `dratopoctl bundles render`, and commit the ResourceClaimTemplates. Pods reference them through `resourceClaimTemplateName`.
- **What changes for users after migrating:**
  - Containers reference the claim without `request:`, or use the expanded names listed in the template's `bundle-requests` annotation (spec 004).
  - VMIs declare their own `hostDevices`.
- **Removal criteria.** All four must hold:
  1. `sum(increase(nodepartition_webhook_expansions_total[30d]))` across **all** webhook pods is zero for every `kind` on the clusters we know about. Use the sum, not a single replica's raw value, because counters are per replica and reset on restart.
  2. The same holds for `nodepartition_webhook_errors_total{reason="expansion"}`. A non-zero value means someone is still trying to use a managed class. Non-zero `reason="lookup"` values need investigation but are not proof of use: they also fire for missing templates and transient API errors.
  3. Spec 004 is released.
  4. The KubeVirt path is decided (see Open decisions).

  A cluster running with `controller.metrics.enabled=false` provides no evidence. It must not be read as zero usage.

### Step 3: Link the notice

- `README.md`: one paragraph in the webhook section linking to the deprecation document.
- `NOTES.txt`: one line near the existing webhook text.

## Tests

Name the tests with `Metric` so `-run Metric` selects them.

**How tests read counters.** The repo vendors dependencies, and `prometheus/testutil` is not vendored (`vendor/modules.txt:85-92`). Read a counter with `c.Write(&dto.Metric{})` from the vendored `github.com/prometheus/client_model/go`. Counters are global, so each test snapshots the values before the call and asserts the delta. These tests must not use `t.Parallel()`.

- **One test per kind:**
  - an expansion increments its `kind` by exactly 1;
  - a pass-through leaves all seven expansion and error series unchanged;
  - a denial increments `webhook_errors_total` for that kind and reason.
- **VMI split:** a VMI that only needs generation increments only `vmi_generate`. A VMI that only needs a rewrite increments only `vmi_rewrite`. A mixed VMI (two claims, one with existing `hostDevices`) increments both.
- **Claim UPDATE:** an UPDATE of an already-expanded claim passes through and is not counted.
- **Marshal failure:** call the counting helper directly with a `jsonPatch` whose `Value` is a `chan int`. Nothing is counted. This cannot be driven through `handleAdmission`, because real patches are always JSON-safe.
- **Zero series:** after init, `prometheus.DefaultGatherer.Gather()` returns every label combination with value 0.
- **Existing webhook tests** pass unchanged.

## Verification

```
make test lint
go test ./internal/webhook/ -run Metric -v
grep -n webhook-deprecation deploy/helm/nodepartition/templates/NOTES.txt README.md   # helm template does not render NOTES.txt
```

On a live cluster:

1. Apply one partition claim.
2. Scrape `/metrics` on **every** coordinator pod and sum `nodepartition_webhook_expansions_total{kind="claim"}`. Expect 1.
3. Every other `kind` is exported as 0.

## Risks and open decisions

- **Users may read the freeze as removal.** The document must say that behavior is unchanged and that removal is gated by the published criteria.
- **Pod counts show only rewrites.** A Pod that references a partition claim without `request:` is never patched, so it isn't counted. Claim-level usage comes from `kind="claim"`.
- **Open: the KubeVirt path.** VMIs depend on the webhook to generate `hostDevices`. Removal waits until `vmi_generate` is zero and a VMI-side answer exists: either rendered templates with `hostDevices` written by the user, or a KubeVirt-native managed-claim mechanism.
