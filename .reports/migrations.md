# `viti kv migrations` — implementation report

## What was built

- `internal/vm/migrations.go`
  - `Migration` — joins a `kubevirtv1.VirtualMachineInstanceMigration` to AZ, KubeVirt cluster,
    and owning Machine name. Accessors: `Name`, `Namespace`, `VMIName`, `Phase`, `SourceNode`,
    `TargetNode`, `Mode`, `Active`. `VMIM` (the raw object) is a public field so JSON/YAML output
    and future fields reach the caller without new accessors.
  - `isMigrationFinished` — classifies by the real `kubevirtv1.MigrationSucceeded` /
    `MigrationFailed` phase constants; everything else (including the unset phase) is active.
  - `CollectMigrations(ctx, azClients, resolver, namespace, warn)` — mirrors `Collect`'s shape:
    groups each zone's Machines by `KubevirtConfig` (reusing `groupByCluster`), resolves one
    KubeVirt cluster per group, and **de-duplicates by resolved cluster name** so a cluster shared
    by several zones/groups is listed exactly once instead of once per group (migrations have no
    per-zone identity to naturally de-dupe by, unlike Machines in `Collect`). A zone's Machine
    listing failure or a cluster resolve/list failure goes to `warn` and is skipped, never fatal.
  - `listClusterMigrations` / `machineNamesByVMIName` — VMI → VM → Machine join via
    `LabelSourceMachine`, with the same name-fallback `indexKubeVirt` uses. An unresolvable owner
    yields an empty `Machine`, never a dropped row.

- `cmd/migrations.go`
  - `newMigrationsCmd(s *scope) *cobra.Command` — **not registered**, returned only, per
    instructions; the controller wires it in.
  - `Use: "migrations"`, alias `"mig"`, `-o` table/wide/json/yaml/name via `internal/output`.
  - Columns: `AZ CLUSTER NAMESPACE VMI MACHINE PHASE NODE AGE` (NODE = `source→target`); wide adds
    `NAME` (migration's own name) and `MODE` (PreCopy/PostCopy/Paused).
  - `--all` (default off) shows only active migrations; empty-result message differs
    ("no active migrations" vs "no migrations found").
  - `--watch` polls on a `time.Ticker` (default 2s, `--interval` to change), explicitly **not**
    the Kubernetes watch API — reasoning in a doc comment (migrations run minutes, span several
    clusters, a real watch means per-cluster reconnect/resourceVersion handling for no visible
    gain). It wires its own `signal.NotifyContext` (SIGINT/SIGTERM) since nothing upstream gives
    `cmd.Context()` cancellation, checks `ctx.Err()` before and after each collect to avoid
    printing a half-gathered frame, and returns `nil` (exit 0) on cancellation. Refused with an
    error when combined with `-o json|yaml|name`.

## Verification (run from repo root via `go -C` / `make -C`, no `cd`)

```
$ go -C .../vitictl-kubevirt build ./...
(clean, exit 0)

$ go -C .../vitictl-kubevirt vet ./...
(clean, exit 0)

$ go -C .../vitictl-kubevirt test ./... -count=1
ok  .../vitictl-kubevirt/cmd            0.851s
ok  .../vitictl-kubevirt/internal/config 0.889s
ok  .../vitictl-kubevirt/internal/guest  1.305s
ok  .../vitictl-kubevirt/internal/kube   2.848s
ok  .../vitictl-kubevirt/internal/output 2.888s
ok  .../vitictl-kubevirt/internal/picker 1.696s
ok  .../vitictl-kubevirt/internal/release 2.317s
ok  .../vitictl-kubevirt/internal/roll   1.858s
ok  .../vitictl-kubevirt/internal/virtctl 2.438s
ok  .../vitictl-kubevirt/internal/viticli 3.157s
ok  .../vitictl-kubevirt/internal/vm     2.264s   (10 new migration tests, all PASS -v)

$ gofmt -l internal/vm/migrations.go internal/vm/migrations_test.go cmd/migrations.go
(no output — clean)

$ make -C .../vitictl-kubevirt lint
16 issues, linter "unused":
  cmd/migrations.go: newMigrationsCmd, watchMigrations, printWatchHeader, collectMigrations,
    activeMigrations, renderMigrations, migrationNode, migrationJSON, structuredMigrations
  cmd/orphans.go: newOrphansCmd + 7 others (concurrent agent's file, untouched by me)
```

The 9 "unused" hits in `cmd/migrations.go` are expected and unavoidable given the ticket's
explicit instruction to build `newMigrationsCmd` but **not register it anywhere** — with no
caller, everything it alone reaches is statically dead until the controller wires it into the
command tree. The same lint pattern appears identically against the concurrently-developed
`cmd/orphans.go`, confirming this is the shared, deliberate state of both unregistered commands,
not a defect. A transient `build failed` / `undefined: kubevirtv1` was also observed twice during
verification, both times traced to the other agent's in-flight edits to `orphans.go` /
`orphans_test.go`; retrying moments later showed a clean build — no relation to this change.

## Design decisions

- **Cluster de-dup by name in `CollectMigrations`**: unlike `Collect`, which naturally avoids
  duplicate rows because each Machine belongs to exactly one zone, migrations are read directly
  from the KubeVirt cluster and have no such natural key. A `seen[clusterName]` guard stops a
  shared cluster (reachable from two zones/groups) from doubling every row while still resolving
  once per group as `Collect` does.
- **VMI → VM → Machine via VM's own name, not a separate VMI-name index**: KubeVirt always names
  a VMI after its owning VM, so the VM list alone (keyed by its own name) answers "which Machine
  owns the VMI named X" — no extra VMI list/round trip needed.
- **`--watch` traps its own signals**: `cmd.Context()` is never given cancellation upstream
  (confirmed via `main.go` / `root.go` — no `signal.NotifyContext` anywhere), so the command
  installs one itself, scoped to the `--watch` branch only.
- **Wide adds NAME + MODE**: the migration's own generated name (useful to `kubectl describe`
  something visible on-screen) and live migration mode, both absent from the narrow view to keep
  it scannable during a rollout.

## Could not do / left as-is

- Did not touch `cmd/root.go`, `cmd/vm.go`, `internal/vm/vm.go`, or `cmd/orphans.go` /
  `internal/vm/orphans.go`, per the constraint — `newMigrationsCmd` is unregistered and unused
  until the controller wires it in, which is by design, not an oversight.
