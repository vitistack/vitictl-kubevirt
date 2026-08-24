package cmd

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	kubevirtv1 "kubevirt.io/api/core/v1"

	"github.com/vitistack/vitictl-kubevirt/internal/vm"
)

// newVMMigrateCmd live-migrates one instance to another hypervisor.
//
// The plugin could already watch migrations and not start one, so moving a
// guest off a host meant hand-writing a VirtualMachineInstanceMigration and
// applying it with kubectl. This is the verb that was missing beside start,
// stop, restart and the rest.
func newVMMigrateCmd(s *scope) *cobra.Command {
	var targetNode string
	var assumeYes, noWait bool
	var interval time.Duration

	cmd := &cobra.Command{
		Use:     "migrate [name]",
		Aliases: []string{"mig"},
		Short:   "🚚 Live-migrate a virtual machine to another hypervisor",
		Long: `Live-migrate a virtual machine to another node of its KubeVirt cluster.

The guest keeps running: its memory is copied to the destination while it
works, and execution switches host at the end. Nothing reboots, and the
instance keeps its identity — unlike "vm changemachineclass", which restarts.

[name] is the Machine name, or the KubeVirt VirtualMachine name if they
differ. Leave it out to pick one from an interactive, fuzzy-searchable list.

--to pins the destination. Without it KubeVirt chooses, which is usually what
you want; pin to steer away from a host you distrust or onto one you do.

Preflight refuses before anything is created: the instance must be running and
KubeVirt must report it live-migratable. A VM on non-shared storage can never
migrate, and being told that immediately beats a migration that fails a minute
later with a target pod already scheduled.

If a failure does happen mid-flight, the guest keeps running where it is —
KubeVirt only switches host once the destination is ready.`,
		Example: `  viti kubevirt vm migrate
  viti kubevirt vm migrate my-worker
  viti kubevirt vm migrate my-worker --to kv-wrk14
  viti kubevirt vm mig my-worker --no-wait`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := contextOrBackground(cmd)
			t, err := resolveTarget(cmd, s, firstArg(args))
			if err != nil {
				return err
			}

			vmi, err := vm.GetVMI(ctx, t.KV, t.VM.Namespace, t.VM.Name)
			if err != nil {
				return err
			}
			if err := checkMigratable(vmi); err != nil {
				return err
			}
			if err := checkTarget(vmi, targetNode); err != nil {
				return err
			}

			if !assumeYes {
				ok, err := confirmMigration(cmd, t, vmi, targetNode)
				if err != nil {
					return err
				}
				if !ok {
					_, _ = fmt.Fprintln(cmd.OutOrStdout(), "aborted")
					return errCancelled
				}
			}

			m, err := vm.StartMigration(ctx, t.KV, t.VM.Namespace, t.VM.Name, targetNode)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "🚚 migrating %s/%s from %s%s\n",
				t.VM.Namespace, t.VM.Name, dash(vmi.Status.NodeName), pinnedTo(targetNode))

			if noWait {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(),
					"   started as %s — follow it with: viti kubevirt migrations --watch\n", m.Name)
				return nil
			}
			return awaitAndReport(cmd, t, m.Name, interval)
		},
	}
	cmd.Flags().StringVar(&targetNode, "to", "",
		"pin the destination hypervisor by node name (default: KubeVirt chooses)")
	cmd.Flags().BoolVarP(&assumeYes, "yes", "y", false, "skip the confirmation prompt")
	cmd.Flags().BoolVar(&noWait, "no-wait", false,
		"return as soon as the migration is accepted instead of waiting for it to finish")
	cmd.Flags().DurationVar(&interval, "interval", 2*time.Second,
		"poll interval while waiting")
	return cmd
}

// checkMigratable refuses before anything is created, carrying KubeVirt's own
// reason rather than a guess at one.
func checkMigratable(vmi *kubevirtv1.VirtualMachineInstance) error {
	if vmi.Status.Phase != kubevirtv1.Running {
		return fmt.Errorf("instance %s/%s is %s, not Running — only a running instance can be live-migrated",
			vmi.Namespace, vmi.Name, dash(string(vmi.Status.Phase)))
	}
	if reason := vm.NotMigratableReason(vmi); reason != "" {
		return fmt.Errorf("instance %s/%s cannot be live-migrated: %s", vmi.Namespace, vmi.Name, reason)
	}
	return nil
}

// checkTarget rejects a pin that asks for nothing. Migrating an instance to
// the host it is already on would be accepted by KubeVirt and then do nothing
// useful, which is a confusing way to spend a minute.
func checkTarget(vmi *kubevirtv1.VirtualMachineInstance, targetNode string) error {
	if targetNode != "" && targetNode == vmi.Status.NodeName {
		return fmt.Errorf("instance %s/%s is already on %s — pick a different --to, or drop it and let KubeVirt choose",
			vmi.Namespace, vmi.Name, targetNode)
	}
	return nil
}

// confirmMigration asks only when there is something worth asking about.
//
// A worker migration is non-destructive and reversible, so prompting every
// time would be noise nobody reads. The one case that deserves a pause is an
// instance that is its cluster's only control plane: live migration ends with
// a brief cutover while the guest is frozen and execution moves host, and with
// a single API server there is nothing to answer during it.
func confirmMigration(cmd *cobra.Command, t target, vmi *kubevirtv1.VirtualMachineInstance, targetNode string) (bool, error) {
	warning := controlPlaneWarning(cmd, t)
	if warning == "" {
		return true, nil
	}
	_, _ = fmt.Fprintln(cmd.ErrOrStderr(), warning)
	return confirm(cmd, fmt.Sprintf("migrate %s/%s from %s%s anyway?",
		t.VM.Namespace, t.VM.Name, dash(vmi.Status.NodeName), pinnedTo(targetNode)))
}

// controlPlaneWarning returns the single-control-plane warning, or "" when
// there is nothing to warn about.
//
// The Machine is nil on the --cluster path, where the Vitistack control plane
// is never consulted: there the guest's shape is genuinely unknown, so this
// says nothing rather than implying it checked and found the cluster safe.
func controlPlaneWarning(cmd *cobra.Command, t target) string {
	if t.Machine == nil || !vm.IsControlPlane(t.Machine) {
		return ""
	}
	if t.AZClient == nil {
		// A control-plane machine whose zone client is missing: say what is
		// known rather than staying silent, because an unverified count is not
		// a reason to withhold the fact that this IS a control plane.
		return fmt.Sprintf("⚠️  %s/%s is a control-plane node; its control-plane peers could not be counted.",
			t.Machine.Namespace, t.Machine.Name)
	}
	peers, cluster, err := vm.ControlPlanePeers(contextOrBackground(cmd), t.AZClient, t.Machine)
	if err != nil {
		warn(cmd, err)
		return fmt.Sprintf("⚠️  %s/%s is a control-plane node; its control-plane peers could not be counted.",
			t.Machine.Namespace, t.Machine.Name)
	}
	if peers != 1 {
		return ""
	}
	return fmt.Sprintf(
		"⚠️  %s is the ONLY control-plane node of cluster %q.\n"+
			"   Live migration ends with a brief pause while the guest is frozen and execution\n"+
			"   switches host. With one API server there is nothing to serve requests during it,\n"+
			"   so expect a short blip on the cluster API.\n"+
			"   If the migration fails, the guest keeps running where it is — nothing is lost.",
		t.Machine.Name, cluster)
}

// awaitAndReport waits for the migration to finish and says what happened,
// including why when it did not work — the same reason migrations --all now
// prints, rather than leaving the operator to go and look it up.
func awaitAndReport(cmd *cobra.Command, t target, name string, interval time.Duration) error {
	ctx := contextOrBackground(cmd)
	m, err := vm.AwaitMigration(ctx, t.KV, t.VM.Namespace, name, interval)
	if err != nil {
		if m != nil {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
				"⚠️  stopped waiting at phase %s — the migration is still running; follow it with: viti kubevirt migrations --watch\n",
				dash(string(m.Status.Phase)))
		}
		return err
	}

	mig := vm.Migration{VMIM: m}
	route := dash(mig.SourceNode()) + "→" + dash(mig.TargetNode())
	if mig.Failed() {
		reason := mig.FailureReason()
		if reason == "" {
			reason = "no reason recorded by KubeVirt — check virt-controller logs and the VMI's events"
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "❌ migration failed (%s): %s\n", route, reason)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "   %s/%s is still running where it was.\n",
			t.VM.Namespace, t.VM.Name)
		return errors.New("migration failed")
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "✅ migrated %s/%s (%s)\n", t.VM.Namespace, t.VM.Name, route)
	return nil
}

// pinnedTo renders the destination clause of a message, empty when KubeVirt is
// choosing.
func pinnedTo(targetNode string) string {
	if targetNode == "" {
		return ""
	}
	return " to " + targetNode
}
