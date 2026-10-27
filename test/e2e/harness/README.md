# Harness bridge

`run-coordinator-e2e.sh` is a coordinator-side verification command for
[`k8s-dra-harness`](../../../../k8s-dra-harness). The harness owns the DRA
driver and workload namespace; this script owns a unique coordinator release,
the evidence directory, and any fixture manifests passed on the command line.

The bridge currently provides the first vertical slice:

- validates the harness contract without contacting a cluster;
- installs the coordinator with a non-default driver name by default;
- waits for a managed `DeviceClass` whose opaque config references that name;
- records cluster and coordinator diagnostics; and
- labels and deletes the run's cluster-scoped managed `DeviceClass` objects;
- uses a run-scoped leader-election lease; and
- cleans up the release, namespace, and supplied fixtures unless
  `--keep-resources` is set.

Run the contract test locally:

```sh
bash test/e2e/harness/run-coordinator-e2e_test.sh
```

Use it from a harness verification command by passing the evidence directory
explicitly:

```yaml
testPlan:
  scenarios: [resource-slices]
  verification:
    scriptsDir: /path/to/k8s-dra-topology-coordinator/test/e2e/harness
    evidenceDir: /tmp/dra-coordinator-evidence
    commands:
      - [run-coordinator-e2e.sh, --evidence-dir, /tmp/dra-coordinator-evidence]
```

The input manifest flags (`--rules`, `--groupings`, and `--resource-slices`)
are intentionally limited to run-owned fixtures. Do not pass shared production
ConfigMaps or ResourceSlices; the bridge deletes those manifests during
cleanup.

The default TLS mode expects cert-manager. On a test cluster without
cert-manager, pass `--tls-mode manual` plus `--tls-ca-bundle`, `--tls-cert`,
and `--tls-key`; the bridge creates the temporary webhook Secret in its
run-owned namespace.
