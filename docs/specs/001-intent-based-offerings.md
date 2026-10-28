# PR2 Implementation Plan: Intent-Based Hardware Offerings (metadata + discovery + docs)

**Status:** Implemented — see PR #13
**Handoff target:** codex
**Repo:** `rh-ecosystem-edge/k8s-dra-topology-coordinator` (Go, Kubernetes DRA)
**Base branch:** `main` — PR1 (https://github.com/rh-ecosystem-edge/k8s-dra-topology-coordinator/pull/12) has merged. `main` at commit `29591d4` already contains PR1's fixes: `publishDeviceClass` is merge-preserving and idempotent, and opaque-config marshalling is deterministic. PR2's aggregate metadata and no-op tests depend on both, and both are now present on `main`. Branch from `main`.

## Context

This is PR2 of a two-PR sequence implementing intent-based hardware offerings: administrators attach optional human-readable metadata (display name, description, intent level, categories, workload types, visibility) to the existing device-grouping ConfigMap mechanism, and the Topology Coordinator propagates that metadata onto the DeviceClasses it already generates, as labels (short/filterable) and annotations (prose). Users then discover offerings with `kubectl get deviceclasses -l ...`. **No new CRD, no new controller, no change to DRA allocation or webhook claim expansion.**

This plan is the result of a spec review (by me) and an architect review (feature-dev:code-architect) against the actual codebase. Both rounds of findings are folded in below — do not re-derive them, the line numbers and defects cited here were verified by reading the code, not assumed from the spec document.

PR1 (merged, see base branch note above) fixed three bugs in the shared DeviceClass publish path that this feature's correctness requirements depend on:
- `publishDeviceClass` is now merge-preserving (foreign labels/annotations survive) and skips the API call when nothing TC-owned changed.
- `PartitionConfig` marshalling is now deterministic (sorted `SubResources` and `Alignments`).
- Deleting the last grouping ConfigMap now runs cleanup instead of orphaning DeviceClasses.

PR2 adds the metadata layer on top of that fixed foundation.

## Decisions already made — do not relitigate

1. **Metadata placement:** intent labels/annotations go on the aggregate DeviceClass only (the one named after the grouping, e.g. `gpu-inference`), never on the per-rail `-rail<N>` variants. Discovery then returns one row per offering.
2. **Offering names (example manifests):** `gpu-inference` (workload), `low-latency-network` (capability), `gpu-nic-aligned` (hardware-specific). These match the project's planning deck; use them consistently in example filenames, README, and DeviceClass names.
3. **`visibility` has no implicit default.** Emit the label only when the ConfigMap sets it explicitly — auto-detected PCIe-root pairings (`DetectPCIeRootPairings`) must never be mistaken for administrator-curated offerings.
4. **Request-level policy/rejection is explicitly out of scope** for this PR. The webhook continues to pass through claims it has no config for (`internal/webhook/webhook.go:181-191`, unchanged). Document this as a known limitation, not a silent gap.
5. **Parse-time validation, not naming-function changes.** Validate that `sanitizeForName(grouping.Name)` yields a valid DNS-1123 label at ConfigMap-parse time; do not touch `sanitizeForName` or `sanitizeDNSLabel` themselves — that would rename existing DeviceClasses and break backward compatibility.
6. **Name collisions:** flag **user-defined-vs-user-defined** collisions only (two ConfigMaps sharing a `data.name`). A user-defined grouping overriding an auto-detected one of the same name is documented, intended behavior (`internal/controller/controller.go:439-441`) and must not be flagged.
7. **"Actionable error" means a structured log line** naming the source ConfigMap's namespace/name and the offending field — no new Events, no RBAC changes. The project has no Events/status mechanism anywhere today, and adding one is out of scope.
8. **On a ConfigMap update that fails to parse,** keep the previously cached grouping live (existing behavior, `internal/controller/controller.go` update handler) rather than evicting it. Log at error level that the metadata is now stale. Document as a known limitation.

## Current code (verified against `main` at `29591d4`, which includes PR1 unchanged — diffed to confirm)

- **`internal/controller/grouping.go`** — `DeviceGrouping` struct (`:24-35`, no struct tags, fields read by hand from `cm.Data`), `parseDeviceGrouping` (`:112-167`, flat key reads + `switch` validation for `alignment`/`fallback`), `GroupingStore.GetGroupings()` (`:86-110`, hand-rolled deep copy that reconstructs the struct field-by-field — **this will silently drop any new field you add unless you also update this copy**).
- **`internal/controller/grouping_builder.go`** — `GroupingInstance` struct (`:14-46`, one value per topology instance; already duplicates per-grouping fields like `GroupingName` and `Alignment` per instance, so adding another per-grouping field here follows existing convention), `BuildGroupings` (`:70-81`).
- **`internal/controller/deviceclass_manager.go`** — `SyncGroupingDeviceClasses` (`:1018-1116`), aggregate emission loop at `:1066-1096` (representative picked deterministically by lowest `RailIndex` as of PR1, `:1080-1086`), `buildGroupingDeviceClass` (`:1211-1268`, `ObjectMeta` currently sets only `Name` and `Labels` — no `Annotations` field at all), `publishDeviceClass` (`:1437-...`, merge-preserving as of PR1 — labels/annotations your code sets will be merged in using the `nodepartition.dra.k8s.io/` prefix as the ownership boundary).
- **`internal/controller/controller.go`** — `reconcile` (`groupings := c.groupingStore.GetGroupings()` at `:228`, `mergedGroupings := mergeGroupings(...)` at `:255`), `mergeGroupings` (`:434-447`, keys by `DeviceGrouping.Name`, `userDefined` wins).
- **`internal/webhook/webhook.go`** — `getPartitionConfig` (`:519-547` approx, reads only `dc.Spec.Config`) — **not touched by this PR**; verify this remains true after your changes.
- **No `examples/` directory exists.** No grouping ConfigMap documentation exists in `README.md` at all — the `### Example: NVIDIA NVLink Domain Grouping` section documents a different mechanism (topology *rules*, not device groupings).
- **The mock device driver and `test/e2e/run-e2e.sh` require external infrastructure** (a Vagrant cluster, `github.com/fabiendupont/mock-device`) not present in this environment. E2E cannot be run here; report as not executed.
- Tests use plain `testing` + `testify`, no Ginkgo. `make test` runs unit tests and an envtest/integration suite together (needs `setup-envtest` k8s binaries, fetched automatically). `golangci-lint` was not installed in the environment this plan was written in — run `make lint` if available in yours, and report if it isn't.

## Implementation steps

### Step 1 — Extend the grouping model and validation

**File: `internal/controller/grouping.go`**

Add:

```go
type IntentMetadata struct {
    DisplayName   string
    Description   string
    IntentLevel   string   // "workload" | "capability" | "hardware-specific"
    Categories    []string
    WorkloadTypes []string
    Visibility    string   // "public" | "internal" — no default; empty means unset
}

func (m IntentMetadata) IsOffering() bool {
    return m.DisplayName != "" || m.Description != "" || m.IntentLevel != "" ||
        len(m.Categories) > 0 || len(m.WorkloadTypes) > 0 || m.Visibility != ""
}
```

Embed `Intent IntentMetadata` into `DeviceGrouping` (`:24-35`).

Extend `parseDeviceGrouping` (`:112-167`) following its existing style — flat `cm.Data` key reads, `switch` for enums (matching the existing `alignment` validation at `:125-130`):

- `displayName` — from `data["displayName"]`, optional, max **63 chars** (keeps it eligible to double as a label value later; this is a project policy choice, not a Kubernetes API limit — say so in the README, don't cite it as a platform constraint)
- `description` — from `data["description"]`, optional, max **1024 chars** (project policy; annotation *values* have no per-key API limit, only a 256 KiB total-annotations-size cap across the whole object, so this bound exists purely to keep output readable)
- `intentLevel` — from `data["intentLevel"]`, optional, must be one of `workload`, `capability`, `hardware-specific` if set; reject with a message identifying the ConfigMap and the invalid value, consistent with the existing `alignment` error style at `:129`
- `categories` — from `data["categories"]`, comma-split, trim each entry, drop empty entries, deduplicate, preserve first-seen order, max **16 entries**, each max **63 chars** (without a bound, a multi-hundred-KB value parses and gets emitted as-is today — there is nothing else stopping it)
- `workloadTypes` — same handling and same bounds as `categories`, from `data["workloadTypes"]`
- `visibility` — from `data["visibility"]`, optional, must be one of `public`, `internal` if set — **no default value**

All fields optional; absent means zero value. Keep error messages leaf-level (no ConfigMap name) since `LoadFromConfigMap` already wraps with namespace/name at `:62`.

**Name validation (resolves the "invalid name reaches the API server" defect):** add a check that `sanitizeForName(grouping.Name)` (`grouping_builder.go:370-376`) produces a valid Kubernetes DNS-1123 label, matching `^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`, **and is ≤56 characters, not 63** — the per-rail aggregate appends `-rail<N>` (`deviceclass_manager.go:1057`: `fmt.Sprintf("%s-rail%d", sanitizeForName(name), RailIndex)`), so a 63-char sanitized name produces a 69-char object name and the Create is rejected anyway. Reject at parse time with a message naming the ConfigMap and the field.

Note for whoever writes the test cases: `sanitizeForName` only lowercases and replaces `/`, `.`, `:` (`grouping_builder.go:370-376`) — it does **not** replace underscores, and does not trim leading/trailing dashes or collapse `--`. So `gpu_inference`, `-gpu`, and `gpu--nic` are all concrete rejection cases your tests should cover, alongside the length case.

This is strictly better than leaving it to fail at the API server, even though an API-level failure is now isolated (verified against the current code, post-PR1): `SyncGroupingDeviceClasses` accumulates into `publishErrs` and returns `stdliberrors.Join(publishErrs...)` at the end (`deviceclass_manager.go:1115`), so one bad name no longer blocks other groupings' DeviceClasses from publishing, and `activeNames` is built from every entry regardless of publish outcome (`:1101-1108`) so a failed publish doesn't cause cleanup to wrongly delete something else. The actual cost of leaving it to the API server: `reconcile` still returns the joined error (`controller.go:262-265`), which increments `metrics.ReconciliationErrors` and triggers a rate-limited requeue (`controller.go:208-211`) that never converges — every reconcile burns a wasted Get+Create on a name the API will never accept, surfacing only as `failed to publish DeviceClass <mangled-name>` with no pointer back to the source ConfigMap. Parse-time rejection names the ConfigMap via the existing `LoadFromConfigMap` wrapper (`grouping.go:62`).

Validate the *sanitized* result, not the raw `data.name` — names like `gpu.nvidia.com` are legitimate today and sanitize fine.

**Sanitize-collision hazard (separate from the above, and more serious — this is the gap my spec review and first-pass plan both missed):** `SyncGroupingDeviceClasses`'s aggregate map is keyed by the **raw** `GroupingName` (`deviceclass_manager.go:1068`), but the published object name is `sanitizeForName(GroupingName)` (`:1091`). Two *distinct* raw names that sanitize to the same string — e.g. `gpu.inference` and `gpu-inference`, or `GPU-inference` and `gpu-inference` — collapse into two map entries racing to publish **the same DeviceClass name**, in random map-iteration order. Labels, annotations, and `Spec.Config` would flap between whichever grouping's map entry happened to be processed last, every reconcile. The same hazard applies to the per-rail name at `:1057`. This is not caught by exact `data.name` equality (the two names are different strings) and is not something PR1's determinism work touches.

Add a second check, on the result of `mergeGroupings` rather than on raw ConfigMap names: group the merged groupings by `sanitizeForName(g.Name)` and flag any sanitized key that maps to more than one distinct raw `Name`. This must run on the **merged** list (after `controller.go:255`), not just the user-defined list, because a user-defined name can also collide with an auto-detected one's sanitized form — auto-detected names already go through `sanitizeDNSLabel` at `partition_builder.go:150-151`, so the clash is always introduced from the user-ConfigMap side, but it has to be checked against the full merged set to catch it. This is a *different* collision from decision #6 below (which is about exact raw-name duplicates and is an intentional, documented override) — do not conflate the two checks or try to satisfy both with one function.

**Fix `GetGroupings()` (`:86-110`):** it reconstructs `DeviceGrouping` field-by-field in the literal at `:102-107`. Do **not** just add `Intent: g.Intent` to that literal — `IntentMetadata` holds two slices (`Categories`, `WorkloadTypes`), and `GetGroupings` exists specifically to deep-copy so callers don't share backing arrays with the store's cached value under only an `RLock` (it already hand-copies `Devices` and each `Capacity` map for exactly this reason, `:91-101`). A shallow `Intent: g.Intent` assignment would alias those two slices. Deep-copy them in the same literal, following the existing `Capacity`-copying pattern immediately above it. **This whole thing is easy to miss**: a shallow copy compiles clean, passes a naive round-trip test that only checks scalar fields, and still has `IsOffering()` behave correctly until something mutates a shared slice concurrently. Write the round-trip test (Step 6) to mutate the returned slice and assert the store's internal value is unaffected.

**Collision detection — two checks, not one, at two different points:**

1. **Raw-name duplicates among user-defined groupings.** Add a function, e.g. `detectDuplicateGroupingNames(userDefined []DeviceGrouping) []string`, returning names appearing more than once in the **user-defined** list only. Call it in `controller.go`'s `reconcile`, **inside the `if !skipGroupings` block** (the block starting at `:250`, after the `skipGroupings` check at `:231`) — not unconditionally at `:228`, since `groupings` is irrelevant when `partitionMode == partitions` and skipped. Call it right after `groupings := c.groupingStore.GetGroupings()` (currently `:228`; move the call, not necessarily the line, into the guarded block), before the merge at `:255`. Log an error identifying the duplicated name — don't return/abort reconciliation. `mergeGroupings` will still pick one arbitrarily via its map, so the log is what makes the ambiguity visible to an operator. Do **not** flag the case where a user-defined grouping shares a raw name with an auto-detected one — that's `mergeGroupings`' documented override behavior (`controller.go:432-433,439-441`).
   - Note: reconcile fires on every debounced ResourceSlice event, so this log line repeats on every reconcile while the conflict exists. That's accepted noise for a misconfiguration that should be fixed, not a signal to build a dedup/rate-limit cache — don't add one.

2. **Sanitized-name collisions across the merged list** — see the dedicated "Sanitize-collision hazard" paragraph above. This is a different bug class (distinct raw names, same object name) and needs its own check on `mergedGroupings`, after `controller.go:255`.

### Step 2 — Carry metadata through the builder

**File: `internal/controller/grouping_builder.go`**

Add `Intent IntentMetadata` to `GroupingInstance` (`:14-46`), and populate it from the source `DeviceGrouping` in `buildInstance` (`grouping_builder.go:273-340`) — this is the **only** production code path that constructs a `GroupingInstance`, called from a single call site at `:235`. (The test helper `multiDeviceGroupingInstance` in `deviceclass_manager_test.go` also builds one directly, as a struct literal in test code — not relevant to this step, don't go hunting for other production call sites, there aren't any.) This is the minimum plumbing — `buildGroupingDeviceClass` already receives a `representative GroupingInstance`, so no signature changes are needed downstream of this.

**Dependency to document in a code comment:** the aggregate DeviceClass (Step 3) reads `Intent` from one arbitrary representative instance among all instances sharing a grouping name. This only produces a consistent result because `mergeGroupings` keys by name with user-defined winning outright (`controller.go:434-447`) — every instance of a given grouping name traces back to the *same* `DeviceGrouping`, so `Intent` is identical across them. If a future change allows a grouping name to be assembled from multiple `DeviceGrouping` sources, this assumption breaks silently. Say so in the comment.

### Step 3 — Emit metadata on the aggregate DeviceClass only

**File: `internal/controller/deviceclass_manager.go`**

Add a helper:

```go
func applyIntentMetadata(dc *resourcev1.DeviceClass, intent IntentMetadata) {
    if !intent.IsOffering() {
        return
    }
    if dc.Labels == nil {
        dc.Labels = map[string]string{}
    }
    if dc.Annotations == nil {
        dc.Annotations = map[string]string{}
    }
    if intent.IntentLevel != "" {
        dc.Labels[CoordinatorDriverName+"/intent-level"] = intent.IntentLevel
    }
    if intent.Visibility != "" {
        dc.Labels[CoordinatorDriverName+"/visibility"] = intent.Visibility
    }
    if intent.DisplayName != "" {
        dc.Annotations[CoordinatorDriverName+"/display-name"] = intent.DisplayName
    }
    if intent.Description != "" {
        dc.Annotations[CoordinatorDriverName+"/description"] = intent.Description
    }
    if len(intent.Categories) > 0 {
        dc.Annotations[CoordinatorDriverName+"/categories"] = strings.Join(intent.Categories, ",")
    }
    if len(intent.WorkloadTypes) > 0 {
        dc.Annotations[CoordinatorDriverName+"/workload-types"] = strings.Join(intent.WorkloadTypes, ",")
    }
}
```

Lead with the `IsOffering()` guard, not a per-field check — this makes decision #3 ("no implicit visibility default", and more generally "a grouping with no intent metadata gets no intent metadata") enforced in exactly one place instead of being an emergent property of four independent `!= ""` checks scattered through the function. It also means this test writes itself: auto-detected groupings from `DetectPCIeRootPairings` build `DeviceGrouping` literals with no `Intent` field set at all (`partition_builder.go:152-156`), so their zero-value `Intent.IsOffering()` is `false` and `applyIntentMetadata` returns immediately — no separate code path needed to keep them metadata-free.

Allocate `dc.Labels`/`dc.Annotations` unconditionally once `IsOffering()` is true, rather than gating each allocation on which specific fields are set — the per-field-conditional-allocation version is a maintenance trap (add a fifth annotation later, forget to extend the guard, nil-map panic). The one-time allocation is safe even though `dc.Labels` is already guaranteed non-nil at the only call site (`buildGroupingDeviceClass` always builds a non-nil map, `:1230-1235`) — guard it anyway so the helper isn't a loaded gun if it's ever called from elsewhere. Leaving an empty `dc.Annotations` map on a grouping with partial metadata is harmless: `mergeTCOwnedMetadata` returns `nil` when the merged result is empty (`:1427-1430`), and `stringMapEqual` treats `nil` and `{}` as equal (`:1399-1409`), so PR1's no-op short-circuit in `publishDeviceClass` (`:1456-1460`) still fires correctly.

Use `CoordinatorDriverName` (the const, not `m.driverName`) for key construction, matching every other DeviceClass metadata key in this file (`:1230-1234` is the pattern to follow) — this is also what makes PR1's prefix-based ownership pruning in `publishDeviceClass` correct for these new keys. `buildGroupingDeviceClass`'s `ObjectMeta` currently has no `Annotations` field at all (`:1243-1247`, confirmed), so this is new, not a modification of existing annotation logic.

Call `applyIntentMetadata` **only** in the aggregate emission loop (`:1088-1096`), passing `entry.representative.Intent`. It can run either before or after `dc.Name` is assigned at `:1091` and either before or after the existing `delete(dc.Labels, CoordinatorDriverName+"/numa")` at `:1092` — the key sets are disjoint, so ordering doesn't matter here. Do **not** call it in the per-instance (`-rail<N>`) loop (`:1030-1061`, above the aggregate loop).

**While you're in the aggregate loop:** confirmed against the current code that PR1 did **not** delete `.../railIndex` from the aggregate — the only delete there is `/numa` at `:1092`; `railIndex` is set unconditionally in `buildGroupingDeviceClass`'s base label map (`:1234`) and nothing removes it for aggregates. Add `delete(dc.Labels, CoordinatorDriverName+"/railIndex")` next to the existing `numa` delete. The reason isn't reconcile-to-reconcile flapping — PR1's deterministic lowest-`RailIndex` representative selection (`:1080-1086`) already makes the value stable at `"0"` in steady state — the reason is that a rail index is semantically meaningless on a grouping-wide aggregate (it's not tied to any one rail), and the value *does* shift if the lowest-rail instance ever disappears from topology (e.g. that GPU goes offline), which is a different reconcile artifact than the metadata changing. Removing it is safe on upgrade for existing clusters too: `mergeTCOwnedMetadata` deletes TC-owned keys present on the live object but absent from the newly-built one (`:1422-1423`), so existing aggregates lose the stale label on the next reconcile, in one Update. Do **not** also delete `/alignment` — it's part of the aggregate's dedup key at `:1089` and belongs there.

### Step 4 — Confirm the webhook is untouched

No changes to `internal/webhook/`. Verify `getPartitionConfig` still reads only `dc.Spec.Config` and nothing from `dc.Labels`/`dc.Annotations` (re-grep the current file; line numbers may have shifted since this plan was written). Add one test asserting that an intent-annotated DeviceClass expands a claim identically to a bare one — same `SubResourceConfig`/`AlignmentConfig` output, metadata is pure decoration.

### Step 5 — Tests

Follow existing conventions: plain `testing` + `testify`, table-driven where the existing file does (see `grouping_test.go` and `deviceclass_manager_test.go` for the house style). **Check `deviceclass_manager_test.go` for test names before adding new ones** — PR1 already added `TestDeviceClassManager_PublishDeviceClass_PreservesForeignMetadata`, `TestDeviceClassManager_PublishDeviceClass_NoOpWhenUnchanged`, `TestDeviceClassManager_SyncGroupingDeviceClasses_DeletionCleansUpOrphans`, `TestDeviceClassManager_SyncGroupingDeviceClasses_AggregateIsDeterministic`, `TestDeviceClassManager_GroupingConfigIsDeterministic`, and a `multiDeviceGroupingInstance` test helper — reuse that helper rather than duplicating it.

- **`internal/controller/grouping_test.go`:**
  - Legacy ConfigMap with none of the new fields still parses and `IsOffering()` is false.
  - Valid offering at each of the three intent levels.
  - Invalid `intentLevel` and invalid `visibility` are rejected with an error naming the field.
  - Empty and malformed `categories`/`workloadTypes` (e.g. `"a,,b, ,c"`) parse to `["a","b","c"]`.
  - Oversized `displayName` and `description` are rejected.
  - A `data.name` that sanitizes to an invalid DNS-1123 label is rejected at parse time: cover a leading digit, an underscore (`gpu_inference` — `sanitizeForName` does not replace `_`), a leading dash (`-gpu`), a double dash (`gpu--nic`), and a name whose sanitized form exceeds 56 chars.
  - **Round-trip through `GetGroupings()` preserves `Intent` without aliasing.** Load a ConfigMap with `categories`/`workloadTypes` set into a `GroupingStore`, call `GetGroupings()` once, mutate an element of the returned `Intent.Categories` slice, call `GetGroupings()` again, and assert the second call's `Categories` is unaffected — this is what actually catches a shallow-copy aliasing bug; a test that only compares values after one call would pass even with `Intent: g.Intent` done wrong.
  - `detectDuplicateGroupingNames` (or whatever you name it) flags two user-defined entries sharing a *raw* name, and does not flag a user-defined entry sharing a raw name with an auto-detected one.
  - A separate test for the sanitized-name collision check: two user-defined groupings named e.g. `gpu.inference` and `gpu-inference` (distinct raw names, same `sanitizeForName` output) are flagged, run against the *merged* list.

- **`internal/controller/deviceclass_manager_test.go`:**
  - The aggregate DeviceClass carries the expected intent labels and annotations; the `-rail<N>` variant does not.
  - A grouping ConfigMap with no intent fields produces an aggregate with no intent labels/annotations (i.e., `IsOffering() == false` means no metadata, not empty-string metadata).
  - Auto-detected groupings (no ConfigMap, synthesized by `DetectPCIeRootPairings`) never acquire intent labels — this is the regression test for decision #3 above (no implicit `visibility` default).
  - `.../railIndex` is absent from the aggregate (new, since PR1 only deleted `.../numa`).

- **`internal/controller/controller_integration_test.go`** (envtest): an enhanced ConfigMap produces a DeviceClass with the expected labels/annotations; updating the ConfigMap's metadata updates the DeviceClass; removing a field from the ConfigMap removes the corresponding annotation (exercises PR1's merge-preserving `publishDeviceClass` together with the new metadata path); a controller restart restores the expected state.

### Step 6 — Documentation and examples

**`README.md`:** the device-grouping ConfigMap mechanism is currently **undocumented** — there is no existing section to extend. Add a `## Device Groupings` section first (covering `name`/`alignment`/`fallback`/`devices`, the base mechanism), then a `## Intent-Based Hardware Offerings` subsection covering: the three intent levels, the optional metadata fields and their bounds, how an administrator creates an offering, how TC generates the DeviceClass, discovery commands (below), and backward compatibility (all new fields optional, legacy ConfigMaps unaffected). Place this after the existing `### Rule Fields` section so topology rules and device groupings sit together in the doc.

State explicitly: `categories` and `workloadTypes` are **annotations**, not labels, and **cannot be used in a label selector** (`kubectl get deviceclasses -l .../categories=...` will not work) — they're comma-joined strings, which aren't valid label values for multi-value matching.

Discovery commands to document (verify the exact JSONPath/custom-columns escaping against a real cluster or envtest before publishing — dotted label keys are easy to get wrong):

```bash
kubectl get deviceclasses -l nodepartition.dra.k8s.io/intent-level=workload
kubectl get deviceclasses -l nodepartition.dra.k8s.io/visibility=public
kubectl get deviceclasses -l nodepartition.dra.k8s.io/visibility=public \
  -o custom-columns='NAME:.metadata.name,LEVEL:.metadata.labels.nodepartition\.dra\.k8s\.io/intent-level,DISPLAY:.metadata.annotations.nodepartition\.dra\.k8s\.io/display-name'
```

State as a known limitation: request-level rejection of unsupported claims is deferred (decision #4) — the webhook currently passes through claims with no matching opaque config unchanged.

**`examples/` (new directory):**

- `examples/intent-workload-gpu-inference.yaml` — `intentLevel: workload`, name `gpu-inference`
- `examples/intent-capability-low-latency-network.yaml` — `intentLevel: capability`, name `low-latency-network`
- `examples/intent-hardware-gpu-nic-aligned.yaml` — `intentLevel: hardware-specific`, name `gpu-nic-aligned`

Base the YAML shape on the existing grouping ConfigMap fixtures in `grouping_test.go:28-36` and `controller_integration_test.go` (search for `device-grouping` label usage) rather than inventing a new shape. **Each example manifest must carry the label `nodepartition.dra.k8s.io/device-grouping: "true"`** (the constant `DeviceGroupingLabel`, `grouping.go:13`) — `LoadFromConfigMap` checks it at `grouping.go:56` and the controller's event handlers re-check it (`controller.go:381`, `:399`); a ConfigMap missing this label is silently ignored entirely, not just treated as a legacy grouping.

## Verification

```bash
go build ./...
go vet ./...
gofmt -l .
make test      # unit + envtest/integration together; fetches setup-envtest binaries on first run
make lint      # if golangci-lint is available in your environment; report if not
```

Manual check against a cluster or envtest with the grouping path active:

```bash
kubectl apply -n nodepartition -f examples/intent-workload-gpu-inference.yaml
kubectl get deviceclasses -l nodepartition.dra.k8s.io/intent-level=workload
kubectl get deviceclass gpu-inference -o yaml   # confirm labels, annotations, and unchanged Spec.Config
```

**End-to-end tests cannot be run in a typical dev environment.** `test/e2e/run-e2e.sh` requires an external mock driver (`github.com/fabiendupont/mock-device`) and a Vagrant cluster publishing `mock-accel.example.com` ResourceSlices. Report as not executed rather than as passing, unless that infrastructure is actually available.

## Deliverables (per the project's standing convention for this feature)

Summary of changes, files modified, the new ConfigMap schema (the six new optional `data` keys), a sample generated DeviceClass showing labels/annotations, the discovery commands, test results, a backward-compatibility statement (all new fields optional; legacy ConfigMaps parse unchanged; no change to webhook claim expansion), and known limitations (deferred request-level rejection, stale-metadata-on-parse-failure behavior, E2E not run).
