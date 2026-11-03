# Topology coordinator administration and DRA debugging

![Demo](demo.gif)

This is a recording-only administration and debugging demo for the topology
coordinator on the live AMD MI300X cluster. It does not create workloads or
modify the cluster.

The recording covers both configuration layers:

- the installed Helm/controller settings, including partition mode and the
  trusted ConfigMap namespace; and
- the device-grouping ConfigMaps that produce the MI300X offering catalog.

It then follows the generated state through `nodepartitionctl`, managed
DeviceClasses, and `dra-inspect` topology, slice, attribute, and capacity
checks. The checked-in `configmaps.yaml` is a normalized snapshot of the live
grouping configuration and is displayed for reference; the tape does not
apply it.

## Recording

```bash
vhs demo.tape
```

The recording expects:

- Kubernetes access to the live cluster at `10.6.62.52`;
- Helm, `kubectl`, `dra-inspect`, and `nodepartitionctl` on `PATH`;
- the topology coordinator and AMD, CPU, memory, and network DRA drivers
  already running; and
- the MI300X grouping ConfigMaps and generated catalog already present.

## Files

| File | Description |
|------|-------------|
| `demo.tape` | VHS recording script |
| `configmaps.yaml` | Normalized snapshot of the live grouping configuration; not applied |
| `demo.gif` | Generated recording |
| `demo.mp4` | Generated recording |
