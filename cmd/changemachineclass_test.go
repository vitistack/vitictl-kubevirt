package cmd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"

	"github.com/vitistack/vitictl-kubevirt/internal/config"
	"github.com/vitistack/vitictl-kubevirt/internal/kube"
	"github.com/vitistack/vitictl-kubevirt/internal/roll"
	"github.com/vitistack/vitictl-kubevirt/internal/virtctl"
	"github.com/vitistack/vitictl-kubevirt/internal/vm"
)

func TestChangeMachineClassIsInTheTree(t *testing.T) {
	out, _, err := run(t, "vm", "--help")
	if err != nil {
		t.Fatalf("run(vm --help) error = %v", err)
	}
	if !strings.Contains(out, "changemachineclass") {
		t.Errorf("vm --help does not list changemachineclass:\n%s", out)
	}
}

func TestChangeMachineClassHelpDocumentsItsFlags(t *testing.T) {
	out, _, err := run(t, "vm", "changemachineclass", "--help")
	if err != nil {
		t.Fatalf("run(vm changemachineclass --help) error = %v", err)
	}
	for _, want := range []string{"--class", "--restart", "--no-restart", "--yes"} {
		if !strings.Contains(out, want) {
			t.Errorf("help does not document %q:\n%s", want, out)
		}
	}
	// The change only takes effect on a reboot; the help must say so.
	if !strings.Contains(strings.ToLower(out), "restart") {
		t.Errorf("help does not explain the restart requirement:\n%s", out)
	}
}

// cmc is what people will actually type.
func TestChangeMachineClassAliases(t *testing.T) {
	for _, alias := range []string{"cmc", "change-class"} {
		out, _, err := run(t, "vm", alias, "--help")
		if err != nil {
			t.Fatalf("run(vm %s --help) error = %v", alias, err)
		}
		if !strings.Contains(out, "changemachineclass") {
			t.Errorf("vm %s --help is not the changemachineclass command:\n%s", alias, out)
		}
	}
}

func TestChangeMachineClassRestartFlagsAreMutuallyExclusive(t *testing.T) {
	isolate(t)
	_, _, err := run(t, "vm", "changemachineclass", "some-vm", "--restart", "--no-restart", "--yes")
	if err == nil {
		t.Fatal("expected an error for --restart together with --no-restart")
	}
}

// The machine class lives in the Vitistack control plane, so the --cluster
// escape hatch (which skips it entirely) cannot serve this command.
func TestChangeMachineClassRefusesClusterFlag(t *testing.T) {
	isolate(t)
	_, _, err := run(t, "vm", "changemachineclass", "some-vm", "--cluster", "kv1", "--yes")
	if err == nil {
		t.Fatal("expected an error when --cluster is given")
	}
	if !strings.Contains(err.Error(), "control plane") {
		t.Errorf("error should explain the control-plane requirement, got: %v", err)
	}
}

func TestChangeSummary(t *testing.T) {
	m := &vitiv1alpha1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "prod"},
		Spec:       vitiv1alpha1.MachineSpec{MachineClass: "small"},
	}
	class := &vitiv1alpha1.MachineClass{
		ObjectMeta: metav1.ObjectMeta{Name: "large"},
		Spec: vitiv1alpha1.MachineClassSpec{
			Memory: vitiv1alpha1.MachineClassMemorySpec{Quantity: resource.MustParse("8Gi")},
			CPU:    vitiv1alpha1.MachineClassCPUSpec{Cores: 4, Sockets: 2, Threads: 2},
		},
	}
	got := changeSummary(m, class, vm.DesiredResources(m, class))
	for _, want := range []string{"prod/web-1", "small", "large", "8Gi", "4"} {
		if !strings.Contains(got, want) {
			t.Errorf("changeSummary = %q, missing %q", got, want)
		}
	}
}

// A machine that never had a class still renders a readable summary.
func TestChangeSummaryWithoutCurrentClass(t *testing.T) {
	m := &vitiv1alpha1.Machine{ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "prod"}}
	class := &vitiv1alpha1.MachineClass{
		ObjectMeta: metav1.ObjectMeta{Name: "large"},
		Spec: vitiv1alpha1.MachineClassSpec{
			Memory: vitiv1alpha1.MachineClassMemorySpec{Quantity: resource.MustParse("8Gi")},
			CPU:    vitiv1alpha1.MachineClassCPUSpec{Cores: 4},
		},
	}
	got := changeSummary(m, class, vm.DesiredResources(m, class))
	if !strings.Contains(got, "large") {
		t.Errorf("changeSummary = %q, missing new class", got)
	}
}

func fakeAZ(t *testing.T, objs ...ctrlclient.Object) *kube.VitistackClient {
	t.Helper()
	s := runtime.NewScheme()
	if err := vitiv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	return &kube.VitistackClient{AZ: config.AvailabilityZone{Name: "az1"}, Ctrl: c}
}

// --no-restart stages sizes and never drains, restarts, or waits on nodes, so
// it must not require what only restarting needs: the guest cluster's
// kubeconfig secret, a local virtctl target, or virtctl itself. A cluster
// discovered from the control plane (no local kubeconfig entry) is exactly
// where staged-only rollouts are the documented workflow.
func TestPrepareRestartIsSkippedEntirelyWithNoRestart(t *testing.T) {
	az := fakeAZ(t)                      // deliberately no guest kubeconfig secret
	discovered := &kube.KubeVirtClient{} // no local entry: VirtctlTarget errors
	plan := &roll.Plan{
		Target: roll.Target{Cluster: &vitiv1alpha1.KubernetesCluster{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "c1"},
		}},
		ClusterID: "c1-id",
		Members: []roll.Member{{
			Machine: &vitiv1alpha1.Machine{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "wrk0"}},
			KV:      discovered,
		}},
	}
	cmd := &cobra.Command{}
	cmd.SetErr(&bytes.Buffer{})

	g, err := prepareRestart(context.Background(), cmd, az, plan, rolloutFlags{noRestart: true})
	if err != nil {
		t.Fatalf("prepareRestart with --no-restart = %v, want nil: staging needs no guest access", err)
	}
	if g != nil {
		t.Errorf("prepareRestart with --no-restart returned a guest client, want none")
	}
}

// Without --no-restart the same missing secret must still fail, before
// anything is written.
func TestPrepareRestartStillRequiresTheGuestSecret(t *testing.T) {
	az := fakeAZ(t)
	plan := &roll.Plan{
		Target: roll.Target{Cluster: &vitiv1alpha1.KubernetesCluster{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "c1"},
		}},
		ClusterID: "c1-id",
	}
	cmd := &cobra.Command{}
	cmd.SetErr(&bytes.Buffer{})

	if _, err := prepareRestart(context.Background(), cmd, az, plan, rolloutFlags{}); err == nil {
		t.Fatal("prepareRestart without a guest secret succeeded, want an error")
	}
}

// Naming the machine's current class must re-sync the VM, not exit early:
// a hand-edited spec.machineClass whose VM was never resized is exactly the
// drift this command exists to repair.
func TestChooseClassCurrentClassIsAResync(t *testing.T) {
	class := &vitiv1alpha1.MachineClass{
		ObjectMeta: metav1.ObjectMeta{Name: "large"},
		Spec: vitiv1alpha1.MachineClassSpec{
			Enabled: true,
			Memory:  vitiv1alpha1.MachineClassMemorySpec{Quantity: resource.MustParse("8Gi")},
			CPU:     vitiv1alpha1.MachineClassCPUSpec{Cores: 4},
		},
	}
	found := vm.Located{
		AZ: fakeAZ(t, class),
		Machine: &vitiv1alpha1.Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "prod"},
			Spec:       vitiv1alpha1.MachineSpec{MachineClass: "large"},
		},
	}
	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})

	got, err := chooseClass(cmd, context.Background(), found, "large")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Name != "large" {
		t.Fatalf("chooseClass returned %v, want the current class back for a re-sync", got)
	}
}

// A cluster discovered from the control plane has no local kubeconfig, so
// virtctl cannot restart it — the command must say so and leave the change
// pending rather than fail after already applying it, and must not hint at
// a retry command that would hit the same wall.
func TestMaybeRestartOnDiscoveredClusterExplainsInsteadOfFailing(t *testing.T) {
	kv := &kube.KubeVirtClient{Cluster: config.Cluster{Name: "admin@kv1"}}
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(""))

	if err := maybeRestart(cmd, kv, "prod", "web-1", true, false); err != nil {
		t.Fatalf("maybeRestart error = %v, want nil (the resize is already applied)", err)
	}
	if !strings.Contains(out.String(), "config add") {
		t.Errorf("output does not tell the user how to make the cluster restartable:\n%s", out.String())
	}
}

func TestChangeMachineClassPoolFlagsAreMutuallyExclusive(t *testing.T) {
	isolate(t)
	_, _, err := run(t, "vm", "changemachineclass", "c1", "--nodepool", "w1", "--controlplane", "--yes")
	if err == nil {
		t.Fatal("expected an error for --nodepool together with --controlplane")
	}
}

func TestChangeMachineClassHelpDocumentsPoolFlags(t *testing.T) {
	out, _, err := run(t, "vm", "changemachineclass", "--help")
	if err != nil {
		t.Fatalf("help error = %v", err)
	}
	for _, want := range []string{"--nodepool", "--controlplane", "--drain-timeout"} {
		if !strings.Contains(out, want) {
			t.Errorf("help does not document %q:\n%s", want, out)
		}
	}
}

// A cluster-owned machine's class comes from the KubernetesCluster topology;
// patching the Machine alone would be reverted by the provider operator, so
// a different class must be refused with the pool-mode command to use.
func TestRefuseOwnedMachine(t *testing.T) {
	owned := &vitiv1alpha1.Machine{ObjectMeta: metav1.ObjectMeta{
		Name: "c1-wrk0", Namespace: "ns",
		Annotations:     map[string]string{roll.AnnotationNodepool: "workers1"},
		OwnerReferences: []metav1.OwnerReference{{Kind: "KubernetesCluster", Name: "c1"}},
	}}
	err := refuseOwned(owned, "medium", "large")
	if err == nil {
		t.Fatal("want refusal for an owned machine changing class")
	}
	if !strings.Contains(err.Error(), "--nodepool workers1") || !strings.Contains(err.Error(), "c1") {
		t.Errorf("refusal does not name the pool-mode fix: %v", err)
	}

	if err := refuseOwned(owned, "large", "large"); err != nil {
		t.Errorf("same-class re-sync must stay allowed, got %v", err)
	}

	cp := owned.DeepCopy()
	cp.Annotations = nil
	cp.Labels = map[string]string{roll.LabelNodeRole: roll.RoleControlPlane}
	if err := refuseOwned(cp, "medium", "large"); err == nil || !strings.Contains(err.Error(), "--controlplane") {
		t.Errorf("control-plane refusal does not point at --controlplane: %v", err)
	}

	standalone := &vitiv1alpha1.Machine{ObjectMeta: metav1.ObjectMeta{Name: "solo", Namespace: "ns"}}
	if err := refuseOwned(standalone, "medium", "large"); err != nil {
		t.Errorf("standalone machine must not be refused, got %v", err)
	}
}

func TestResolveRollTarget(t *testing.T) {
	targets := []roll.Target{
		{Kind: roll.KindControlPlane, Class: "medium", Replicas: 1},
		{Kind: roll.KindNodePool, Pool: "workers1", Class: "medium", Replicas: 3},
	}
	got, err := resolveRollTarget(targets, "workers1", false)
	if err != nil || got.Pool != "workers1" {
		t.Fatalf("got %+v, %v", got, err)
	}
	got, err = resolveRollTarget(targets, "", true)
	if err != nil || got.Kind != roll.KindControlPlane {
		t.Fatalf("got %+v, %v", got, err)
	}
	_, err = resolveRollTarget(targets, "nope", false)
	if err == nil || !strings.Contains(err.Error(), "workers1") {
		t.Fatalf("unknown pool error should list the pools, got %v", err)
	}
}

func TestRollSummary(t *testing.T) {
	kc := &vitiv1alpha1.KubernetesCluster{ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "ns"}}
	class := &vitiv1alpha1.MachineClass{
		ObjectMeta: metav1.ObjectMeta{Name: "medium"},
		Spec: vitiv1alpha1.MachineClassSpec{
			Memory: vitiv1alpha1.MachineClassMemorySpec{Quantity: resource.MustParse("16Gi")},
			CPU:    vitiv1alpha1.MachineClassCPUSpec{Cores: 4, Sockets: 1, Threads: 1},
		},
	}
	plan := &roll.Plan{
		Target: roll.Target{Cluster: kc, Kind: roll.KindNodePool, Pool: "workers1", Class: "large"},
		Class:  class,
		Members: []roll.Member{
			{Machine: &vitiv1alpha1.Machine{ObjectMeta: metav1.ObjectMeta{Name: "w0"}}},
			{Machine: &vitiv1alpha1.Machine{ObjectMeta: metav1.ObjectMeta{Name: "w1"}}},
		},
	}
	got := rollSummary(plan)
	for _, want := range []string{"2 machines", "large", "medium", "16Gi", "workers1", "c1"} {
		if !strings.Contains(got, want) {
			t.Errorf("rollSummary = %q, missing %q", got, want)
		}
	}
	if strings.Contains(got, "unavailable") {
		t.Errorf("worker rollout should not warn about API outage: %q", got)
	}

	cpPlan := &roll.Plan{
		Target: roll.Target{Cluster: kc, Kind: roll.KindControlPlane, Class: "large", Replicas: 1},
		Class:  class,
		Members: []roll.Member{
			{Machine: &vitiv1alpha1.Machine{ObjectMeta: metav1.ObjectMeta{Name: "ctp0"}}},
		},
	}
	if got := rollSummary(cpPlan); !strings.Contains(got, "unavailable") {
		t.Errorf("single-replica controlplane must warn about the API outage: %q", got)
	}
}

// The virtctl binary must be checked BEFORE any disruption: discovering it is
// missing after the control plane is already drained is exactly the stranding
// preflight exists to prevent.
// The cluster-first flow: --rollout declares "I want to roll a cluster's
// control plane or a nodepool" without naming anything up front — the cluster
// is picked interactively, then the target, then the class.
func TestChangeMachineClassHelpDocumentsRollout(t *testing.T) {
	out, _, err := run(t, "vm", "changemachineclass", "--help")
	if err != nil {
		t.Fatalf("run(vm changemachineclass --help) error = %v", err)
	}
	if !strings.Contains(out, "--rollout") {
		t.Errorf("help does not document --rollout:\n%s", out)
	}
}

// The picker takes over the terminal, so a piped or CI invocation must be
// told to name its cluster rather than left hanging on a UI that cannot be
// drawn — and it must fail before opening any zone connection.
func TestRolloutWithoutNameRefusedOffTerminal(t *testing.T) {
	isolate(t)
	_, _, err := run(t, "vm", "changemachineclass", "--rollout", "--yes")
	if err == nil {
		t.Fatal("expected an error for --rollout without a name off-terminal")
	}
	if !strings.Contains(err.Error(), "cluster") {
		t.Errorf("error should tell the user to name a KubernetesCluster, got: %v", err)
	}
}

// One broken zone must not empty the cluster picker — clusters in healthy
// zones are listed and the broken zone is warned about, like every other
// fleet listing.
func TestListClustersToleratesBrokenZones(t *testing.T) {
	broken := brokenAZ(t, "az-broken")
	healthy := fakeAZ(t,
		&vitiv1alpha1.KubernetesCluster{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "c1"}},
		&vitiv1alpha1.KubernetesCluster{ObjectMeta: metav1.ObjectMeta{Namespace: "other", Name: "c2"}},
	)

	var warned []error
	hits := listClusters(context.Background(),
		[]*kube.VitistackClient{broken, healthy}, "",
		func(e error) { warned = append(warned, e) })
	if len(hits) != 2 {
		t.Fatalf("hits = %d, want the healthy zone's two clusters", len(hits))
	}
	if len(warned) != 1 {
		t.Errorf("warned %v, want exactly the broken zone reported", warned)
	}

	// An explicit namespace narrows the listing.
	hits = listClusters(context.Background(),
		[]*kube.VitistackClient{healthy}, "ns", func(error) {})
	if len(hits) != 1 || hits[0].cluster.Name != "c1" {
		t.Errorf("hits = %v, want only ns/c1", hits)
	}
}

// brokenAZ is a zone whose API refuses every read — a missing CRD, denied
// RBAC, or an unreachable API server all land here.
func brokenAZ(t *testing.T, name string) *kube.VitistackClient {
	t.Helper()
	s := runtime.NewScheme()
	if err := vitiv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	boom := func() error { return errors.New("zone " + name + ": forbidden") }
	c := fake.NewClientBuilder().WithScheme(s).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, ctrlclient.WithWatch, ctrlclient.ObjectKey, ctrlclient.Object, ...ctrlclient.GetOption) error {
			return boom()
		},
		List: func(context.Context, ctrlclient.WithWatch, ctrlclient.ObjectList, ...ctrlclient.ListOption) error {
			return boom()
		},
	}).Build()
	return &kube.VitistackClient{AZ: config.AvailabilityZone{Name: name}, Ctrl: c}
}

// One broken zone (missing CRD, denied RBAC, API down) must not block rolling
// a cluster that lives in a healthy zone — machine listings already tolerate
// broken zones with a warning, and the cluster path must match.
func TestScanZonesForClusterToleratesBrokenZones(t *testing.T) {
	broken := brokenAZ(t, "az-broken")
	healthy := fakeAZ(t, &vitiv1alpha1.KubernetesCluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "c1"},
	})

	var warned []error
	az, targets, err := scanZonesForCluster(context.Background(),
		[]*kube.VitistackClient{broken, healthy}, "", "c1",
		func(e error) { warned = append(warned, e) })
	if err != nil {
		t.Fatalf("scanZonesForCluster = %v, want the cluster found in the healthy zone", err)
	}
	if az != healthy || len(targets) == 0 {
		t.Errorf("az = %v targets = %d, want the healthy zone's targets", az, len(targets))
	}
	if len(warned) != 1 {
		t.Errorf("warned %v, want exactly the broken zone reported", warned)
	}
}

// A cluster found nowhere must keep reporting ErrClusterNotFound even when a
// zone was unreadable, so a typo'd machine name falls back to its original
// "no machine named X" guidance instead of surfacing the broken zone's error.
func TestScanZonesForClusterNotFoundSurvivesBrokenZones(t *testing.T) {
	broken := brokenAZ(t, "az-broken")
	healthy := fakeAZ(t)

	_, _, err := scanZonesForCluster(context.Background(),
		[]*kube.VitistackClient{broken, healthy}, "", "no-such-cluster", func(error) {})
	if !errors.Is(err, roll.ErrClusterNotFound) {
		t.Fatalf("err = %v, want ErrClusterNotFound so the machine-name fallback keeps working", err)
	}
	if !strings.Contains(err.Error(), "az-broken") {
		t.Errorf("err = %v, want the unreadable zone named — the cluster may live there", err)
	}
}

// --restart pre-answers the per-VM restart question in single-machine mode;
// it must NOT stand in for --yes on a whole-pool rollout, which cordons,
// drains and reboots every node in the pool. Only an explicit --yes may skip
// that confirmation.
func TestRolloutOptionsOnlyYesSkipsConfirmation(t *testing.T) {
	opts := rolloutOptions("large", "workers1", false, time.Minute, true, false, false)
	if opts.skipConfirm {
		t.Error("--restart alone skipped the whole-pool rollout confirmation")
	}
	opts = rolloutOptions("large", "workers1", false, time.Minute, false, false, true)
	if !opts.skipConfirm {
		t.Error("--yes did not skip the confirmation")
	}
}

func TestEnsureVirtctl(t *testing.T) {
	orig := virtctl.Binary
	virtctl.Binary = "definitely-not-a-real-binary-anywhere"
	defer func() { virtctl.Binary = orig }()

	if err := ensureVirtctl(); err == nil {
		t.Fatal("want an error when virtctl is missing and a restart is planned")
	}
}
