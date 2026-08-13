package cmd

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	kubevirtv1 "kubevirt.io/api/core/v1"

	"github.com/vitistack/vitictl-kubevirt/internal/config"
	"github.com/vitistack/vitictl-kubevirt/internal/kube"
	"github.com/vitistack/vitictl-kubevirt/internal/picker"
	"github.com/vitistack/vitictl-kubevirt/internal/vm"
)

// selectMachine resolves the machine a command should act on: the name when it
// is given and unambiguous, otherwise an interactive pick.
//
// The candidates come from the availability zones alone, so the picker opens
// without first waiting on every KubeVirt cluster in the fleet. The cost is
// that PHASE is the Machine's own and can lag reality; live state is fetched
// afterwards, for the one machine actually chosen.
func selectMachine(cmd *cobra.Command, s *scope, name string) (vm.Located, error) {
	// Checked before listing anything: with no name and no terminal there is
	// no way to choose, so four zone listings would be wasted work.
	if name == "" && !picker.Interactive() {
		return vm.Located{}, errors.New(
			"no machine given — pass one (e.g. 'viti kubevirt reboot my-vm'), " +
				"or run in a terminal to pick one interactively")
	}

	zones, err := config.AvailabilityZones(s.az)
	if err != nil {
		return vm.Located{}, err
	}
	ctx := contextOrBackground(cmd)
	clients, err := kube.ConnectVitistack(ctx, zones, func(e error) { warn(cmd, e) })
	if err != nil {
		return vm.Located{}, err
	}

	candidates := vm.FindMachines(ctx, clients, name, s.namespace, func(e error) { warn(cmd, e) })
	switch len(candidates) {
	case 1:
		return candidates[0], nil
	case 0:
		if name == "" {
			return vm.Located{}, fmt.Errorf("no machines found in any availability zone%s",
				inNamespace(s.namespace))
		}
		return vm.Located{}, fmt.Errorf(
			"no machine named %q found in any availability zone%s — if that is a KubeVirt "+
				"VirtualMachine name rather than a Machine name, name its cluster with "+
				"--cluster to act on it directly",
			name, inNamespace(s.namespace))
	}

	if !picker.Interactive() {
		return vm.Located{}, ambiguous(name, candidates)
	}
	return pickMachine(cmd, candidates)
}

// describe resolves one machine to a full Entry, contacting only the cluster
// that machine actually runs on.
//
// Deliberately not Collect: showing one machine should not cost a join against
// every KubeVirt cluster in the fleet. A cluster that cannot be reached costs
// the entry its live state but is not fatal — the Machine half is still worth
// printing, and the warning says why the rest is missing.
func describe(cmd *cobra.Command, s *scope, name string) (vm.Entry, error) {
	resolver, err := s.resolver()
	if err != nil {
		return vm.Entry{}, err
	}
	found, err := selectMachine(cmd, s, name)
	if err != nil {
		return vm.Entry{}, err
	}

	ctx := contextOrBackground(cmd)
	entry := vm.Entry{AZ: found.AZ.AZ.Name, Machine: found.Machine}
	kv, err := resolver.For(ctx, found.AZ, found.ConfigName)
	if err != nil {
		warn(cmd, fmt.Errorf("availability zone %q: %w — showing machine state only",
			found.AZ.AZ.Name, err))
		return entry, nil
	}
	entry.Cluster = kv.Cluster.Name
	if err := vm.Attach(ctx, kv, &entry); err != nil {
		warn(cmd, fmt.Errorf("%w — showing machine state only", err))
	}
	return entry, nil
}

// pickMachine shows the candidates and returns the chosen one.
func pickMachine(cmd *cobra.Command, candidates []vm.Located) (vm.Located, error) {
	now := time.Now()
	items := make([]picker.Item, 0, len(candidates))
	for _, c := range candidates {
		columns := []string{
			c.AZ.AZ.Name, c.Machine.Namespace, c.Machine.Name,
			dash(c.Phase()), age(c.Machine.CreationTimestamp, now),
		}
		items = append(items, picker.Item{
			// Matched on every column, so "no-central", "cephtest" or a node
			// name narrow the list as readily as a machine name does.
			Label:   strings.Join(columns, " "),
			Columns: columns,
			Value:   c,
		})
	}

	chosen, err := picker.Select(" Select a machine ",
		[]string{"AZ", "NAMESPACE", "NAME", "PHASE", "AGE"}, items)
	if err != nil {
		if errors.Is(err, picker.ErrCancelled) {
			// Cancelling is a decision, not a failure.
			return vm.Located{}, errCancelled
		}
		return vm.Located{}, err
	}
	got, ok := chosen.Value.(vm.Located)
	if !ok {
		return vm.Located{}, fmt.Errorf("picker returned an unexpected item %T", chosen.Value)
	}
	echo(cmd, got.Describe())
	return got, nil
}

// pickVM chooses among a cluster's VirtualMachines directly.
//
// This is the --cluster path, which never consults the control plane, so there
// are no Machines to choose from — only the VMs the cluster itself reports.
func pickVM(cmd *cobra.Command, kv *kube.KubeVirtClient, namespace string) (*kubevirtv1.VirtualMachine, error) {
	if !picker.Interactive() {
		return nil, errors.New(
			"no machine given — pass one, or run in a terminal to pick one interactively")
	}
	vms, err := vm.ListVMs(contextOrBackground(cmd), kv, namespace)
	if err != nil {
		return nil, err
	}
	if len(vms) == 0 {
		return nil, fmt.Errorf("no VirtualMachines in kubevirt cluster %q%s",
			kv.Cluster.Name, inNamespace(namespace))
	}

	now := time.Now()
	items := make([]picker.Item, 0, len(vms))
	for i := range vms {
		v := &vms[i]
		columns := []string{
			v.Namespace, v.Name,
			dash(string(v.Status.PrintableStatus)),
			age(v.CreationTimestamp, now),
		}
		items = append(items, picker.Item{
			Label:   strings.Join(columns, " "),
			Columns: columns,
			Value:   v,
		})
	}

	chosen, err := picker.Select(" Select a virtual machine ",
		[]string{"NAMESPACE", "NAME", "STATUS", "AGE"}, items)
	if err != nil {
		if errors.Is(err, picker.ErrCancelled) {
			return nil, errCancelled
		}
		return nil, err
	}
	got, ok := chosen.Value.(*kubevirtv1.VirtualMachine)
	if !ok {
		return nil, fmt.Errorf("picker returned an unexpected item %T", chosen.Value)
	}
	echo(cmd, kv.Cluster.Name+"/"+got.Namespace+"/"+got.Name)
	return got, nil
}

// firstArg returns the machine name a command was given, or "" when it was
// left out so one can be picked interactively.
func firstArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

// echo reports an interactive choice on stderr, so stdout stays pipeable.
func echo(cmd *cobra.Command, what string) {
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "▶ %s\n", what)
}

// ambiguous explains a name that matches several machines, for when there is
// no terminal to resolve it interactively.
func ambiguous(name string, candidates []vm.Located) error {
	where := make([]string, 0, len(candidates))
	for _, c := range candidates {
		where = append(where, c.AZ.AZ.Name+"/"+c.Machine.Namespace)
	}
	return fmt.Errorf(
		"%q is ambiguous — it exists in %s; narrow it with --namespace or --availabilityzone, "+
			"or run in a terminal to pick one interactively",
		name, strings.Join(where, ", "))
}

// inNamespace renders the namespace qualifier used in the messages above.
func inNamespace(ns string) string {
	if ns == "" {
		return ""
	}
	return " (namespace " + ns + ")"
}
