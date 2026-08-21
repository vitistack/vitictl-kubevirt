package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubevirtv1 "kubevirt.io/api/core/v1"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"

	"github.com/vitistack/vitictl-kubevirt/internal/config"
	"github.com/vitistack/vitictl-kubevirt/internal/kube"
	"github.com/vitistack/vitictl-kubevirt/internal/kubevirt"
	"github.com/vitistack/vitictl-kubevirt/internal/viticli"
	"github.com/vitistack/vitictl-kubevirt/internal/vm"
	"github.com/vitistack/vitictl/pkg/plugin/output"
)

// scope holds the flags every vm subcommand shares.
type scope struct {
	cluster   string
	namespace string
	az        string
}

func (s *scope) register(cmd *cobra.Command) {
	cmd.PersistentFlags().StringVar(&s.cluster, "cluster", "",
		"KubeVirt cluster to use (default: the configured default)")
	cmd.PersistentFlags().StringVarP(&s.namespace, "namespace", "n", "",
		"limit to this namespace")
	cmd.PersistentFlags().StringVarP(&s.az, "availabilityzone", "z", "",
		"limit to a single Vitistack availability zone")
}

// resolver builds this invocation's KubeVirt lookup.
//
// Without --cluster the clusters are discovered from the machines themselves,
// so no local entry is needed and a fleet spanning several KubeVirt clusters
// resolves each machine to its own. With --cluster the user has named one
// outright and it overrides discovery everywhere.
//
// A selectable cluster still supplies the default namespace, so configuring
// one keeps narrowing listings as before. Failing to select is fatal only when
// --cluster was given explicitly: a listing discovers what it needs, and must
// not be blocked by an absent or ambiguous local default.
func (s *scope) resolver() (*kube.Discoverer, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	d := &kube.Discoverer{Local: cfg.Clusters}

	cluster, err := cfg.Select(s.cluster)
	if err != nil {
		if s.cluster != "" {
			return nil, err
		}
		return d, nil
	}
	if s.namespace == "" {
		s.namespace = cluster.Namespace
	}
	if s.cluster != "" {
		kv, err := kube.ConnectKubeVirt(cluster)
		if err != nil {
			return nil, err
		}
		d.Override = kv
	}
	return d, nil
}

// resolveTarget finds the VM a command should act on and the cluster to reach
// it through, picking interactively when the name is absent or ambiguous.
//
// With --cluster the VM is looked up straight in that cluster, which is what
// keeps lifecycle actions and consoles working while the Vitistack control
// plane is down. Otherwise the Machine's own zone names its cluster, so an
// action lands on the right one out of a fleet instead of searching a single
// default and reporting the VM missing.
//
// This deliberately avoids Collect: naming a machine's cluster needs only the
// Machine, so an action costs one list per zone rather than a fleet-wide join
// against every KubeVirt cluster.
func resolveTarget(cmd *cobra.Command, s *scope, name string) (target, error) {
	resolver, err := s.resolver()
	if err != nil {
		return target{}, err
	}
	ctx := contextOrBackground(cmd)

	if resolver.Override != nil {
		t := target{KV: resolver.Override}
		if name == "" {
			t.VM, err = pickVM(cmd, resolver.Override, s.namespace)
			return t, err
		}
		t.VM, err = vm.ResolveVM(ctx, resolver.Override, name, s.namespace)
		return t, err
	}

	found, err := selectMachine(cmd, s, name)
	if err != nil {
		return target{}, err
	}
	kv, err := resolver.For(ctx, found.AZ, found.ConfigName)
	if err != nil {
		return target{}, fmt.Errorf("availability zone %q: %w", found.AZ.AZ.Name, err)
	}
	t := target{KV: kv, Machine: found.Machine, AZ: found.AZ.AZ.Name}
	t.VM, err = vm.ResolveVM(ctx, kv, found.Machine.Name, found.Machine.Namespace)
	return t, err
}

// target is what a single-machine command resolved to.
type target struct {
	KV *kube.KubeVirtClient
	VM *kubevirtv1.VirtualMachine
	// Machine is nil on the --cluster path, where the control plane is never
	// consulted and only the KubeVirt side is known. Anything reading it must
	// cope with not knowing what the guest runs.
	Machine *vitiv1alpha1.Machine
	// AZ is the zone the Machine was found in, empty alongside a nil Machine.
	AZ string
}

func newVMCmd() *cobra.Command {
	s := &scope{}

	cmd := &cobra.Command{
		Use:     "vm",
		Aliases: []string{"vms", "machine", "machines", "m"},
		Short:   "🖥️  List and control KubeVirt virtual machines",
		Long: `List Vitistack Machines enriched with the state of the KubeVirt
VirtualMachine and VirtualMachineInstance backing each one, and act on them.

Machines come from viti's availability zones, and their live state from the
KubeVirt cluster each machine actually runs on — discovered from the machine
itself, so a fleet spanning several zones and clusters lists correctly with no
extra configuration.

--cluster pins every command to one KubeVirt cluster instead, skipping the
control plane. That is the path to use when Vitistack is down: lifecycle
actions and the consoles then need nothing but the KubeVirt cluster.`,
	}
	s.register(cmd)
	cmd.AddCommand(newVMListCmd(s), newVMGetCmd(s), newVMChangeClassCmd(s))
	for _, a := range vmActions() {
		cmd.AddCommand(newVMActionCmd(s, a))
	}
	cmd.AddCommand(newVMConsoleCmd(s, "vnc",
		"🖧  Open a VNC console to a virtual machine",
		`Opens a VNC connection through virtctl, which proxies it to your local
VNC viewer. Requires virtctl on PATH and a running instance.`))
	cmd.AddCommand(newVMConsoleCmd(s, "console",
		"⌨️  Open a virtual machine's console",
		`Attaches to the serial console through virtctl. Detach with Ctrl-] .

Talos guests have no serial shell — no getty, no login, no SSH — so attaching
to one connects successfully and then shows nothing. For those, this opens the
Talos node dashboard through "viti machine console" instead, which resolves the
owning cluster's credentials and the cert-valid node address. --force attaches
to the bare serial line regardless.`))
	return cmd
}

func newVMListCmd(s *scope) *cobra.Command {
	var outputFlag, sortFlag string

	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List machines with their KubeVirt state",
		Example: `  viti kubevirt vm list
  viti kubevirt vm list -o wide
  viti kubevirt vm list -n my-namespace --sort status`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := output.Parse(outputFlag)
			if err != nil {
				return err
			}
			entries, err := collect(cmd, s)
			if err != nil {
				return err
			}
			if err := sortEntries(entries, sortFlag); err != nil {
				return err
			}
			return renderEntries(cmd, entries, format)
		},
	}
	cmd.Flags().StringVarP(&outputFlag, "output", "o", "",
		fmt.Sprintf("output format: %s", strings.Join(output.ValidFormats, ", ")))
	cmd.Flags().StringVarP(&sortFlag, "sort", "s", "",
		"sort by: name, namespace, az, status, node, age")
	return cmd
}

func newVMGetCmd(s *scope) *cobra.Command {
	var outputFlag string

	cmd := &cobra.Command{
		Use:   "get [name]",
		Short: "Show one machine in detail",
		Long: `Show one machine in detail, joining its Machine with the KubeVirt
VirtualMachine and VirtualMachineInstance backing it.

Leave [name] out to pick one from an interactive, fuzzy-searchable list.

Only the chosen machine's cluster is contacted, not the whole fleet, so this
stays fast however many availability zones are configured.`,
		Args:    cobra.MaximumNArgs(1),
		Example: `  viti kubevirt vm get my-vm`,
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := output.Parse(outputFlag)
			if err != nil {
				return err
			}
			entry, err := describe(cmd, s, firstArg(args))
			if err != nil {
				return err
			}
			if format == output.FormatTable {
				printEntry(cmd, entry)
				return nil
			}
			return renderEntries(cmd, []vm.Entry{entry}, format)
		},
	}
	cmd.Flags().StringVarP(&outputFlag, "output", "o", "",
		fmt.Sprintf("output format: %s", strings.Join(output.ValidFormats, ", ")))
	return cmd
}

// action is one virtctl lifecycle verb exposed as a subcommand.
type action struct {
	verb    string
	short   string
	long    string
	aliases []string
	// destructive marks the actions that interrupt a running guest, which are
	// confirmed before they run.
	destructive bool
}

func vmActions() []action {
	return []action{
		{verb: "start", short: "▶️  Start a virtual machine"},
		{verb: "stop", short: "⏹️  Stop a virtual machine", destructive: true,
			long: "Stops the VM gracefully. Use --force to power it off immediately."},
		{verb: "restart", short: "🔄 Restart a virtual machine", aliases: []string{"reboot"},
			destructive: true},
		{verb: "pause", short: "⏸️  Pause a virtual machine", destructive: true,
			long: "Freezes the guest's vCPUs. The guest keeps its memory but stops executing."},
		{verb: "unpause", short: "⏯️  Resume a paused virtual machine"},
		{verb: "reset", short: "⏻  Hard-reset a virtual machine", destructive: true,
			long: "Equivalent to pressing the reset button: the guest is not asked first."},
		{verb: "soft-reboot", short: "🔁 Soft-reboot a virtual machine", aliases: []string{"softreboot"},
			destructive: true, long: "Asks the guest OS to reboot, via the guest agent or ACPI."},
	}
}

func newVMActionCmd(s *scope, a action) *cobra.Command {
	var assumeYes bool
	var extra []string

	long := a.long
	if long == "" {
		long = a.short
	}
	cmd := &cobra.Command{
		Use:     a.verb + " [name]",
		Short:   a.short,
		Aliases: a.aliases,
		Long: long + `

[name] is the Machine name, or the KubeVirt VirtualMachine name if they
differ. Leave it out to pick one from an interactive, fuzzy-searchable list;
a name matching several machines opens the same list narrowed to those.

The KubeVirt cluster is discovered from the machine itself, so the action
lands on the cluster that machine actually runs on; --cluster overrides that
and skips the control plane entirely.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := resolveTarget(cmd, s, firstArg(args))
			if err != nil {
				return err
			}
			if a.destructive && !assumeYes {
				ok, err := confirm(cmd, fmt.Sprintf("%s %s/%s on cluster %q?",
					a.verb, t.VM.Namespace, t.VM.Name, t.KV.Cluster.Name))
				if err != nil {
					return err
				}
				if !ok {
					_, _ = fmt.Fprintln(cmd.OutOrStdout(), "aborted")
					return errCancelled
				}
			}
			// restart goes straight through KubeVirt's subresource API rather
			// than virtctl: it needs nothing virtctl provides (no local
			// kubeconfig, no binary on PATH), and the plugin already holds an
			// authenticated client for the cluster. The rest stay on virtctl —
			// pause/unpause/soft-reboot are VirtualMachineInstance
			// subresources with their own paths and semantics, and start/stop
			// /reset map cleanly enough to virtctl that duplicating them buys
			// nothing yet.
			if a.verb == "restart" {
				err = kubevirt.Restart(contextOrBackground(cmd), t.KV, t.VM.Namespace, t.VM.Name)
			} else {
				err = runVirtctl(cmd, t.KV, a.verb, t.VM.Namespace, t.VM.Name, extra...)
			}
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "✅ %s %s/%s\n", a.verb, t.VM.Namespace, t.VM.Name)
			return err
		},
	}
	cmd.Flags().BoolVarP(&assumeYes, "yes", "y", false, "skip the confirmation prompt")
	if a.verb == "stop" {
		cmd.Flags().BoolVar(&forceStop, "force", false, "power off immediately instead of stopping gracefully")
		cmd.PreRun = func(*cobra.Command, []string) {
			extra = nil
			if forceStop {
				extra = []string{"--force", "--grace-period=0"}
			}
		}
	}
	return cmd
}

// forceStop backs the stop command's --force flag.
var forceStop bool

func newVMConsoleCmd(s *scope, verb, short, long string) *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   verb + " [name]",
		Short: short,
		Long: long + `

[name] is the Machine name, or the KubeVirt VirtualMachine name if they
differ. Leave it out to pick one from an interactive, fuzzy-searchable list.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := resolveTarget(cmd, s, firstArg(args))
			if err != nil {
				return err
			}
			// Only the serial console is useless on Talos; VNC shows its
			// dashboard, so vnc is left alone.
			if verb == "console" && !force && vm.IsTalos(t.Machine) {
				return talosDashboard(cmd, s, t)
			}
			return runVirtctl(cmd, t.KV, verb, t.VM.Namespace, t.VM.Name)
		},
	}
	if verb == "console" {
		cmd.Flags().BoolVar(&force, "force", false,
			"attach to the raw serial console even for a guest that has no shell on it")
	}
	return cmd
}

// talosDashboard opens the Talos node dashboard for a machine.
//
// The serial console a Talos guest exposes is useless — it runs no getty, no
// login shell and no SSH, so virtctl attaches, reports success, and then shows
// nothing at all. The dashboard is the real equivalent, and viti already knows
// how to reach it; see the viticli package for why that work is delegated
// rather than duplicated.
func talosDashboard(cmd *cobra.Command, s *scope, t target) error {
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
		"🖥️  %s runs Talos — it has no serial shell, opening its dashboard instead\n",
		t.Machine.Name)
	return viticli.MachineConsole(
		contextOrBackground(cmd),
		viticli.Streams{In: cmd.InOrStdin(), Out: cmd.OutOrStdout(), Err: cmd.ErrOrStderr()},
		viticli.Target{
			Name:      t.Machine.Name,
			Namespace: t.Machine.Namespace,
			// s.az is the zone the user asked for, which may be empty; the
			// machine's own zone is always known and is what was searched.
			AvailabilityZone: firstNonEmpty(s.az, t.AZ),
		},
	)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// collect connects to both sides and joins them.
func collect(cmd *cobra.Command, s *scope) ([]vm.Entry, error) {
	resolver, err := s.resolver()
	if err != nil {
		return nil, err
	}
	zones, err := config.AvailabilityZones(s.az)
	if err != nil {
		return nil, err
	}
	ctx := contextOrBackground(cmd)
	clients, err := kube.ConnectVitistack(ctx, zones, func(e error) { warn(cmd, e) })
	if err != nil {
		return nil, err
	}
	return vm.Collect(ctx, clients, resolver, s.namespace, func(e error) { warn(cmd, e) })
}

func sortEntries(entries []vm.Entry, key string) error {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "", "name":
		return nil // Collect already orders by namespace then name.
	case "namespace":
		sortBy(entries, func(e vm.Entry) string { return e.Namespace() + "/" + e.Name() })
	case "az":
		sortBy(entries, func(e vm.Entry) string { return e.AZ + "/" + e.Name() })
	case "status":
		sortBy(entries, func(e vm.Entry) string { return e.Status() + "/" + e.Name() })
	case "node":
		sortBy(entries, func(e vm.Entry) string { return e.Node() + "/" + e.Name() })
	case "age":
		sortBy(entries, func(e vm.Entry) string {
			return e.Machine.CreationTimestamp.UTC().Format(time.RFC3339Nano)
		})
	default:
		return fmt.Errorf("unknown sort key %q (valid: name, namespace, az, status, node, age)", key)
	}
	return nil
}

func renderEntries(cmd *cobra.Command, entries []vm.Entry, format output.Format) error {
	out := cmd.OutOrStdout()
	switch format {
	case output.FormatJSON:
		return output.WriteJSON(out, structured(entries))
	case output.FormatYAML:
		return output.WriteYAML(out, structured(entries))
	case output.FormatName:
		for _, e := range entries {
			_, _ = fmt.Fprintf(out, "virtualmachine/%s/%s\n", e.Namespace(), e.VMName())
		}
		return nil
	}
	if len(entries) == 0 {
		_, err := fmt.Fprintln(out, "🤷 no machines found")
		return err
	}

	wide := format == output.FormatWide
	header := "AZ\tCLUSTER\tNAMESPACE\tNAME\tSTATUS\tREADY\tNODE\tIPS\tAGE"
	if wide {
		header = "AZ\tCLUSTER\tNAMESPACE\tNAME\tVM\tSTATUS\tREADY\tPHASE\tNODE\tIPS\tCPU\tMEMORY\tAGE"
	}
	rows := make([]string, 0, len(entries))
	now := time.Now()
	for _, e := range entries {
		ips := strings.Join(e.IPs(), ",")
		if wide {
			rows = append(rows, strings.Join([]string{
				dash(e.AZ), dash(e.Cluster), dash(e.Namespace()), dash(e.Name()), dash(e.VMName()),
				dash(e.Status()), dash(e.Ready()), dash(e.Machine.Status.Phase),
				dash(e.Node()), dash(ips),
				fmt.Sprintf("%d", e.Machine.Spec.CPU.Cores), humanBytes(e.Machine.Spec.Memory),
				age(e.Machine.CreationTimestamp, now),
			}, "\t"))
			continue
		}
		rows = append(rows, strings.Join([]string{
			dash(e.AZ), dash(e.Cluster), dash(e.Namespace()), dash(e.Name()),
			dash(e.Status()), dash(e.Ready()), dash(e.Node()),
			dash(truncate(ips, 40)), age(e.Machine.CreationTimestamp, now),
		}, "\t"))
	}
	return output.WriteTable(out, header, rows)
}

// entryJSON is the machine-readable shape: both halves, unflattened, so a
// consumer can reach anything either resource carries.
type entryJSON struct {
	AvailabilityZone string `json:"availabilityZone"`
	// KubeVirtCluster names the cluster the live state came from, absent when
	// it could not be reached.
	KubeVirtCluster string `json:"kubevirtCluster,omitempty"`
	Machine         any    `json:"machine"`
	VirtualMachine  any    `json:"virtualMachine,omitempty"`
	Instance        any    `json:"virtualMachineInstance,omitempty"`
}

func structured(entries []vm.Entry) []entryJSON {
	out := make([]entryJSON, 0, len(entries))
	for _, e := range entries {
		row := entryJSON{AvailabilityZone: e.AZ, KubeVirtCluster: e.Cluster, Machine: e.Machine}
		if e.VM != nil {
			row.VirtualMachine = e.VM
		}
		if e.VMI != nil {
			row.Instance = e.VMI
		}
		out = append(out, row)
	}
	return out
}

func printEntry(cmd *cobra.Command, e vm.Entry) {
	out := cmd.OutOrStdout()
	pf := func(format string, a ...any) { _, _ = fmt.Fprintf(out, format, a...) }

	pf("🏷️  Name:          %s\n", e.Name())
	if e.VM != nil && e.VM.Name != e.Name() {
		pf("🖥️  VM:            %s\n", e.VM.Name)
	}
	pf("📦 Namespace:     %s\n", e.Namespace())
	pf("🎯 AZ:            %s\n", e.AZ)
	pf("☸️  KubeVirt:      %s\n", dash(e.Cluster))
	pf("🚦 Status:        %s\n", dash(e.Status()))
	pf("✅ Ready:         %s\n", dash(e.Ready()))
	pf("📊 Machine phase: %s\n", dash(e.Machine.Status.Phase))
	if e.Machine.Spec.CPU.Cores > 0 || e.Machine.Spec.Memory > 0 {
		pf("🧠 CPU / Memory:  %d cores / %s\n", e.Machine.Spec.CPU.Cores, humanBytes(e.Machine.Spec.Memory))
	}
	pf("⏱️  Age:           %s\n", age(e.Machine.CreationTimestamp, time.Now()))

	if e.VM == nil {
		pf("\n⚠️  No KubeVirt VirtualMachine found for this Machine on cluster.\n")
		return
	}
	if e.VM.Spec.RunStrategy != nil {
		pf("🔁 Run strategy:  %s\n", string(*e.VM.Spec.RunStrategy))
	}
	if e.VMI == nil {
		pf("\n💤 Not running — no VirtualMachineInstance.\n")
		return
	}
	pf("\n🖧  Instance:\n")
	pf("  - node:    %s\n", dash(e.VMI.Status.NodeName))
	pf("  - phase:   %s\n", string(e.VMI.Status.Phase))
	if len(e.IPs()) > 0 {
		pf("  - ips:     %s\n", strings.Join(e.IPs(), ", "))
	}
	for _, c := range e.VMI.Status.Conditions {
		if c.Reason != "" {
			pf("  - %s: %s (%s)\n", string(c.Type), string(c.Status), c.Reason)
		}
	}
}

func sortBy(entries []vm.Entry, key func(vm.Entry) string) {
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0 && key(entries[j]) < key(entries[j-1]); j-- {
			entries[j], entries[j-1] = entries[j-1], entries[j]
		}
	}
}

func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}

// age renders a timestamp as a short duration, matching viti's column style.
func age(t metav1.Time, now time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := now.Sub(t.Time)
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	default:
		return fmt.Sprintf("%dy", int(d.Hours())/(24*365))
	}
}

// humanBytes renders a byte count as "4Gi" or "512Mi", matching viti.
func humanBytes(b int64) string {
	if b <= 0 {
		return "-"
	}
	const (
		kib = 1024
		mib = 1024 * kib
		gib = 1024 * mib
		tib = 1024 * gib
	)
	switch {
	case b >= tib:
		return fmt.Sprintf("%dTi", b/tib)
	case b >= gib:
		return fmt.Sprintf("%dGi", b/gib)
	case b >= mib:
		return fmt.Sprintf("%dMi", b/mib)
	case b >= kib:
		return fmt.Sprintf("%dKi", b/kib)
	default:
		return fmt.Sprintf("%dB", b)
	}
}
