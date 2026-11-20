# Controller Bundle Stamping

**Status:** Proposed. Gated on demand. Build this only if the webhook metrics from spec 003 and user interviews show that namespaces want ready-made templates without a CLI or GitOps step.

**Scope:** An optional, off-by-default feature. It renders curated offerings as ResourceClaimTemplates and keeps them current in namespaces that opt in.

**Related specs:**
- [002: Intent request and catalog options](002-intent-request-and-catalog-options.md) (Option A)
- [003: Webhook feature freeze](003-webhook-feature-freeze.md)
- [004: `dratopoctl bundles`](004-dratopoctl-bundles.md) (this spec reuses its renderer)
- [006: KEP draft](006-kep-extended-resource-device-bundles.md)

## Context

Spec 004 makes bundles usable without the webhook. Admins render offerings into ResourceClaimTemplates with `dratopoctl bundles render`, commit them, and guard them in CI with `bundles check`. That works well on homogeneous clusters whose topology rarely changes.

Heterogeneous or fast-changing clusters are different. New hardware generations, firmware that changes attributes, and failed devices make hand re-rendering error-prone, and some teams have no GitOps flow at all. For them, the coordinator can keep the templates current itself. A Pod then needs only:

```yaml
spec:
  resourceClaims:
  - name: acc
    resourceClaimTemplateName: gpu-local-bundle
  containers:
  - name: app
    resources:
      claims:
      - name: acc
```

kube-controller-manager creates each Pod's claim from the template. The template is already expanded, so nothing happens at admission.

## Decisions

- **Off by default** (`--stamp-bundles=false`). Admins enable it explicitly.
- **Namespaces opt in** through a label selector. The default is `nodepartition.dra.k8s.io/bundles=enabled`. When stamping is on, startup rejects an empty or unparsable selector, because `labels.Parse("")` matches every namespace, including `kube-system`.
- **Same renderer as the CLI** (`internal/bundles.Render`). Stamped templates are byte-identical to `dratopoctl bundles render` output for the same inputs.
- **Count is 1.** Each curated offering gets one template, named after the offering. Users who need more use the CLI's `--count`.
- **Delete and recreate, never update.** ResourceClaimTemplate `spec` is immutable: the API server rejects spec changes (`ValidateResourceClaimTemplateUpdate`, kubernetes `pkg/apis/resource/validation/validation.go:846`). When the desired spec differs, the stamper deletes the old template and creates the new one. Claims that already exist are unaffected. A Pod created in the short gap fails and is retried by its workload controller, which must be documented.
- **Separate package, no import cycle.** `internal/webhook` imports `internal/controller` (`webhook.go:28`), and `internal/bundles` imports `internal/webhook`. The stamper therefore lives in a new package, `internal/stamper`. The controller only exposes a hook that runs after reconcile. `cmd/nodepartition-controller/main.go` wires the two together.
- **Ownership needs two markers.** The stamper deletes or recreates a template only if it has both the label `nodepartition.dra.k8s.io/stamped-by=<driver>` and the annotation `nodepartition.dra.k8s.io/bundle-offering`. Copied manifests that carry only the label are left alone. Including the driver in the label value keeps two coordinators with different drivers apart.
- **Failures in one object don't stop the rest.** Each failed object is logged (rate-limited) and counted in a metric. Reconcile returns an error only for infrastructure failures such as a failed List. No Kubernetes Events are emitted: the controller has no event recorder and no `events` RBAC.
- **Leader only.** The hook runs only on the leader and only after a reconcile succeeds, the same as DeviceClass publishing.

## Current code (verified at `ba64fff`)

**Controller** (`internal/controller/controller.go`):
- `reconcile` is at `:336`.
- The worker applies the leader gate (`:299-313`). Errors are retried with `AddRateLimited` (`:315-318`).
- The success path ends around `:488`.
- `StopPublishing` waits on `reconcileWG` (`:271`).
- Groupings are skipped in partitions mode (`:346`).
- The default resync period is 5 minutes (`:25`).
- `StartCache` (`:124-191`) builds the shared informer factory and waits for sync (`:188`). It has no namespace informer.

**Publishing** (`internal/controller/deviceclass_manager.go`):
- `publishDeviceClass` (`:2204`) never takes over objects it doesn't own.

**Webhook** (`internal/webhook/webhook.go`):
- It imports `internal/controller` (`:28`).
- The expander reads classes with a live Get (`:761`).

**Wiring and deployment:**
- Wiring is in `cmd/nodepartition-controller/main.go`. The expander is built at `:226`.
- RBAC is in `deploy/helm/nodepartition/templates/controller-clusterrole.yaml`. It already grants `resourceclaimtemplates: get` (`:17-19`).

## Implementation steps

### Step 1: Controller APIs

Add three methods to `Controller`. `main.go` must call all of them before `go ctrl.RunWorker` (`main.go:152`), so the hook field needs no synchronization:

- `SetNamespaceWatch(selector labels.Selector)`: tells `StartCache` to build the namespace informer (Step 3). Call it before `StartCache`.
- `NamespaceLister() corelisters.NamespaceLister`: returns the lister from that informer. Valid after `StartCache`.
- `SetPostReconcile(func(ctx context.Context) error)`: the hook.

Call the hook on the success path at the end of `reconcile`, after both DeviceClass syncs have succeeded and after `ReconciliationDuration` is observed (`controller.go:479`), so stamping time does not skew that metric. A failed sync skips stamping and cleanup. A hook error is retried through `AddRateLimited`. If the error is `context.Canceled` (leadership lost), log nothing and return nil: `runWorker` would drop the item anyway (`controller.go:299-303`).

Wiring order in `main.go`: `SetNamespaceWatch` → `StartCache` → build the stamper with `clientset`, `ctrl.Model()` and `ctrl.NamespaceLister()` → `SetPostReconcile` → `RunWorker`.

### Step 2: The stamper (`internal/stamper/stamper.go`)

Construct the stamper once with the client, the driver name, the namespace selector, and `webhook.NewClaimExpanderWithDriver(client, driver, model)`. The model is a pointer, so the expander always sees current topology.

Each run does the following:

1. **Skip in partitions mode.** Curated offerings exist only as grouping aggregates. Log this once.
2. **Gather inputs. If any of these fails, return an error and do nothing else:**
   - List the managed curated aggregate DeviceClasses (`managed=true`, `aggregate=true`, `offering=true`) through the API.
   - Read the opted-in namespaces from the namespace informer store. Skip namespaces that are not `Active`.
   - Make one cluster-wide List of ResourceClaimTemplates with the `stamped-by=<driver>` label.
3. **Render each offering once** with `bundles.Render` and `Count: 1`, then copy the result per opted-in namespace, setting only `metadata.namespace`. The namespace does not affect the spec, and the expander does a live DeviceClass Get per call (`webhook.go:761`), so rendering per pair would cost N×M×k Gets.
   - A Render error marks that offering **unresolved**. Skip create, recreate and delete for it, count it under `reason="render"`, and block cleanup for the whole run.
   - A name longer than 63 characters is also unresolved for that offering.
4. **Diff and apply.** Compare the desired templates with the listed ones using `apiequality.Semantic.DeepEqual` on `spec` and the stamper's annotations.
   - **Missing:** create it. If Create returns `AlreadyExists`, a template with that name exists without the `stamped-by` label (it was not in the list). Treat it as a collision: do not delete it, log rate-limited, and count it under `reason="collision"`.
   - **Present, owned (both markers), and different:** delete it, then create it.
     - Delete with `DeleteOptions{Preconditions: {UID: &listed.UID}}`. A former leader whose Delete is still in flight then cannot remove an object the new leader just created. Treat `NotFound` and `Conflict` as skip.
     - Run the Delete and Create pair on a short detached timeout context, never the cancellable reconcile context, so leadership loss cannot cut it in half.
     - If the Create after a successful Delete fails, return an error from the hook so `AddRateLimited` retries. Otherwise the template stays missing until the next resync (up to 5 minutes).
   - **Present with the label but without the annotation:** not owned. Leave it, and treat it as a collision.
   - **Create forbidden because the namespace is terminating:** detect with `apierrors.HasStatusCause(err, corev1.NamespaceTerminatingCause)` and skip. The `Active` filter alone is racy because the store lags.
   - Check `ctx.Err()` between objects, so losing leadership doesn't wait for N×M API calls.
5. **Clean up.** Delete an owned template only when one of these holds:
   - its offering DeviceClass no longer exists, or no longer has `offering=true`;
   - its namespace no longer matches the selector.

   The keep set comes from the curated DeviceClasses listed in step 2, **not** from rendered output. A transient topology change that makes Render fail or drop an offering therefore never deletes templates. Cleanup runs only if step 2 succeeded completely, no offering is unresolved, and no per-object errors occurred. Deletes use the same UID precondition. This covers namespace opt-out, offerings that were removed or are no longer curated, and renames.

Metrics:
- `nodepartition_stamped_templates` (gauge)
- `nodepartition_stamp_errors_total{reason}`, where `reason` is `render`, `collision`, `create`, `delete` or `terminating`

Stamped template metadata:
- label `nodepartition.dra.k8s.io/stamped-by=<driver>`, where `<driver>` is the `--driver` flag value. This is not the `CoordinatorDriverName` constant used by the managed DeviceClass labels (`deviceclass_manager.go:35-45`).
- annotations `bundle-offering`, `bundle-count`, `bundle-requests` (from spec 004)

### Step 3: Namespace trigger

Only when stamping is on:
- Add `factory.Core().V1().Namespaces().Informer()` to the shared factory in `StartCache`, and include its `HasSynced` in `WaitForCacheSync`.
- `AddFunc` and `DeleteFunc` enqueue with `AddAfter("reconcile", reconcileDebounceDelay)`.
- `UpdateFunc` enqueues only when the namespace labels changed. Otherwise every resync would trigger a reconcile for every namespace.

ResourceSlice and ConfigMap events already re-run reconcile. The periodic resync repairs manual edits. No template informer is needed.

### Step 4: Flags, Helm, RBAC

**Flags:**
- `--stamp-bundles` (default `false`)
- `--stamp-namespace-selector` (default `nodepartition.dra.k8s.io/bundles=enabled`; rejected when empty or invalid if stamping is on)

**Helm values:**

  ```yaml
  bundleStamping:
    enabled: false
    namespaceSelector: nodepartition.dra.k8s.io/bundles=enabled
  ```

Both values are wired into the controller Deployment args.

**RBAC.** Keep the existing `resourceclaimtemplates: get` rule. Add, only under `{{- if .Values.bundleStamping.enabled }}`:
- `resource.k8s.io` `resourceclaimtemplates`: `list`, `create`, `delete`. There is no `update` (the spec is immutable) and no `watch` (no informer).
- core `namespaces`: `get`, `list`, `watch`

### Step 5: Documentation

Add a "Stamped bundles" section to `docs/bundles.md` (spec 004) that covers:
- enabling stamping and opting a namespace in;
- what a Pod writes;
- that opting out deletes the namespace's templates and breaks Pods created later that reference them;
- that recreation leaves a short gap;
- that namespaces with stamped templates and namespaces with GitOps-committed templates must be separate sets. A later GitOps apply over a stamped template fails on the immutable spec.

`bundles check` (spec 004) ignores templates that carry `stamped-by`.

## Tests

**Fake-client unit tests** (`internal/stamper`):
- Opting a namespace in creates one template per curated offering.
- Opting out deletes them.
- Removing an offering, or setting `offering: "false"`, deletes it everywhere.
- A changed topology deletes and recreates. Assert that the actions are delete (with a UID precondition) then create, with no update.
- A template with the same name that has only the label, or no markers at all, is never deleted or recreated. An unlabeled one surfaces as `AlreadyExists` and is counted as a collision.
- A failed List or an empty desired set causes no deletes.
- A Render error for one offering causes no deletes anywhere, including for that offering's existing templates.
- A Create that fails after a successful Delete makes the hook return an error.
- Output for each namespace is byte-identical to `bundles.Render` called with that namespace.
- Partitions mode makes no writes.
- A terminating namespace is skipped.
- A name longer than 63 characters is skipped and reported.
- A second run with no changes makes no writes.
- A non-leader makes no writes.
- A cancelled context stops the loop.

**Controller tests:**
- The hook runs only after a successful reconcile.
- A namespace update without a label change doesn't enqueue.

**Envtest** (`internal/controller/controller_integration_test.go`):
- Enable stamping and label a namespace; templates appear.
- **Change the topology; the template is recreated, not updated.** This catches the immutability rule.
- Remove the label; the templates are deleted.

**Helm:** `helm template` with stamping on and off. The extra rules and flags appear only when it is on, and `get` is always present.

**Startup:** with stamping on, an empty or invalid selector fails startup.

## Verification

```
make test lint
go test ./internal/stamper/ ./internal/controller/ -run 'Stamp|PostReconcile|Namespace'
helm template deploy/helm/nodepartition --set bundleStamping.enabled=true | grep -B2 -A8 resourceclaimtemplates
helm template deploy/helm/nodepartition | grep -B2 -A4 resourceclaimtemplates   # get only
```

On a live cluster:
1. Enable stamping and run `kubectl label ns demo nodepartition.dra.k8s.io/bundles=enabled`.
2. Run `kubectl get resourceclaimtemplates -n demo` and confirm every curated offering is listed.
3. Diff a template against `dratopoctl bundles render OFFERING -n demo`, ignoring the stamper label. They must be identical.
4. A Pod that references the template schedules with aligned devices.
5. Change an offering. The template's UID changes, which shows it was recreated, and existing Pods keep running.
6. Remove the label. The templates are deleted.

## Risks and open decisions

- **Opting out and recreation are disruptive** to Pods created during the gap or afterwards. Existing claims are unaffected.
- **Flapping topology churns templates.** A device flap that is later reverted causes delete and recreate twice. The debounce is only 2s (`controller.go:26`). If this shows up in practice, add an in-memory minimum recreate interval per template (for example, 1 minute).
- **Object count grows** as offerings × opted-in namespaces. The selector is the control.
- **Preferred alignments are re-evaluated on every run**, unlike a committed CLI render. A stamped template can therefore change when topology changes. That is intended.
- **Write RBAC on templates is cluster-wide.** It is off by default and gated behind the same Helm value.
- **Open: counts per offering.** If users need `-x2` variants in-cluster, the offering could declare which counts it allows. Deferred until someone asks.
