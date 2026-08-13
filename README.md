# vitictl-kubevirt

🖥️ KubeVirt commands for the [viti](https://github.com/vitistack/vitictl) CLI.

`viti-kubevirt` is a viti **plugin**: viti discovers any `viti-*` binary on
`PATH` and exposes it as a subcommand, so this binary is reachable as
`viti kubevirt ...` — and as `viti kv ...` through the `viti-kv` symlink it
installs alongside itself. It also runs standalone as `viti-kubevirt ...`.

It lists Vitistack **Machines** enriched with the state of the KubeVirt
**VirtualMachine** and **VirtualMachineInstance** backing each one, and drives
their lifecycle: start, stop, restart, pause, unpause, reset, soft-reboot, plus
VNC and serial consoles.

## Two clusters, two configs

This is the thing to understand before configuring anything.

| Where | What lives there | Configured by |
| ----- | ---------------- | ------------- |
| Vitistack availability zones | `Machine` resources — what was asked for | `viti config add` (vitictl's own) |
| KubeVirt cluster | `VirtualMachine` / `VirtualMachineInstance` — what is actually running | `viti kubevirt config add` |

The plugin **reads** viti's availability zones rather than redefining them:
configuring the same clusters twice would let the two CLIs drift. When viti
dispatches the plugin it passes `VITI_CONFIG`, so `viti kubevirt` always uses
whatever config the parent `viti` was using.

Only the KubeVirt side needs its own file, at
`~/.vitistack/kubevirt.config.yaml`.

### Which KubeVirt cluster a machine runs on

A fleet spans one KubeVirt cluster per availability zone, so there is no single
"the KubeVirt cluster" to list against. The plugin works this out per machine
rather than asking you to maintain a mapping: a `Machine` is annotated with the
`KubevirtConfig` it was provisioned through, that resource names a Secret, and
the Secret holds the kubeconfig for the cluster its VM actually runs on.

```
Machine  vitistack.io/kubevirt-config: kubevirt-provider
  └─► KubevirtConfig "kubevirt-provider"   (cluster-scoped, in the AZ)
        └─► Secret vitistack/kubevirt-provider
              └─► kubeconfig ──► the KubeVirt cluster
```

Nothing in `kubevirt.config.yaml` is required for this — listings discover what
they need. That file still matters for two things:

- **`virtctl`** takes a kubeconfig *path*, not credentials, so lifecycle verbs
  and the consoles need the discovered cluster to also have a local entry.
  Matching is by context name. Without one you get an error naming the context
  to add, rather than a kubeconfig written to disk behind your back.
- **`--cluster`** pins every command to one cluster and skips discovery
  entirely. That is the path to use when the Vitistack control plane is down.

## Install

### With viti (recommended)

```sh
viti plugin install kubevirt
viti kubevirt --help
```

### From source

```sh
make install          # builds bin/viti-kubevirt, installs it + the viti-kv symlink into $GOBIN
```

### Requirements

**`virtctl` must be on `PATH`.** The lifecycle verbs and the consoles are
KubeVirt *subresource* endpoints — they are not ordinary object writes, and
VNC needs a websocket proxied to a local viewer. `virtctl` already does all of
that and ships with KubeVirt, so this plugin drives it instead of
reimplementing it. Install it from the
[KubeVirt docs](https://kubevirt.io/user-guide/operations/virtctl_client_tool/),
or `brew install virtctl`.

Match `virtctl`'s minor version to your cluster's.

## Configure

```sh
viti kubevirt config add kv-osl-01 --kubeconfig ~/kubeconfig/kv-osl-01
viti kubevirt config add kv-trd-02 --context admin@kv-trd-02 --default

viti kubevirt config list      # what is configured, and which is default
viti kubevirt config default kv-osl-01
viti kubevirt config remove kv-trd-02
viti kubevirt config path      # both config file locations
viti kubevirt config test      # virtctl + KubeVirt cluster + Vitistack zones
```

The first cluster you add becomes the default, so a single-cluster setup never
needs `--cluster`. With several configured and none marked default, commands
refuse to guess.

`config test` checks every leg and reports them all, rather than stopping at
the first failure — when you are fixing a setup you want the whole picture:

```
✅ virtctl (/opt/homebrew/bin/virtctl)
✅ kubevirt cluster kv-osl-01 (https://10.0.0.1:6443)
✅ vitistack availability zones (2/2 reachable)
```

An omitted `--kubeconfig` falls back to `$KUBECONFIG` or `~/.kube/config`; an
omitted `--context` uses that kubeconfig's current-context — the same rules
viti applies to availability zones.

For CI, `VITI_KUBEVIRT_KUBECONFIG` (with optional `VITI_KUBEVIRT_CONTEXT` and
`VITI_KUBEVIRT_NAMESPACE`) defines a cluster inline and wins over the file, so
nothing has to be written to disk. `VITI_KUBEVIRT_CONFIG` overrides the config
path outright.

## Listing

```sh
viti kubevirt vm list
viti kubevirt vm list -o wide
viti kubevirt vm list -n my-namespace --sort status
viti kubevirt vm get my-vm
viti kv vm list -o json | jq -r '.[].virtualMachineInstance.status.nodeName'
```

```
AZ      NAMESPACE   NAME     STATUS    READY   NODE      IPS          AGE
osl-01  platform    web-01   Running   True    node-3    10.0.0.5     12d
osl-01  platform    web-02   Stopped   False   -         -            12d
osl-01  data        db-01    Running   True    node-7    10.0.1.9     44d
```

`vm` also answers to `vms`, `machine`, `machines`, and `m`.

Three details worth knowing:

- **A Machine with no VM is still listed**, with an empty status. That is what
  a failed provision looks like, and hiding it would hide the problem.
- **`STATUS` prefers KubeVirt's view** (`VirtualMachine.status.printableStatus`),
  falling back to the VMI phase and then the Machine phase, so the column
  reflects what is actually running rather than what was requested.
- **`IPS` comes from the running instance**, falling back to the Machine's
  recorded addresses when nothing is running.

`-o wide` adds the KubeVirt VM name, the Machine phase, CPU and memory.
`-o json` / `-o yaml` emit both halves unflattened, under `machine`,
`virtualMachine`, and `virtualMachineInstance`.

## Acting on a VM

```sh
viti kubevirt vm start   web-01
viti kubevirt vm stop    web-01           # graceful; --force to power off
viti kubevirt vm restart web-01           # also spelled: reboot
viti kubevirt vm pause   web-01
viti kubevirt vm unpause web-01
viti kubevirt vm reset   web-01           # like the reset button
viti kubevirt vm soft-reboot web-01       # asks the guest OS

viti kubevirt vm vnc     web-01
viti kubevirt vm console web-01           # serial console; detach with Ctrl-]
```

### Talos guests get their dashboard, not a dead serial line

Talos is API-driven: no getty, no login shell, no SSH on any TTY. `virtctl
console` therefore attaches, reports success, and then shows nothing — which
reads as a broken tool rather than as the OS working as designed.

`console` recognises a Talos machine from `spec.os.distribution` and opens its
node dashboard instead:

```sh
viti kubevirt vm console                  # pick a machine, get its dashboard
viti kubevirt vm console t-test-004-43ss-wrk0
```
```
🖥️  t-test-004-43ss-wrk0 runs Talos — it has no serial shell, opening its dashboard instead
🖥️  talosctl dashboard (context "t-test-004-43ss") — endpoints: [100.64.1.16], nodes: [100.64.1.24]
```

The dashboard is opened through `viti machine console`, not reimplemented here.
Reaching a Talos node needs its owning cluster, that cluster's credentials
secret, the cert-valid control-plane endpoints, and the one node address in the
certificate's SANs — a KubeVirt guest reports its CNI and overlay addresses
alongside the real NIC, and dialling the wrong one fails with `x509:
certificate is valid for X, not Y`. viti already resolves all of that; copying
it here would mean maintaining the same subtle logic in two repositories. Same
reasoning as shelling out to `virtctl` rather than reimplementing KubeVirt's
subresource API.

`vnc` also shows the Talos console and is left alone. `--force` attaches to the
raw serial line regardless. With `--cluster` no Machine is fetched, so the guest
OS is unknown and the console attaches as before.

### Picking a machine interactively

Leave the name out and you get a fuzzy-searchable list to choose from — worth
having once a fleet is large enough that nobody remembers exact names:

```sh
viti kubevirt vm reboot                   # pick from every zone
viti kubevirt vm reboot -z no-central-az1 # pick from one zone
viti kubevirt vm get                      # works for every single-machine verb
```

The list is built from the Machines alone, so it opens without first waiting on
every KubeVirt cluster in the fleet. `PHASE` is therefore the Machine's own and
can lag reality; the live state is fetched afterwards, for the machine actually
chosen. Type to filter on any column, `Enter` to select, `Esc` to cancel.

A name that matches several machines opens the same list narrowed to those,
instead of failing outright.

The picker takes over the terminal, so a piped or CI invocation is told to name
its machine rather than left hanging on a UI that cannot be drawn. The choice is
echoed to stderr, keeping stdout pipeable.

Anything that interrupts a running guest — `stop`, `restart`, `pause`,
`reset`, `soft-reboot` — confirms first, naming the namespace and cluster.
Picking selects; confirming authorises. `--yes` skips the prompt for scripts.
A non-terminal stdin is refused rather than read as consent.

Two design points that matter in practice:

- **The name you type is the Machine name.** kubevirt-operator derives the VM
  name separately and links the two with a `vitistack.io/source-machine`
  label, so matching on name alone would act on the wrong VM or none at all.
  Actions resolve through that label, falling back to the VM's own name for
  hand-made VMs. The KubeVirt VM name works too.
- **Actions land on the machine's own cluster.** The zone holding the Machine
  names the KubeVirt cluster it runs on, so acting on a VM works wherever it
  lives without naming its cluster. Only that one zone listing is needed — not
  the fleet-wide join a listing performs.
- **`--cluster` skips the control plane entirely.** It pins the command to one
  KubeVirt cluster and resolves the target there directly, so stopping or
  restarting a VM keeps working when Vitistack is down — which is exactly when
  you are most likely to be doing it. Leaving the name out then picks from that
  cluster's VirtualMachines rather than from Machines.

An ambiguous name — the same Machine in two zones or namespaces — is never
guessed at. On a terminal it opens the picker narrowed to those matches;
without one it is refused, and `--namespace` or `--availabilityzone` narrows it.

The cluster's kubeconfig and context are passed to `virtctl` explicitly on
every call, so an action can never land on whatever cluster your `KUBECONFIG`
happened to point at.

## Version and upgrades

These mirror `viti version` and `viti upgrade`, for the plugin:

```sh
viti kubevirt version
viti kubevirt version --check
viti kubevirt upgrade
viti kubevirt upgrade --run
```

`version --check` reports a problem and still exits `0` — being offline is not
a failure of "what version am I". `upgrade` exits non-zero, because it was
asked to do something it could not do.

`--run` does not download anything itself; it runs `viti plugin upgrade
kubevirt`, which verifies the SHA-256 checksum and the Sigstore signature and
replaces the binary atomically. So `viti` must be on `PATH`, the plugin must
have been installed by viti, and `--run` is refused on Windows (the upgrade
replaces this very binary, which Windows will not do to a running `.exe`).

## Development

```sh
make build        # build bin/viti-kubevirt
make test         # go test ./... with coverage
make lint         # golangci-lint
make gosec        # security analysis
make govulncheck  # known-vulnerability scan
```

Tests use controller-runtime's fake client, so no cluster and no credentials
are required. The `virtctl` integration is tested by asserting on the argument
list rather than by executing it.

### Layout

| Path                 | Responsibility                                             |
| -------------------- | ---------------------------------------------------------- |
| `cmd/`               | cobra tree: `vm`, `config`, `version`, `upgrade`           |
| `internal/config/`   | the plugin's cluster list, and reading viti's own config   |
| `internal/kube/`     | clients for both cluster kinds, and KubeVirt discovery      |
| `internal/vm/`       | the Machine ↔ VirtualMachine join and target resolution    |
| `internal/picker/`   | the interactive fuzzy list (shared in spirit with nhn's)    |
| `internal/virtctl/`  | shelling out to virtctl                                    |
| `internal/viticli/`  | shelling out to viti for the Talos dashboard                |
| `internal/output/`   | `-o` parsing, aligned tables, JSON/YAML encoding           |
| `internal/release/`  | latest-release lookup on GitHub, version comparison        |

### Releasing

Tag and push, or publish a release from the website — the workflow handles
both and is idempotent, so a re-run is safe:

```sh
git tag v0.1.0
git push origin v0.1.0
```

The asset names are a contract with `viti plugin install`, which derives them
from vitictl's `plugins.yaml` defaults:

| Asset                                     | Purpose                                  |
| ----------------------------------------- | ---------------------------------------- |
| `viti-kubevirt-<tag>-<os>-<arch>.tar.gz`  | archive, binary at `<dir>/viti-kubevirt` |
| `viti-kubevirt-<tag>-SHA256SUMS`          | aggregate checksums                      |
| `<archive>.cosign.bundle`                 | Sigstore signature                       |
| `viti-kubevirt-<tag>-windows-<arch>.zip`  | manual installs on Windows               |

The workflow's own path is part of that contract: the default cosign identity
is `^https://github.com/<repo>/.github/workflows/release.yml@refs/tags/`, so
renaming or moving it breaks signature verification on install.
