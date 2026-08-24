package vm

import (
	"context"
	"fmt"
	"sort"

	kubevirtv1 "kubevirt.io/api/core/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"

	"github.com/vitistack/vitictl-kubevirt/internal/kube"
)

// Migration is one VirtualMachineInstanceMigration as seen from the layer
// above it: the zone and KubeVirt cluster it came from, and the Machine it
// belongs to.
type Migration struct {
	AZ string
	// Cluster is the KubeVirt cluster the migration was read from.
	Cluster string
	// Machine is the owning Machine's name, resolved through the migrated
	// VMI's VM and its source-machine label — see LabelSourceMachine — never
	// through name matching alone. Empty when no owning VM or Machine could
	// be found; that is shown rather than dropped, because an unattributable
	// migration is information, not noise — it usually means the VM was
	// created by hand or the label was lost.
	Machine string
	// VMIM is the migration object itself, kept so every field it carries —
	// including ones this package exposes no accessor for — still reaches
	// JSON/YAML output.
	VMIM *kubevirtv1.VirtualMachineInstanceMigration
	// VMIState is the migrating instance's own status.migrationState, which
	// KubeVirt fills in WHILE the migration runs; the copy on the VMIM is only
	// populated once it finishes. Observed on ptr1-kv-cl01: mid-flight the VMI
	// already read wrk02->wrk14 while the VMIM's state was still nil, so a
	// --watch built on the VMIM alone showed an empty NODE column for the
	// entire migration and filled it in only after there was nothing left to
	// watch.
	//
	// Nil when the instance could not be read; see migrationState for why it
	// is not simply trusted in preference to the VMIM.
	VMIState *kubevirtv1.VirtualMachineInstanceMigrationState
}

// migrationState is the state to report for THIS migration.
//
// The VMI's copy is preferred because it is the live one — but only when its
// MigrationUID identifies this VMIM, because a VMI keeps the state of its most
// RECENT migration. A VMI migrated twice would otherwise have its earlier,
// finished migration re-labelled with the later one's nodes: t-jraviti-123's
// wrk3 has exactly that history — a Failed migration to wrk18 followed by a
// Succeeded one to wrk14 — and the Failed row must keep reporting wrk18.
//
// KubeVirt sets MigrationUID to the migration object's own metadata.uid
// (verified against both of those migrations), so the comparison is exact
// rather than heuristic.
func (m Migration) migrationState() *kubevirtv1.VirtualMachineInstanceMigrationState {
	if m.VMIState != nil && m.VMIState.MigrationUID == m.VMIM.UID {
		return m.VMIState
	}
	return m.VMIM.Status.MigrationState
}

// Name is the migration object's own, KubeVirt-generated name. A user
// recognises the VMI it names, not this — see VMIName.
func (m Migration) Name() string { return m.VMIM.Name }

// Namespace is shared with the VMI being migrated.
func (m Migration) Namespace() string { return m.VMIM.Namespace }

// VMIName is the instance being migrated — what an operator recognises,
// unlike the migration object's own generated Name.
func (m Migration) VMIName() string { return m.VMIM.Spec.VMIName }

// Phase is the migration's own phase, straight from KubeVirt.
func (m Migration) Phase() string { return string(m.VMIM.Status.Phase) }

// SourceNode is the node the instance is migrating away from, empty until
// KubeVirt has populated the migration state.
func (m Migration) SourceNode() string {
	s := m.migrationState()
	if s == nil {
		return ""
	}
	return s.SourceNode
}

// TargetNode is the node the instance is migrating to, empty until KubeVirt
// has scheduled it.
func (m Migration) TargetNode() string {
	s := m.migrationState()
	if s == nil {
		return ""
	}
	return s.TargetNode
}

// Mode reports whether a running migration is pre-copy, post-copy, or paused;
// empty before KubeVirt has decided, which is most of a migration's Pending
// phase.
func (m Migration) Mode() string {
	s := m.migrationState()
	if s == nil {
		return ""
	}
	return string(s.Mode)
}

// Failed reports whether KubeVirt gave up on this migration, using the API's
// own terminal phase rather than a string this package invents.
func (m Migration) Failed() bool { return m.VMIM.Status.Phase == kubevirtv1.MigrationFailed }

// FailureReason is KubeVirt's account of why a migration failed — in practice
// the libvirt error, e.g. "internal error: client socket is closed".
//
// Empty is a real and distinct answer, not an oversight: a migration that
// failed before it was ever scheduled has a migrationState carrying little
// more than a sourcePod, and no reason at all. Observed on pos1-kv-cl01. A
// caller must say "none recorded" rather than imply the failure was silent —
// the reason lives in virt-controller's logs and the VMI's events instead.
func (m Migration) FailureReason() string {
	s := m.migrationState()
	if s == nil {
		return ""
	}
	return s.FailureReason
}

// Active reports whether the migration is still in flight.
//
// A rollout operator wants to see what is happening now, not the archive of
// everything that already finished, so this is the default view; see
// isMigrationFinished for what "finished" means and why.
func (m Migration) Active() bool { return !isMigrationFinished(m.VMIM) }

// isMigrationFinished checks the real phase constants KubeVirt defines —
// Succeeded and Failed are the only two the API itself treats as terminal —
// rather than inventing a string to compare against. Anything else, including
// the empty phase a brand-new migration briefly has, counts as active: a
// phase this package does not recognise should default to "still watch it",
// not "safe to hide".
func isMigrationFinished(m *kubevirtv1.VirtualMachineInstanceMigration) bool {
	switch m.Status.Phase {
	case kubevirtv1.MigrationSucceeded, kubevirtv1.MigrationFailed:
		return true
	default:
		return false
	}
}

// CollectMigrations lists VirtualMachineInstanceMigrations across the given
// availability zones' KubeVirt clusters, joining each one back to the Machine
// it belongs to.
//
// Zones are consulted only to discover which KubeVirt clusters are in play —
// the migrations themselves are read from those clusters, not from the zones
// — which is why a zone with no Machines contributes no clusters and so no
// migrations, even when its cluster would otherwise answer. Grouping machines
// by the KubevirtConfig they name is deliberately the same as Collect's: it is
// what lets a cluster be resolved and listed once for however many machines
// point at it, rather than once per row.
//
// A cluster reachable from more than one zone's group — one KubeVirt cluster
// fronting several availability zones, or several KubevirtConfigs naming the
// same cluster — is listed exactly once: the first group to resolve it wins
// the AZ column, and every later group naming the same cluster is skipped
// rather than doubling its rows. Migrations have no per-zone identity of their
// own to de-duplicate by, unlike Machines, so this is done by cluster name.
//
// A zone or cluster that cannot be reached is passed to warn and skipped —
// the same contract Collect follows — so one bad zone or cluster does not
// blank the rest of the fleet.
func CollectMigrations(ctx context.Context, azClients []*kube.VitistackClient, resolver KubeVirtResolver, namespace string, warn func(error)) ([]Migration, error) {
	var out []Migration
	seen := make(map[string]bool)

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
			kv, err := resolver.For(ctx, c, g.configName)
			if err != nil {
				if warn != nil {
					warn(fmt.Errorf("availability zone %q: %w", c.AZ.Name, err))
				}
				continue
			}
			if seen[kv.Cluster.Name] {
				continue
			}
			seen[kv.Cluster.Name] = true
			out = append(out, listClusterMigrations(ctx, c.AZ.Name, kv, namespace, warn)...)
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Namespace() != out[j].Namespace() {
			return out[i].Namespace() < out[j].Namespace()
		}
		if out[i].VMIName() != out[j].VMIName() {
			return out[i].VMIName() < out[j].VMIName()
		}
		return out[i].Name() < out[j].Name()
	})
	return out, nil
}

// listClusterMigrations lists one KubeVirt cluster's migrations and pairs
// each with the Machine name that owns it.
//
// A migration names a VMI, not a Machine, so the join goes through the VM it
// was made for — the same source-machine label indexKubeVirt uses, and for
// the same reason: the operator derives VM (and so VMI) names independently
// of the Machine, so matching on name alone would silently mispair a
// migration with the wrong machine. A migration whose owner cannot be
// resolved this way is still returned, with an empty Machine, rather than
// dropped — see Migration.Machine.
func listClusterMigrations(ctx context.Context, azName string, kv *kube.KubeVirtClient, namespace string, warn func(error)) []Migration {
	var opts []ctrlclient.ListOption
	if namespace != "" {
		opts = append(opts, ctrlclient.InNamespace(namespace))
	}

	var migList kubevirtv1.VirtualMachineInstanceMigrationList
	if err := kv.Ctrl.List(ctx, &migList, opts...); err != nil {
		if warn != nil {
			warn(fmt.Errorf("kubevirt cluster %q: listing VirtualMachineInstanceMigrations: %w", kv.Cluster.Name, err))
		}
		return nil
	}
	if len(migList.Items) == 0 {
		return nil
	}

	// A cluster with migrations but an owner lookup that fails must still show
	// them — unattributable is a worse answer than the failure hiding real,
	// in-flight migrations entirely.
	owners, err := machineNamesByVMIName(ctx, kv, namespace)
	if err != nil && warn != nil {
		warn(fmt.Errorf("kubevirt cluster %q: %w — showing migrations without their owning machine", kv.Cluster.Name, err))
	}

	// Live migration state, for the same reason the owner lookup is tolerated
	// to fail: losing the NODE column is a worse answer than hiding
	// in-flight migrations, so a failure here degrades the row rather than
	// dropping it.
	states, err := migrationStatesByVMIName(ctx, kv, namespace)
	if err != nil && warn != nil {
		warn(fmt.Errorf("kubevirt cluster %q: %w — showing migrations without live source/target nodes", kv.Cluster.Name, err))
	}

	out := make([]Migration, 0, len(migList.Items))
	for i := range migList.Items {
		m := &migList.Items[i]
		out = append(out, Migration{
			AZ:       azName,
			Cluster:  kv.Cluster.Name,
			Machine:  owners[objectKey(m.Namespace, m.Spec.VMIName)],
			VMIM:     m,
			VMIState: states[objectKey(m.Namespace, m.Spec.VMIName)],
		})
	}
	return out
}

// migrationStatesByVMIName returns each VirtualMachineInstance's own
// status.migrationState, keyed by the VMI's namespace and name — which is what
// a migration names.
//
// This is the live view: KubeVirt updates the instance's copy during the
// migration and only mirrors it onto the migration object at completion. The
// state is returned as-is, unfiltered — Migration.migrationState decides
// whether it belongs to the migration being rendered.
func migrationStatesByVMIName(ctx context.Context, kv *kube.KubeVirtClient, namespace string) (map[string]*kubevirtv1.VirtualMachineInstanceMigrationState, error) {
	var opts []ctrlclient.ListOption
	if namespace != "" {
		opts = append(opts, ctrlclient.InNamespace(namespace))
	}
	var vmiList kubevirtv1.VirtualMachineInstanceList
	if err := kv.Ctrl.List(ctx, &vmiList, opts...); err != nil {
		return nil, fmt.Errorf("listing VirtualMachineInstances: %w", err)
	}
	out := make(map[string]*kubevirtv1.VirtualMachineInstanceMigrationState, len(vmiList.Items))
	for i := range vmiList.Items {
		v := &vmiList.Items[i]
		if v.Status.MigrationState != nil {
			out[objectKey(v.Namespace, v.Name)] = v.Status.MigrationState
		}
	}
	return out, nil
}

// machineNamesByVMIName lists a cluster's VirtualMachines and returns, for
// each one, the Machine name that owns it — keyed by the VM's own name, which
// is what a migration names: a VirtualMachineInstance always shares its
// owning VM's name, so the VMI a migration targets is found under the VM
// carrying that same name.
//
// The source-machine label is preferred over the VM's own name because the
// operator derives that name independently — see LabelSourceMachine. A VM
// with no label, hand-made or predating the label, still resolves under its
// own name, matching indexKubeVirt's fallback.
func machineNamesByVMIName(ctx context.Context, kv *kube.KubeVirtClient, namespace string) (map[string]string, error) {
	var opts []ctrlclient.ListOption
	if namespace != "" {
		opts = append(opts, ctrlclient.InNamespace(namespace))
	}
	var vmList kubevirtv1.VirtualMachineList
	if err := kv.Ctrl.List(ctx, &vmList, opts...); err != nil {
		return nil, fmt.Errorf("listing VirtualMachines: %w", err)
	}
	out := make(map[string]string, len(vmList.Items))
	for i := range vmList.Items {
		v := &vmList.Items[i]
		name := v.Name
		if src := v.Labels[LabelSourceMachine]; src != "" {
			name = src
		}
		out[objectKey(v.Namespace, v.Name)] = name
	}
	return out, nil
}
