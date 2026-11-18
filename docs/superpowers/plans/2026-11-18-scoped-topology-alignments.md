# Scoped topology alignments implementation plan

Goal: publish one stable offering whose named GPU/NIC entries share a PCIe root and whose GPU/CPU/memory entries share NUMA. Kubernetes chooses placement. Spec: approved conversation and architect review, November 18, 2026.

## Contract and constraints

- Add ConfigMap data.alignments YAML list of {attribute, requests}; requests reference uniquely named devices entries. Required constraints only. Initially support resource.kubernetes.io/pcieRoot (string) and resource.kubernetes.io/numaNode (int), actually published by participating drivers.
- New format excludes nonempty legacy alignment/fallback/matchAttribute and rail:true. Legacy configurations unchanged. Keep unique topology/request classes; repeated-class MIG uses existing format. Strict unknown-field, empty-list, names, references, duplicate-reference and attribute validation. Singleton constraints remain meaningful and must be emitted.
- Example alignments: pcieRoot requests [gpu,nic]; numaNode requests [gpu,cpu,memory]. CPU/memory keep existing consumable capacity fields. No driver-specific-name translation, placement variants, dependencies, CRDs or scheduler changes.
- Use PartitionConfig v1alpha3 and existing scope:group to cover a named subset, all its counted/split devices, per offering copy. Strict named-only v3 references; preserve v1/v2 semantics. Older admission endpoints must reject v3.

## Tasks

1. [ ] Parse/store scoped alignments, deep-copy and canonicalize throughout grouping instances, aggregate state and request-contract comparison. Preserve original typed published values for the two standard attributes separately from normalized/mapped topology fields, including deep-copy support.
2. [ ] Add deterministic joint per-node matching. Keep ALL class/profile alternatives before representative selection or greedy packing; backtrack with incremental equality and unique-device checks. Track EVERY shared-counter reference using (driver,pool,set,counter), use published capacities, fail closed on missing limits, and carry committed usage between bundles. Repeatedly commit complete disjoint bundles; do not promise maximum packing or current capacity availability. Scheduler remains authoritative for selectors, overrides and consumable capacities.
3. [ ] Publish only aggregate scoped offerings, alignment=mixed, with exact explicit required constraints, no implicit topology constraints or locality selectors. Include new mode in collision/cleanup/active-name logic. Permit legacy/rail conversion; remove old variants ONLY after replacement aggregate publication succeeds. Preserve stable offering name.
4. [ ] Admit/explain v3. Expand selected named subsets correctly for counts, passthrough splits and singleton scopes; separate each outer offering copy. Never drop required constraints for nil/stale topology caches or driver overrides. Derive v3 remapping membership from emitted constraints, deduplicating same-attribute membership. Bound generated plus caller constraints as well as requests. CLI shows mixed, scheduler-selected, named memberships and scope.
5. [ ] Add admin documentation and full example ConfigMap, common published-attribute requirements, preview instructions and rollout/rollback instructions. Upgrade ALL controller/webhook replicas before applying; remove/convert new ConfigMaps and reconcile before downgrading. Templates referencing removed variants must migrate to stable aggregate names.
6. [ ] Perform independent code review, fix material findings with regression tests, run final checks, commit, push feature branch to personal fork and open upstream PR.

## Required tests

- Parser positives/negatives, deep-copy isolation, canonical ordering and legacy regressions.
- Four-device match; NIC without NUMA; different roots/NUMA; absent and wrong-type attributes; conflicting native vs mapsTo values; greedy trap requiring previously discarded alternatives; profile and counter compatibility.
- Same counter names in different drivers/pools, secondary shared sets, missing limits, fractional counter arithmetic or explicitly correct quantity accounting.
- Aggregate-only deterministic reconciliation, update/deletion, legacy and rail conversion, failed replacement publication retaining old variants.
- Exact emitted scopes including singleton, count2 independent bundles, passthrough split membership, overrides, cache absence, external constraint remapping, limits, v1/v2/MIG regression.
- Synthetic slice->ConfigMap->DeviceClass->claim/CLI integration where feasible. Focused tests first, then make test and git diff --check. Live driver allocation only with disposable compatible cluster.

## Review amendments

Architect: approve with revisions. Incorporated unfiltered candidates, fully qualified published counter accounting, publication-success cleanup boundary, singleton emission, and confirmed v3 version skew protection. No implementation completed at plan creation.
