// Package vm joins Vitistack Machines with the KubeVirt VirtualMachines and
// VirtualMachineInstances that back them.
//
// The two halves live in different clusters: a Machine is a Vitistack
// control-plane resource describing what was asked for, while the VM and VMI
// live in the KubeVirt cluster and describe what is actually running. Neither
// half alone answers "is this machine up, and where".
package vm

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	kubevirtv1 "kubevirt.io/api/core/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"

	"github.com/vitistack/vitictl-kubevirt/internal/kube"
)

// LabelSourceMachine is the label kubevirt-operator stamps on every VM it
// creates, naming the Machine it came from.
//
// This is the join key, not the VM name: the operator derives the VM name
// separately, so matching on name alone would silently mispair or drop rows.
const LabelSourceMachine = "vitistack.io/source-machine"

// Entry is one machine as seen from both sides. Machine is always set; VM and
// VMI are nil when the KubeVirt cluster has no counterpart, which is itself
// worth showing — it usually means the machine failed to provision.
type Entry struct {
	AZ string
	// Cluster is the KubeVirt cluster that answered for this machine, empty
	// when it could not be reached. Machines in different zones run on
	// different clusters, so without it a half-populated row is unreadable.
	Cluster string
	Machine *vitiv1alpha1.Machine
	VM      *kubevirtv1.VirtualMachine
	VMI     *kubevirtv1.VirtualMachineInstance
}

// KubeVirtResolver hands back the KubeVirt cluster a machine's VM runs on,
// given the zone the machine came from and the KubevirtConfig it names.
//
// Collect takes this rather than a single client because a fleet spans one
// KubeVirt cluster per zone: joining every zone against one cluster leaves
// every other zone's machines unpaired.
type KubeVirtResolver interface {
	For(ctx context.Context, az *kube.VitistackClient, configName string) (*kube.KubeVirtClient, error)
}

// Name is the Machine's name, which is what a user types.
func (e Entry) Name() string { return e.Machine.Name }

// Namespace is shared by the Machine and its VM.
func (e Entry) Namespace() string { return e.Machine.Namespace }

// VMName returns the KubeVirt VM's name, which the operator may derive
// differently from the Machine's. Actions must target this, not Name().
func (e Entry) VMName() string {
	if e.VM != nil {
		return e.VM.Name
	}
	return e.Machine.Name
}

// Status summarises the VM's printable status, preferring KubeVirt's own view
// and falling back to the Machine phase when there is no VM at all.
func (e Entry) Status() string {
	if e.VM != nil && e.VM.Status.PrintableStatus != "" {
		return string(e.VM.Status.PrintableStatus)
	}
	if e.VMI != nil && e.VMI.Status.Phase != "" {
		return string(e.VMI.Status.Phase)
	}
	if e.Machine.Status.Phase != "" {
		return e.Machine.Status.Phase
	}
	return ""
}

// Ready reports the VM's Ready condition as a yes/no, or "" when unknown.
func (e Entry) Ready() string {
	if e.VM == nil {
		return ""
	}
	for _, c := range e.VM.Status.Conditions {
		if c.Type == kubevirtv1.VirtualMachineReady {
			if c.Status == "True" {
				return "True"
			}
			return "False"
		}
	}
	return ""
}

// Node is the node the instance is running on, empty when it is not running.
func (e Entry) Node() string {
	if e.VMI == nil {
		return ""
	}
	return e.VMI.Status.NodeName
}

// IPs returns the addresses KubeVirt reports for the running instance,
// falling back to the Machine's when nothing is running.
func (e Entry) IPs() []string {
	var out []string
	if e.VMI != nil {
		for _, iface := range e.VMI.Status.Interfaces {
			if iface.IP != "" {
				out = append(out, iface.IP)
			}
		}
	}
	if len(out) == 0 {
		out = append(out, e.Machine.Status.IPAddresses...)
	}
	return dedupe(out)
}

// Collect lists Machines across the given availability zones and pairs each
// with the KubeVirt VM and VMI backing it.
//
// Machines are grouped by the KubeVirt cluster they name, and each cluster is
// listed once for the whole group rather than once per machine: a fleet
// listing would otherwise issue two API calls per row. Indexing per group and
// not globally is also what keeps the join honest — two zones may hold
// same-named machines in same-named namespaces, and a single shared index
// would pair one of them with the other's VM.
//
// A zone whose listing fails is skipped rather than failing the whole command,
// so one bad zone cannot hide the machines in the others. The failure is passed
// to warn — except when the zone simply has no vitistack CRDs, which is normal
// and silent; see isVitistackNotInstalled.
func Collect(ctx context.Context, azClients []*kube.VitistackClient, resolver KubeVirtResolver, namespace string, warn func(error)) ([]Entry, error) {
	var out []Entry
	for _, c := range azClients {
		var list vitiv1alpha1.MachineList
		var opts []ctrlclient.ListOption
		if namespace != "" {
			opts = append(opts, ctrlclient.InNamespace(namespace))
		}
		if err := c.Ctrl.List(ctx, &list, opts...); err != nil {
			if warn != nil && !isVitistackNotInstalled(err) {
				warn(fmt.Errorf("availability zone %q: listing machines: %w", c.AZ.Name, err))
			}
			continue
		}
		for _, g := range groupByCluster(list.Items) {
			out = append(out, pair(ctx, c, resolver, g, namespace, warn)...)
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Namespace() != out[j].Namespace() {
			return out[i].Namespace() < out[j].Namespace()
		}
		return out[i].Name() < out[j].Name()
	})
	return out, nil
}

// group is the set of machines in one zone that share a KubeVirt cluster.
type group struct {
	configName string
	machines   []*vitiv1alpha1.Machine
}

// groupByCluster buckets a zone's machines by the KubevirtConfig they name,
// preserving first-seen order so the result does not shuffle between runs.
//
// Machines with no annotation fall into the empty-named group, which the
// resolver answers with the zone's sole KubeVirt cluster.
func groupByCluster(machines []vitiv1alpha1.Machine) []group {
	var out []group
	index := make(map[string]int, 1)
	for i := range machines {
		m := &machines[i]
		name := m.Annotations[kube.AnnotationKubevirtConfig]
		at, ok := index[name]
		if !ok {
			index[name] = len(out)
			out = append(out, group{configName: name})
			at = len(out) - 1
		}
		out[at].machines = append(out[at].machines, m)
	}
	return out
}

// pair joins one group's machines against their own KubeVirt cluster.
//
// A cluster that cannot be reached costs the group its live state but not its
// rows: the Machines exist regardless, and dropping them would hide real
// infrastructure behind a transient outage. One warning is emitted for the
// group rather than one per machine.
func pair(ctx context.Context, az *kube.VitistackClient, resolver KubeVirtResolver, g group, namespace string, warn func(error)) []Entry {
	out := make([]Entry, 0, len(g.machines))

	kv, err := resolver.For(ctx, az, g.configName)
	if err != nil {
		if warn != nil {
			warn(fmt.Errorf("availability zone %q: %w — showing machine state only", az.AZ.Name, err))
		}
		for _, m := range g.machines {
			out = append(out, Entry{AZ: az.AZ.Name, Machine: m})
		}
		return out
	}

	vms, vmis, err := indexKubeVirt(ctx, kv, namespace)
	if err != nil && warn != nil {
		warn(fmt.Errorf("availability zone %q: %w — showing machine state only", az.AZ.Name, err))
	}
	for _, m := range g.machines {
		e := Entry{AZ: az.AZ.Name, Cluster: kv.Cluster.Name, Machine: m}
		if v, ok := vms[machineKey(m.Namespace, m.Name)]; ok {
			e.VM = v
			e.VMI = vmis[objectKey(v.Namespace, v.Name)]
		}
		out = append(out, e)
	}
	return out
}

// isVitistackNotInstalled reports whether a Machine listing failed only because
// the cluster has no vitistack CRDs.
//
// Clusters are configured as availability zones before the Vitistack operators
// land on them — a freshly bootstrapped management cluster is the usual case —
// and such a zone holds no Machines by definition. Warning about it on every
// listing trains the reader to ignore the warnings that do matter, so it is
// skipped in silence.
//
// The check is deliberately narrow. controller-runtime's RESTMapper reports a
// missing group version as ErrResourceDiscoveryFailed, which unwraps the
// underlying 404 into meta.NoResourceMatchError; a missing kind within a
// present group arrives as meta.NoKindMatchError. Nothing else matches, so a
// forbidden, unreachable, or timed-out zone still surfaces.
func isVitistackNotInstalled(err error) bool {
	return meta.IsNoMatchError(err)
}

// indexKubeVirt lists the KubeVirt side once and keys VMs by the Machine they
// came from, and VMIs by their own namespace/name.
//
// A VM without the source-machine label is still indexed under its own name,
// so machines whose VM predates the label — or was created by hand — still
// pair up instead of silently showing as missing.
func indexKubeVirt(ctx context.Context, kv *kube.KubeVirtClient, namespace string) (
	map[string]*kubevirtv1.VirtualMachine, map[string]*kubevirtv1.VirtualMachineInstance, error,
) {
	var opts []ctrlclient.ListOption
	if namespace != "" {
		opts = append(opts, ctrlclient.InNamespace(namespace))
	}

	var vmList kubevirtv1.VirtualMachineList
	if err := kv.Ctrl.List(ctx, &vmList, opts...); err != nil {
		return nil, nil, fmt.Errorf("kubevirt cluster %q: listing VirtualMachines: %w", kv.Cluster.Name, err)
	}
	vms := make(map[string]*kubevirtv1.VirtualMachine, len(vmList.Items)*2)
	for i := range vmList.Items {
		v := &vmList.Items[i]
		vms[machineKey(v.Namespace, v.Name)] = v
		if src := v.Labels[LabelSourceMachine]; src != "" {
			vms[machineKey(v.Namespace, src)] = v
		}
	}

	var vmiList kubevirtv1.VirtualMachineInstanceList
	if err := kv.Ctrl.List(ctx, &vmiList, opts...); err != nil {
		// A cluster with VMs but no running instances still lists cleanly, so
		// an error here is real — but it must not lose the VM data already
		// gathered, which is the more useful half.
		return vms, map[string]*kubevirtv1.VirtualMachineInstance{},
			fmt.Errorf("kubevirt cluster %q: listing VirtualMachineInstances: %w", kv.Cluster.Name, err)
	}
	vmis := make(map[string]*kubevirtv1.VirtualMachineInstance, len(vmiList.Items))
	for i := range vmiList.Items {
		v := &vmiList.Items[i]
		vmis[objectKey(v.Namespace, v.Name)] = v
	}
	return vms, vmis, nil
}

// Located is a Machine found by name together with the zone holding it and the
// KubevirtConfig naming the cluster it runs on.
type Located struct {
	AZ         *kube.VitistackClient
	Machine    *vitiv1alpha1.Machine
	ConfigName string
}

// Describe renders the machine as "zone/namespace/name", the form used in
// error messages and echoed after an interactive choice.
func (l Located) Describe() string {
	return l.AZ.AZ.Name + "/" + l.Machine.Namespace + "/" + l.Machine.Name
}

// Phase is the Machine's own phase, which is all that is known about a machine
// without contacting the KubeVirt cluster it runs on. It can lag reality — a
// Machine whose VM was deleted keeps reporting Running — so it is offered for
// choosing between machines, not for judging one.
func (l Located) Phase() string { return l.Machine.Status.Phase }

// FindMachines lists the machines matching name across the given zones,
// without touching any KubeVirt cluster.
//
// An empty name matches every machine, which is what an interactive picker
// lists. Deciding between several matches is the caller's business: this
// package stays free of terminal concerns so it remains testable without one.
//
// Naming the cluster a machine runs on needs only the Machine itself, so this
// costs one list per zone rather than the fleet-wide join Collect performs.
// Zones that cannot be listed are reported to warn and skipped, so a single
// unreachable zone does not block acting on a machine in another.
func FindMachines(ctx context.Context, azClients []*kube.VitistackClient, name, namespace string, warn func(error)) []Located {
	var hits []Located
	for _, c := range azClients {
		var list vitiv1alpha1.MachineList
		var opts []ctrlclient.ListOption
		if namespace != "" {
			opts = append(opts, ctrlclient.InNamespace(namespace))
		}
		if err := c.Ctrl.List(ctx, &list, opts...); err != nil {
			if warn != nil && !isVitistackNotInstalled(err) {
				warn(fmt.Errorf("availability zone %q: listing machines: %w", c.AZ.Name, err))
			}
			continue
		}
		for i := range list.Items {
			m := &list.Items[i]
			if name != "" && m.Name != name {
				continue
			}
			hits = append(hits, Located{
				AZ:         c,
				Machine:    m,
				ConfigName: m.Annotations[kube.AnnotationKubevirtConfig],
			})
		}
	}

	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Describe() < hits[j].Describe() })
	return hits
}

// TalosDistribution is what the operators record in a Machine's OS for a Talos
// image.
const TalosDistribution = "talos"

// IsTalos reports whether a Machine runs Talos Linux.
//
// It matters because Talos is API-driven: it runs no getty, no login shell and
// no SSH on any TTY. Attaching to its serial console therefore succeeds and
// then shows nothing at all, which reads as a broken tool rather than as the
// operating system working exactly as designed.
func IsTalos(m *vitiv1alpha1.Machine) bool {
	return m != nil && strings.EqualFold(m.Spec.OS.Distribution, TalosDistribution)
}

// ErrNoVirtualMachine reports that a cluster holds no VM for a machine.
//
// It is a sentinel rather than a bare message because the two callers want
// opposite things from it: acting on a machine that has no VM is a failure,
// while describing one is not — a Machine with no VM is how a failed provision
// looks, and showing that is the point.
var ErrNoVirtualMachine = errors.New("no VirtualMachine")

// Attach fills in the KubeVirt half of an entry from one cluster.
//
// This is how a single machine is described without a fleet-wide Collect: two
// targeted reads against one cluster instead of a list against every one. A
// machine with no VM, or a VM with no running instance, leaves those fields nil
// rather than erroring — both are ordinary states the Entry already renders.
func Attach(ctx context.Context, kv *kube.KubeVirtClient, e *Entry) error {
	v, err := ResolveVM(ctx, kv, e.Machine.Name, e.Machine.Namespace)
	switch {
	case errors.Is(err, ErrNoVirtualMachine):
		return nil
	case err != nil:
		return err
	}
	e.VM = v

	key := ctrlclient.ObjectKey{Namespace: v.Namespace, Name: v.Name}
	var vmi kubevirtv1.VirtualMachineInstance
	if err := kv.Ctrl.Get(ctx, key, &vmi); err != nil {
		if apierrors.IsNotFound(err) {
			return nil // Not running; Entry reports that on its own.
		}
		return fmt.Errorf("kubevirt cluster %q: reading VirtualMachineInstance %s/%s: %w",
			kv.Cluster.Name, key.Namespace, key.Name, err)
	}
	e.VMI = &vmi
	return nil
}

// ListVMs returns a cluster's VirtualMachines, ordered for display.
//
// This is the --cluster path's equivalent of FindMachines: with the control
// plane deliberately bypassed there are no Machines to choose from, only what
// the KubeVirt cluster itself reports.
func ListVMs(ctx context.Context, kv *kube.KubeVirtClient, namespace string) ([]kubevirtv1.VirtualMachine, error) {
	var opts []ctrlclient.ListOption
	if namespace != "" {
		opts = append(opts, ctrlclient.InNamespace(namespace))
	}
	var list kubevirtv1.VirtualMachineList
	if err := kv.Ctrl.List(ctx, &list, opts...); err != nil {
		return nil, fmt.Errorf("kubevirt cluster %q: listing VirtualMachines: %w", kv.Cluster.Name, err)
	}
	sort.SliceStable(list.Items, func(i, j int) bool {
		if list.Items[i].Namespace != list.Items[j].Namespace {
			return list.Items[i].Namespace < list.Items[j].Namespace
		}
		return list.Items[i].Name < list.Items[j].Name
	})
	return list.Items, nil
}

// Find resolves a machine by name, optionally narrowed to a namespace.
func Find(entries []Entry, name, namespace string) ([]Entry, error) {
	var hits []Entry
	for _, e := range entries {
		if e.Name() != name && e.VMName() != name {
			continue
		}
		if namespace != "" && e.Namespace() != namespace {
			continue
		}
		hits = append(hits, e)
	}
	if len(hits) == 0 {
		return nil, fmt.Errorf("no machine named %q found", name)
	}
	return hits, nil
}

// One resolves a machine by name and insists on exactly one match, because
// an action must never be applied to an ambiguous target.
func One(entries []Entry, name, namespace string) (Entry, error) {
	hits, err := Find(entries, name, namespace)
	if err != nil {
		return Entry{}, err
	}
	if len(hits) > 1 {
		var where []string
		for _, h := range hits {
			where = append(where, h.AZ+"/"+h.Namespace())
		}
		return Entry{}, fmt.Errorf(
			"%q is ambiguous — it exists in %s; narrow it with --namespace",
			name, strings.Join(where, ", "))
	}
	return hits[0], nil
}

// ResolveVM finds the KubeVirt VirtualMachine a user means by name.
//
// A user types the Machine name, but the operator may have given the VM a
// different one, so the source-machine label is tried first and the VM's own
// name second. Actions resolve this way instead of through a full Machine
// listing, so stopping or restarting a VM keeps working even when the
// Vitistack control plane is unreachable — which is exactly when someone is
// most likely to be doing it.
func ResolveVM(ctx context.Context, kv *kube.KubeVirtClient, name, namespace string) (*kubevirtv1.VirtualMachine, error) {
	var scope []ctrlclient.ListOption
	if namespace != "" {
		scope = append(scope, ctrlclient.InNamespace(namespace))
	}

	byLabel := append(append([]ctrlclient.ListOption{}, scope...),
		ctrlclient.MatchingLabels{LabelSourceMachine: name})
	var matched []kubevirtv1.VirtualMachine

	var labelled kubevirtv1.VirtualMachineList
	if err := kv.Ctrl.List(ctx, &labelled, byLabel...); err != nil {
		return nil, fmt.Errorf("kubevirt cluster %q: looking up %q: %w", kv.Cluster.Name, name, err)
	}
	matched = append(matched, labelled.Items...)

	if len(matched) == 0 {
		var all kubevirtv1.VirtualMachineList
		if err := kv.Ctrl.List(ctx, &all, scope...); err != nil {
			return nil, fmt.Errorf("kubevirt cluster %q: listing VirtualMachines: %w", kv.Cluster.Name, err)
		}
		for i := range all.Items {
			if all.Items[i].Name == name {
				matched = append(matched, all.Items[i])
			}
		}
	}

	switch len(matched) {
	case 0:
		return nil, fmt.Errorf("%w for %q in kubevirt cluster %q%s",
			ErrNoVirtualMachine, name, kv.Cluster.Name, inNamespace(namespace))
	case 1:
		return &matched[0], nil
	default:
		where := make([]string, 0, len(matched))
		for i := range matched {
			where = append(where, matched[i].Namespace+"/"+matched[i].Name)
		}
		return nil, fmt.Errorf("%q is ambiguous — it matches %s; narrow it with --namespace",
			name, strings.Join(where, ", "))
	}
}

func inNamespace(ns string) string {
	if ns == "" {
		return ""
	}
	return " (namespace " + ns + ")"
}

func machineKey(namespace, machine string) string { return namespace + "/" + machine }
func objectKey(namespace, name string) string     { return namespace + "/" + name }

func dedupe(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := in[:0]
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
