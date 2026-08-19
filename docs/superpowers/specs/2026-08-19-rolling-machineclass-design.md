# Rolling machineclass changes — design

Date: 2026-08-19
Repos: vitictl-kubevirt (core), vitictl-nhn (wrapper)
Status: approved in discussion; this document records the design.

## Problem

`vm changemachineclass` operates on one Machine. Two gaps surfaced in the
2026-08-19 live test on d-stackops-1010:

1. **Cluster-owned machines**: the talos-operator reconciles
   `Machine.spec.machineClass` from `KubernetesCluster.spec.topology`
   (`applyMachine`, machine_manager.go), so patching only the Machine gets
   reverted. The topology is the source of truth for these machines.
2. **Fleet changes**: changing a whole nodepool means sequencing restarts by
   hand — nothing prevents rebooting every worker at once, and nothing drains
   nodes first, so pods die abruptly instead of being rescheduled gracefully.

## Goals

- Target a whole nodepool or the control plane of a KubernetesCluster.
- The tool owns the sequencing: strictly one node at a time, each fully back
  before the next.
- Cordon + drain (PDB-respecting eviction) before each restart.
- Typing a cluster name opens an interactive picker over its targets
  (controlplane + nodepools), fzf-style like the existing pickers.
- Refuse the footgun: a per-machine class change on a cluster-owned Machine
  would be silently reverted by talos-operator — say so and point at the pool
  mode instead.

## Non-goals

- Parallel rolling, surge capacity, or replica-count changes.
- Talos upgrade orchestration or any node OS management.
- Proxmox/other providers (kubevirt only, as before).
- Automatic PDB overrides — a blocked drain aborts; it never force-kills.

## CLI surface (vitictl-kubevirt)

    viti kubevirt vm changemachineclass [name] [flags]

- `name` resolves as: Machine first (existing behavior); when no Machine
  matches but a KubernetesCluster named `name` exists in an AZ, the command
  targets that cluster. Empty `name` keeps today's machine picker.
- New flags, mutually exclusive with each other:
  - `--nodepool <pool>` — roll one nodepool of cluster `name`.
  - `--controlplane` — roll the control plane of cluster `name`.
  With either flag, `name` must be a KubernetesCluster. Cluster resolved but
  neither flag given: interactive picker over controlplane + nodepools
  (columns: TARGET, CLASS, REPLICAS); non-interactive → error listing them.
- `--drain-timeout` (default 5m) — per-node eviction budget.
- Existing flags keep their meaning. In pool mode: `--class` (or picker);
  `--no-restart` stages VM templates and stops before any cordon/restart;
  pool mode has exactly one confirmation (the up-front rollout prompt), and
  `--yes` or `--restart` both skip it — they are equivalent there, kept for
  symmetry with machine mode. `--cluster` remains refused.
- Machine mode + `--nodepool`/`--controlplane` on a name that is a Machine:
  error.

Single-machine hardening: a Machine with a KubernetesCluster ownerReference
asked to change to a **different** class is refused with a message naming the
cluster, the owning field (topology.controlplane / nodePool <name>), and the
pool-mode invocation to use. Same-class re-sync stays allowed (it repairs VM
drift and cannot be reverted into wrongness).

## Wrapper (vitictl-nhn)

`viti nhn kc edit machineclass` passes `--nodepool`, `--controlplane`, and
`--drain-timeout` through unchanged. Also fixes the doubled error output: the
wrapper no longer re-prints "viti kubevirt vm changemachineclass: exit status
1" as a second ❌ line when the child already printed its error — it exits
with the child's code silently (stderr already carried the real message).

## Architecture

New packages in vitictl-kubevirt:

### internal/guest

Builds a client for the workload (guest) cluster from the control plane:

- `FindClusterSecret(ctx, azClient, namespace, clusterID)` — Secret named
  `<clusterID>` in the cluster's namespace, falling back to label selector
  `vitistack.io/clusterid=<clusterID>` (mirrors vitictl's extract package).
- `Connect(secret)` — builds a kubernetes.Interface from the secret's
  `kube.config` key. clusterID comes from the member Machines' own
  `vitistack.io/clusterid` label.

### internal/roll

The orchestrator. Owns no I/O formatting; reports through a narrow
`Reporter` interface (step started / step done / warning) the cmd layer
implements on the command's streams.

Types:

- `Target` — cluster (namespace/name), kind (controlplane | nodepool), pool
  name, current class, replicas.
- `Member` — Machine + resolved VM + KubeVirt client (from the existing
  Discoverer).
- `Plan` — target, members, old/new class, computed Resources.

Phases (each a method, called by cmd in order):

1. `LoadTargets(ctx, az, cluster)` — read the KubernetesCluster, return its
   targets for the picker/flags. Field paths: lowercase
   `spec.topology.controlplane`, `spec.topology.workers.nodePools[]`.
2. `BuildPlan(...)` — enumerate member Machines: label
   `vitistack.io/clusterid` equal to the members' shared clusterID, plus
   annotation `vitistack.io/nodepool=<pool>` (nodepool) or label
   `vitistack.io/node-role=control-plane` (controlplane). Resolve every VM.
3. `Preflight(...)` — before any write: every VM resolves, VirtctlTarget
   succeeds (restart possible), guest client builds and reaches the API,
   every member node exists in the guest cluster (node name == machine
   name). Any failure aborts with nothing changed.
4. `PatchTopology(...)` — controlplane: merge patch. NodePool: JSON patch
   `[{op: test, path: .../nodePools/<i>/name, value: <pool>}, {op: replace,
   ...machineClass}]` — the test op guards against index drift.
5. `AwaitPropagation(...)` — poll member Machines until all show the new
   class (talos-operator's applyMachine does the work); timeout 2m.
6. `StageVMs(...)` — vm.PatchVMResources on every member (idempotent
   re-sync). `--no-restart` ends here.
7. `RollOne(...)` per member, strictly serial:
   cordon → drain → virtctl restart → wait VMI Running with new
   guestAtBoot → wait node Ready (poll tolerates guest API being down —
   required when rolling a control plane through its own node) → uncordon.

Failure semantics:

- Drain stall (timeout, PDB block): uncordon that node, abort the rollout,
  report the blocking pods verbatim from the drain library's error. Exit
  non-zero. Already-rolled nodes keep the new size; re-running the same
  command resumes idempotently (topology patch is a no-op, staging is a
  re-sync, already-rolled nodes drain fast and reboot once more — accepted
  cost for a simple resume story).
- Node not Ready within 10m after restart: abort the rollout. If the node
  object is reachable it is uncordoned (scheduling decisions belong to the
  cluster, not to a half-finished rollout); if it never rejoined, uncordoning
  is impossible and the report says exactly that — node name, last observed
  VMI phase, and that remaining members were not touched.
- Preflight failure: nothing was changed; plain error.

### Drain

`k8s.io/kubectl/pkg/drain` — the library kubectl itself uses. Settings:
IgnoreAllDaemonSets, DeleteEmptyDirData, Force=false (unmanaged pods block
the drain and abort the rollout — visible, not silent), timeout from
`--drain-timeout`. Wrapped behind a small interface in internal/roll so unit
tests stub it.

### Confirmation UX (cmd layer)

One confirmation for the whole rollout, shown after the plan is built:

    Roll nodepool workers1 of d-stackops-1010: 3 machines, class large → medium
    (4 cores × 1 sockets × 1 threads, 16Gi memory), one at a time with
    cordon+drain — continue?

Single-replica controlplane adds: "the cluster API will be unavailable while
its only control-plane node reboots". Progress lines per member as the roll
proceeds (cordoned / drained N pods / restarted / node Ready / uncordoned).

## Testing

- internal/guest: secret lookup by name and by label; kube.config parsing;
  clear error when the key is absent.
- internal/roll: fake-client tests for LoadTargets (field casing!), member
  selection, topology patch shapes (test-op guard), propagation wait
  (success, timeout), and RollOne ordering + abort-on-drain-failure with a
  stubbed drainer and virtctl runner (assert cordon happened before drain,
  uncordon on abort, nothing after the failure).
- cmd: flag exclusivity, machine-vs-cluster resolution, target picker rows,
  refusal message for owned machines, non-interactive errors.
- vitictl-nhn: flag passthrough, single-error output.
- Live validation: roll d-stackops-1010 workers1 large → medium with the new
  mode (the user's requested downscale test).

## Open items deliberately deferred

- `--skip-drain` escape hatch: not included until a real need appears.
- Parallelism (`--max-unavailable`): explicitly out of scope.
- Nodepool support in other provider operators (proxmox): out of scope.
