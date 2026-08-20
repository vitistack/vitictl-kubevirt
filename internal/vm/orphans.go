package vm

import (
	"context"
	"fmt"
	"sort"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubevirtv1 "kubevirt.io/api/core/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"

	"github.com/vitistack/vitictl-kubevirt/internal/kube"
)

// OrphanKind classifies a drift finding between the Vitistack control plane
// and a KubeVirt cluster. Each one is a distinct way the two layers can
// disagree about what exists.
type OrphanKind string

const (
	// KindVMWithoutMachine is a VirtualMachine that claims vitistack
	// ownership — it carries LabelSourceMachine — naming a Machine that does
	// not exist. This is the dangerous direction: real CPU, memory and
	// storage held by something nobody is tracking, typically left behind by
	// a failed or partial cluster teardown.
	KindVMWithoutMachine OrphanKind = "vm-without-machine"

	// KindMachineWithoutVM is a Machine whose KubeVirt cluster answered but
	// holds no VM for it. It is the same state Entry.VM == nil already shows
	// in "vm list", surfaced here as a named finding rather than a blank
	// column.
	KindMachineWithoutVM OrphanKind = "machine-without-vm"

	// KindVMIWithoutVM is a running VirtualMachineInstance with no
	// VirtualMachine object behind it — normally impossible through the
	// ordinary lifecycle, since KubeVirt only ever creates a VMI from a VM,
	// so one appearing alone means the VM was removed out from under it.
	KindVMIWithoutVM OrphanKind = "vmi-without-vm"
)

// Orphan is one detected disagreement between the two layers.
//
// A finding here is a candidate for investigation, not a verdict: some are
// legitimate — a VM deliberately kept running while its Machine is being
// recreated looks identical to an abandoned one until someone checks.
type Orphan struct {
	Kind      OrphanKind
	AZ        string
	Cluster   string
	Namespace string
	Name      string
	// Detail names what's missing or relevant — the absent Machine's name
	// for KindVMWithoutMachine, for instance.
	Detail    string
	CreatedAt metav1.Time
}

// Coverage tracks which zones and KubeVirt clusters an orphan sweep actually
// reached, as distinct from how many were configured.
//
// This exists because "no orphans found" and "nothing was checked" render
// identically unless the scope itself is reported: a zone that could not be
// listed, or a cluster that could not be resolved or queried, must not read
// as a zone or cluster with nothing wrong in it.
//
// KubeVirt cluster counts are populated per zone, only once that zone's own
// Machine listing succeeds — a zone that could not be listed cannot even say
// how many clusters it fronts, so it counts against the Zones fields alone.
type Coverage struct {
	ZonesConfigured    int
	ZonesChecked       int
	ClustersConfigured int
	ClustersChecked    int
}

// Complete reports whether every configured zone and cluster was actually
// audited. False means "no orphans" is unproven for whatever was missed, not
// that the fleet is clean.
func (c Coverage) Complete() bool {
	return c.ZonesChecked == c.ZonesConfigured && c.ClustersChecked == c.ClustersConfigured
}

// OrphanReport is the result of one orphan sweep: the findings, the coverage
// that backs them, and how many candidates the age filter held back.
type OrphanReport struct {
	Orphans  []Orphan
	Coverage Coverage
	// Suppressed is how many findings --min-age excluded as too young to
	// judge. It is surfaced rather than folded silently into the count, so a
	// sweep with every finding suppressed still shows that something was
	// filtered instead of reading as an unqualified clean pass.
	Suppressed int
}

// DetectOrphans audits every given availability zone and the KubeVirt
// cluster each of its machines names, and reports where the two layers
// disagree — see the OrphanKind constants for what is reported and why.
//
// minAge excludes anything younger than it from the findings. A Machine and
// its VM are created by separate reconcilers a few seconds apart, so an audit
// run mid-provisioning would otherwise flag routine reconciliation as drift;
// now is passed in explicitly rather than read from time.Now() so tests can
// fix it.
//
// Discovery of a KubeVirt cluster is anchored on a zone's Machines, the same
// way Collect works: a zone with literally zero Machines left in it — every
// one already deleted — has no path in this design to name which cluster to
// inspect, so an orphan on such a cluster goes unseen. In practice a
// teardown leaves stragglers behind other machines on the same cluster, or a
// zone with a single KubevirtConfig is still reached through the unnamed
// group, so this gap is narrow rather than the common case.
//
// The returned error is always nil today; it exists for symmetry with
// Collect; a genuinely fatal problem (not merely a zone or cluster that
// could not be reached) has somewhere to go without a signature break later.
func DetectOrphans(
	ctx context.Context,
	azClients []*kube.VitistackClient,
	resolver KubeVirtResolver,
	namespace string,
	minAge time.Duration,
	now time.Time,
	warn func(error),
) (OrphanReport, error) {
	var report OrphanReport
	report.Coverage.ZonesConfigured = len(azClients)

	for _, c := range azClients {
		var list vitiv1alpha1.MachineList
		var opts []ctrlclient.ListOption
		if namespace != "" {
			opts = append(opts, ctrlclient.InNamespace(namespace))
		}
		if err := c.Ctrl.List(ctx, &list, opts...); err != nil {
			if isVitistackNotInstalled(err) {
				// A zone with no vitistack CRDs holds no Machines by
				// definition — that is confirmed information, not a gap.
				report.Coverage.ZonesChecked++
				continue
			}
			if warn != nil {
				warn(fmt.Errorf("availability zone %q: listing machines: %w — orphans there cannot be audited",
					c.AZ.Name, err))
			}
			continue
		}
		report.Coverage.ZonesChecked++

		known := machineNames(list.Items)
		for _, g := range groupByCluster(list.Items) {
			report.Coverage.ClustersConfigured++
			kv, err := resolver.For(ctx, c, g.configName)
			if err != nil {
				if warn != nil {
					warn(fmt.Errorf("availability zone %q: %w — orphans on that cluster cannot be audited",
						c.AZ.Name, err))
				}
				continue
			}

			co, err := detectClusterOrphans(ctx, c.AZ.Name, kv, g, known, namespace, minAge, now)
			if err != nil {
				if warn != nil {
					warn(fmt.Errorf("availability zone %q: kubevirt cluster %q: %w — orphans there cannot be audited",
						c.AZ.Name, kv.Cluster.Name, err))
				}
				continue
			}
			report.Coverage.ClustersChecked++
			report.Orphans = append(report.Orphans, co.found...)
			report.Suppressed += co.suppressed
		}
	}

	sort.SliceStable(report.Orphans, func(i, j int) bool {
		a, b := report.Orphans[i], report.Orphans[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.AZ != b.AZ {
			return a.AZ < b.AZ
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})
	return report, nil
}

// clusterOrphans is one KubeVirt cluster's contribution to a sweep.
type clusterOrphans struct {
	found      []Orphan
	suppressed int
}

// detectClusterOrphans audits one KubeVirt cluster against g, the subset of
// the zone's machines that name it, and known, every machine name in the
// whole zone.
//
// known has to span the whole zone rather than just g: a VM's source-machine
// label naming a Machine that was never created — or was deleted — carries
// no KubevirtConfig annotation of its own to say which group it would have
// belonged to, so the only honest check is against everything the zone has.
//
// A failure listing either VMs or VMIs fails the whole cluster rather than
// returning what was gathered before the failure: partial data here cannot
// be told apart from a clean result once it is mixed into the findings, and
// this command's whole premise is not misreporting drift as absence of it.
func detectClusterOrphans(
	ctx context.Context,
	az string,
	kv *kube.KubeVirtClient,
	g group,
	known map[string]struct{},
	namespace string,
	minAge time.Duration,
	now time.Time,
) (clusterOrphans, error) {
	var opts []ctrlclient.ListOption
	if namespace != "" {
		opts = append(opts, ctrlclient.InNamespace(namespace))
	}

	var vmList kubevirtv1.VirtualMachineList
	if err := kv.Ctrl.List(ctx, &vmList, opts...); err != nil {
		return clusterOrphans{}, fmt.Errorf("listing VirtualMachines: %w", err)
	}
	var vmiList kubevirtv1.VirtualMachineInstanceList
	if err := kv.Ctrl.List(ctx, &vmiList, opts...); err != nil {
		return clusterOrphans{}, fmt.Errorf("listing VirtualMachineInstances: %w", err)
	}

	var out clusterOrphans
	record := func(kind OrphanKind, ns, name, detail string, created metav1.Time) {
		if tooYoung(created, minAge, now) {
			out.suppressed++
			return
		}
		out.found = append(out.found, Orphan{
			Kind: kind, AZ: az, Cluster: kv.Cluster.Name,
			Namespace: ns, Name: name, Detail: detail, CreatedAt: created,
		})
	}

	// vm-without-machine: only VMs that claim vitistack ownership are in
	// scope. A VM with no LabelSourceMachine may be someone's hand-made
	// workload on a shared cluster — none of this tool's business — and
	// reporting it would train people to ignore the command.
	vmExists := make(map[string]struct{}, len(vmList.Items))
	for i := range vmList.Items {
		v := &vmList.Items[i]
		vmExists[objectKey(v.Namespace, v.Name)] = struct{}{}

		src := v.Labels[LabelSourceMachine]
		if src == "" {
			continue
		}
		if _, ok := known[machineKey(v.Namespace, src)]; ok {
			continue
		}
		record(KindVMWithoutMachine, v.Namespace, v.Name,
			fmt.Sprintf("no Machine named %q", src), v.CreationTimestamp)
	}

	// machine-without-vm, derived from the VM listing already in hand rather
	// than a second query — mirroring indexKubeVirt's own double keying, a
	// machine matches a VM by the label first and by the VM's own name
	// second, so a hand-made or pre-label VM still counts as backing it.
	matched := vmIndex(vmList.Items)
	for _, m := range g.machines {
		if _, ok := matched[machineKey(m.Namespace, m.Name)]; ok {
			continue
		}
		record(KindMachineWithoutVM, m.Namespace, m.Name,
			"no VirtualMachine on this cluster", m.CreationTimestamp)
	}

	// vmi-without-vm is unconditional: unlike a VM it carries no ownership
	// label to gate on, and a live instance with nothing managing it is real
	// compute either way.
	for i := range vmiList.Items {
		vmi := &vmiList.Items[i]
		if _, ok := vmExists[objectKey(vmi.Namespace, vmi.Name)]; ok {
			continue
		}
		record(KindVMIWithoutVM, vmi.Namespace, vmi.Name,
			"no VirtualMachine object", vmi.CreationTimestamp)
	}

	return out, nil
}

// vmIndex maps a Machine's namespace/name to the VM backing it, keyed the
// same way indexKubeVirt keys its own map: by the VM's own identity, and
// additionally by the source-machine label when present. It is
// re-derived here rather than shared with indexKubeVirt because that
// function also lists the cluster itself, and this one is handed a list
// already fetched for other reasons.
func vmIndex(vms []kubevirtv1.VirtualMachine) map[string]*kubevirtv1.VirtualMachine {
	out := make(map[string]*kubevirtv1.VirtualMachine, len(vms)*2)
	for i := range vms {
		v := &vms[i]
		out[machineKey(v.Namespace, v.Name)] = v
		if src := v.Labels[LabelSourceMachine]; src != "" {
			out[machineKey(v.Namespace, src)] = v
		}
	}
	return out
}

// machineNames is the set of every Machine's namespace/name in one zone,
// used to answer "does the Machine a VM names actually exist" without
// caring which KubevirtConfig group it happens to belong to.
func machineNames(machines []vitiv1alpha1.Machine) map[string]struct{} {
	out := make(map[string]struct{}, len(machines))
	for i := range machines {
		out[machineKey(machines[i].Namespace, machines[i].Name)] = struct{}{}
	}
	return out
}

// tooYoung reports whether t is within minAge of now, the provisioning-race
// guard: a zero timestamp — never set by a real API server, only possible in
// a test — is treated as old enough to report rather than silently hidden,
// since suppressing on missing data would be the wrong failure mode for a
// safety filter.
func tooYoung(t metav1.Time, minAge time.Duration, now time.Time) bool {
	if t.IsZero() {
		return false
	}
	return now.Sub(t.Time) < minAge
}
