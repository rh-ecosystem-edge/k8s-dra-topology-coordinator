# Intent-Based Hardware Offerings Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

> **Historical status:** Implemented and merged. The unchecked boxes below are
> preserved as the original execution trace; use the current README, E2E guides,
> and harness test plan for operational instructions. Later changes added
> explicit rails, passthrough modes, versioned contracts, HA admission
> consistency, and custom driver identity beyond this plan's scope.

**Goal:** Add optional intent metadata to user-defined device groupings and publish it on their aggregate DeviceClasses for discovery.

**Architecture:** Parse and validate the six optional ConfigMap fields into `DeviceGrouping.Intent`, deep-copy them through the grouping store, and carry them through `GroupingInstance`. Add labels and annotations only when building the aggregate DeviceClass; keep the per-rail classes and webhook claim expansion unchanged. Extend the existing controller collision checks, reconciliation tests, README grouping docs, and examples.

**Tech Stack:** Go 1.25, Kubernetes DRA DeviceClasses, client-go, envtest, standard `testing` and `testify`.

**Spec:** `PR2_PLAN.md`

## Global Constraints

- All six new ConfigMap `data` keys are optional: `displayName`, `description`, `intentLevel`, `categories`, `workloadTypes`, and `visibility`.
- `intentLevel` accepts only `workload`, `capability`, or `hardware-specific`; `visibility` accepts only `public` or `internal`, with no default.
- Enforce the project limits: display name 63 characters; description 1024 characters; categories and workload types at most 16 entries, each at most 63 characters.
- Count project character limits as Unicode code points, as confirmed by the user on 2026-10-23.
- Split list values on commas, trim whitespace, drop empty entries, deduplicate, and preserve first-seen order.
- Validate `sanitizeForName(name)` as a DNS-1123 label of at most 56 characters. Do not change `sanitizeForName` or `sanitizeDNSLabel`.
- Emit intent labels and annotations on aggregate DeviceClasses only. Do not add intent metadata to auto-detected groupings or per-rail DeviceClasses. Emit visibility only when explicitly configured.
- `IsOffering()` is true exactly when at least one scalar field is non-empty or either list has an entry.
- Preserve the existing exact-name override: a user-defined grouping replaces an auto-detected grouping of the same raw name. Detect duplicate raw names only among user-defined groupings.
- Detect sanitized-name collisions on the merged list and collisions between emitted aggregate/per-rail DeviceClass names. Log the conflicting `data.name` values and ConfigMap sources. Return an error before grouping publication when any collision exists. This prevents nondeterministic writes and preserves the last successfully published grouping DeviceClasses; other grouping updates wait until the conflict is fixed.
- Keep the last valid cached grouping after an invalid ConfigMap update; log that its cached metadata is stale and identify the source ConfigMap.
- Do not change webhook claim expansion, add a CRD/controller/Event/RBAC permission, or implement request-level claim rejection.
- Categories and workload types are annotations, not label-selector fields. Document that distinction and the deferred request-level rejection limitation.
- Use the example offering names `gpu-inference`, `low-latency-network`, and `gpu-nic-aligned`; every example ConfigMap must carry `nodepartition.dra.k8s.io/device-grouping: "true"`.

## Review Focus

- A leading digit, dotted raw name such as `gpu.nvidia.com`, and consecutive internal hyphens such as `gpu--nic` are valid after sanitization; test them as accepted. Reject an underscore, `-gpu`, `gpu-`, or a sanitized name over 56 characters.
- Empty and whitespace-only lists normalize to empty; repeated raw entries are deduplicated before the 16-item limit; trimming happens before Unicode code-point length validation; pin first-seen order and 63/64-code-point boundaries with multibyte parser tests.
- Mutating either metadata slice returned by `GetGroupings()` must not mutate the store's cached grouping.
- Missing metadata and auto-detected groupings produce no intent keys; aggregate classes omit `railIndex`; rail classes receive no intent keys.
- Removing configured metadata removes the coordinator-owned key while preserving foreign labels/annotations and leaving `Spec.Config` unchanged.
- Exact raw-name duplicates are diagnosed only among user groupings; distinct raw names that sanitize identically are diagnosed after merge and excluded from the build input.
- Different grouping names must not emit the same aggregate/per-rail DeviceClass name (for example, `gpu`'s emitted `gpu-rail0` and another grouping's aggregate `gpu-rail0`); detect collisions only among manager-published representative rails after applying the same coalescing key, and preserve existing classes on a real collision.

---

## File Map

- `internal/controller/grouping.go`: metadata model, ConfigMap parsing/validation, ConfigMap source provenance, and deep copy.
- `internal/controller/grouping_test.go`: parser, name validation, and deep-copy tests.
- `internal/controller/controller.go`: stale-cache update log, duplicate-name diagnostics, and merged sanitized-name collision handling.
- `internal/controller/controller_test.go`: duplicate and sanitized collision tests plus invalid-update cache retention.
- `internal/controller/grouping_builder.go`: copy intent metadata into each grouping instance.
- `internal/controller/deviceclass_manager.go`: apply metadata to aggregate DeviceClasses and remove aggregate `railIndex`.
- `internal/controller/deviceclass_manager_test.go`: aggregate-versus-rail and metadata-free behavior.
- `internal/webhook/webhook_test.go`: verify metadata does not affect claim expansion; no production webhook changes.
- `internal/controller/controller_integration_test.go`: envtest coverage for create, update, removal, and restart.
- `README.md`: extend the existing Device groupings section with the intent offering schema and discovery guidance.
- `examples/intent-workload-gpu-inference.yaml`, `examples/intent-capability-low-latency-network.yaml`, `examples/intent-hardware-gpu-nic-aligned.yaml`: runnable ConfigMap examples.

## Tasks

### Task 1: Parse, validate, and retain intent metadata

**Files:**
- Modify: `internal/controller/grouping.go`
- Test: `internal/controller/grouping_test.go`

**Interfaces:**
- Add `IntentMetadata` with fields `DisplayName string`, `Description string`, `IntentLevel string`, `Categories []string`, `WorkloadTypes []string`, and `Visibility string`.
- Add `func (m IntentMetadata) IsOffering() bool`.
- Add `Intent IntentMetadata` and private ConfigMap source provenance to `DeviceGrouping`; copy both in `GroupingStore.GetGroupings()`.
- Use `func parseCommaSeparatedValues(field, value string) ([]string, error)` for both bounded list fields.

- [ ] **Step 1: Add parser tests** named `TestParseDeviceGrouping_IntentMetadata` and `TestParseDeviceGrouping_InvalidIntentMetadata`. Cover legacy ConfigMaps (assert `IsOffering() == false`); all three intent levels; `public` and `internal`; invalid enum values; normalization of `a,,b, ,c,a` to `a,b,c` for both list fields; 16 versus 17 entries; 63 versus 64 code points for display name and each list item; and 1024 versus 1025 code points for description. Include multibyte text to prove code-point counting. Use reflection for the not-yet-added `Intent` field and `IsOffering()` method so these first tests compile and fail as assertions.
- [ ] **Step 2: Add name-validation tests** named `TestParseDeviceGrouping_SanitizedNameValidation`; assert `1-gpu`, `gpu.nvidia.com`, and `gpu--nic` succeed and `gpu_inference`, `-gpu`, `gpu-`, and sanitized names longer than 56 characters fail. Assert wrapped errors identify both source ConfigMap (for example, `default/test`) and offending field (for example, `name`).
- [ ] **Step 3: Add `TestGroupingStore_GetGroupingsDeepCopiesIntent`**. Use reflection for the not-yet-added field, mutate a returned `Categories` element, and verify the next `GetGroupings()` result retains the original value.
- [ ] **Step 4: Run the focused tests** with `go test ./internal/controller -run 'Test(ParseDeviceGrouping_(IntentMetadata|InvalidIntentMetadata|SanitizedNameValidation)|GroupingStore_GetGroupingsDeepCopiesIntent)'`. Expected: tests compile and fail because metadata parsing, name validation, and deep-copy behavior are absent.
- [ ] **Step 5: Add the model, parser, and sanitized-name validation** in `grouping.go`. Keep leaf errors field-specific so `LoadFromConfigMap` supplies namespace/name. Use `utf8.RuneCountInString` for the confirmed code-point limits and retain source ConfigMap identity for collision logs.
- [ ] **Step 6: Deep-copy both metadata slices** in the existing field-by-field `GetGroupings()` copy. Replace the initial reflection assertions with direct typed assertions now that the model exists.
- [ ] **Step 7: Rerun the focused grouping tests** and verify all parser, validation, and aliasing assertions pass.

### Task 2: Diagnose grouping collisions and stale invalid updates

**Files:**
- Modify: `internal/controller/controller.go`
- Test: `internal/controller/controller_test.go`

**Interfaces:**
- Add `func detectDuplicateGroupingNames(userDefined []DeviceGrouping) map[string][]string`, mapping each duplicate raw name to its source ConfigMaps.
- Add `func detectSanitizedNameCollisions(groupings []DeviceGrouping) map[string][]DeviceGrouping`.
- Add `func detectGroupingDeviceClassNameCollisions(results []GroupingResult) map[string][]string` to detect output names owned by multiple groupings.

- [ ] **Step 1: Add `TestController_Reconcile_DuplicateUserNamesLogsActionableError`** with two user-defined ConfigMaps sharing `data.name`; capture logs and assert they identify the field, raw name, and both source ConfigMaps. Include a user-defined grouping that exactly overrides an auto-detected grouping and assert it is not reported as a duplicate.
- [ ] **Step 2: Add `TestController_Reconcile_SanitizedCollisionPreservesExistingDeviceClass`**. Seed the fake client with an existing managed grouping DeviceClass, load ConfigMaps named `gpu.inference` and `gpu-inference`, call `reconcile`, and assert it returns an actionable error while preserving the existing DeviceClass.
- [ ] **Step 3: Add generated-name collision tests**. Use satisfiable groupings named `gpu` and `gpu-rail0`, seed an existing managed `gpu-rail0` class, and assert reconciliation returns an actionable error without changing that class. Also cover `gpu` and `gpu-rail1` on one PCIe root with two same-root devices: the manager coalesces `gpu` instances and emits only `gpu-rail0`, so `gpu-rail1` remains a valid aggregate name and reconciliation succeeds.
- [ ] **Step 4: Add `TestGroupingConfigMapUpdate_InvalidUpdateKeepsCachedGrouping`**. Load a valid grouping, call the update handler with invalid metadata, capture the error log, and verify that it names the ConfigMap/stale cache and that the previous grouping remains available.
- [ ] **Step 5: Run the focused controller tests** with `go test ./internal/controller -run 'Test(Controller_Reconcile_(DuplicateUserNamesLogsActionableError|SanitizedCollisionPreservesExistingDeviceClass|GeneratedNameCollisionPreservesExistingDeviceClass)|GroupingConfigMapUpdate_InvalidUpdateKeepsCachedGrouping)'`. Expected: tests compile and fail because the diagnostics/recovery and stale-cache log are not implemented.
- [ ] **Step 6: Implement duplicate-name detection** inside `if !skipGroupings`, on the user-defined slice only and before merge. Log `field=data.name`, duplicate raw name, and source ConfigMaps at error level; do not abort reconciliation or change exact-name override behavior.
- [ ] **Step 7: Implement sanitized-name detection after `mergeGroupings`**. Log `field=data.name`, conflicting sanitized key, raw names, and sources; increment reconciliation errors and return before `BuildGroupings` and `SyncGroupingDeviceClasses`, preserving last-known-good grouping DeviceClasses and preventing cleanup.
- [ ] **Step 8: Detect collisions among emitted class names after `BuildGroupings` and before `SyncGroupingDeviceClasses`**. Report the emitted class name, conflicting raw grouping names, and ConfigMap sources; increment reconciliation errors and return before publication/cleanup.
- [ ] **Step 9: Clarify the invalid-update error log** to say the cached grouping is retained and stale, including ConfigMap namespace/name. Preserve the existing behavior of not replacing the cached value.
- [ ] **Step 10: Rerun the focused controller tests** and the existing `TestMergeGroupings_UserOverridesAuto`.

### Task 3: Carry intent metadata through grouping instances

**Files:**
- Modify: `internal/controller/grouping_builder.go`
- Test: `internal/controller/grouping_test.go`

**Interfaces:**
- Add `Intent IntentMetadata` to `GroupingInstance`; set it from the source `DeviceGrouping` in `buildInstance`.

- [ ] **Step 1: Add `TestGroupingBuilder_BuildGroupingsCarriesIntentMetadata`** with a satisfiable test topology and assert the generated instances contain the configured scalar and slice metadata.
- [ ] **Step 2: Run the focused test** with `go test ./internal/controller -run TestGroupingBuilder_BuildGroupingsCarriesIntentMetadata`. Expected: it fails because instances do not yet carry intent.
- [ ] **Step 3: Copy `grouping.Intent` into the `GroupingInstance` literal** in `buildInstance`.
- [ ] **Step 4: Add the representative invariant comment** where the aggregate path consumes the representative: all instances for a grouping name share one merged source, so intent is identical; combining sources under one name later would invalidate this assumption.
- [ ] **Step 5: Rerun the focused builder test.**

### Task 4: Publish intent metadata on aggregate DeviceClasses

**Files:**
- Modify: `internal/controller/deviceclass_manager.go`
- Test: `internal/controller/deviceclass_manager_test.go`
- Test: `internal/controller/controller_integration_test.go`

**Interfaces:**
- Add `func applyIntentMetadata(dc *resourcev1.DeviceClass, intent IntentMetadata)`.
- Map labels to `nodepartition.dra.k8s.io/intent-level` and `nodepartition.dra.k8s.io/visibility`; map annotations to `display-name`, `description`, `categories`, and `workload-types` under the same prefix. Join list values with commas.

- [ ] **Step 1: Add `TestIntegration_IntentMetadataPropagation`** to envtest before manager changes. Create a grouping ConfigMap and matching GPU/NIC ResourceSlices; assert aggregate labels, annotations, and unchanged `Spec.Config`; update a metadata value; remove a field and assert its coordinator-owned annotation disappears; delete the aggregate, restart the controller, and assert it is recreated. It compiles against current APIs and should fail because metadata is not published.
- [ ] **Step 2: Add manager tests** named `TestDeviceClassManager_SyncGroupingDeviceClasses_IntentMetadataOnlyOnAggregate`, `TestDeviceClassManager_SyncGroupingDeviceClasses_NoIntentMetadata`, `TestDeviceClassManager_SyncGroupingDeviceClasses_AutoDetectedHasNoIntentMetadata`, and `TestDeviceClassManager_SyncGroupingDeviceClasses_AggregateOmitsRailIndex`. Reuse `multiDeviceGroupingInstance`; assert aggregate labels `intent-level=workload` and `visibility=public`, annotations `display-name`, `description`, `categories=ai,inference`, and `workload-types=training,serving`; no intent keys on the rail class; no intent keys for an empty Intent; and no `railIndex` on the aggregate. Generate the auto-detected case through `DetectPCIeRootPairings`.
- [ ] **Step 3: Run the focused manager and integration tests**. Expected: tests fail because aggregate metadata is absent and aggregate `railIndex` remains.
- [ ] **Step 4: Implement `applyIntentMetadata`** with an `IsOffering()` early return, lazily initialized maps, `CoordinatorDriverName` keys, intent-level and visibility labels, and display-name, description, categories, and workload-types annotations.
- [ ] **Step 5: Call the helper only in the aggregate emission loop** using `entry.representative.Intent`; delete the aggregate `railIndex` label next to the existing `numa` deletion. Keep the aggregate `alignment` label and leave per-rail output untouched.
- [ ] **Step 6: Rerun the focused manager and integration tests** and the existing aggregate determinism, no-op, and foreign-metadata-preservation tests.

### Task 5: Prove webhook neutrality and controller reconciliation

**Files:**
- Test: `internal/webhook/webhook_test.go`
- No production changes to `internal/webhook/`

- [ ] **Step 1: Add `TestIntentMetadataDoesNotChangeClaimExpansion`**. Expand the same claim against otherwise identical bare and intent-annotated DeviceClasses; compare the generated request/constraint patch values and assert both use the same `PartitionConfig`.
- [ ] **Step 2: Run the focused webhook test** with `go test ./internal/webhook -run TestIntentMetadataDoesNotChangeClaimExpansion`. Expected: it passes once the test fixture is set up; this is a behavior characterization, not a webhook feature change.
- [ ] **Step 3: Recheck `ClaimExpander.getPartitionConfig`** and confirm it reads only `DeviceClass.Spec.Config`, not labels or annotations.
- [ ] **Step 4: Run the focused webhook test** with `go test ./internal/webhook -run TestIntentMetadataDoesNotChangeClaimExpansion`. Expected: it passes; this is a behavior characterization because production webhook code is unchanged.

### Task 6: Document the offering schema and add examples

**Files:**
- Modify: `README.md`
- Create: `examples/intent-workload-gpu-inference.yaml`
- Create: `examples/intent-capability-low-latency-network.yaml`
- Create: `examples/intent-hardware-gpu-nic-aligned.yaml`

- [ ] **Step 1: Extend the existing `## Device groupings` section** with the intent offering subsection. Cover the three levels, all optional fields, and state explicitly that the 63-character display-name and 1024-character description caps are project policies, not Kubernetes API limits. Also cover list limits, backward compatibility, aggregate DeviceClass behavior, stale cached metadata after invalid updates, and deferred request-level rejection. Include these discovery commands:

```bash
kubectl get deviceclasses -l nodepartition.dra.k8s.io/intent-level=workload
kubectl get deviceclasses -l nodepartition.dra.k8s.io/visibility=public
kubectl get deviceclasses -l nodepartition.dra.k8s.io/visibility=public \
  -o custom-columns='NAME:.metadata.name,LEVEL:.metadata.labels.nodepartition\.dra\.k8s\.io/intent-level,DISPLAY:.metadata.annotations.nodepartition\.dra\.k8s\.io/display-name'
```

State that `categories` and `workloadTypes` are comma-joined annotations and cannot be queried with a Kubernetes label selector.
- [ ] **Step 2: Add the three example ConfigMaps** using the existing label and YAML shape. Give each the matching name and intent level from its filename; include representative optional categories/workload types/visibility values.
- [ ] **Step 3: Verify the documented `kubectl get deviceclasses` label selectors and custom-column escaping** against envtest or a cluster with a generated aggregate DeviceClass.
- [ ] **Step 4: Review all examples** for the required `nodepartition.dra.k8s.io/device-grouping: "true"` label and valid metadata values.

### Task 7: Run final verification and review the diff

**Files:** all files above.

- [ ] **Step 1: Run `gofmt`** on changed Go files and confirm `gofmt -l` reports no formatting issues for those files. Report pre-existing vendor formatting separately without rewriting vendored code.
- [ ] **Step 2: Run `go build ./...` and `go vet ./...`.** Both must succeed.
- [ ] **Step 3: Run `make test`** for unit and envtest coverage. Report environment/setup failures accurately.
- [ ] **Step 4: Run `make lint` if `golangci-lint` is available; otherwise report it as unavailable.**
- [ ] **Step 5: Review the final diff** for unchanged webhook production code, aggregate-only metadata, no accidental changes to sanitizer behavior, and all three example labels.
- [ ] **Step 6: Report the E2E lane actually executed.** The direct suite requires the external mock-device environment; the coordinator bridge can run under `k8s-dra-harness` with its own adapter and evidence boundary.
- [ ] **Step 7: Report the six optional ConfigMap keys, a generated DeviceClass metadata example, discovery commands, test results, backward compatibility, and the known request-level rejection, stale-cache, and E2E limitations.**

## Plan Adjustments and Risks

- The source plan says README has no device-grouping documentation, but the current checkout already has a basic `## Device groupings` section. Extend it rather than duplicate it. The source plan's `### Rule Fields` insertion point does not exist; the current README has a Rule fields table inside `## Topology rules`.
- The source plan lists a leading digit among rejected names, which conflicts with its own DNS-1123 regex. This plan treats `1-gpu` as valid and pins the actual invalid cases.
- The source plan also calls `gpu--nic` invalid even though its DNS-1123 regex allows repeated internal hyphens. The implementation follows the regex and Kubernetes validator: consecutive internal hyphens are accepted.
- The architect flagged character-count semantics as unspecified. The user confirmed Unicode code-point counting; tests pin that choice with multibyte input.
- The architect found the prior collision-suppression proposal could delete existing managed DeviceClasses through normal cleanup. The user confirmed the revised policy: return before grouping publication, preserving prior classes and delaying all grouping updates until the conflict is fixed.
- Parser tests must assert that invalid values identify both the offending field and source ConfigMap. Collision diagnostics must identify `data.name`, raw/sanitized names, and source ConfigMaps.
- Direct full E2E remains environment-dependent; the harness bridge provides a separate hardware-free and hardware-backed execution path. Local unit/envtest results must not be reported as real-driver allocation evidence.
- Final review found that distinct valid grouping names can still collide after per-rail suffixes are added; the plan now checks actual satisfied output names before manager sync.

## Architect Review

**Verdict:** Approve with changes. The plan incorporates the review by preserving previously published grouping DeviceClasses on sanitized-name conflicts, requiring the README to distinguish project policies from Kubernetes limits, and pinning field/source context in validation and collision diagnostics. The user confirmed Unicode code-point counting and the collision-recovery policy on 2026-10-23.
