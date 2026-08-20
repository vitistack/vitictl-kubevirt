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

	"github.com/vitistack/vitictl-kubevirt/internal/config"
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
	Kind OrphanKind
	// AZ is empty for a cluster reached only through the user's local
	// ~/.vitistack/kubevirt.config.yaml rather than discovered from a
	// Machine — there being no zone to name is itself the point: the
	// cluster it came from has no surviving Machine anywhere.
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
// listed, or a cluster that could not be resolved, connected to, or queried,
// must not read as a zone or cluster with nothing wrong in it.
//
// KubeVirt cluster counts are populated per zone, only once that zone's own
// Machine listing succeeds — a zone that could not be listed cannot even say
// how many clusters it fronts, so it counts against the Zones fields alone.
// A locally-configured cluster with no owning zone (see DetectOrphans) is
// folded into the same Clusters totals so the "N/M clusters audited" figure
// stays one honest number rather than two the caller has to add up.
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

// LocalConnector connects to one of the user's locally-configured KubeVirt
// clusters (~/.vitistack/kubevirt.config.yaml, the same file "viti kubevirt
// config list" reads). It is a function value rather than DetectOrphans
// calling kube.ConnectKubeVirt directly, so a test can fake a connection
// without a real kubeconfig; kube.ConnectKubeVirt is the production
// implementation.
type LocalConnector func(config.Cluster) (*kube.KubeVirtClient, error)

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
// way Collect works — which by itself would miss exactly the case this
// command exists for: the last guest cluster on a KubeVirt cluster is torn
// down, every Machine that named it vanishes with it, and any VM left
// running becomes permanently invisible to a Machine-anchored sweep. So
// localClusters — the user's own ~/.vitistack/kubevirt.config.yaml — is
// unioned in as a second, independent source: every one of them is also
// audited, whether or not any zone's Machine currently names it.
//
// A cluster reachable both ways is audited once, not twice — see
// clusterIdentity for the key used to recognise that they are the same
// cluster — and localClusters is otherwise strictly additive: it never
// narrows or replaces what zone discovery already found.
//
// An unanchored local cluster has no g.machines of its own, so
// machine-without-vm never applies to it — there is no zone's worth of
// Machines to check it against. vm-without-machine is trickier: "the named
// Machine does not exist" can only be asserted against every Machine this
// sweep actually saw, across every zone, since an unanchored cluster carries
// no annotation saying which zone it would belong to. If any zone's Machine
// listing failed outright (not merely lacked vitistack CRDs, which is
// confirmed emptiness, not a gap), that check is skipped for every
// unanchored cluster rather than risked: a real Machine sitting in the
// unlisted zone must never be reported as an abandoned VM's missing owner.
// The cluster still counts as configured but not checked in that case, and
// vmi-without-vm — which needs no Machine knowledge at all — is still
// reported normally.
//
// What this still cannot find: a KubeVirt cluster that is neither named by
// any Machine anywhere nor present in the user's local configuration. There
// is no third source of cluster identity in this design, so such a cluster
// is invisible to this sweep — narrower than the zero-Machines gap alone,
// but not eliminated by it.
//
// The returned error is always nil today; it exists for symmetry with
// Collect; a genuinely fatal problem (not merely a zone or cluster that
// could not be reached) has somewhere to go without a signature break later.
func DetectOrphans(
	ctx context.Context,
	azClients []*kube.VitistackClient,
	resolver KubeVirtResolver,
	localClusters []config.Cluster,
	connect LocalConnector,
	namespace string,
	minAge time.Duration,
	now time.Time,
	warn func(error),
) (OrphanReport, error) {
	var report OrphanReport
	report.Coverage.ZonesConfigured = len(azClients)

	// visited dedupes a cluster reached through both zone discovery and
	// localClusters, keyed by clusterIdentity so it is recognised as the
	// same cluster regardless of which path found it first.
	visited := make(map[string]struct{})
	globalKnown := make(map[string]struct{})
	// zonesFullyKnown says whether every zone's Machines were actually
	// listed — as opposed to merely attempted — which is the precondition
	// for trusting globalKnown as a complete negative ("no Machine anywhere
	// is named this"). A zone with no vitistack CRDs does not break it: that
	// is a zone confirmed to hold zero Machines, not one that was skipped.
	zonesFullyKnown := true

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
			zonesFullyKnown = false
			continue
		}
		report.Coverage.ZonesChecked++

		known := machineNames(list.Items)
		for k := range known {
			globalKnown[k] = struct{}{}
		}

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
			visited[clusterIdentity(kv.Cluster)] = struct{}{}

			co, err := detectClusterOrphans(ctx, c.AZ.Name, kv,
				clusterAudit{machines: g.machines, known: known, checkVMOwnership: true},
				namespace, minAge, now)
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

	for _, cl := range localClusters {
		id := clusterIdentity(cl)
		if _, dup := visited[id]; dup {
			// Already reached through zone discovery — auditing it again
			// would report every finding on it twice.
			continue
		}
		visited[id] = struct{}{}
		report.Coverage.ClustersConfigured++

		kv, err := connect(cl)
		if err != nil {
			if warn != nil {
				warn(fmt.Errorf("kubevirt cluster %q: %w — orphans there cannot be audited", cl.Name, err))
			}
			continue
		}

		co, err := detectClusterOrphans(ctx, "", kv,
			clusterAudit{known: globalKnown, checkVMOwnership: zonesFullyKnown},
			namespace, minAge, now)
		if err != nil {
			if warn != nil {
				warn(fmt.Errorf("kubevirt cluster %q: %w — orphans there cannot be audited", kv.Cluster.Name, err))
			}
			continue
		}
		report.Orphans = append(report.Orphans, co.found...)
		report.Suppressed += co.suppressed

		if !zonesFullyKnown {
			if warn != nil {
				warn(fmt.Errorf(
					"kubevirt cluster %q has no owning availability zone, and at least one zone could not be "+
						"listed — vm-without-machine cannot be judged safely there, so it is not reported and the "+
						"cluster counts as unaudited", kv.Cluster.Name))
			}
			continue
		}
		report.Coverage.ClustersChecked++
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

// clusterIdentity is the key used to recognise that a cluster reached
// through zone discovery and one reached through localClusters are the same
// physical cluster, so it is audited once rather than twice.
//
// Context is preferred because it is exactly what
// ConnectKubeVirtFromKubeconfig already matches a discovered cluster's local
// counterpart on (see kube.go): two config.Cluster values naming the same
// context are, by that same logic, the same cluster. Kubeconfig path is the
// fallback for a locally-configured cluster with no context override — a
// bare kubeconfig's current-context is used implicitly, so there is nothing
// else stable to key on — and the cluster's own Name is the last resort for
// the ad-hoc, no-file cluster the environment variables define (see
// config.EnvClusterName).
func clusterIdentity(cl config.Cluster) string {
	switch {
	case cl.Context != "":
		return "context:" + cl.Context
	case cl.Kubeconfig != "":
		return "kubeconfig:" + cl.Kubeconfig
	default:
		return "name:" + cl.Name
	}
}

// clusterOrphans is one KubeVirt cluster's contribution to a sweep.
type clusterOrphans struct {
	found      []Orphan
	suppressed int
}

// clusterAudit controls what detectClusterOrphans checks for one cluster.
type clusterAudit struct {
	// machines is the subset of a zone's Machines that name this cluster.
	// Left nil for a cluster reached only through localClusters, which skips
	// machine-without-vm entirely — there being no zone's worth of Machines
	// to check it against, not "checked and found none".
	machines []*vitiv1alpha1.Machine
	// known is the Machine-name set a VM's source-machine label is checked
	// against: the whole zone's Machines for a zone-discovered cluster, or
	// every Machine seen across every zone for an unanchored one.
	known map[string]struct{}
	// checkVMOwnership gates vm-without-machine outright. It is false only
	// for an unanchored cluster audited while some zone's Machines could not
	// be listed, when known is not trustworthy as a complete negative and a
	// real Machine's VM must never be reported as abandoned merely because
	// this sweep did not manage to look in the right zone.
	checkVMOwnership bool
}

// detectClusterOrphans audits one KubeVirt cluster against audit — see
// clusterAudit for what it controls and why.
//
// A failure listing either VMs or VMIs fails the whole cluster rather than
// returning what was gathered before the failure: partial data here cannot
// be told apart from a clean result once it is mixed into the findings, and
// this command's whole premise is not misreporting drift as absence of it.
func detectClusterOrphans(
	ctx context.Context,
	az string,
	kv *kube.KubeVirtClient,
	audit clusterAudit,
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

		if !audit.checkVMOwnership {
			continue
		}
		src := v.Labels[LabelSourceMachine]
		if src == "" {
			continue
		}
		if _, ok := audit.known[machineKey(v.Namespace, src)]; ok {
			continue
		}
		record(KindVMWithoutMachine, v.Namespace, v.Name,
			fmt.Sprintf("no Machine named %q", src), v.CreationTimestamp)
	}

	// machine-without-vm, derived from the VM listing already in hand rather
	// than a second query — mirroring indexKubeVirt's own double keying, a
	// machine matches a VM by the label first and by the VM's own name
	// second, so a hand-made or pre-label VM still counts as backing it.
	// Ranging over a nil audit.machines — an unanchored cluster — is simply a
	// no-op, which is exactly the intended skip.
	matched := vmIndex(vmList.Items)
	for _, m := range audit.machines {
		if _, ok := matched[machineKey(m.Namespace, m.Name)]; ok {
			continue
		}
		record(KindMachineWithoutVM, m.Namespace, m.Name,
			"no VirtualMachine on this cluster", m.CreationTimestamp)
	}

	// vmi-without-vm is unconditional: unlike a VM it carries no ownership
	// label to gate on, and a live instance with nothing managing it is real
	// compute either way — true regardless of whether this cluster has an
	// owning zone.
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
