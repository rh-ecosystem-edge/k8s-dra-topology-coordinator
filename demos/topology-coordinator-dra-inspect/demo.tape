# Topology coordinator offering + dra-inspect recording demo
# Run against the live MI300X cluster with the offering already installed.

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
Type "export DEMO=$HOME/devel/k8s-dra-topology-coordinator/demos/topology-coordinator-dra-inspect"
Enter
Sleep 1s
Type "kubectl apply -f $DEMO/pod-example.yaml >/dev/null 2>&1"
Enter
Sleep 12s
Type "kubectl wait --for=condition=Ready pod/demo-mi300x-inference-pod -n default --timeout=120s >/dev/null 2>&1"
Enter
Sleep 2s
Type "clear"
Enter
Sleep 0.5s
Show

# 1. Show the offering catalog.
Type "nodepartitionctl offerings list"
Enter
Sleep 5s
Type "clear"
Enter
Sleep 0.5s
Type "nodepartitionctl offerings explain mi300x-inference-numa"
Enter
Sleep 6s
Type "clear"
Enter
Sleep 0.5s

# 2. Show the minimal workload-side usage.
Type "cat $DEMO/pod-example.yaml"
Enter
Sleep 6s
Type "clear"
Enter
Sleep 0.5s

# 3. Inspect the hardware and published DRA attributes.
Type "dra-inspect inspect topology --compact"
Enter
Sleep 8s
Type "clear"
Enter
Sleep 0.5s
Type "dra-inspect inspect slices"
Enter
Sleep 8s
Type "clear"
Enter
Sleep 0.5s
Type "dra-inspect inspect attributes | more"
Enter
Sleep 10s
Type " "
Sleep 8s
Type " "
Sleep 8s
Type " "
Sleep 8s
Type " "
Sleep 8s
Type "clear"
Enter
Sleep 0.5s

# 4. Inspect the running user workload and its allocation.
Type "clear"
Enter
Sleep 0.5s
Type "kubectl get pod demo-mi300x-inference-pod -n default -o wide"
Enter
Sleep 5s
Type "clear"
Enter
Sleep 0.5s
Type "dra-inspect inspect claims -n default"
Enter
Sleep 8s
Type "clear"
Enter
Sleep 0.5s
Type "dra-inspect check alignment demo-mi300x-inference -n default"
Enter
Sleep 8s
Type "clear"
Enter
Sleep 0.5s
Type "dra-inspect check capacity"
Enter
Sleep 8s
Type "clear"
Enter
Sleep 0.5s

# Cleanup only the resources created by this recording.
Hide
Type "kubectl delete pod demo-mi300x-inference-pod -n default --ignore-not-found >/dev/null 2>&1"
Enter
Sleep 2s
Type "kubectl delete resourceclaim demo-mi300x-inference -n default --ignore-not-found >/dev/null 2>&1"
Enter
Sleep 2s
Show
