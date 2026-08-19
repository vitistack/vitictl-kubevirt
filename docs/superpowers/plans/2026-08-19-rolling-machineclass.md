# Rolling Machineclass Changes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `vm changemachineclass` can target a KubernetesCluster's nodepool or control plane and roll the change one node at a time with cordon+drain, owned entirely by the tool.

**Architecture:** Two new packages in vitictl-kubevirt — `internal/guest` (workload-cluster client from the control-plane secret, cordon/drain via k8s.io/kubectl's drain library) and `internal/roll` (pure orchestration behind small interfaces so every phase unit-tests with fakes). The cmd layer resolves machine-vs-cluster targets, renders the picker/confirmation, and wires the phases. vitictl-nhn passes the new flags through and stops double-printing errors.

**Tech Stack:** Go, cobra, controller-runtime (typed clients + fake), client-go, k8s.io/kubectl/pkg/drain, vitistack/common v1alpha1 types.

**Spec:** docs/superpowers/specs/2026-08-19-rolling-machineclass-design.md

## Global Constraints

- Field paths are lowercase in YAML but the Go types are `Spec.Topology.ControlPlane` (json `controlplane`) and `Spec.Topology.Workers.NodePools` (json `nodePools`) — always go through the typed structs.
- Member selection: label `vitistack.io/clusterid`, annotation `vitistack.io/nodepool`, label `vitistack.io/node-role: control-plane`.
- Guest kubeconfig lives in Secret key `kube.config`; Secret found by name `<clusterID>` then label `vitistack.io/clusterid=<clusterID>`.
- Drain: IgnoreAllDaemonSets=true, DeleteEmptyDirData=true, Force=false.
- All new behavior TDD: failing test first, minimal code, run, commit per task.
- Every write to a cluster object is a patch (merge patch, or JSON patch with a `test` op guard for array elements) — never Update.

---

### Task 1: internal/guest — secret lookup and client construction

**Files:**
- Create: `internal/guest/guest.go`
- Test: `internal/guest/guest_test.go`

**Interfaces:**
- Produces: `guest.FindClusterSecret(ctx, c ctrlclient.Client, namespace, clusterID string) (*corev1.Secret, error)`, `guest.Connect(secret *corev1.Secret) (kubernetes.Interface, error)`, consts `guest.KeyKubeConfig = "kube.config"`, `guest.LabelClusterID = "vitistack.io/clusterid"`.

- [ ] **Step 1: Write failing tests**

```go
package guest

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func coreClient(t *testing.T, objs ...client.Object) client.Client { // import sigs.k8s.io/controller-runtime/pkg/client as client
	t.Helper()
	s := runtime.NewScheme()
	if err := scheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

func TestFindClusterSecretByName(t *testing.T) {
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "d-stackops-1010-qjxq", Namespace: "ns"}}
	c := coreClient(t, sec)
	got, err := FindClusterSecret(context.Background(), c, "ns", "d-stackops-1010-qjxq")
	if err != nil || got.Name != "d-stackops-1010-qjxq" {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestFindClusterSecretByLabel(t *testing.T) {
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: "prefixed-d-stackops-1010-qjxq", Namespace: "ns",
		Labels: map[string]string{LabelClusterID: "d-stackops-1010-qjxq"},
	}}
	c := coreClient(t, sec)
	got, err := FindClusterSecret(context.Background(), c, "ns", "d-stackops-1010-qjxq")
	if err != nil || got.Name != "prefixed-d-stackops-1010-qjxq" {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestFindClusterSecretMissing(t *testing.T) {
	if _, err := FindClusterSecret(context.Background(), coreClient(t), "ns", "nope"); err == nil {
		t.Fatal("want error")
	}
}

const miniKubeconfig = `apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: "https://127.0.0.1:1"}}]
contexts: [{name: c, context: {cluster: c, user: u}}]
users: [{name: u, user: {}}]
current-context: c
`

func TestConnectBuildsAClientset(t *testing.T) {
	sec := &corev1.Secret{Data: map[string][]byte{KeyKubeConfig: []byte(miniKubeconfig)}}
	if _, err := Connect(sec); err != nil {
		t.Fatal(err)
	}
}

func TestConnectRejectsMissingKey(t *testing.T) {
	_, err := Connect(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s"}})
	if err == nil || !strings.Contains(err.Error(), KeyKubeConfig) {
		t.Fatalf("want error naming %s, got %v", KeyKubeConfig, err)
	}
}
```

- [ ] **Step 2: Run** `go test ./internal/guest/` — expect FAIL (undefined symbols).

- [ ] **Step 3: Minimal implementation**

```go
// Package guest reaches the workload (guest) cluster that a KubernetesCluster
// provisions, using the kubeconfig the control plane itself stores — the same
// secret vitictl's extract command reads. No local kubeconfig entry needed.
package guest

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	KeyKubeConfig  = "kube.config"
	LabelClusterID = "vitistack.io/clusterid"
)

// FindClusterSecret locates the Secret holding a cluster's kubeconfig: named
// exactly <clusterID>, else labelled vitistack.io/clusterid=<clusterID>
// (covers operators configured with a SECRET_PREFIX).
func FindClusterSecret(ctx context.Context, c ctrlclient.Client, namespace, clusterID string) (*corev1.Secret, error) {
	var direct corev1.Secret
	err := c.Get(ctx, ctrlclient.ObjectKey{Namespace: namespace, Name: clusterID}, &direct)
	if err == nil {
		return &direct, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("reading secret %s/%s: %w", namespace, clusterID, err)
	}
	var list corev1.SecretList
	if err := c.List(ctx, &list, ctrlclient.InNamespace(namespace),
		ctrlclient.MatchingLabels{LabelClusterID: clusterID}); err != nil {
		return nil, fmt.Errorf("listing secrets for cluster %q: %w", clusterID, err)
	}
	if len(list.Items) == 0 {
		return nil, fmt.Errorf("no secret for cluster %q in namespace %q — cannot reach the guest cluster to drain nodes", clusterID, namespace)
	}
	return &list.Items[0], nil
}

// Connect builds a clientset from the secret's kube.config key.
func Connect(secret *corev1.Secret) (kubernetes.Interface, error) {
	raw, ok := secret.Data[KeyKubeConfig]
	if !ok {
		return nil, fmt.Errorf("secret %s has no %q key", secret.Name, KeyKubeConfig)
	}
	cfg, err := clientcmd.RESTConfigFromKubeConfig(raw)
	if err != nil {
		return nil, fmt.Errorf("parsing guest kubeconfig: %w", err)
	}
	return kubernetes.NewForConfig(cfg)
}
```

- [ ] **Step 4: Run** `go test ./internal/guest/` — expect PASS.
- [ ] **Step 5: Commit** `git add internal/guest && git commit -m "Add guest package: workload-cluster client from the control-plane secret"`

---

### Task 2: internal/roll — targets from a KubernetesCluster

**Files:**
- Create: `internal/roll/roll.go` (types + constants), `internal/roll/targets.go`
- Test: `internal/roll/targets_test.go`

**Interfaces:**
- Produces:
  - consts: `roll.LabelClusterID`, `roll.AnnotationNodepool = "vitistack.io/nodepool"`, `roll.LabelNodeRole = "vitistack.io/node-role"`, `roll.RoleControlPlane = "control-plane"`.
  - `type TargetKind string` with `KindControlPlane TargetKind = "controlplane"`, `KindNodePool TargetKind = "nodepool"`.
  - `type Target struct { Cluster *vitiv1alpha1.KubernetesCluster; Kind TargetKind; Pool string; Class string; Replicas int; PoolIndex int }`
  - `func LoadTargets(ctx context.Context, az *kube.VitistackClient, namespace, clusterName string) ([]Target, error)` — controlplane first, then nodepools in spec order. Distinguishable not-found: `roll.ErrClusterNotFound`.
  - `func (t Target) Describe() string` → `"controlplane"` or `"nodepool <name>"`.

- [ ] **Step 1: Failing tests** (test helpers mirror `internal/vm/vm_test.go`: fake AZ client with core+viti scheme)

```go
func TestLoadTargets(t *testing.T) {
	kc := &vitiv1alpha1.KubernetesCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "ns"},
		Spec: vitiv1alpha1.KubernetesClusterSpec{Topology: vitiv1alpha1.KubernetesClusterSpecTopology{
			ControlPlane: vitiv1alpha1.KubernetesClusterSpecControlPlane{Replicas: 1, MachineClass: "medium"},
			Workers: vitiv1alpha1.KubernetesClusterWorkers{NodePools: []vitiv1alpha1.KubernetesClusterNodePool{
				{Name: "workers1", MachineClass: "medium", Replicas: 3},
				{Name: "gpu", MachineClass: "large", Replicas: 1},
			}},
		}},
	}
	got, err := LoadTargets(context.Background(), azClient(t, kc), "ns", "c1")
	// want: [controlplane medium r1, nodepool workers1 medium r3 idx0, nodepool gpu large r1 idx1]
	// assert kinds, Pool names, Class, Replicas, PoolIndex
}

func TestLoadTargetsClusterNotFound(t *testing.T) {
	_, err := LoadTargets(context.Background(), azClient(t), "ns", "nope")
	if !errors.Is(err, ErrClusterNotFound) { t.Fatalf("got %v", err) }
}
```

- [ ] **Step 2: Run, verify FAIL.**
- [ ] **Step 3: Implement** — Get the KubernetesCluster typed; build the slice; `ErrClusterNotFound = errors.New(...)` wrapped with cluster name on IsNotFound.
- [ ] **Step 4: Run, verify PASS.**
- [ ] **Step 5: Commit.**

---

### Task 3: internal/roll — plan building (member selection + VM resolution)

**Files:**
- Create: `internal/roll/plan.go`
- Test: `internal/roll/plan_test.go`

**Interfaces:**
- Produces:
  - `type Member struct { Machine *vitiv1alpha1.Machine; VM *kubevirtv1.VirtualMachine; KV *kube.KubeVirtClient }`
  - `type VMResolver func(ctx context.Context, m *vitiv1alpha1.Machine) (*kube.KubeVirtClient, *kubevirtv1.VirtualMachine, error)`
  - `type Plan struct { Target Target; Class *vitiv1alpha1.MachineClass; Members []Member; ClusterID string }`
  - `func BuildPlan(ctx context.Context, az *kube.VitistackClient, t Target, class *vitiv1alpha1.MachineClass, resolve VMResolver) (*Plan, error)`

Selection: list Machines in `t.Cluster.Namespace` owned by the cluster (ownerReference name match) AND label `vitistack.io/node-role: control-plane` (controlplane) or annotation `vitistack.io/nodepool == t.Pool` (nodepool). ClusterID = shared `vitistack.io/clusterid` label (error if absent or mixed). Members sorted by name. Zero members → error. resolve() called per member; any error aborts.

- [ ] **Step 1: Failing tests** — machines with the labels/annotations observed live:

```go
func poolMachine(name, pool, clusterID string) *vitiv1alpha1.Machine {
	return &vitiv1alpha1.Machine{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "ns",
		Labels:      map[string]string{LabelClusterID: clusterID, LabelNodeRole: "worker"},
		Annotations: map[string]string{AnnotationNodepool: pool},
		OwnerReferences: []metav1.OwnerReference{{Kind: "KubernetesCluster", Name: "c1"}},
	}}
}
// TestBuildPlanSelectsPoolMembers: 3 workers1 + 1 gpu + 1 controlplane machine;
//   nodepool target "workers1" → exactly the 3, sorted, ClusterID filled.
// TestBuildPlanControlPlane: node-role label selects the ctp machine only.
// TestBuildPlanFailsWhenAResolveFails: resolver errors on one member → error, no plan.
// TestBuildPlanFailsOnZeroMembers.
```

- [ ] **Step 2-5:** RED → implement → GREEN → commit.

---

### Task 4: internal/roll — topology patch + propagation wait

**Files:**
- Create: `internal/roll/topology.go`
- Test: `internal/roll/topology_test.go`

**Interfaces:**
- Produces:
  - `func PatchTopology(ctx context.Context, az *kube.VitistackClient, t Target, newClass string) error` — controlplane: merge patch `{"spec":{"topology":{"controlplane":{"machineClass":X}}}}`; nodepool: JSON patch `[{"op":"test","path":"/spec/topology/workers/nodePools/<i>/name","value":<pool>},{"op":"replace","path":".../machineClass","value":X}]` via `ctrlclient.RawPatch(types.JSONPatchType, ...)`.
  - `type Options struct { DrainTimeout, ReadyTimeout, PropagationTimeout, PollInterval time.Duration }` + `func (o Options) withDefaults() Options` (5m / 10m / 2m / 5s).
  - `func AwaitPropagation(ctx context.Context, az *kube.VitistackClient, plan *Plan, opts Options) error` — re-Get each member Machine until all `spec.machineClass == plan.Class.Name`; timeout → error naming the laggards.

- [ ] **Step 1: Failing tests** — patch each kind against a fake client and re-Get to assert only machineClass changed (nodepool: other pools untouched); JSON-patch test-op mismatch (PoolIndex pointing at the wrong pool name) must error and change nothing; AwaitPropagation with a machine already updated returns nil, with `PollInterval: 10*time.Millisecond, PropagationTimeout: 50*time.Millisecond` and a stale machine returns a timeout error naming it.
- [ ] **Step 2-5:** RED → implement → GREEN → commit.

---

### Task 5: internal/roll — preflight and staging

**Files:**
- Create: `internal/roll/phases.go`
- Test: `internal/roll/phases_test.go`

**Interfaces:**
- Produces:
  - `type Guest interface { Node(ctx context.Context, name string) (*corev1.Node, error); Cordon(ctx context.Context, name string, desired bool) error; Drain(ctx context.Context, name string) error }`
  - `type Reporter interface { Step(format string, args ...any); Warn(err error) }`
  - `func Preflight(ctx context.Context, plan *Plan, g Guest) error` — for every member: `KV.VirtctlTarget()` succeeds and `g.Node(machine.Name)` returns a node. Errors aggregate into one message; nothing is written.
  - `func StageVMs(ctx context.Context, plan *Plan, rep Reporter) error` — per member `vm.PatchVMResources(ctx, m.KV, m.VM, vm.DesiredResources(m.Machine, plan.Class))`, reporting each.

- [ ] **Step 1: Failing tests** — stub Guest (map of nodes / error injection); KV clients built with the fake helper, one with empty Cluster (VirtctlTarget fails) to assert Preflight catches it BEFORE any node lookup errors mask it; StageVMs against fake KV asserts all templates patched.
- [ ] **Step 2-5:** RED → implement → GREEN → commit.

---

### Task 6: internal/roll — the serial rolling loop

**Files:**
- Create: `internal/roll/rollout.go`
- Test: `internal/roll/rollout_test.go`

**Interfaces:**
- Produces:
  - `type Restarter func(ctx context.Context, m Member) error`
  - `func Roll(ctx context.Context, plan *Plan, g Guest, restart Restarter, opts Options, rep Reporter) error`

Per member, strictly in order: `g.Cordon(name,true)` → `g.Drain(name)` → `restart(m)` → wait VMI Running with `status.memory.guestAtBoot == desired` (poll `m.KV.Ctrl` Get VMI by VM name/namespace; nil memory status tolerated as ready) → wait `g.Node(name)` Ready==True (poll; errors count as not-ready — the API may be down while a control plane reboots) → `g.Cordon(name,false)`. Failure handling exactly per spec: drain error → `g.Cordon(name,false)` then abort with the drain error wrapped; node-ready timeout → attempt uncordon, report whether it succeeded, abort. Remaining members never touched after an abort.

- [ ] **Step 1: Failing tests** — scripted fake Guest recording call order:

```go
// TestRollHappyPathOrder: 2 members; assert exact call sequence
//   cordon(a,true) drain(a) restart(a) cordon(a,false) cordon(b,true) ...
//   (VMI in fake KV pre-set to Running with matching guestAtBoot; node Ready)
// TestRollDrainFailureAbortsAndUncordons: drain(a) errors →
//   cordon(a,false) called, restart NEVER called, member b untouched, error names a.
// TestRollNodeNeverReadyAborts: node stays NotReady →
//   with tiny ReadyTimeout, uncordon attempted, error returned, b untouched.
```

- [ ] **Step 2-5:** RED → implement → GREEN → commit.

---

### Task 7: internal/guest — cordon/drain implementation (roll.Guest)

**Files:**
- Modify: `internal/guest/guest.go` (add Client)
- Test: `internal/guest/client_test.go`
- Modify: `go.mod` (`go get k8s.io/kubectl@v0.36.3` — match the client-go minor)

**Interfaces:**
- Produces: `type Client struct { Clientset kubernetes.Interface; DrainTimeout time.Duration; ErrOut io.Writer }` implementing `roll.Guest`:
  - `Node` — `Clientset.CoreV1().Nodes().Get`.
  - `Cordon` — `drain.RunCordonOrUncordon(c.helper(ctx), node, desired)`.
  - `Drain` — `drain.RunNodeDrain(c.helper(ctx), name)`.
  - helper: `&drain.Helper{Ctx: ctx, Client: c.Clientset, IgnoreAllDaemonSets: true, DeleteEmptyDirData: true, Force: false, Timeout: c.DrainTimeout, Out: io.Discard, ErrOut: c.ErrOut}` (ErrOut surfaces "cannot delete..." pod lists).

- [ ] **Step 1: Failing tests** — `k8s.io/client-go/kubernetes/fake` clientset: Cordon flips `spec.unschedulable` on a fake node and back; Drain on a node with no pods returns nil; Node returns the fake node.
- [ ] **Step 2-5:** RED → implement (plus `go get`) → GREEN → `go mod tidy` → commit.

---

### Task 8: cmd — flags, owned-machine refusal, cluster resolution, picker

**Files:**
- Modify: `cmd/changemachineclass.go`
- Test: `cmd/changemachineclass_test.go` (extend)

**Interfaces:**
- Consumes: `roll.LoadTargets`, `roll.Target`, `roll.ErrClusterNotFound`.
- Produces (used by Task 9): `func resolveRollTarget(cmd *cobra.Command, ctx context.Context, az *kube.VitistackClient, namespace, clusterName, nodepool string, controlplane bool) (roll.Target, error)` and the refusal in the machine path.

Changes:
1. Flags: `--nodepool string`, `--controlplane bool`, `--drain-timeout time.Duration` (default 5m); `MarkFlagsMutuallyExclusive("nodepool", "controlplane")`.
2. Machine path refusal: after `chooseClass`, if the Machine has an ownerReference `Kind == "KubernetesCluster"` and `class.Name != oldClass`, return an error naming the owner and the exact pool-mode command (pool name from `roll.AnnotationNodepool`, else `--controlplane` when `roll.LabelNodeRole` is `control-plane`). Same-class re-sync proceeds.
3. Cluster resolution: when `--nodepool`/`--controlplane` given, `name` is required (error otherwise) and machine search is skipped. When neither is given and `selectMachine` fails with its "no machine named" error, try `roll.LoadTargets` on each AZ client; a hit → target picker (`picker.Select(" Select a target ", []string{"TARGET","CLASS","REPLICAS"}, ...)`, non-interactive → error listing targets); no hit → the original machine error.
4. `resolveRollTarget` picks by flag (`Pool == nodepool` / `Kind == KindControlPlane`) with "no nodepool named X (have: ...)" errors.

- [ ] **Step 1: Failing tests**

```go
// TestChangeMachineClassPoolFlagsAreMutuallyExclusive: --nodepool a --controlplane → error (isolate(t)).
// TestChangeMachineClassRefusesOwnedMachineDifferentClass: fakeAZ machine with
//   ownerReferences KubernetesCluster + nodepool annotation "workers1"; call the
//   refusal helper (extract `func refuseOwned(m *vitiv1alpha1.Machine, newClass, oldClass string) error`)
//   → error contains "--nodepool workers1"; same class → nil.
// TestResolveRollTargetByFlags: LoadTargets fixtures; --nodepool workers1 picks it;
//   unknown pool error lists available; --controlplane picks the CP target.
// TestChangeMachineClassHelpDocumentsPoolFlags: help mentions --nodepool, --controlplane, --drain-timeout.
```

- [ ] **Step 2-5:** RED → implement → GREEN → commit.

---

### Task 9: cmd — rollout wiring

**Files:**
- Modify: `cmd/changemachineclass.go`
- Test: `cmd/changemachineclass_test.go` (extend)

**Interfaces:**
- Consumes: everything from Tasks 1–8.

`runRollout(cmd, s, az/selection..., target, className, drainTimeout, noRestart, assumeYes, doRestart)`:
1. Class via the existing `chooseClass`-equivalent against the target's AZ (reuse `vm.ListEnabledClasses` + `pickClass`; `--class` validated; current class allowed = full-pool re-sync).
2. `roll.BuildPlan` with a VMResolver built on `s.resolver()` + `vm.ResolveVM`.
3. Guest client: `guest.FindClusterSecret(ctx, az.Ctrl, plan.Target.Cluster.Namespace, plan.ClusterID)` → `guest.Connect` → `&guest.Client{Clientset: cs, DrainTimeout: drainTimeout, ErrOut: cmd.ErrOrStderr()}`.
4. `roll.Preflight`.
5. Confirmation (skipped by `--yes`/`--restart`): `"Roll <target> of <cluster>: N machines, class old → new (<describeResources>), one at a time with cordon+drain — continue?"`; single-replica controlplane appends the API-outage warning. Declined → `errCancelled`.
6. `roll.PatchTopology` → `roll.AwaitPropagation` → `roll.StageVMs`. `--no-restart` → print the pending hint, stop.
7. `roll.Roll` with `Restarter` = `runVirtctl(cmd, m.KV, "restart", m.VM.Namespace, m.VM.Name)` and a Reporter printing `▶ <step>` lines to stderr, ✅ summaries to stdout.

cmd-level tests stay at wiring granularity (the orchestration itself is covered in internal/roll): reuse the fake-backed helpers to test the confirmation string builder `rollSummary(plan) string` (includes count, classes, resources; single-CP warning) — the network paths were validated live.

- [ ] **Step 1: Failing test for `rollSummary`** (2 members medium→large 32Gi contains "2 machines", "medium → large"; 1-replica controlplane target contains "unavailable").
- [ ] **Step 2-5:** RED → implement rollSummary + full wiring (wiring compiles + existing suite green) → commit.

---

### Task 10: vitictl-nhn — flag passthrough and single error output

**Files:**
- Modify: `internal/viticli/viticli.go`, `cmd/kc.go`, `cmd/root.go`
- Test: `internal/viticli/viticli_test.go`, `cmd/kc_test.go` (extend)

**Interfaces:**
- Produces: `ChangeMachineClass` gains `NodePool string`, `ControlPlane bool`, `DrainTimeout string`; `Args` appends `--nodepool X` / `--controlplane` / `--drain-timeout D`; `viticli.ErrChildFailed` sentinel.

Error-output fix: when the child ran and exited non-zero and the subcommand probe succeeds (ordinary failure — child already printed `❌ Error: ...`), `Run` returns `fmt.Errorf("%w: %v", ErrChildFailed, err)`. nhn's `cmd/root.go Execute()` treats `errors.Is(err, viticli.ErrChildFailed)` like `errCancelled`: exit non-zero, print nothing. The upgrade-hint error keeps printing (it is NOT wrapped in ErrChildFailed).

- [ ] **Step 1: Failing tests** — Args passthrough for the three new flags; `TestRunOrdinaryFailureIsSilentlyWrapped`: failing stub whose `--help` probe succeeds → `errors.Is(err, ErrChildFailed)`; kc_test: `run(t, "kc", ...)` with that stub prints no "❌ Error:" on stderr but exits non-zero; upgrade-hint case still prints.
- [ ] **Step 2-5:** RED → implement → GREEN → commit.

---

### Task 11: Full verification + binaries

- [ ] `go build ./... && go vet ./... && go test ./... -count=1` in both repos — all green, fresh.
- [ ] `GOOS=linux GOARCH=386 go build ./internal/...` in vitictl-kubevirt still compiles.
- [ ] Rebuild `~/.local/bin/viti-kubevirt` (+`viti-kv`) and `~/.local/bin/viti-nhn` from the feature branches.
- [ ] Manual live validation (with the user): `viti nhn kc edit machineclass d-stackops-1010 --nodepool workers1 --class medium` — expect: plan summary + confirmation, topology patched, propagation, staged templates, then wrk0/wrk1/wrk2 each cordoned→drained→restarted→Ready→uncordoned serially; end state all workers medium/16Gi, cluster healthy. Also demo `viti nhn kc edit machineclass d-stackops-1010` bare → target picker.
