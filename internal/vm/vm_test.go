package vm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubevirtv1 "kubevirt.io/api/core/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"

	"github.com/vitistack/vitictl-kubevirt/internal/config"
	"github.com/vitistack/vitictl-kubevirt/internal/kube"
)

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := kubevirtv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := vitiv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func kvClient(t *testing.T, objs ...ctrlclient.Object) *kube.KubeVirtClient {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(objs...).Build()
	return &kube.KubeVirtClient{Cluster: config.Cluster{Name: "kv-test"}, Ctrl: c}
}

func azClient(t *testing.T, objs ...ctrlclient.Object) *kube.VitistackClient {
	t.Helper()
	return namedAZ(t, "az1", objs...)
}

func namedKV(t *testing.T, name string, objs ...ctrlclient.Object) *kube.KubeVirtClient {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(objs...).Build()
	return &kube.KubeVirtClient{Cluster: config.Cluster{Name: name}, Ctrl: c}
}

func namedAZ(t *testing.T, name string, objs ...ctrlclient.Object) *kube.VitistackClient {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(objs...).Build()
	return &kube.VitistackClient{AZ: config.AvailabilityZone{Name: name}, Ctrl: c}
}

// machineOn is a Machine that names the KubevirtConfig it was provisioned
// through, the way the operators stamp it.
func machineOn(namespace, name, configName string) *vitiv1alpha1.Machine {
	m := machine(namespace, name)
	m.Annotations = map[string]string{kube.AnnotationKubevirtConfig: configName}
	return m
}

// entryFor finds the row for a zone, so assertions do not depend on sort order.
func entryFor(t *testing.T, entries []Entry, az string) Entry {
	t.Helper()
	for _, e := range entries {
		if e.AZ == az {
			return e
		}
	}
	t.Fatalf("no entry for availability zone %q", az)
	return Entry{}
}

// fixedResolver answers every zone with the same KubeVirt cluster — the
// single-cluster world the older tests were written for.
type fixedResolver struct{ kv *kube.KubeVirtClient }

func (r fixedResolver) For(context.Context, *kube.VitistackClient, string) (*kube.KubeVirtClient, error) {
	return r.kv, nil
}

// mapResolver answers each zone with its own cluster, keyed "az/configName",
// and reports a miss as an error the way a real discovery failure would.
type mapResolver struct {
	byKey map[string]*kube.KubeVirtClient
	// asked records every lookup, so tests can assert a cluster is contacted
	// once per group rather than once per machine.
	asked []string
}

func (r *mapResolver) For(_ context.Context, az *kube.VitistackClient, configName string) (*kube.KubeVirtClient, error) {
	key := az.AZ.Name + "/" + configName
	r.asked = append(r.asked, key)
	if kv, ok := r.byKey[key]; ok {
		return kv, nil
	}
	return nil, fmt.Errorf("no KubeVirt cluster for %s", key)
}

// listErrClient stands in for a zone whose Machine listing fails, so the
// distinction Collect draws between kinds of failure can be tested without a
// live apiserver.
type listErrClient struct {
	ctrlclient.Client
	err error
}

func (c listErrClient) List(context.Context, ctrlclient.ObjectList, ...ctrlclient.ListOption) error {
	return c.err
}

func failingAZ(name string, err error) *kube.VitistackClient {
	return &kube.VitistackClient{
		AZ:   config.AvailabilityZone{Name: name},
		Ctrl: listErrClient{err: err},
	}
}

// noCRDErr reproduces what controller-runtime's RESTMapper returns when the
// vitistack CRDs are absent: discovery 404s for the whole group version.
func noCRDErr() error {
	e := apiutil.ErrResourceDiscoveryFailed{
		vitiv1alpha1.GroupVersion: apierrors.NewNotFound(
			schema.GroupResource{Group: vitiv1alpha1.GroupVersion.Group}, ""),
	}
	return &e
}

func machine(namespace, name string) *vitiv1alpha1.Machine {
	return &vitiv1alpha1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}
}

// virtualMachine builds a VM whose own name may differ from the Machine it
// came from — the case the label exists to handle.
func virtualMachine(namespace, vmName, sourceMachine string) *kubevirtv1.VirtualMachine {
	vm := &kubevirtv1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: vmName, Namespace: namespace},
	}
	if sourceMachine != "" {
		vm.Labels = map[string]string{LabelSourceMachine: sourceMachine}
	}
	return vm
}

func instance(namespace, name, node string, ips ...string) *kubevirtv1.VirtualMachineInstance {
	vmi := &kubevirtv1.VirtualMachineInstance{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}
	vmi.Status.NodeName = node
	vmi.Status.Phase = kubevirtv1.Running
	for _, ip := range ips {
		vmi.Status.Interfaces = append(vmi.Status.Interfaces, kubevirtv1.VirtualMachineInstanceNetworkInterface{IP: ip})
	}
	return vmi
}

// The operator names the VM independently of the Machine, so the join has to
// go through the label. Matching on name alone would drop this pairing.
func TestCollectPairsThroughTheSourceMachineLabel(t *testing.T) {
	kv := kvClient(t,
		virtualMachine("prod", "vm-abc123", "web-01"),
		instance("prod", "vm-abc123", "node-3", "10.0.0.5"),
	)
	az := azClient(t, machine("prod", "web-01"))

	entries, err := Collect(context.Background(), []*kube.VitistackClient{az}, fixedResolver{kv}, "", nil)
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	e := entries[0]
	if e.VM == nil {
		t.Fatal("Machine was not paired with its VM")
	}
	if e.VMName() != "vm-abc123" {
		t.Errorf("VMName() = %q, want the VM's own name", e.VMName())
	}
	if e.Name() != "web-01" {
		t.Errorf("Name() = %q, want the Machine name", e.Name())
	}
	if e.VMI == nil || e.Node() != "node-3" {
		t.Errorf("instance not paired: node = %q", e.Node())
	}
	if got := strings.Join(e.IPs(), ","); got != "10.0.0.5" {
		t.Errorf("IPs() = %q, want the VMI address", got)
	}
}

// A hand-made VM, or one predating the label, shares the Machine's name.
func TestCollectFallsBackToMatchingOnName(t *testing.T) {
	kv := kvClient(t, virtualMachine("prod", "web-01", ""))
	az := azClient(t, machine("prod", "web-01"))

	entries, err := Collect(context.Background(), []*kube.VitistackClient{az}, fixedResolver{kv}, "", nil)
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if entries[0].VM == nil {
		t.Error("an unlabelled VM sharing the Machine name should still pair")
	}
}

// A Machine with no VM is the interesting failure case — it must be listed,
// not silently dropped, because that is how a failed provision looks.
func TestCollectKeepsMachinesWithNoVirtualMachine(t *testing.T) {
	kv := kvClient(t)
	az := azClient(t, machine("prod", "orphan"))

	entries, err := Collect(context.Background(), []*kube.VitistackClient{az}, fixedResolver{kv}, "", nil)
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want the unbacked machine listed", len(entries))
	}
	if entries[0].VM != nil || entries[0].VMI != nil {
		t.Error("expected no VM/VMI")
	}
	if entries[0].VMName() != "orphan" {
		t.Errorf("VMName() = %q, want the Machine name as a fallback", entries[0].VMName())
	}
}

// A cluster configured as an availability zone but without the vitistack CRDs
// holds no Machines by definition — a management cluster that has not been
// provisioned yet looks exactly like this. It is a normal state, so it must be
// skipped in silence rather than warned about on every listing.
func TestCollectSkipsZonesWithoutVitistackCRDs(t *testing.T) {
	kv := kvClient(t)
	good := azClient(t, machine("prod", "web-01"))
	bare := failingAZ("no-crds", noCRDErr())

	var warnings []error
	entries, err := Collect(context.Background(),
		[]*kube.VitistackClient{bare, good}, fixedResolver{kv}, "",
		func(e error) { warnings = append(warnings, e) })
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("a zone without vitistack CRDs warned: %v", warnings)
	}
	if len(entries) != 1 || entries[0].Name() != "web-01" {
		t.Errorf("got %d entries, want only the machine from the provisioned zone", len(entries))
	}
}

// The silence above must be narrow: an unreachable or forbidden zone is a real
// problem and hiding it would turn a broken listing into a short one.
func TestCollectStillWarnsOnRealListFailures(t *testing.T) {
	kv := kvClient(t)
	good := azClient(t, machine("prod", "web-01"))

	for name, listErr := range map[string]error{
		"forbidden": apierrors.NewForbidden(
			schema.GroupResource{Group: "vitistack.io", Resource: "machines"}, "", errors.New("nope")),
		"unreachable": errors.New("dial tcp 10.0.0.1:6443: connect: connection refused"),
		"timeout":     context.DeadlineExceeded,
	} {
		t.Run(name, func(t *testing.T) {
			var warnings []error
			entries, err := Collect(context.Background(),
				[]*kube.VitistackClient{failingAZ("broken", listErr), good}, fixedResolver{kv}, "",
				func(e error) { warnings = append(warnings, e) })
			if err != nil {
				t.Fatalf("Collect() error = %v", err)
			}
			if len(warnings) != 1 {
				t.Fatalf("got %d warnings, want 1 for a real failure", len(warnings))
			}
			if !strings.Contains(warnings[0].Error(), "broken") {
				t.Errorf("warning %q should name the zone", warnings[0])
			}
			if len(entries) != 1 {
				t.Errorf("got %d entries, want the healthy zone still listed", len(entries))
			}
		})
	}
}

// The whole point of the resolver: a fleet spans one KubeVirt cluster per
// zone, and every zone's machines must be paired against their own.
func TestCollectPairsEachZoneWithItsOwnCluster(t *testing.T) {
	westKV := namedKV(t, "west-kv",
		virtualMachine("prod", "vm-west", "web-01"),
		instance("prod", "vm-west", "node-west", "10.0.0.1"))
	eastKV := namedKV(t, "east-kv",
		virtualMachine("prod", "vm-east", "web-02"),
		instance("prod", "vm-east", "node-east", "10.0.0.2"))

	west := namedAZ(t, "west", machine("prod", "web-01"))
	east := namedAZ(t, "east", machine("prod", "web-02"))
	r := &mapResolver{byKey: map[string]*kube.KubeVirtClient{"west/": westKV, "east/": eastKV}}

	entries, err := Collect(context.Background(),
		[]*kube.VitistackClient{west, east}, r, "", nil)
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	for _, tc := range []struct{ az, node, cluster string }{
		{"west", "node-west", "west-kv"},
		{"east", "node-east", "east-kv"},
	} {
		e := entryFor(t, entries, tc.az)
		if e.Node() != tc.node {
			t.Errorf("%s: Node() = %q, want %q", tc.az, e.Node(), tc.node)
		}
		if e.Cluster != tc.cluster {
			t.Errorf("%s: Cluster = %q, want %q", tc.az, e.Cluster, tc.cluster)
		}
	}
}

// Two zones may hold same-named machines in same-named namespaces. Indexing
// the KubeVirt side per group rather than globally is what stops one zone's
// machine from being paired with another zone's VM and reporting its node.
func TestCollectNeverPairsAcrossZones(t *testing.T) {
	westKV := namedKV(t, "west-kv",
		virtualMachine("prod", "vm-west", "web-01"),
		instance("prod", "vm-west", "node-west", "10.0.0.1"))
	eastKV := namedKV(t, "east-kv",
		virtualMachine("prod", "vm-east", "web-01"),
		instance("prod", "vm-east", "node-east", "10.0.0.2"))

	// Identical namespace and machine name in both zones.
	west := namedAZ(t, "west", machine("prod", "web-01"))
	east := namedAZ(t, "east", machine("prod", "web-01"))
	r := &mapResolver{byKey: map[string]*kube.KubeVirtClient{"west/": westKV, "east/": eastKV}}

	entries, err := Collect(context.Background(),
		[]*kube.VitistackClient{west, east}, r, "", nil)
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if got := entryFor(t, entries, "west").Node(); got != "node-west" {
		t.Errorf("west machine landed on %q, want node-west", got)
	}
	if got := entryFor(t, entries, "east").Node(); got != "node-east" {
		t.Errorf("east machine landed on %q, want node-east", got)
	}
}

// A zone fronting two KubeVirt clusters resolves each machine through the
// KubevirtConfig it names, which is why the lookup is per-machine.
func TestCollectResolvesEachMachineToTheClusterItNames(t *testing.T) {
	kvA := namedKV(t, "kv-a",
		virtualMachine("prod", "vm-a", "m-a"), instance("prod", "vm-a", "node-a"))
	kvB := namedKV(t, "kv-b",
		virtualMachine("prod", "vm-b", "m-b"), instance("prod", "vm-b", "node-b"))

	az := namedAZ(t, "az1",
		machineOn("prod", "m-a", "cfg-a"),
		machineOn("prod", "m-b", "cfg-b"))
	r := &mapResolver{byKey: map[string]*kube.KubeVirtClient{"az1/cfg-a": kvA, "az1/cfg-b": kvB}}

	entries, err := Collect(context.Background(), []*kube.VitistackClient{az}, r, "", nil)
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	want := map[string]string{"m-a": "node-a", "m-b": "node-b"}
	for _, e := range entries {
		if got := e.Node(); got != want[e.Name()] {
			t.Errorf("%s: Node() = %q, want %q", e.Name(), got, want[e.Name()])
		}
	}
}

// A cluster must be contacted once for the whole group. Resolving per machine
// would turn a fleet listing into thousands of connections.
func TestCollectResolvesOncePerGroup(t *testing.T) {
	kv := namedKV(t, "kv")
	az := namedAZ(t, "az1",
		machine("prod", "a"), machine("prod", "b"), machine("prod", "c"))
	r := &mapResolver{byKey: map[string]*kube.KubeVirtClient{"az1/": kv}}

	if _, err := Collect(context.Background(), []*kube.VitistackClient{az}, r, "", nil); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if len(r.asked) != 1 {
		t.Errorf("resolved %d times for 3 machines sharing a cluster, want 1: %v", len(r.asked), r.asked)
	}
}

// An unreachable KubeVirt cluster costs the machines their live state but not
// their rows — they exist regardless, and hiding them behind a transient
// outage would misreport the fleet as smaller than it is.
func TestCollectKeepsMachinesWhenTheirClusterIsUnreachable(t *testing.T) {
	az := namedAZ(t, "az1", machine("prod", "a"), machine("prod", "b"))
	r := &mapResolver{} // every lookup misses

	var warnings []error
	entries, err := Collect(context.Background(),
		[]*kube.VitistackClient{az}, r, "", func(e error) { warnings = append(warnings, e) })
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want both machines still listed", len(entries))
	}
	if len(warnings) != 1 {
		t.Fatalf("got %d warnings, want exactly 1 for the group: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0].Error(), "az1") {
		t.Errorf("warning %q should name the zone", warnings[0])
	}
	for _, e := range entries {
		if e.VM != nil || e.Cluster != "" {
			t.Errorf("%s: expected no live state, got VM=%v cluster=%q", e.Name(), e.VM, e.Cluster)
		}
	}
}

func TestFindMachinesReturnsTheZoneAndConfig(t *testing.T) {
	west := namedAZ(t, "west", machine("prod", "web-01"))
	east := namedAZ(t, "east", machineOn("prod", "web-02", "cfg-east"))

	got := FindMachines(context.Background(),
		[]*kube.VitistackClient{west, east}, "web-02", "", nil)
	if len(got) != 1 {
		t.Fatalf("got %d matches, want 1", len(got))
	}
	if got[0].AZ.AZ.Name != "east" {
		t.Errorf("zone = %q, want east", got[0].AZ.AZ.Name)
	}
	if got[0].ConfigName != "cfg-east" {
		t.Errorf("ConfigName = %q, want cfg-east", got[0].ConfigName)
	}
	if want := "east/prod/web-02"; got[0].Describe() != want {
		t.Errorf("Describe() = %q, want %q", got[0].Describe(), want)
	}
}

// A name in two zones yields both, so the caller can offer a choice instead of
// picking one at random.
func TestFindMachinesReturnsEveryMatch(t *testing.T) {
	west := namedAZ(t, "west", machine("prod", "web-01"))
	east := namedAZ(t, "east", machine("prod", "web-01"))

	got := FindMachines(context.Background(),
		[]*kube.VitistackClient{west, east}, "web-01", "", nil)
	if len(got) != 2 {
		t.Fatalf("got %d matches, want both zones", len(got))
	}
	// Stable order, so a picker's rows do not shuffle between runs.
	if got[0].Describe() != "east/prod/web-01" || got[1].Describe() != "west/prod/web-01" {
		t.Errorf("matches are not sorted: %q, %q", got[0].Describe(), got[1].Describe())
	}
}

// An empty name lists the fleet, which is what the picker shows.
func TestFindMachinesWithNoNameListsEverything(t *testing.T) {
	west := namedAZ(t, "west", machine("prod", "a"), machine("prod", "b"))
	east := namedAZ(t, "east", machine("test", "c"))

	got := FindMachines(context.Background(), []*kube.VitistackClient{west, east}, "", "", nil)
	if len(got) != 3 {
		t.Fatalf("got %d machines, want all 3", len(got))
	}
}

func TestFindMachinesNarrowsByNamespace(t *testing.T) {
	az := namedAZ(t, "az1", machine("prod", "a"), machine("test", "b"))

	got := FindMachines(context.Background(), []*kube.VitistackClient{az}, "", "test", nil)
	if len(got) != 1 || got[0].Machine.Name != "b" {
		t.Fatalf("got %d machines, want only the one in test", len(got))
	}
}

func TestFindMachinesReturnsNothingForAnUnknownName(t *testing.T) {
	az := namedAZ(t, "az1", machine("prod", "web-01"))

	if got := FindMachines(context.Background(), []*kube.VitistackClient{az}, "vm-abc123", "", nil); len(got) != 0 {
		t.Errorf("got %d matches for an unknown name, want none", len(got))
	}
}

// Describing a machine must not pay for a fleet-wide join, and a machine with
// no VM is a state to show rather than an error to raise.
func TestAttachFillsOneEntryAndToleratesNoVM(t *testing.T) {
	kv := namedKV(t, "kv",
		virtualMachine("prod", "vm-abc123", "web-01"),
		instance("prod", "vm-abc123", "node-3", "10.0.0.5"))

	paired := Entry{Machine: machine("prod", "web-01")}
	if err := Attach(context.Background(), kv, &paired); err != nil {
		t.Fatalf("Attach() error = %v", err)
	}
	if paired.VM == nil || paired.VMName() != "vm-abc123" {
		t.Errorf("VM not attached: %+v", paired.VM)
	}
	if paired.Node() != "node-3" {
		t.Errorf("Node() = %q, want node-3", paired.Node())
	}

	orphan := Entry{Machine: machine("prod", "nothing-here")}
	if err := Attach(context.Background(), kv, &orphan); err != nil {
		t.Fatalf("Attach() on a machine with no VM = %v, want no error", err)
	}
	if orphan.VM != nil || orphan.VMI != nil {
		t.Error("expected no VM or VMI for an unbacked machine")
	}
}

// A VM that exists but is not running has no instance, which is ordinary.
func TestAttachToleratesAStoppedVM(t *testing.T) {
	kv := namedKV(t, "kv", virtualMachine("prod", "web-01", ""))

	e := Entry{Machine: machine("prod", "web-01")}
	if err := Attach(context.Background(), kv, &e); err != nil {
		t.Fatalf("Attach() error = %v", err)
	}
	if e.VM == nil {
		t.Fatal("expected the VM to attach")
	}
	if e.VMI != nil {
		t.Error("expected no instance for a VM that is not running")
	}
}

// Talos is what the operators record for a factory.talos.dev image; the check
// must not care about case, and must cope with the --cluster path where no
// Machine was ever fetched.
func TestIsTalos(t *testing.T) {
	talos := machine("prod", "wrk0")
	talos.Spec.OS.Distribution = "talos"
	if !IsTalos(talos) {
		t.Error("a talos machine was not recognised")
	}

	shouty := machine("prod", "wrk0")
	shouty.Spec.OS.Distribution = "Talos"
	if !IsTalos(shouty) {
		t.Error("the distribution check should ignore case")
	}

	ubuntu := machine("prod", "wrk0")
	ubuntu.Spec.OS.Distribution = "ubuntu"
	if IsTalos(ubuntu) {
		t.Error("ubuntu was reported as talos")
	}

	if IsTalos(machine("prod", "wrk0")) {
		t.Error("a machine with no recorded distribution was reported as talos")
	}
	if IsTalos(nil) {
		t.Error("a nil machine — the --cluster path — was reported as talos")
	}
}

func TestListVMsIsSortedByNamespaceThenName(t *testing.T) {
	kv := namedKV(t, "kv",
		virtualMachine("b-ns", "z", ""),
		virtualMachine("a-ns", "b", ""),
		virtualMachine("a-ns", "a", ""))

	got, err := ListVMs(context.Background(), kv, "")
	if err != nil {
		t.Fatalf("ListVMs() error = %v", err)
	}
	var names []string
	for i := range got {
		names = append(names, got[i].Namespace+"/"+got[i].Name)
	}
	if want := "a-ns/a,a-ns/b,b-ns/z"; strings.Join(names, ",") != want {
		t.Errorf("order = %v, want %v", names, want)
	}
}

func TestCollectSortsByNamespaceThenName(t *testing.T) {
	kv := kvClient(t)
	az := azClient(t,
		machine("b-ns", "z"), machine("a-ns", "b"), machine("a-ns", "a"),
	)
	entries, err := Collect(context.Background(), []*kube.VitistackClient{az}, fixedResolver{kv}, "", nil)
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Namespace()+"/"+e.Name())
	}
	if want := "a-ns/a,a-ns/b,b-ns/z"; strings.Join(got, ",") != want {
		t.Errorf("order = %v, want %v", got, want)
	}
}

// Actions resolve against the KubeVirt cluster alone, by label first.
func TestResolveVMPrefersTheLabel(t *testing.T) {
	kv := kvClient(t,
		virtualMachine("prod", "vm-abc123", "web-01"),
		virtualMachine("prod", "unrelated", ""),
	)
	got, err := ResolveVM(context.Background(), kv, "web-01", "")
	if err != nil {
		t.Fatalf("ResolveVM() error = %v", err)
	}
	if got.Name != "vm-abc123" {
		t.Errorf("ResolveVM() = %q, want vm-abc123", got.Name)
	}
}

func TestResolveVMFallsBackToTheVMName(t *testing.T) {
	kv := kvClient(t, virtualMachine("prod", "vm-abc123", "web-01"))

	got, err := ResolveVM(context.Background(), kv, "vm-abc123", "")
	if err != nil {
		t.Fatalf("ResolveVM() error = %v", err)
	}
	if got.Name != "vm-abc123" {
		t.Errorf("ResolveVM() = %q, want the VM addressed by its own name", got.Name)
	}
}

func TestResolveVMUnknownNameIsActionable(t *testing.T) {
	kv := kvClient(t)
	_, err := ResolveVM(context.Background(), kv, "nope", "")
	if err == nil {
		t.Fatal("expected an error for an unknown name")
	}
	for _, want := range []string{"nope", "kv-test"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

// The same Machine name in two namespaces must never be acted on blind.
func TestResolveVMAmbiguousNameIsRefused(t *testing.T) {
	kv := kvClient(t,
		virtualMachine("prod", "vm-a", "web-01"),
		virtualMachine("test", "vm-b", "web-01"),
	)
	_, err := ResolveVM(context.Background(), kv, "web-01", "")
	if err == nil {
		t.Fatal("expected an error for an ambiguous name")
	}
	if !strings.Contains(err.Error(), "--namespace") {
		t.Errorf("error %q should suggest narrowing by namespace", err)
	}

	// Narrowing resolves it.
	got, err := ResolveVM(context.Background(), kv, "web-01", "test")
	if err != nil {
		t.Fatalf("ResolveVM() with a namespace error = %v", err)
	}
	if got.Name != "vm-b" {
		t.Errorf("ResolveVM() = %q, want vm-b", got.Name)
	}
}

func TestEntryStatusPrefersKubeVirtThenFallsBack(t *testing.T) {
	m := machine("prod", "web-01")
	m.Status.Phase = "Provisioned"

	e := Entry{Machine: m}
	if got := e.Status(); got != "Provisioned" {
		t.Errorf("with no VM, Status() = %q, want the Machine phase", got)
	}

	vmObj := virtualMachine("prod", "vm-1", "web-01")
	vmObj.Status.PrintableStatus = kubevirtv1.VirtualMachineStatusRunning
	e.VM = vmObj
	if got := e.Status(); got != string(kubevirtv1.VirtualMachineStatusRunning) {
		t.Errorf("Status() = %q, want KubeVirt's printable status to win", got)
	}
}

func TestEntryReady(t *testing.T) {
	e := Entry{Machine: machine("prod", "web-01")}
	if got := e.Ready(); got != "" {
		t.Errorf("Ready() with no VM = %q, want empty", got)
	}

	vmObj := virtualMachine("prod", "vm-1", "web-01")
	vmObj.Status.Conditions = []kubevirtv1.VirtualMachineCondition{
		{Type: kubevirtv1.VirtualMachineReady, Status: "True"},
	}
	e.VM = vmObj
	if got := e.Ready(); got != "True" {
		t.Errorf("Ready() = %q, want True", got)
	}
}

// With nothing running, the Machine's own addresses are the best available.
func TestEntryIPsFallBackToTheMachine(t *testing.T) {
	m := machine("prod", "web-01")
	m.Status.IPAddresses = []string{"10.1.2.3", "10.1.2.3"}

	e := Entry{Machine: m}
	got := e.IPs()
	if len(got) != 1 || got[0] != "10.1.2.3" {
		t.Errorf("IPs() = %v, want the deduplicated machine address", got)
	}
}

func TestOneRefusesAnAmbiguousMatch(t *testing.T) {
	entries := []Entry{
		{AZ: "az1", Machine: machine("prod", "web-01")},
		{AZ: "az2", Machine: machine("test", "web-01")},
	}
	if _, err := One(entries, "web-01", ""); err == nil {
		t.Fatal("expected an error for an ambiguous match")
	}
	got, err := One(entries, "web-01", "test")
	if err != nil {
		t.Fatalf("One() error = %v", err)
	}
	if got.AZ != "az2" {
		t.Errorf("One() = %q, want the az2 entry", got.AZ)
	}
}
