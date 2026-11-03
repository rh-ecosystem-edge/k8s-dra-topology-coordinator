# Topology coordinator + KubeVirt passthrough recording demo
# Run on the live MI300X cluster after the rehearsal in README.md passes.

Output demo.gif
Output demo.mp4

Set Shell "bash"
Set FontSize 14
Set Width 1400
Set Height 1200
Set Padding 10
Set Theme "Catppuccin Mocha"
Set TypingSpeed 35ms

Hide
Type "export PATH=$HOME/.local/bin:$PATH"
Enter
Sleep 5s
Type "export DEMO=$HOME/devel/k8s-dra-topology-coordinator/demos/topology-coordinator-kubevirt"
Enter
Sleep 5s
Type "kubectl delete vmi demo-mi300x-rdma-rail -n default --ignore-not-found --wait=true --timeout=180s >/dev/null 2>&1"
Enter
Sleep 3s
Type "kubectl get resourceclaims -n default -o name | grep '^resourceclaim/demo-mi300x-rdma-rail-' | xargs -r kubectl delete -n default --wait=true --timeout=60s >/dev/null 2>&1"
Enter
Sleep 5s
Type "kubectl delete resourceclaimtemplate demo-mi300x-rdma-rail -n default --ignore-not-found --wait=true --timeout=60s >/dev/null 2>&1"
Enter
Sleep 3s
Type "kubectl apply -f $DEMO/resourceclaim-template.yaml >/dev/null 2>&1"
Enter
Sleep 3s
Type "kubectl apply -f $DEMO/vmi-example.yaml >/dev/null 2>&1"
Enter
Sleep 5s
Type "kubectl wait --for=jsonpath='{.status.phase}'=Running vmi/demo-mi300x-rdma-rail -n default --timeout=180s >/dev/null 2>&1"
Enter
Sleep 5s
Type "kubectl wait --for=condition=Ready vmi/demo-mi300x-rdma-rail -n default --timeout=180s >/dev/null 2>&1"
Enter
Sleep 5s
Type "clear"
Enter
Sleep 5s
Show

# 1. Discover the user-facing passthrough offering.
Type "nodepartitionctl offerings list"
Enter
Sleep 10s
Type "clear"
Enter
Sleep 5s
Type "nodepartitionctl offerings explain mi300x-rdma-rail"
Enter
Sleep 10s
Type "clear"
Enter
Sleep 5s

# 2. Show the minimal user-side ResourceClaimTemplate and VMI.
Type "cat $DEMO/resourceclaim-template.yaml"
Enter
Sleep 10s
Type "clear"
Enter
Sleep 5s
Type "cat $DEMO/vmi-example.yaml"
Enter
Sleep 10s
Type "clear"
Enter
Sleep 5s

# 3. Show the running VMI and the coordinator-generated host-device mapping.
Type "kubectl get vmi demo-mi300x-rdma-rail -n default -o wide"
Enter
Sleep 10s
Type "clear"
Enter
Sleep 5s
Type "kubectl get vmi demo-mi300x-rdma-rail -n default -o yaml | sed -n '/resourceClaims:/,/volumes:/p'"
Enter
Sleep 12s
Type "clear"
Enter
Sleep 5s

# 4. Inspect topology, slices, and published attributes.
Type "dra-inspect inspect topology --compact"
Enter
Sleep 12s
Type "clear"
Enter
Sleep 5s
Type "dra-inspect inspect slices"
Enter
Sleep 12s
Type "clear"
Enter
Sleep 5s
Type "dra-inspect inspect attributes | more"
Enter
Sleep 15s
Type " "
Sleep 10s
Type " "
Sleep 10s
Type " "
Sleep 10s
Type " "
Sleep 10s
Type "clear"
Enter
Sleep 5s

# 5. Inspect allocation, KubeVirt mapping, alignment, counters, and VFIO.
Type "dra-inspect inspect claims -n default"
Enter
Sleep 12s
Type "clear"
Enter
Sleep 5s
Type "dra-inspect check alignment -n default"
Enter
Sleep 12s
Type "clear"
Enter
Sleep 5s
Type "dra-inspect check counters"
Enter
Sleep 15s
Type "clear"
Enter
Sleep 5s
Type "dra-inspect check vfio"
Enter
Sleep 15s
Type "clear"
Enter
Sleep 5s

# Cleanup only the resources created by this recording.
Hide
Type "kubectl delete vmi demo-mi300x-rdma-rail -n default --ignore-not-found --wait=true --timeout=180s >/dev/null 2>&1"
Enter
Sleep 3s
Type "kubectl get resourceclaims -n default -o name | grep '^resourceclaim/demo-mi300x-rdma-rail-' | xargs -r kubectl delete -n default --wait=true --timeout=60s >/dev/null 2>&1"
Enter
Sleep 5s
Type "kubectl delete resourceclaimtemplate demo-mi300x-rdma-rail -n default --ignore-not-found --wait=true --timeout=60s >/dev/null 2>&1"
Enter
Sleep 3s
Show
