# Changelog

Notable changes. Format follows [Keep a Changelog](https://keepachangelog.com/).

## [Unreleased]

### Added
- `--health-port` controller flag (default `8081`); the Helm chart passes `controller.healthPort` to it.
- `docs/configuration.md`: reference for topology-rule and device-grouping ConfigMaps.
- Controller flag, DeviceClass label/annotation, and `dratopoctl` global-flag tables in the README.

### Changed
- Renamed the `nodepartitionctl` CLI to `dratopoctl` (binary, `cmd/` path, release assets, docs, demos).
- The chart now passes `--health-port`; upgrade the chart and controller image together (v0.2.0 images reject the flag).
- The controller version reported on the health endpoint is injected at build time (`-X main.version`) by `make build`, the Dockerfile `VERSION` build arg, and the release workflow. Unstamped builds report `dev`.
- `examples/intent-workload-gpu-inference.yaml` sets `offering: "true"`.
- `docs/specs/002-intent-request-and-catalog-options.md` marks Option A as implemented.

### Fixed
- `controller.healthPort` was templated into the container port and NetworkPolicy but ignored by the controller, which always listened on 8081.
- Broken relative links to `k8s-dra-harness` in `docs/harness-test-plan.md` and `test/e2e/harness/README.md`.
- Removed a personal host address from the demo READMEs.

## [0.2.0]

### Added
- Explicit `nodepartition.dra.k8s.io/offering` label and `offering` grouping key; managed offering names are completed.
- `nodepartitionctl offerings list|get|explain`, shell completion, and bounded variant-name lookup.
- `nodepartitionctl` release archives for Linux `amd64` and `arm64`.
- Topology coordinator demos.

### Fixed
- `nodepartitionctl` shows partition topology placement and preserves offering kind in variant explanations.

### Earlier hardening included in this release line
- Versioned `PartitionConfig` with strict decoding of unknown fields.
- Webhook and controller hardening, startup RBAC fixes, and grouping alignment contract fixes.
