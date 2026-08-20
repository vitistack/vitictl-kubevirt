# `viti kv vm orphans` — implementation report

## What was built

- `internal/vm/orphans.go` — `DetectOrphans`, the detection logic. Anchors discovery on
  each availability zone's Machines (same model as `Collect`), groups them by KubeVirt
  cluster via the existing unexported `groupByCluster`, resolves each group's cluster
  through the same `KubeVirtResolver`, then per cluster lists VMs and VMIs directly and
  derives:
  - `KindVMWithoutMachine` — a VM carrying `LabelSourceMachine` whose named Machine does
    not exist anywhere in the zone (checked zone-wide, not just within the VM's own
    KubevirtConfig group — a fabricated/deleted name has no annotation of its own to say
    which group it would belong to). VMs without the label are never inspected.
  - `KindMachineWithoutVM` — a Machine whose cluster answered but has no VM, derived from
    the VM listing already fetched (same double-keyed by-label/by-name match `indexKubeVirt`
    uses), not a second query.
  - `KindVMIWithoutVM` — a running VMI with no VirtualMachine object of the same
    namespace/name.
  - `Coverage` (zones/clusters configured vs. checked, `Complete()`) and a `Suppressed`
    count from the `--min-age` filter (default 15m, applied to all three kinds via each
    object's own `CreationTimestamp`; a zero timestamp — only possible in tests — is
    treated as old enough to report, not suppressed).
- `cmd/orphans.go` — `newOrphansCmd(s *scope)`, sibling of `newVMListCmd`, **not
  registered** (per instructions, the controller wires it in). Table columns:
  `KIND AZ CLUSTER NAMESPACE NAME DETAIL AGE`. `-o json/yaml` emit `{orphans, coverage,
  suppressedByMinAge}` so the payload stays honest about partial coverage past a pipe.
  Exit code: 0 whenever findings exist (candidates, not failures); non-zero only when
  `Coverage.Complete()` is false — matching `viti nn orphans` in vitictl exactly. Coverage
  is patched to use `len(zones)` (not `len(clients)`) after `ConnectVitistack`, mirroring
  a bug `nn orphans`'s own comments call out: zones that fail to even *connect* are
  invisible to `DetectOrphans` and would otherwise silently count as "configured". No
  destructive flags exist; command is read-only.
- `internal/vm/orphans_test.go` — 10 tests: each of the 3 kinds detected, unlabelled VM
  ignored, labelled VM with an existing Machine ignored, young finding suppressed and
  counted (old one still reported), incomplete coverage on a failed cluster and on a
  failed zone, the no-CRDs zone still counting as checked, and a clean-fleet sanity check.

## Verification (real output)

```
$ go -C vitictl-kubevirt build ./...
(clean)

$ go -C vitictl-kubevirt vet ./...
(clean)

$ go -C vitictl-kubevirt test ./... -count=1
ok  	.../vitictl-kubevirt/cmd            0.808s
ok  	.../vitictl-kubevirt/internal/config 0.251s
ok  	.../vitictl-kubevirt/internal/guest   1.824s
ok  	.../vitictl-kubevirt/internal/kube    0.662s
ok  	.../vitictl-kubevirt/internal/output  0.891s
ok  	.../vitictl-kubevirt/internal/picker  0.758s
ok  	.../vitictl-kubevirt/internal/release 1.237s
ok  	.../vitictl-kubevirt/internal/roll    2.344s
ok  	.../vitictl-kubevirt/internal/virtctl 1.041s
ok  	.../vitictl-kubevirt/internal/viticli 2.828s
ok  	.../vitictl-kubevirt/internal/vm      2.649s

$ gofmt -l cmd/orphans.go internal/vm/orphans.go internal/vm/orphans_test.go
(no output — clean)

$ make -C vitictl-kubevirt lint
16 issues, all `unused` — every one is `newOrphansCmd`/its helper types/funcs plus the
concurrently-added, likewise-unregistered `newMigrationsCmd` and its helpers. Expected:
the task explicitly forbids registering the command, and the sibling migrations command
(added by another agent this same branch) shows the identical pattern for the same
reason. No other lint category fired.
```

## Not done / known limitation

- Discovery of a KubeVirt cluster is anchored on a zone's Machines (matching `Collect`'s
  own model). A zone with *zero* Machines left in it — every one already deleted — has no
  path to name which cluster to inspect, so a fully-orphaned cluster with no surviving
  Machine anywhere in its zone goes unseen. Noted in the `DetectOrphans` doc comment; not
  fixable without changing the resolver's contract, which is out of scope here.
- Did not register the command anywhere, per instructions.

## Files touched

- `/Users/jonra82/Rotemappe/github/vitistack/vitictl-kubevirt/internal/vm/orphans.go`
- `/Users/jonra82/Rotemappe/github/vitistack/vitictl-kubevirt/internal/vm/orphans_test.go`
- `/Users/jonra82/Rotemappe/github/vitistack/vitictl-kubevirt/cmd/orphans.go`

## Update: local-cluster union (closes the zero-Machines gap)

The command is now merged and registered (`cmd/root.go`, `withScope`), and its first live
run found a real orphan (a Machine `Running` with an IP for 172 days, no VM/VMI on the
KubeVirt cluster — verified independently). This update closes the documented limitation:
a KubeVirt cluster whose last Machine has already been torn down had no zone left to
discover it through.

### What changed

- `DetectOrphans` now takes `localClusters []config.Cluster` and a `LocalConnector` (a
  `func(config.Cluster) (*kube.KubeVirtClient, error)`, abstracting `kube.ConnectKubeVirt`
  for testability) and unions them into the sweep. `cmd/orphans.go` loads
  `~/.vitistack/kubevirt.config.yaml` via `config.Load()` and passes `kube.ConnectKubeVirt`
  as the connector — skipped entirely when `--cluster` is given, since that flag already
  names one exact cluster and widening to every locally-configured one would silently
  defeat the narrowing.
- **Dedupe**: `clusterIdentity(cl config.Cluster)` keys on `Context` first (the same field
  `ConnectKubeVirtFromKubeconfig` already matches a discovered cluster's local counterpart
  on), then `Kubeconfig` path, then `Name` as the last resort for an env-defined cluster.
  A `visited` set built while walking zone-discovered clusters is checked before auditing
  any local one, so a cluster reachable both ways is queried exactly once.
- **Safety for `vm-without-machine` on unanchored clusters**: an unanchored cluster has no
  `g.machines`, so `machine-without-vm` never applies to it (an empty/nil slice is a no-op
  range, no special-casing needed). For `vm-without-machine`, "the named Machine does not
  exist" is checked against a `globalKnown` set unioned across every zone actually listed,
  gated by a `zonesFullyKnown` flag that goes false the moment any zone's Machine listing
  fails for a real reason (not the silent no-CRDs case). When false, the check is skipped
  for every unanchored cluster — never asserted as either orphan or clean — and the
  cluster counts as configured-but-not-checked; `vmi-without-vm` still runs and reports
  normally, since it needs no Machine knowledge at all.
- **Coverage**: local clusters fold into the same `ClustersConfigured`/`ClustersChecked`
  counters as discovered ones (single honest `N/M` figure). An unreachable local cluster
  warns via the same `warn` callback and continues, exactly like an unreachable discovered
  one, and counts against coverage.
- Updated `DetectOrphans`'s doc comment to describe the two-source discovery and the
  remaining, narrower gap: a cluster neither named by any Machine nor locally configured
  is still invisible (no third source of identity exists).

### New tests (`internal/vm/orphans_test.go`)

- `TestDetectOrphansAuditsUnanchoredLocalCluster` — zero zones, a local-only cluster's
  labelled-but-unowned VM is still reported (`AZ` empty).
- `TestDetectOrphansDedupesClusterReachableBothWays` — same cluster via zone discovery and
  local config yields exactly one finding and `ClustersConfigured/Checked == 1`, not 2.
- `TestDetectOrphansUnreachableLocalClusterWarnsAndCounts` — connect failure warns,
  continues, counts against coverage.
- `TestDetectOrphansSkipsUnanchoredJudgmentWhenAZoneIsIncomplete` (bonus) — directly proves
  the safety property: with one zone unreadable, an unanchored cluster's labelled VM is
  never asserted as `vm-without-machine`, and that cluster is not counted as checked.

All 14 tests in the package pass (10 prior + 4 new).

### Verification (real output)

```
$ go -C vitictl-kubevirt build ./...            (clean)
$ go -C vitictl-kubevirt vet ./...              (clean)
$ go -C vitictl-kubevirt test ./... -count=1
ok  	.../cmd            0.842s
ok  	.../internal/config 0.212s
ok  	.../internal/guest   0.783s
ok  	.../internal/kube    1.252s
ok  	.../internal/output  2.203s
ok  	.../internal/picker  2.077s
ok  	.../internal/release 1.669s
ok  	.../internal/roll    1.754s
ok  	.../internal/virtctl 0.173s
ok  	.../internal/viticli 2.483s
ok  	.../internal/vm      2.032s
$ gofmt -l cmd/orphans.go internal/vm/orphans.go internal/vm/orphans_test.go   (clean)
$ make -C vitictl-kubevirt lint
0 issues.
```

### Files touched (this update)

- `internal/vm/orphans.go`
- `internal/vm/orphans_test.go`
- `cmd/orphans.go`
