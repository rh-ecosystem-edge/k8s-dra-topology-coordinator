# Topology coordinator + `dra-inspect` recording

![Demo](demo.gif)

This is a recording-only, user-facing demo of the topology coordinator's
curated offering catalog and the resulting DRA allocation on the live AMD
MI300X cluster.

It uses the existing `mi300x-inference-numa` offering. The offering expands to
one AMD GPU plus 4 CPUs and 4 GiB of consumable memory on a single NUMA-local
CPU/memory device. `nodepartitionctl` explains the offering contract; `dra-inspect` shows
the published topology, device slices and attributes, claim allocation,
alignment, and consumable-capacity validation.

The tape applies `pod-example.yaml` offscreen, waits for the Pod to become
Ready, and then clears the terminal before the visible demo begins. The same
file is shown during the recording, so the displayed user configuration is
the configuration used for the allocation.
The visible workflow is intentionally user-oriented: discover the offering,
see the minimal Pod-side claim reference, inspect the running workload, and
inspect its placement and capacity.

## Recording

```bash
vhs demo.tape
```

The recording expects:

- Kubernetes access to the live cluster at `10.6.62.52`
- `dra-inspect` and `nodepartitionctl` on `PATH`
- the topology coordinator and capacity-backed `mi300x-inference-numa` offering already deployed
- the AMD GPU, CPU, memory, and network DRA drivers already running

The tape creates only `demo-mi300x-inference` and
`demo-mi300x-inference-pod`, then deletes those named resources during cleanup.
It does not recreate or delete offerings.

## Files

| File | Description |
|------|-------------|
| `demo.tape` | VHS recording script |
| `offering.yaml` | Captured live ConfigMap for the capacity-backed offering; not applied by the tape |
| `pod-example.yaml` | ResourceClaim-plus-Pod usage example applied during hidden setup and shown in the recording |
| `demo.gif` | Generated recording |
| `demo.mp4` | Generated recording |
