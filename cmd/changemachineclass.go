package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"

	"github.com/vitistack/vitictl-kubevirt/internal/config"
	"github.com/vitistack/vitictl-kubevirt/internal/kube"
	"github.com/vitistack/vitictl-kubevirt/internal/picker"
	"github.com/vitistack/vitictl-kubevirt/internal/vm"
)

func newVMChangeClassCmd(s *scope) *cobra.Command {
	var className string
	var doRestart, noRestart, assumeYes bool

	cmd := &cobra.Command{
		Use:     "changemachineclass [name]",
		Aliases: []string{"cmc", "change-class"},
		Short:   "📐 Change a machine's machine class",
		Long: `Change the machine class of a KubeVirt machine and resize its VM to match.

The kubevirt-operator only sizes a VirtualMachine when it first creates it, so
editing the Machine's class by hand changes nothing until someone also edits
the VM. This command does the whole workflow: it sets spec.machineClass on the
Machine in its availability zone, writes the class's CPU and memory into the
KubeVirt VirtualMachine's template, and offers to restart the VM — the running
guest keeps its old size until it restarts.

[name] is the Machine name. Leave it out to pick one interactively; leave
--class out to pick the new class from the enabled machine classes ("viti mc
list" shows the same set). A Machine carrying its own spec.cpu or spec.memory
keeps those values — they override any class, so changing the class alone
will not resize such a machine.

This needs the Vitistack control plane, so --cluster cannot be used here.`,
		Example: `  viti kubevirt vm changemachineclass
  viti kubevirt vm cmc my-vm
  viti kubevirt vm cmc my-vm --class large --restart --yes`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if s.cluster != "" {
				return errors.New("--cluster skips the Vitistack control plane, but the machine " +
					"class lives there — run again without --cluster")
			}
			return runChangeClass(cmd, s, firstArg(args), className, doRestart, noRestart, assumeYes)
		},
	}
	cmd.Flags().StringVar(&className, "class", "",
		"machine class to change to (default: pick interactively)")
	cmd.Flags().BoolVar(&doRestart, "restart", false,
		"restart the VM afterwards without asking")
	cmd.Flags().BoolVar(&noRestart, "no-restart", false,
		"never restart; the new size applies on the next restart")
	cmd.Flags().BoolVarP(&assumeYes, "yes", "y", false, "skip the confirmation prompt")
	cmd.MarkFlagsMutuallyExclusive("restart", "no-restart")
	_ = cmd.RegisterFlagCompletionFunc("class", completeClasses(s))
	return cmd
}

func runChangeClass(cmd *cobra.Command, s *scope, name, className string,
	doRestart, noRestart, assumeYes bool) error {
	ctx := contextOrBackground(cmd)

	found, err := selectMachine(cmd, s, name)
	if err != nil {
		return err
	}
	m := found.Machine
	if m.Spec.Provider != "" && m.Spec.Provider != vitiv1alpha1.MachineProviderTypeKubevirt {
		return fmt.Errorf("machine %s/%s runs on provider %q — this plugin can only resize kubevirt machines",
			m.Namespace, m.Name, m.Spec.Provider)
	}

	class, err := chooseClass(cmd, ctx, found, className)
	if err != nil {
		return err
	}
	if class == nil {
		// Choosing the current class is a decision, not a failure.
		return nil
	}

	// Resolve the VM before writing anything, so a machine whose VM is gone
	// fails cleanly instead of leaving the Machine renamed and nothing sized.
	resolver, err := s.resolver()
	if err != nil {
		return err
	}
	kv, err := resolver.For(ctx, found.AZ, found.ConfigName)
	if err != nil {
		return fmt.Errorf("availability zone %q: %w", found.AZ.AZ.Name, err)
	}
	vmObj, err := vm.ResolveVM(ctx, kv, m.Name, m.Namespace)
	if err != nil {
		return err
	}

	res := vm.DesiredResources(m, class)
	if vm.HasResourceOverrides(m) {
		warn(cmd, fmt.Errorf("machine %s/%s sets its own spec.cpu/spec.memory, which override the "+
			"class — the VM keeps those values", m.Namespace, m.Name))
	}
	if !assumeYes {
		ok, err := confirm(cmd, changeSummary(m, class, res)+" — continue?")
		if err != nil {
			return err
		}
		if !ok {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "aborted")
			return errCancelled
		}
	}

	if err := vm.PatchMachineClass(ctx, found.AZ, m, class.Name); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "✅ machine %s/%s: class %s → %s\n",
		m.Namespace, m.Name, dash(m.Spec.MachineClass), class.Name)

	if err := vm.PatchVMResources(ctx, kv, vmObj, res); err != nil {
		return fmt.Errorf("the Machine's class was already changed, but sizing its VM failed: %w\n"+
			"Re-run 'viti kubevirt vm changemachineclass %s --class %s' to retry",
			err, m.Name, class.Name)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "✅ vm %s/%s sized to %s\n",
		vmObj.Namespace, vmObj.Name, describeResources(res))

	return maybeRestart(cmd, kv, vmObj.Namespace, vmObj.Name, doRestart, noRestart)
}

// chooseClass resolves the class to change to: the named one when --class was
// given, otherwise an interactive pick over the enabled classes. It returns
// nil when the choice is the machine's current class, which is a no-op.
func chooseClass(cmd *cobra.Command, ctx context.Context, found vm.Located, className string) (*vitiv1alpha1.MachineClass, error) {
	classes, err := vm.ListEnabledClasses(ctx, found.AZ)
	if err != nil {
		return nil, err
	}
	current := found.Machine.Spec.MachineClass

	if className != "" {
		if className == current {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "machine %s/%s already has class %q — nothing to do\n",
				found.Machine.Namespace, found.Machine.Name, current)
			return nil, nil
		}
		for i := range classes {
			if classes[i].Name == className {
				return &classes[i], nil
			}
		}
		return nil, fmt.Errorf("machine class %q is not an enabled kubevirt class in zone %q (valid: %s)",
			className, found.AZ.AZ.Name, strings.Join(classNames(classes), ", "))
	}

	if !picker.Interactive() {
		return nil, errors.New("no class given — pass one with --class, " +
			"or run in a terminal to pick one interactively")
	}
	candidates := make([]vitiv1alpha1.MachineClass, 0, len(classes))
	for _, c := range classes {
		if c.Name != current {
			candidates = append(candidates, c)
		}
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("zone %q has no enabled kubevirt machine class to change to", found.AZ.AZ.Name)
	}
	return pickClass(cmd, candidates)
}

func pickClass(cmd *cobra.Command, classes []vitiv1alpha1.MachineClass) (*vitiv1alpha1.MachineClass, error) {
	items := make([]picker.Item, 0, len(classes))
	for i := range classes {
		c := &classes[i]
		columns := []string{
			c.Name, dash(c.Spec.Category),
			fmt.Sprintf("%d", c.Spec.CPU.Cores), c.Spec.Memory.Quantity.String(),
			dash(c.Spec.Description),
		}
		items = append(items, picker.Item{
			Label:   strings.Join(columns, " "),
			Columns: columns,
			Value:   c,
		})
	}
	chosen, err := picker.Select(" Select a machine class ",
		[]string{"NAME", "CATEGORY", "CPU", "MEMORY", "DESCRIPTION"}, items)
	if err != nil {
		if errors.Is(err, picker.ErrCancelled) {
			return nil, errCancelled
		}
		return nil, err
	}
	got, ok := chosen.Value.(*vitiv1alpha1.MachineClass)
	if !ok {
		return nil, fmt.Errorf("picker returned an unexpected item %T", chosen.Value)
	}
	echo(cmd, got.Name)
	return got, nil
}

// maybeRestart applies the new size by restarting the VM: outright with
// --restart, never with --no-restart, and after asking otherwise. Declining —
// or having no terminal to ask on — leaves the change pending with a hint,
// because a resize someone may want to schedule must not force a reboot.
func maybeRestart(cmd *cobra.Command, kv *kube.KubeVirtClient, namespace, name string, doRestart, noRestart bool) error {
	restart := doRestart
	if !doRestart && !noRestart {
		if !picker.Interactive() {
			restart = false
		} else {
			ok, err := confirm(cmd, fmt.Sprintf("Restart %s/%s now to apply the new size?", namespace, name))
			if err != nil {
				return err
			}
			restart = ok
		}
	}
	if !restart {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(),
			"💤 not restarted — the new size applies when the VM restarts (viti kubevirt vm restart %s)\n", name)
		return nil
	}
	if err := runVirtctl(cmd, kv, "restart", namespace, name); err != nil {
		return fmt.Errorf("the class change is applied, but the restart failed: %w\n"+
			"Restart later with 'viti kubevirt vm restart %s'", err, name)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "✅ restart %s/%s\n", namespace, name)
	return nil
}

// changeSummary renders what is about to happen, for the confirmation prompt.
func changeSummary(m *vitiv1alpha1.Machine, class *vitiv1alpha1.MachineClass, r vm.Resources) string {
	return fmt.Sprintf("Change machine %s/%s: class %s → %s (%d cores × %d sockets × %d threads, %s memory)",
		m.Namespace, m.Name, dash(m.Spec.MachineClass), class.Name,
		r.Cores, r.Sockets, r.Threads, r.Memory)
}

func describeResources(r vm.Resources) string {
	return fmt.Sprintf("%d cores × %d sockets × %d threads, %s memory", r.Cores, r.Sockets, r.Threads, r.Memory)
}

func classNames(classes []vitiv1alpha1.MachineClass) []string {
	out := make([]string, 0, len(classes))
	for _, c := range classes {
		out = append(out, c.Name)
	}
	return out
}

// completeClasses offers the enabled class names for --class completion,
// staying silent on any error — completion must never print diagnostics.
func completeClasses(s *scope) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
		zones, err := config.AvailabilityZones(s.az)
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		ctx := contextOrBackground(cmd)
		clients, err := kube.ConnectVitistack(ctx, zones, nil)
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		seen := map[string]bool{}
		var names []string
		for _, c := range clients {
			classes, err := vm.ListEnabledClasses(ctx, c)
			if err != nil {
				continue
			}
			for _, cl := range classes {
				if !seen[cl.Name] {
					seen[cl.Name] = true
					names = append(names, cl.Name)
				}
			}
		}
		return names, cobra.ShellCompDirectiveNoFileComp
	}
}
