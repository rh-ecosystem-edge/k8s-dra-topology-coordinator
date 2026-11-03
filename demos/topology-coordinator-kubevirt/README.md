# Topology coordinator + KubeVirt passthrough recording

![Demo](demo.gif)

This is a recording-only, user-facing demo of a KubeVirt VMI consuming the
existing `mi300x-rdma-rail` passthrough offering on the live AMD MI300X
cluster.

The user-facing YAML contains a `ResourceClaimTemplate` reference and a
normal `1Gi` VMI memory request. It does not request DRA CPU or memory devices
and it does not manually define GPU or NIC host devices. The topology
coordinator expands the passthrough offering during VMI admission; the demo
then shows the generated host-device mappings, DRA allocation, topology,
alignment, counters, and host-device binding.

The recording uses a KubeVirt development build because the cluster runs
Kubernetes 1.37.1. The official installation and DRA references are:

- <https://kubevirt.io/user-guide/cluster_admin/installation/>
- <https://kubevirt.io/user-guide/compute/dra_gpu/>
- <https://github.com/kubevirt/kubevirt/blob/main/docs/kubernetes-compatibility.md>

For this rehearsal, pin the daily build to `20261103` and record the resolved
commit in the session notes:

```bash
KUBEVIRT_BUILD=20261103
kubectl apply -f "https://storage.googleapis.com/kubevirt-prow/devel/nightly/release/kubevirt/kubevirt/${KUBEVIRT_BUILD}/kubevirt-operator.yaml"
kubectl apply -f "https://storage.googleapis.com/kubevirt-prow/devel/nightly/release/kubevirt/kubevirt/${KUBEVIRT_BUILD}/kubevirt-cr.yaml"
kubectl wait --for=condition=Available kubevirt/kubevirt --timeout=300s
```

Verify that `HostDevices`, `HostDevicesWithDRA`, and `GPUsWithDRA` are active.
Preserve any existing feature gates when adding missing gates.
`NetworkDevicesWithDRA` is not needed: this demo uses the coordinator-generated
generic host-device entries for the SR-IOV VF.

## Host VF preparation

The node must use one consistent driver mode for the complete GIM VF set:
each MI300X PF remains bound to `gim`, and each `1002:74b5` VF is bound to
`vfio-pci`. This demo does not blacklist `amdgpu`; the included systemd unit
binds the complete VF set after GIM creates it and before kubelet starts.

Install the included host configuration as root on the demo node:

```bash
sudo install -o root -g root -m 0755 host-vfio-vfs-bind.sh /usr/local/sbin/amd-gim-vfio-vfs
sudo install -o root -g root -m 0644 host-vfio-vfs.service /etc/systemd/system/amd-gim-vfio-vfs.service
sudo systemctl daemon-reload
sudo systemctl enable --now amd-gim-vfio-vfs.service
```

Confirm every GPU VF is on `vfio-pci` and every parent PF is on `gim` before
starting the rehearsal. The current AMD DRA image must also publish KEP-5304
VFIO metadata (`resource.kubernetes.io/pciBusID`) for KubeVirt to construct the
PCI host device.

## Rehearsal

Run this manually on `jhull@10.6.62.52` before generating media:

```bash
export PATH=$HOME/.local/bin:$PATH
export DEMO=$HOME/devel/k8s-dra-topology-coordinator/demos/topology-coordinator-kubevirt
kubectl delete vmi demo-mi300x-rdma-rail -n default --ignore-not-found --wait=true --timeout=180s
kubectl get resourceclaims -n default -o name | grep '^resourceclaim/demo-mi300x-rdma-rail-' | xargs -r kubectl delete -n default --wait=true --timeout=60s
kubectl delete resourceclaimtemplate demo-mi300x-rdma-rail -n default --ignore-not-found --wait=true --timeout=60s
kubectl apply -f $DEMO/resourceclaim-template.yaml
kubectl apply -f $DEMO/vmi-example.yaml
kubectl wait --for=jsonpath='{.status.phase}'=Running vmi/demo-mi300x-rdma-rail -n default --timeout=180s
kubectl wait --for=condition=Ready vmi/demo-mi300x-rdma-rail -n default --timeout=180s
```

Then run the commands in `demo.tape` interactively and verify every output.
In particular, confirm that the admitted VMI contains two generated
host-device requests, alignment passes at `pcieRoot`, counters are readable,
and `check vfio` reports `vfio-pci` for the allocated GPU and NIC.

After rehearsal, delete the VMI and template, wait for the generated claim to
disappear, and verify the expected host state: the Mellanox VF returns to
`mlx5_core`, while AMD GPU VFs remain on `vfio-pci` under the persistent GIM
configuration.

## Recording

```bash
vhs demo.tape
```

The tape clears after every visible command, uses `more` for the attributes
view, shows counters but not capacity, and does not show CLI versions,
DeviceClasses, `-o table`, or `offerings list --all`.

The tape creates only the named demo VMI, ResourceClaimTemplate, and generated
claim. It does not recreate or delete the offering, KubeVirt installation, or
DRA drivers.

`dra-inspect v0.1.2` cannot resolve a KubeVirt VMI that references a
`ResourceClaimTemplate` in `check kubevirt`; it reports that result as
unavailable. The recording therefore shows the admitted VMI host-device
mapping directly, then verifies allocation with `inspect claims`, alignment
with `check alignment`, shared capacity with `check counters`, and host binding
with `check vfio`.

## Files

| File | Description |
|------|-------------|
| `demo.tape` | VHS recording script |
| `resourceclaim-template.yaml` | User-side DRA claim template |
| `vmi-example.yaml` | User-side KubeVirt VMI |
| `demo.gif` | Generated recording |
| `demo.mp4` | Generated recording |
| `host-vfio-vfs-bind.sh` | Persistent all-VF VFIO binding script |
| `host-vfio-vfs.service` | Boot ordering for the VFIO binding script |
