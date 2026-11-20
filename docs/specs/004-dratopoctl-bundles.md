# `dratopoctl bundles`: Generation-Time Bundles

**Status:** Proposed

**Scope:** Extend `dratopoctl` so admins (and AI assistants working for them) can inspect topology, author offerings, render fully expanded ResourceClaimTemplates, report fit, and detect drift, with no webhook involved. The same binary also works as a `kubectl dratopo` plugin.

**Related specs:**

- [002: Intent request and catalog options](002-intent-request-and-catalog-options.md) (this work extends Option A)
- [003: Webhook feature freeze](003-webhook-feature-freeze.md)
- [005: Controller bundle stamping](005-controller-bundle-stamping.md) (reuses this renderer)
- [006: KEP draft, extended-resource device bundles](006-kep-extended-resource-device-bundles.md) (long-term one-line path)

## Context

Users want to reference a curated, topology-aligned bundle by name from a Pod (for example, a GPU and a NIC on one PCIe root plus NUMA-local CPU) without writing a ResourceClaim or ResourceClaimTemplate by hand. Today the webhook does this at admission time. Spec 003 freezes the webhook. This spec moves expansion to generation time.

- **Expansion is pure.** `expandRequest` (`internal/webhook/webhook.go:389`) depends only on the request and the class `PartitionConfig`, so a template can be expanded once, ahead of time.
- **Kubernetes already creates a claim per Pod from a template.** Once a template is expanded, kube-controller-manager stamps an already-correct claim for every Pod. Nothing runs at admission.
- **On homogeneous clusters a one-shot generator is enough.** Render once, commit, and check for drift in CI. Clusters with heterogeneous or drifting hardware can add spec 005 later.

After this spec, the pieces are:

| Piece | Role |
|---|---|
| Offering ConfigMaps | The admin defines the bundle (unchanged) |
| Controller | Discovers topology, validates offerings, publishes DeviceClasses (unchanged) |
| `dratopoctl` | Inspects, drafts, validates, renders, reports fit, checks drift |
| Webhook | Frozen. Still serves hand-written partition claims |

A Pod consumes a rendered bundle like this:

```yaml
spec:
  resourceClaims:
  - name: acc
    resourceClaimTemplateName: gpu-local-bundle
  containers:
  - name: app
    resources:
      claims:
      - name: acc        # no request: → the container gets every device in the bundle
```

## Decisions

- **Reuse, don't extract.**
  - The CLI runs the controller's existing `reconcile` against an in-memory fake clientset (`k8s.io/client-go/kubernetes/fake`).
  - It calls the webhook's existing expansion code through a thin exported wrapper.
  - It does not pull about 300 lines out of `SyncGroupingDeviceClasses`.
  - One code path serves the controller, the webhook, and the CLI.
- **Two input sources.** Live cluster by default (kubeconfig). `--from-dir` reads YAML dumps of ResourceSlices and ConfigMaps for offline use and CI.
- **Output is deterministic.** It is byte-identical across runs, so `check` can diff and goldens stay stable.
- **The CLI is read-only.** It never creates, updates, or deletes cluster objects, which makes it safe to hand to an AI agent.
- **AI writes intent; the CLI writes mechanics.** An assistant may draft an offering. Templates come only from `render`.
- **Minimal kubectl plugin.** The same binary is also built as `kubectl-dratopo`, keeping its current flags. No `cli-runtime`, no krew yet.

## Current code (verified at `ba64fff`)

**`cmd/dratopoctl`**
- `offerings list|get|explain` (`main.go`, `contract.go`) and `claims expand` (`claims.go`).
- `claims expand` does a server-side dry-run Create, so it needs the webhook installed.
- `offeringPreviewClaim` is at `claims.go:110`.
- Formats `table|json|yaml` go through `checkFormat` (`main.go:256`).
- Root persistent flags (`main.go:46-49`): `--kubeconfig`, `--context`, and `-o` (default `table`). `claims expand` adds `-n` and shadows `-o` locally (`claims.go:76`).
- `showAll` is bound to `offerings list`.
- `main()` always exits 1 on error (`main.go:33-36`).

**Grouping parsing and class mapping (`internal/controller`)**
- Parse errors are free text, e.g. `device[%d] missing required field 'class'` (`grouping.go:231-440`), prefixed at `:148`. There is no field-path structure.
- `onGroupingConfigMapAdd` only logs a parse error and drops the ConfigMap (`controller.go:603-606`).
- Named `devices` entries require `matchAttribute` or `alignments` (`grouping.go:368-371`), and `alignments` cannot be combined with `alignment` (`:319`).
- The `devices` key holds a YAML string, not a nested list (`grouping.go:339`).
- A grouping naming a class a node lacks yields zero instances on that node, with no diagnostic (`nodeHasRequiredClasses`).
- `GetDeviceClassForDriver` (`topology_rules.go:232-242`) falls back to the driver name itself.
- `GetDeviceClassForDevice` (`:249`) applies per-device `whenValue` rules (e.g. MIG profiles).
- Auto-detected groupings from `DetectPCIeRootPairings` (defined at `partition_builder.go:97`, called at `controller.go:393`) appear in `GroupingResult` alongside curated ones.
- An empty config namespace means all namespaces (`controller.go:62-64`).
- `reconcile` refreshes topology rules into the model (`refreshModelRules`, called at `controller.go:341`, defined at `:491`).
- The rule store is the unexported `ruleStore` field (`controller.go:56`).
- Curated-offering selection (`buildEntries`, `groupingKind`, `kindOffering`) lives in `package main` (`cmd/dratopoctl/catalog.go:41-85`).
- Live mode lists only managed classes (`loadManagedClasses`, `cmd/dratopoctl/main.go:129`).
- `lookupPartitionConfig` returns `(nil, false, nil)` for a missing class (`webhook.go:766`), so expansion of a missing aggregate is a silent no-op.

**Webhook expansion (`internal/webhook/webhook.go`)**
- `expandClaim` (`:195`) returns JSON patches. Its only API call is `lookupPartitionConfig` (`:760`), which gets the DeviceClass.
- It adds an `expanded-requests` annotation (`:352-378`) and mutates `claim.Annotations` in place (`:359-363`).
- Preferred alignments depend on `model.IsConstraintSatisfiable` (around `:561`).
- The deployed webhook gets the live model (`cmd/nodepartition-controller/main.go:226`).

**Pod hook with a pre-expanded template**
- `handlePodAdmission` recomputes the mapping from the template (`:871-889`).
- Driver classes have no `PartitionConfig`, so the mapping is empty and the Pod is allowed unchanged. Rendered templates therefore work whether or not the webhook is installed.

**Controller (`internal/controller/controller.go`)**
- `NewController` (`:81`), `reconcile` (`:336`).
- Handlers: `onSliceAdd` (`:498`), `onConfigMapAdd` (`:539`), `onGroupingConfigMapAdd` (`:595`).
- `reconcile` uses no informers or listers.
- Leader gating happens in the worker, not in `reconcile`.
- The ConfigMap handlers do not filter by namespace (the informer does).

**Nondeterministic output (several map iterations)**
- `SyncGroupingDeviceClasses` (`deviceclass_manager.go:1362`) seeds each aggregate from the first entry of `range seen` (`:1482`). Fields such as `alignment` and `mode` therefore depend on map order. `minInstanceIndex` (`:1508`) and `acc.add` device order (`:1511`) are affected too.
- Further upstream:
  - `BuildGroupings` ranges the `nodes` map (`grouping_builder.go:122`).
  - `NodeTopology.AllDevices()` ranges `DevicesByDriver` (`topology_model.go:110-115`). `buildNodeGroupings` uses it (`grouping_builder.go:139`), which makes `InstanceIndex` (`:438`) order-dependent.
  - `GroupingStore.GetGroupings` ranges a map (`grouping.go:177`).
  - `DetectPCIeRootPairings` ranges nodes (`partition_builder.go:104`).
  - `mergeGroupings` builds its result from a `byName` map (`controller.go:654-667`), which randomizes `GroupingResult.Instances` order and `publishErrs` order.
  - The rule store ranges `s.rules` in `GetRules` (`topology_rules.go:161`), `GetNUMAAttributeForDriver` (`:221`), `GetDeviceClassForDriver` (`:236`), `GetDeviceClassForDevice` (`:253`) and `GetMatchConstraintRules`. `driverForClass` (`deviceclass_manager.go:2081`) iterates `GetRules()`. Two rules for one driver (MIG `whenValue` rules plus a default) can resolve differently per run. `sortPartitionConfig` (`:273-297`) only orders alignments.
  - `SetRules` → `reextractAllLocked` ranges `m.rawSlices` and groups (`topology_model.go:183-187`), so `DevicesByDriver` order is random. `GetNodeTopologies()` returns a map.
- The existing `..._AggregateIsDeterministic` test (`deviceclass_manager_test.go:1547`) reuses one fake client across syncs, so the last write wins and it proves nothing.
- Homogeneous nodes share an instance key, so the "representative" node, and its device names, vary between runs.

**Other**
- Topology data: `TopologyModel.GetNodeTopologies` (`topology_model.go:413`), `TopologyDevice` (`:46`, with `PCIeRoot`, `NUMANode`).
- Driver-to-class mapping: `TopologyRuleStore.GetDeviceClassForDriver` (`topology_rules.go:232`).
- Aggregate class naming: `sanitizeForName` (`grouping_builder.go:598`).
- `go.mod` has `evanphx/json-patch/v5` and `google/go-cmp` only as indirect dependencies.
- The binary is built at `Makefile:18`.

## Implementation steps

Each step is one PR.

### 3.1 Deterministic grouping output (bug fix)

Make every map iteration on the grouping path ordered:

- **Input order:**
  - `BuildGroupings`: iterate node names sorted.
  - `NodeTopology.AllDevices()`: return devices sorted by `(driver, pool, device name)`.
  - `GroupingStore.GetGroupings` and `DetectPCIeRootPairings`: return sorted output.
  - `mergeGroupings`: sort its output by `Name`.
- **Rule store:** `GetRules` returns rules sorted by name, and every other getter iterates that sorted slice instead of ranging `s.rules`. First match in name order wins.
- **Model:** add `NodeTopology.SortedDevices()` (devices sorted by `(driver, pool, name)`) and `TopologyModel.SortedNodeNames()`. `AllDevices()`, `topology show` and `init` use them, never `DevicesByDriver` or the `GetNodeTopologies()` map directly.
- **`SyncGroupingDeviceClasses`:**
  - iterate `seen` keys sorted by `(InstanceIndex, key)`, so the seed matches `minInstanceIndex`;
  - the same ordering drives `acc.add`;
  - the publish loops that range over maps also iterate sorted, and `publishErrs` is sorted, so JSON errors (3.10) are stable.
- **Request order:** confirm that `buildGroupingAggregateConfig` emits sub-requests in a stable order, because request order feeds into render output.

**Test:** a grouping with mixed primary and fallback instances (different `Alignment`) over several homogeneous nodes, plus two rules for one driver (a MIG `whenValue` rule and a default). Build and sync 20 times, each with a **fresh** fake client, and compare the marshaled DeviceClasses and `GroupingResult`s byte for byte. Mixed instances and the rule pair are required; a uniform fixture would pass without the fix. Replace the existing `..._AggregateIsDeterministic` test, which shares one client.

### 3.2 `controller.Simulate`

New file `internal/controller/simulate.go`:

```go
type SimulationResult struct {
    DeviceClasses []resourcev1.DeviceClass // sorted by name
    Groupings     []GroupingResult         // sorted by node, grouping, instance
    Model         *TopologyModel
    Rules         *TopologyRuleStore       // for GetDeviceClassForDevice in init (3.8)
    LoadErrors    []error                  // per-ConfigMap parse errors, sorted by source
}

func Simulate(ctx context.Context, slices []resourcev1.ResourceSlice, cms []corev1.ConfigMap,
    driverName string, mode PartitionMode, configNamespace string) (*SimulationResult, error)
```

1. Call `NewController(fake.NewSimpleClientset(), driverName, mode)`.
2. Feed slices to `onSliceAdd`. Load ConfigMaps by calling `TopologyRuleStore.LoadFromConfigMap` and `GroupingStore.LoadFromConfigMap` **directly**, not through the handlers. The handlers only log parse errors and drop the ConfigMap; calling the stores directly lets `Simulate` collect every load error.
   - **Dispatch on label.** A ConfigMap with the rule label goes to the rule store, one with the grouping label to the grouping store. A ConfigMap with neither label is skipped, not an error (`LoadFromConfigMap` errors on unlabeled input, `grouping.go:142`).
   - Filter ConfigMaps by `configNamespace` here, because the handlers don't. An empty value means all namespaces, matching the controller. In `--from-dir` mode a ConfigMap with no namespace (such as `examples/intent-hardware-gpu-nic-aligned.yaml`) defaults to `configNamespace` instead of being dropped.
   - **Load errors are returned, not fatal.** Like the controller, `Simulate` drops a malformed ConfigMap and keeps going, recording the error in `LoadErrors`. `validate` treats them as fatal. `render`, `fit` and `init` print them as warnings, so one bad unrelated ConfigMap does not block them.
3. Call `c.reconcile(ctx)` once, directly. This bypasses the leader gate, which is intended. Return its error: duplicate and collision checks return early and must surface.
4. `defer c.workqueue.ShutDown()`. `onSliceAdd` does not enqueue while `synced` is false, but `onConfigMapAdd` paths and the queue goroutine still need stopping.
5. List and sort the DeviceClasses from the fake client.

Add a `lastGroupingResults []GroupingResult` field that `reconcile` sets just before `SyncGroupingDeviceClasses`. It is one line and changes no behavior.

Mark the approach in code: `// shortcut: reuses reconcile via fake clientset; extract pure builders if CLI size/latency matters`.

**Tests** (`simulate_test.go`), reusing `makeGroupingConfigMap` (`grouping_test.go:14`) and `makeTestDevice` (`:596`):
- class names, count labels, and per-node instances are as expected;
- a duplicate grouping returns an error;
- ConfigMaps in other namespaces are ignored.

### 3.3 Exported expansion

In `internal/webhook/webhook.go`:

```go
// ExpandClaim returns the claim as admission would mutate it, plus the request-name mapping.
func (ce *ClaimExpander) ExpandClaim(ctx context.Context, claim *resourcev1.ResourceClaim) (*resourcev1.ResourceClaim, map[string][]string, error)
```

- Deep-copy the claim first, because `expandClaim` mutates the annotations map.
- Call the unchanged `expandClaim` and apply the patches with `evanphx/json-patch/v5`. Promote it to a direct dependency with `go mod tidy`, then run `go mod vendor` (the repo commits `vendor/`). It is already vendored, so this should be a no-op. The `replace` block only touches `k8s.io/{api,apimachinery,client-go}`, so there is no conflict.
- Unmarshal the result. Get the mapping from the existing `expansionMapping` (`webhook.go:949`) rather than parsing it back out of the annotation.

**Tests:**
- The output matches the existing expected expansions, using the helpers in `webhook_test.go`.
- **Idempotency:** expanding an already expanded claim returns nil patches. Run this against a fake clientset that also contains the unmanaged driver classes, as a live cluster would. This is the guarantee that a rendered template passes an installed webhook unchanged.

This step can be developed in parallel with 3.2.

### 3.4 `internal/bundles` and `bundles render`

**`internal/bundles/render.go`** (shared with spec 005):

```go
type Options struct {
    Offerings []string
    All       bool
    Count     int64
    Namespace string
    Name      string
}

func Render(ctx context.Context, exp *webhook.ClaimExpander, classes []resourcev1.DeviceClass, opts Options) ([]resourcev1.ResourceClaimTemplate, error)
```

- **Offering selection.** Add `bundles.CuratedOfferings(classes []resourcev1.DeviceClass) []resourcev1.DeviceClass`, which keeps classes labeled `managed=true`, `aggregate=true` and `offering=true`, sorted by name. `--all` and the "is this a curated offering" check for named offerings use it. The equivalent logic in `cmd/dratopoctl/catalog.go:41-85` is in `package main` and cannot be imported. `catalog.go` keeps its own copy for now.
- **Input.** Move `offeringPreviewClaim` here from `cmd/dratopoctl/claims.go`. For each selected curated offering, build its claim, call `ExpandClaim`, and wrap the spec in a ResourceClaimTemplate.
- **Add `controller.AggregateClassName(groupingName)`** here (one exported line wrapping `sanitizeForName`). `Render` tests and 3.5 both need it.
- **Name.** `<offering>`, or `<offering>-x<N>` when the count is above 1. `--name` is allowed only with a single offering. A name longer than 63 characters is rejected with a clear error.
- **Annotations**, on the template's top-level `metadata` (not `spec.metadata`), so they describe the template for `check` and do not propagate onto per-Pod claims:
  - `nodepartition.dra.k8s.io/bundle-offering`
  - `nodepartition.dra.k8s.io/bundle-count`
  - `nodepartition.dra.k8s.io/bundle-requests`: JSON mapping of offering request to expanded request names, for Pod and VMI authors.
- **Strip** the `expanded-requests` annotation that `ExpandClaim` adds. The Pod hook never reads it from templates.
- **Guard.** After `ExpandClaim`, fail if either holds:
  - the mapping for the offering request is empty. A missing aggregate class makes expansion a silent no-op (`webhook.go:766`), which would emit a template naming a nonexistent class;
  - any final request's class is in the managed set, which would break idempotency.
- **Output order:** sorted by name. Namespace is omitted when empty.

**`cmd/dratopoctl/source.go`** adds persistent flags `--from-dir`, `--driver-name` (default `nodepartition.dra.k8s.io`), `--partition-mode` (default `auto`), and `--config-namespace` (empty means all namespaces).

Both modes build their inputs through `Simulate`, so the model always includes the topology rules that preferred alignments depend on (`webhook.go:562`).
The source also exposes **all DeviceClasses** (managed and unmanaged). `init` (3.8) needs the unmanaged driver classes to check that a proposed class exists, and the expander needs them for the idempotency guarantee.
- **Live:**
  - list ResourceSlices, rule ConfigMaps, grouping ConfigMaps, and all DeviceClasses (no label selector; `loadManagedClasses` filters later);
  - run `Simulate` to get the model;
  - expand against the **published** DeviceClasses, using `NewClaimExpanderWithDriver(client, driver, model)`.
  - If ResourceSlices or ConfigMaps can't be listed, fail with a clear error. Never silently drop preferred alignments.
- **`--from-dir`:**
  - decode multi-document YAML (this also covers JSON), plus `kind: List` via `meta.ExtractList`, the format `kubectl get -o yaml` emits;
  - run `Simulate`, load its classes plus the dump's unmanaged `kind: DeviceClass` objects into a fake clientset, and build the expander from that.
  - Managed DeviceClasses found in the dump are ignored with a warning, because `Simulate` regenerates them.
- **`--partition-mode auto`** also runs the partition sync, so unrelated partition collision errors can surface. That is deliberate, for parity with the controller; document it.

**`cmd/dratopoctl/bundles.go`** adds `newBundlesCmd()` with `render [OFFERING...]` and flags `--count` (default 1), `--namespace`, `--name`, `--all`.
- `--all` gets its own variable (`showAll` belongs to `offerings list`).
- `render` shadows `-o` locally with default `yaml`, as `claims expand` does (`claims.go:76`), and rejects `table`.

**`main.go`:**
- Register the command.
- **klog.** klog already writes to stderr (`logtostderr=true`), so stdout YAML is safe. `Simulate`'s Info logs are noise on stderr, though. Discard them by default with `klog.LogToStderr(false)` followed by `klog.SetOutput(io.Discard)`; `LogToStderr(false)` alone would write log files under `/tmp`. A `-v` flag restores stderr logging.
- **Exit-code plumbing** lands with this step, not later:
  - add a typed `exitError{code int; err error}`;
  - `main()` maps it to `os.Exit(code)`;
  - exit code 2 needs explicit wrapping, because `SilenceErrors` is true and cobra returns plain errors. Wrap via `SetFlagErrorFunc` (flag errors), an args-validator wrapper around `cobra.NoArgs`/`ExactArgs`/etc., unknown subcommands, and input-validation errors such as `validateExpandInput`;
  - when `-o json` is set, `main()` prints errors as JSON (see 3.10).

**Golden tests:**
- Inputs in `cmd/dratopoctl/testdata/bundles/input/`:
  - `slices.yaml`;
  - `grouping.yaml`, based on `examples/intent-hardware-gpu-nic-aligned.yaml`, plus a preferred-alignment case.
- Goldens `render.golden.yaml` and `render-count2.golden.yaml`, regenerated with an `-update` flag. Neither the flag nor a `testdata` directory exists yet; declare `var update = flag.Bool("update", false, ...)` in the test file.
- A render against a missing aggregate class fails (the guard above).
- 10 consecutive runs must produce byte-identical output.

### 3.5 `bundles fit`

- `bundles.Fit(groupings []controller.GroupingResult) []FitRow{Node, Offering, Bundles}` counts instances per node, **only for curated offerings**: `Intent.Offering && !AutoDetected`. Auto-detected PCIe-root pairings are excluded.
- **Document two limits:** fit counts each offering independently and ignores device overlap between offerings, and it ignores existing allocations. It reports capacity, not current availability.
- Uses `controller.AggregateClassName` from 3.4.
- Fit always runs `Simulate`. In live mode it first lists ResourceSlices and labeled ConfigMaps, because published classes carry only totals.
- Output is a table (`NODE  OFFERING  BUNDLES`) or json/yaml, through `checkFormat`. `--count N` reports `floor(instances / N)`.
- Golden: `fit.golden.txt`.

### 3.6 `bundles check -f FILE`

- Decode the templates in `FILE` and keep those that carry `bundle-offering`.
- Re-render each one with its recorded count, name, and namespace, and compare byte for byte.
- On mismatch, print `drift: <ns>/<name>` and a `cmp.Diff`. Promote `go-cmp` to a direct dependency.
- An offering that no longer exists counts as drift.
- Exit codes: 0 when clean, 1 on drift.

**Tests:**
- a clean file exits 0;
- an edited golden exits 1 and prints a diff;
- an offering removed from the inputs exits 1.

### 3.7 Documentation

New `docs/bundles.md`:

- **Workflow:** `render` → commit → CI runs `bundles check --from-dir`.
- **Pod usage:** `resources.claims` without `request:`, or the expanded names from `bundle-requests`.
- **VMI usage:** declare `hostDevices` yourself, using `bundle-requests`.
- **Caveat:** preferred alignments are decided at render time and are not re-evaluated when topology changes. `check` detects that.
- **Caveat:** `--from-dir` must use the same driver name, partition mode, and config namespace as the controller.

Mark `claims expand` as "requires the webhook" in its help text and in the docs. Link `docs/bundles.md` from `README.md`.

### 3.8 `bundles init NAME`: offering authoring helper

Proposes an offering from the discovered topology. It only proposes; it never applies.

- **Flags:**
  - `--alignment pcieRoot|numaNode` (default `pcieRoot`)
  - `--drivers d1,d2` (filter)
  - `--class driver=DeviceClass` (override)
  - the source flags
- **Algorithm** (`internal/bundles/init.go`, a pure function over `SimulationResult.Model` and `SimulationResult.Rules`):
  1. For each node in `SortedNodeNames()`, group `SortedDevices()` by `PCIeRoot` or `NUMANode`.
  2. Map each device to its class with `GetDeviceClassForDevice`, so per-device rules such as MIG profiles apply. `--class` overrides this.
  3. In each group, count devices per class.
  4. Pick the most common composition across all groups. Ties go to more classes, then lexical order.
- **Unmapped classes.** `GetDeviceClassForDriver` falls back to the driver name, so a missing rule never errors. Instead, a resulting class that does not exist (not among the source's DeviceClasses, managed or unmanaged; see 3.4) is an error. It names the driver and suggests `--class` or a topology rule.
- **Output (stdout):** a grouping ConfigMap that `parseDeviceGrouping` accepts:
  - the `nodepartition.dra.k8s.io/device-grouping=true` label;
  - `metadata.namespace` set to `--config-namespace`, when that is non-empty;
  - `name`, `alignment`, and `offering: "true"`;
  - a `devices` key holding a **YAML string** of `{class, count}` entries, with no `name` (named entries need `matchAttribute` or `alignments`; `grouping.go:368-371`);
  - empty `displayName` and `description` for the admin to fill in.
- **Output (stderr):** a fit summary from running `Simulate` and `Fit` with the proposed ConfigMap added: nodes, bundles per node, and groups that don't match the chosen composition.

**Tests:**
- goldens for pcieRoot and numaNode;
- heterogeneous input chooses the majority composition and reports mismatches;
- an unmapped driver errors;
- **round trip:** the proposed ConfigMap loads through `GroupingStore.LoadFromConfigMap` and yields at least one instance.

**Docs:** an "Authoring an offering" section: `init` → edit → apply → `render`.

### 3.9 `topology show` and `bundles validate`

- **`dratopoctl topology show [NODE...]`:** for each node (sorted), groups by `pcieRoot` and by `numaNode` (sorted), each listing devices `{driver, name, class}` from `SortedDevices()`, taken from `Simulate().Model`. Output is table, json, or yaml.
- **`dratopoctl bundles validate -f offering.yaml`:**
  - Load each ConfigMap with `GroupingStore.LoadFromConfigMap` directly, so parse errors are returned rather than logged. A ConfigMap without a namespace defaults to `--config-namespace`.
  - Run `Simulate` with the ConfigMaps added. Any `LoadErrors` are fatal here. It reports duplicate and collision errors, then the fit.
  - When the fit is zero, add a per-node diagnostic listing the requested classes that each node lacks. Without it, "unknown class" and "doesn't fit" look the same.
  - Exit 0 when valid with at least one bundle. Exit 1 when invalid or when nothing fits.
- `topology show` is lower priority. `init` already walks the same model, so it can be deferred if time is short. Its value is giving an AI assistant (and humans) the raw facts.

**Tests:**
- goldens for `topology show`;
- `validate` on valid, malformed (parse error returned), unknown-class (missing-class diagnostic), and zero-fit inputs.

### 3.10 Agent-friendly CLI contract

Applies to every `bundles` and `topology` command. Each command's PR implements it, and the docs come last.

- **JSON everywhere:** `-o json` with stable field names, documented under "Output schema" in `docs/bundles.md`.
- **Structured errors:** with `-o json`, errors are printed as `{"errors":[{"source":"<ns>/<configmap>","message":"..."}]}`. The parser's errors are free text today, so there is no `field` path. Adding a typed `FieldError` to `parseDeviceGrouping` is possible future work.
- **Exit codes:**
  - 0: ok
  - 1: drift, invalid input, or zero fit
  - 2: usage or input error

  Document them.
- **Read-only:** no command writes to the cluster. Tests assert that the live-mode fake client records no create, update, or delete actions.
- **Docs:** "Using dratopoctl with an AI assistant" describes the loop `topology show` → `bundles init` → edit → `validate` / `fit` → a human applies the offering → `render` → commit → `check` in CI. The assistant never hand-writes templates.

### 3.11 Minimal kubectl plugin

- **`Makefile`:** copy (or symlink) the built binary to `$(BINARY_DIR)/kubectl-dratopo`.
- **`cmd/dratopoctl/main.go`:** keep `root.Use` as the base binary name. When `filepath.Base(os.Args[0])` is `kubectl-dratopo`, change only the **usage display**, through a usage template or `Example`, so it reads `kubectl dratopo ...`.
  - Setting `Use` to `kubectl dratopo` would make cobra's `Name()` return `kubectl`. That would corrupt `CommandPath` and produce a completion script that collides with kubectl's own.
- **Completion:** shell completion for the plugin form is out of scope. kubectl needs a separate `kubectl_complete-dratopo` executable for that. `dratopoctl completion` keeps working for the standalone name.
- **Flags:** keep the current ones (`--kubeconfig`, `--context`, `-o`, and `-n` where defined).
- **Docs:** an install note: put `kubectl-dratopo` on `PATH` and check with `kubectl plugin list`.
- **Test:** usage output shows `kubectl dratopo` for the plugin name and `dratopoctl` otherwise. `Name()` stays `dratopoctl`.
- **Out of scope, with triggers:**
  - full kubectl flag parity through `k8s.io/cli-runtime`, once users need `--as`, `--token`, or `--server`;
  - plugin completion, plus a krew manifest and release artifacts, once the `bundles` command set is stable and there are external users.

### Order

1. 3.1
2. 3.2, with 3.3 developed in parallel
3. 3.4
4. 3.5
5. 3.6
6. 3.7, plus 3.8 and 3.9 (both need `Simulate` and `Fit`)
7. 3.11, which can ship anytime

3.10 is part of every command's PR, with its docs last. The exit-code and JSON-error plumbing lands with 3.4.

## Verification

```
make test lint                                   # existing webhook tests pass unchanged
go test ./internal/controller/ -run 'Simulate|Aggregate' -count=20
go test ./internal/webhook/ -run ExpandClaim
go test ./cmd/dratopoctl/ -run 'Bundles|Topology'   # -args -update regenerates goldens
D=cmd/dratopoctl/testdata/bundles/input
go run ./cmd/dratopoctl bundles render --from-dir $D > r.yaml
go run ./cmd/dratopoctl bundles check -f r.yaml --from-dir $D; echo $?   # 0
go run ./cmd/dratopoctl bundles fit --from-dir $D
go run ./cmd/dratopoctl topology show --from-dir $D -o json
go run ./cmd/dratopoctl bundles init demo --from-dir $D > offering.yaml
go run ./cmd/dratopoctl bundles validate -f offering.yaml --from-dir $D; echo $?   # 0
make build && PATH=$PWD/bin:$PATH kubectl dratopo --help
```

On a live cluster with the webhook installed, a rendered template must pass admission unchanged:

```
dratopoctl bundles render OFFERING | kubectl create --dry-run=server -o yaml -f - | diff - <(dratopoctl bundles render OFFERING)
```

A Pod that references the template must schedule with aligned devices.

## Risks and open decisions

- **Preferred alignments are frozen at render time.** They are caught only by `check`, or re-evaluated continuously by spec 005.
- **Flag mismatch.** `--from-dir` output differs from the cluster if the driver name, partition mode, or config namespace differ from the controller's configuration. Document this. A live-mode `check` compares against the real published classes.
- **VMIs lose automatic `hostDevices`** on rendered templates. `bundle-requests` is a hint only. See spec 003's open KubeVirt decision.
- **Limits:**
  - count above 1 can exceed the 32-request claim limit; `ExpandClaim` already returns that error;
  - long names plus `-xN` can exceed 63 characters and are rejected.
- **Binary size.** Simulation pulls the fake clientset into the CLI binary. This is accepted and marked as a shortcut. Keep `fake.NewSimpleClientset`. The pinned client-go already has the newer `NewClientset`, but its object tracker behaves differently; switching is separate work.
- **Stamped templates.** `bundles check` ignores templates labeled `stamped-by` (spec 005).
- **`bundles init` heuristics.** Majority composition can mislead on very mixed fleets. The stderr mismatch report makes that visible, and the admin edits the result.
