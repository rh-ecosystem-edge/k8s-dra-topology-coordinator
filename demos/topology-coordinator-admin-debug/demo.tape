# Topology coordinator administration and DRA debugging recording
# Run against the live MI300X cluster with the coordinator already installed.

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
Type "export DEMO=$HOME/devel/k8s-dra-topology-coordinator/demos/topology-coordinator-admin-debug"
Enter
Sleep 1s
Type "clear"
Enter
Sleep 0.5s
Show

# 1. Inspect the release configuration and rendered controller settings.
Type "helm get values nodepartition -n nodepartition -a | grep -E '^(partitionMode:|controller:|  configNamespace:|  watchAllNamespaces:)'"
Enter
Sleep 5s
Type "clear"
Enter
Sleep 0.5s
Type "kubectl -n nodepartition get deployment nodepartition-controller -o yaml | grep -E 'partition-mode|config-namespace'"
Enter
Sleep 5s
Type "clear"
Enter
Sleep 0.5s

# 2. Inspect the configuration inputs that produce the catalog.
Type "kubectl get configmaps -n nodepartition -l nodepartition.dra.k8s.io/device-grouping=true -o custom-columns=NAME:.metadata.name,ALIGNMENT:.data.alignment,INTENT:.data.intentLevel"
Enter
Sleep 5s
Type "clear"
Enter
Sleep 0.5s
Type "cat $DEMO/configmaps.yaml | more"
Enter
Sleep 8s
Type " "
Sleep 7s
Type " "
Sleep 7s
Type " "
Sleep 7s
Type " "
Sleep 7s
Type "clear"
Enter
Sleep 0.5s
Type "kubectl get configmaps -n nodepartition -l nodepartition.dra.k8s.io/topology-rule=true"
Enter
Sleep 5s
Type "clear"
Enter
Sleep 0.5s

# 3. Inspect the generated offering catalog.
Type "nodepartitionctl offerings list --all | more"
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
Type " "
Sleep 8s
Type "clear"
Enter
Sleep 0.5s
Type "nodepartitionctl offerings get mi300x-inference-numa"
Enter
Sleep 6s
Type "clear"
Enter
Sleep 0.5s
Type "nodepartitionctl offerings explain mi300x-inference-numa"
Enter
Sleep 8s
Type "clear"
Enter
Sleep 0.5s

# 4. Inspect generated DeviceClasses and their embedded partition config.
Type "kubectl get deviceclasses -l nodepartition.dra.k8s.io/managed=true --show-labels | more"
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
Type " "
Sleep 8s
Type "clear"
Enter
Sleep 0.5s
Type "kubectl get deviceclass numa -o yaml | more"
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
Type " "
Sleep 8s
Type "clear"
Enter
Sleep 0.5s

# 5. Inspect DRA topology, slices, attributes, and capacity.
Type "dra-inspect check topology"
Enter
Sleep 8s
Type "clear"
Enter
Sleep 0.5s
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
Type " "
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
