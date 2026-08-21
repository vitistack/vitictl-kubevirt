# vitictl-kubevirt → pkg/plugin migration (spec step 2, v0.1.5)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace vitictl-kubevirt's duplicated toolkit (`internal/picker`, `internal/output`, `internal/release`, the exec plumbing of `internal/viticli`, and `cmd/upgrade.go`/`cmd/version.go`) with imports of `github.com/vitistack/vitictl/pkg/plugin/...` at v0.0.33, deleting ~1,700 duplicated lines with behavior locked by the existing cmd-level tests.

**Architecture:** Pure import swaps for picker/output; `selfupgrade.New{Version,Upgrade}Cmd` replaces the two command files and removes the need for `internal/release`; `internal/viticli` keeps its domain layer (`Target`, `Args`, `MachineConsole`) and delegates the exec to the kit with an inline `DiagnoseFunc` that preserves today's loud, machine-named failure.

**Tech Stack:** Go; new dependency `github.com/vitistack/vitictl v0.0.33`.

**Spec:** `/home/andreh/repo/vitictl/docs/superpowers/specs/2026-08-21-plugin-toolkit-design.md` (migration step 2). Binding ruling from the toolkit's final review: kubevirt's viticli is NOT a pure subset — `viti machine console` failures must stay loud with the machine's name.

## Global Constraints

- Behavior-identical: every existing cmd-level test passes unmodified unless a task explicitly says otherwise. Two accepted, documented deviations: (a) the upgrade/version `--help` text gains the kit's neutral "If the plugin's repository is private…" sentence and loses any kubevirt-specific phrasing; (b) untested edge wordings in viticli's signal-death/cancel paths follow the kit's classification (quiet on cancelled context, generic loud message on signal death) — normal non-zero exits keep today's exact `"viti machine console <name>: …"` wrapping.
- Commits are local only; nothing is pushed. The v0.1.5 release is gated separately at the end.
- Every task ends with `go test ./... && go build ./...` green in `/home/andreh/repo/vitictl-kubevirt`.
- Commit messages end with `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`.
- Never modify `/home/andreh/repo/vitictl` (read-only reference for this plan) or `/home/andreh/repo/vitictl-talos`.

---

### Task 1: Dependency + picker/output import swap, delete the copies

**Files:**
- Modify: `go.mod`, `go.sum` (add `github.com/vitistack/vitictl v0.0.33`)
- Modify (import path only): `cmd/rollout.go`, `cmd/orphans.go`, `cmd/select.go`, `cmd/vm.go`, `cmd/migrations.go`, `cmd/changemachineclass.go`, and any other file `grep -rln "vitictl-kubevirt/internal/picker\|vitictl-kubevirt/internal/output" --include='*.go'` finds outside those packages
- Delete: `internal/picker/` (3 files), `internal/output/` (2 files)

**Interfaces:**
- Consumes: `github.com/vitistack/vitictl/pkg/plugin/picker` and `.../pkg/plugin/output` — drop-in supersets of the deleted packages (identical exported names/signatures; picker adds `SelectMulti`, unused here).
- Produces: nothing new — later tasks are independent.

- [ ] **Step 1: Add the dependency**

```bash
cd /home/andreh/repo/vitictl-kubevirt && go get github.com/vitistack/vitictl@v0.0.33 && go mod tidy
```

- [ ] **Step 2: Swap the imports**

In every consumer file replace exactly:
- `"github.com/vitistack/vitictl-kubevirt/internal/picker"` → `"github.com/vitistack/vitictl/pkg/plugin/picker"`
- `"github.com/vitistack/vitictl-kubevirt/internal/output"` → `"github.com/vitistack/vitictl/pkg/plugin/output"`

No call-site changes: the kit's `picker.Item/Interactive/Select/ErrCancelled` and `output.*` are signature-identical.

- [ ] **Step 3: Delete the duplicated packages**

```bash
git rm -r internal/picker internal/output
```

- [ ] **Step 4: Verify (the existing cmd tests are the behavior lock)**

```bash
go test ./... && go build ./... && go vet ./...
```
Expected: all green with zero test edits. If any test imported the deleted packages directly, that is a finding to report, not silently fix.

- [ ] **Step 5: Commit**

```bash
git add -u && git add go.mod go.sum && \
git commit -m "refactor: use vitictl pkg/plugin for picker and output" \
  -m "Import-path swap onto the shared toolkit (vitictl v0.0.33); deletes the local copies. The kit's picker is the talos superset with an identical Select signature, so no call site changes." \
  -m "Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 2: selfupgrade replaces cmd/upgrade.go + cmd/version.go; delete internal/release

**Files:**
- Modify: `cmd/root.go` (wire the constructed commands)
- Delete: `cmd/upgrade.go`, `cmd/version.go`, `internal/release/` (2 files)
- Modify: `cmd/upgrade_test.go`, `cmd/version_test.go` (trim to registration/behavior assertions that survive; delete tests that duplicated the kit's own suite)

**Interfaces:**
- Consumes: `github.com/vitistack/vitictl/pkg/plugin/selfupgrade` — `Options{Name, Repo, Version string}`, `NewVersionCmd(o) *cobra.Command`, `NewUpgradeCmd(o) *cobra.Command`.
- Produces: nothing new.

- [ ] **Step 1: Read the current wiring**

Read `cmd/root.go` to find how `newVersionCmd()`/`newUpgradeCmd()` (or equivalents) are registered and where the ldflags `version` variable lives. Read `cmd/upgrade_test.go` and `cmd/version_test.go` in full before touching them.

- [ ] **Step 2: Swap the wiring**

In `cmd/root.go`, replace the two registrations with:

```go
o := selfupgrade.Options{
	Name:    "kubevirt",
	Repo:    "vitistack/vitictl-kubevirt",
	Version: version,
}
root.AddCommand(selfupgrade.NewVersionCmd(o))
root.AddCommand(selfupgrade.NewUpgradeCmd(o))
```

(Adapt to the file's actual structure — if `version` is applied via `SetVersion`, build `o` where the root is constructed so it carries the final value.) Then `git rm cmd/upgrade.go cmd/version.go && git rm -r internal/release`.

- [ ] **Step 3: Trim the tests honestly**

Keep (adapting imports/assertions only where the accepted help-text deviation requires): tests asserting the commands are registered in the tree, and any behavior test that still compiles against the tree-level commands (e.g. "version subcommand output matches --version flag" — this is the root-level assertion the kit itself could not carry; it MUST be kept and pass). Delete tests that duplicated what now lives in the kit's own suite (confirm the kit has an equivalent before each deletion; list every deleted test and its kit equivalent in the commit message). A help-text assertion broken only by the accepted neutral private-repo sentence is updated, not deleted.

- [ ] **Step 4: Verify and compare help**

```bash
go test ./... && go build -o /tmp/viti-kubevirt-new . && /tmp/viti-kubevirt-new upgrade --help && /tmp/viti-kubevirt-new version --help
```
Expected: green; help output equals today's except the accepted deviation (a). Paste both helps into the report.

- [ ] **Step 5: Commit**

```bash
git add -u && \
git commit -m "refactor: use vitictl pkg/plugin/selfupgrade for version and upgrade" \
  -m "Replaces cmd/upgrade.go, cmd/version.go and internal/release with the shared constructors (Options{kubevirt, vitistack/vitictl-kubevirt}). Help text now carries the kit's neutral private-repo sentence. Deleted tests and their kit equivalents: <list>." \
  -m "Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 3: viticli delegates exec to the kit, keeps the domain layer

**Files:**
- Modify: `internal/viticli/viticli.go`, `internal/viticli/viticli_test.go`

**Interfaces:**
- Consumes: `github.com/vitistack/vitictl/pkg/plugin/viticli` — `Run(ctx, Streams{In,Out,Err}, args []string, diagnose DiagnoseFunc) error`, `var Binary`, `Path()`, `ErrNotInstalled`.
- Produces (unchanged for cmd/vm.go): `Target`, `Streams`, `Args(Target) []string`, `MachineConsole(ctx, Streams, Target) error`.

- [ ] **Step 1: Rewrite viticli.go**

Keep the package comment, `Target`, `Streams`, and `Args` exactly as they are. Replace `Binary`, `Path`, and `MachineConsole`'s body with delegation:

```go
import kitcli "github.com/vitistack/vitictl/pkg/plugin/viticli"

// MachineConsole runs `viti machine console <name>` attached to the caller's
// terminal, replacing this process's foreground for the dashboard's lifetime.
// A normal failure is wrapped loudly with the machine's name — the dashboard
// is interactive, and a silent non-zero exit would read as it simply closing.
func MachineConsole(ctx context.Context, s Streams, t Target) error {
	return kitcli.Run(ctx,
		kitcli.Streams{In: s.In, Out: s.Out, Err: s.Err},
		Args(t),
		func(_ context.Context, _ string, childErr error) error {
			return fmt.Errorf("viti machine console %s: %w", t.Name, childErr)
		})
}
```

Delete the local `Binary`, `Path`, and `ErrNotInstalled`; re-export for compatibility only if a caller outside the package uses them (check with grep; cmd/vm.go uses only `MachineConsole`, `Streams`, `Target`). The kit's `Path()` error text is generic; kubevirt's dashboard-specific hint ("run 'talosctl dashboard' yourself against the node") is lost — acceptable, note it in the commit message, OR keep a local `Path()` wrapper that adds the hint and call it before `kitcli.Run` (choose whichever keeps the existing `TestPathReportsAMissingViti`-style test passing; if no such local test exists, take the simpler deletion).

- [ ] **Step 2: Adapt the tests**

`TestMachineConsoleRunsTheBinaryWithTheRightArgs` and `TestMachineConsoleReportsAFailure` now stub the KIT's binary: `kitcli.Binary = stubPath` (import the kit in the test). Assertions stay identical — the args test still sees the full argv via the echo stub; the failure test still asserts the error contains "viti machine console" and the machine name. `Args` tests unchanged.

- [ ] **Step 3: Verify**

```bash
go test ./internal/viticli/ ./cmd/ && go test ./... && go vet ./...
```

- [ ] **Step 4: Commit**

```bash
git add -u && \
git commit -m "refactor: viticli delegates exec to vitictl pkg/plugin/viticli" \
  -m "Target/Args/MachineConsole stay; the exec plumbing and failure classification move to the kit. Normal failures keep the loud 'viti machine console <name>' wrapping via a DiagnoseFunc (the kit's cancelled-context and signal-death classifications apply to those untested edges)." \
  -m "Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 4: Final verification, README layout table, gate

**Files:**
- Modify: `README.md` (the Layout table rows for `internal/picker`, `internal/output`, `internal/release`, `internal/viticli`)

- [ ] **Step 1: Full verification**

```bash
go test ./... && go vet ./... && make lint && make build
```
Expected: all green, lint 0 issues. Also `grep -rn "vitictl-kubevirt/internal/picker\|vitictl-kubevirt/internal/output\|vitictl-kubevirt/internal/release" --include='*.go' .` → no matches.

- [ ] **Step 2: README Layout table**

Remove the `internal/picker`, `internal/output`, `internal/release` rows (or fold them into one line noting the shared kit) and reword `internal/viticli`'s row to "domain layer over vitictl's pkg/plugin/viticli". Add one sentence under Development: "The picker, output, release-check and version/upgrade scaffolding come from `github.com/vitistack/vitictl/pkg/plugin` (pinned in go.mod)."

- [ ] **Step 3: Commit, then STOP**

```bash
git add README.md && git commit -m "docs: point the layout at vitictl's shared pkg/plugin" \
  -m "Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```
Present the release gate for **v0.1.5** to the user. Do NOT tag or push.
