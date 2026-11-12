# dratopoctl Offering Claim Preview Plan

> **For agentic workers:** Use the `superpowers:executing-plans` workflow. Steps use checkbox syntax.

**Goal:** Let users preview a curated offering by name while preserving manifest-based ResourceClaim and ResourceClaimTemplate previews.

**Architecture:** `dratopoctl claims expand` accepts either a manifest (`-f`) or an offering (`--offering NAME`). The offering path verifies the name against curated catalog entries, builds one ResourceClaim request whose count means copies of the whole offering bundle, and passes it through the same namespace resolution and server-side dry-run CREATE path as manifest input.

**Tech Stack:** Go, Cobra, Kubernetes `client-go`, fake client tests.

**Spec:** User request in this conversation; the manifest preview design was architect-reviewed previously.

## Global Constraints

- Server-side dry-run admission remains the source of expansion behavior.
- The preview must not persist ResourceClaims.
- Existing `-f` claim and template input behavior and catalog output formats remain unchanged.
- Offering shortcut accepts curated catalog entries (`kindOffering`) only.

## Review Focus

- Offering name that is missing, unmanaged, or not a curated offering must fail before CREATE.
- Count must be positive and must apply only to `--offering` mode; explicitly specifying `--count 1` with manifest input is an error.
- Count is the number of whole offering bundles; `Entry.Count` remains catalog information and does not limit the preview count.
- `-f` and `--offering` must be mutually exclusive; one input mode is required.
- Namespace resolution and JSON output must be shared with manifest previews.
- Dry-run CREATE must keep `DryRunAll` and return the webhook-mutated object.

---

### Task 1: Add offering-name mode

**Files:**
- Modify: `cmd/dratopoctl/claims.go`
- Test: `cmd/dratopoctl/claims_test.go`

**Interfaces:**
- Add `offeringPreviewClaim(name string, count int64) (*resourcev1.ResourceClaim, error)` to build a generated-name claim whose single `Exactly` request is named `offering`, uses `name` as `DeviceClassName`, and carries `count`.
- Add `runExpandClaim(ctx context.Context, client kubernetes.Interface, w io.Writer, claim *resourcev1.ResourceClaim, namespace, contextNamespace string) error`; both input modes use it for namespace selection, dry-run CREATE, and JSON output.

- [x] Add tests for offering request construction, count validation, namespace selection, and returned admitted request.
- [x] Run the focused tests and confirm the new offering-mode tests fail before implementation.
- [x] Remove the `-f` required annotation and add `--offering`, `--count` (default `1`), and input-mode validation. Require exactly one of explicitly supplied `-f` or `--offering`; reject count values below `1` and reject `--count` with manifest input when the flag was explicitly supplied, including `--count 1`.
- [x] Resolve `--offering` against `loadEntries` and require a matching `Entry.Kind == kindOffering` before issuing a claim CREATE.
- [x] Route the generated claim through `runExpandClaim`; retain the manifest path through the same helper.
- [x] Run `go test ./cmd/dratopoctl` and confirm all focused tests pass.

### Task 2: Document and verify the public command

**Files:**
- Modify: `README.md`
- Test: `cmd/dratopoctl/claims_test.go`

- [x] Document `dratopoctl claims expand --offering NAME [--count N]`, default count, that N means whole offering bundles, accepted curated offerings, and the `list deviceclasses` plus namespaced `create resourceclaims` permissions needed for the offering preview.
- [x] Add command tests for missing input, simultaneous `-f` and `--offering`, count with manifest input, unknown/non-offering names, and help text.
- [x] Run `go test ./...` and `git diff --check`.
- [x] Build `dratopoctl` and run live server-side dry-runs for `nvidia-mig-1g-10gb` at count 1 and above the webhook request limit; verify expansion and rejection respectively, and confirm no claim was persisted.
