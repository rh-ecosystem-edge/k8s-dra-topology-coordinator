# NVIDIA dynamic MIG allocation pinned to NUMA 0.
# Run from the repository root with DEMO_HOST set to the H100 node.

Output demos/topology-coordinator-mig/demo.gif
Output demos/topology-coordinator-mig/demo.mp4

Set Shell "bash"
Set FontSize 14
Set Width 1400
Set Height 1000
Set Padding 10
Set Theme "Catppuccin Mocha"
Set TypingSpeed 35ms

Hide
Type "set -e"
Enter
Type "export DEMO=demos/topology-coordinator-mig"
Enter
Type "source $DEMO/remote-commands.sh"
Enter
Type "topoctl offerings explain nvidia-mig-numa-demo >/dev/null"
Enter
Type "topoctl offerings explain nvidia-mig-numa-demo-numa0 >/dev/null"
Enter
Type "kubectl delete pod demo-nvidia-mig-numa-pod -n default --ignore-not-found --wait=true --timeout=60s >/dev/null 2>&1"
Enter
Type "kubectl delete resourceclaim demo-nvidia-mig-numa -n default --ignore-not-found --wait=true --timeout=60s >/dev/null 2>&1"
Enter
Type "kubectl apply -f - < $DEMO/pod-example.yaml >/dev/null"
Enter
Type "kubectl wait --for=condition=Ready pod/demo-nvidia-mig-numa-pod -n default --timeout=120s >/dev/null"
Enter
Sleep 2s
Type "clear"
Enter
Sleep 0.5s
Show

# 1. Discover the stable profile offering and its NUMA-specific placement.
Type "topoctl offerings list | grep nvidia-mig"
Enter
Sleep 5s
Type "clear"
Enter
Sleep 0.5s
Type "topoctl offerings explain nvidia-mig-numa-demo"
Enter
Sleep 6s
Type "clear"
Enter
Sleep 0.5s
Type "topoctl offerings explain nvidia-mig-numa-demo-numa0"
Enter
Sleep 6s
Type "clear"
Enter
Sleep 0.5s

# 2. Show the user-side claim and Pod.
Type "cat $DEMO/pod-example.yaml"
Enter
Sleep 6s
Type "clear"
Enter
Sleep 0.5s

# 3. Verify the running Pod and its allocated MIG device.
Type "kubectl get pod demo-nvidia-mig-numa-pod -n default -o wide"
Enter
Sleep 5s
Type "clear"
Enter
Sleep 0.5s
Type "dra-inspect inspect claims -n default"
Enter
Sleep 7s
Type "clear"
Enter
Sleep 0.5s
Type "dra-inspect inspect topology --compact"
Enter
Sleep 6s
Type "clear"
Enter
Sleep 0.5s
Type "dra-inspect inspect topology | more"
Enter
Sleep 6s
Type " "
Sleep 6s
Type " "
Sleep 6s
Type "clear"
Enter
Sleep 0.5s
Type "dra-inspect check counters | more"
Enter
Sleep 6s
Type " "
Sleep 6s
Type "clear"
Enter
Sleep 0.5s
Type "dra-inspect check topology"
Enter
Sleep 7s
Type "clear"
Enter
Sleep 0.5s
Type "dra-inspect inspect slices | grep demo-nvidia-mig-numa"
Enter
Sleep 7s

# Cleanup only the resources created by this recording.
Hide
Type "kubectl delete pod demo-nvidia-mig-numa-pod -n default --ignore-not-found --wait=true --timeout=60s >/dev/null 2>&1"
Enter
Type "kubectl delete resourceclaim demo-nvidia-mig-numa -n default --ignore-not-found --wait=true --timeout=60s >/dev/null 2>&1"
Enter
Type "kubectl wait --for=delete resourceclaim/demo-nvidia-mig-numa -n default --timeout=60s >/dev/null 2>&1 || true"
Enter
Show
