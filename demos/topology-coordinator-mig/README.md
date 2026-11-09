# Topology coordinator + NVIDIA MIG recording

![Demo](demo.gif)

This user-facing demo shows how a topology-specific offering selects one
dynamic NVIDIA `1g.10gb` MIG slice from NUMA node 0. It discovers the stable
aggregate offering, explains the generated NUMA-specific variant, creates a
small Pod claim, and inspects the resulting allocation.

The Pod uses `pause:3.9`; it demonstrates DRA allocation and placement, not
CUDA execution. The offering ConfigMap is installed before recording and is
left in place. The tape creates and removes only the named ResourceClaim and
Pod.

## Prerequisites

- The H100 node has the topology coordinator and NVIDIA DRA driver running.
- The base dynamic-MIG topology rules and offerings from the
  [NVIDIA MIG example](../../examples/nvidia-dynamic-mig-offerings.yaml) are
  installed.
- SSH key access to the node is configured, and `kubectl`, `topoctl`, and
  `dra-inspect` work on the node.
- Local `vhs` and `ssh` are installed.

Install the demo offering once from the repository root:

```bash
export DEMO_HOST=jhull@10.6.62.50
ssh -o BatchMode=yes "$DEMO_HOST" kubectl apply -f - \
  < demos/topology-coordinator-mig/offering.yaml
```

Confirm both the aggregate and the NUMA 0 variant are published:

```bash
ssh -o BatchMode=yes "$DEMO_HOST" topoctl offerings explain nvidia-mig-numa-demo
ssh -o BatchMode=yes "$DEMO_HOST" topoctl offerings explain nvidia-mig-numa-demo-numa0
```

## Recording

Run from the repository root. The tape expects `DEMO_HOST` to identify the
configured H100 node and forwards cluster commands over SSH.

```bash
export DEMO_HOST=jhull@10.6.62.50
vhs demos/topology-coordinator-mig/demo.tape
```

The recording shows the MIG profile and NUMA placement contract, the minimal
claim and Pod, the allocated claim, and the topology check. Its cleanup removes
only `demo-nvidia-mig-numa-pod` and `demo-nvidia-mig-numa`.

## Files

| File | Description |
|------|-------------|
| `demo.tape` | VHS recording script |
| `offering.yaml` | Demo-specific NUMA-aligned MIG offering; apply before recording |
| `pod-example.yaml` | ResourceClaim and Pod used by the demo |
| `remote-commands.sh` | Forwards cluster commands to `DEMO_HOST` over SSH |
| `demo.gif` | Generated recording |
| `demo.mp4` | Generated recording |
