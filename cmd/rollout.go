package cmd

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	kubevirtv1 "kubevirt.io/api/core/v1"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"

	"github.com/vitistack/vitictl-kubevirt/internal/config"
	"github.com/vitistack/vitictl-kubevirt/internal/guest"
	"github.com/vitistack/vitictl-kubevirt/internal/kube"
	"github.com/vitistack/vitictl-kubevirt/internal/picker"
	"github.com/vitistack/vitictl-kubevirt/internal/roll"
	"github.com/vitistack/vitictl-kubevirt/internal/vm"
)

// rolloutFlags carries the changemachineclass flags a rollout needs.
type rolloutFlags struct {
	class        string
	nodepool     string
	controlplane bool
	drainTimeout time.Duration
	noRestart    bool
	skipConfirm  bool
}

// isNotFound reports whether selectMachine failed only because no machine has
// the given name — the case where the name may be a KubernetesCluster.
func isNotFound(err error) bool {
	var nm *noMachineError
	return errors.As(err, &nm)
}

// refuseOwned rejects a per-machine class change on a machine a
// KubernetesCluster owns: the provider operator reconciles the machine's
// class from the cluster topology, so the change would be silently reverted.
// A same-class re-sync stays allowed — it repairs the VM, not the Machine.
func refuseOwned(m *vitiv1alpha1.Machine, oldClass, newClass string) error {
	if oldClass == newClass {
		return nil
	}
	owner := ""
	for _, ref := range m.OwnerReferences {
		if ref.Kind == "KubernetesCluster" {
			owner = ref.Name
			break
		}
	}
	if owner == "" {
		return nil
	}
	fix := "--controlplane"
	if pool := m.Annotations[roll.AnnotationNodepool]; pool != "" {
		fix = "--nodepool " + pool
	}
	return fmt.Errorf(
		"machine %s/%s is owned by kubernetescluster %q, whose topology is the source of truth — "+
			"a class set here would be reverted by the provider operator.\n"+
			"Change it for the whole group instead:\n  viti kubevirt vm changemachineclass %s %s --class %s",
		m.Namespace, m.Name, owner, owner, fix, newClass)
}

// tryClusterRollout interprets name as a KubernetesCluster. handled=false
// means no cluster matched and the caller should return its original error.
func tryClusterRollout(cmd *cobra.Command, s *scope, name string, opts rolloutFlags) (error, bool) {
	az, targets, err := findClusterTargets(cmd, s, name)
	if err != nil {
		if errors.Is(err, roll.ErrClusterNotFound) {
			return nil, false
		}
		return err, true
	}
	return rolloutOn(cmd, s, az, targets, opts), true
}

// runRollout is the --nodepool/--controlplane entry point, where name must be
// a KubernetesCluster.
func runRollout(cmd *cobra.Command, s *scope, name string, opts rolloutFlags) error {
	az, targets, err := findClusterTargets(cmd, s, name)
	if err != nil {
		return err
	}
	return rolloutOn(cmd, s, az, targets, opts)
}

// findClusterTargets searches the availability zones for the named
// KubernetesCluster and returns its rollable targets.
func findClusterTargets(cmd *cobra.Command, s *scope, name string) (*kube.VitistackClient, []roll.Target, error) {
	if _, err := s.resolver(); err != nil { // defaults s.namespace, like every path
		return nil, nil, err
	}
	zones, err := config.AvailabilityZones(s.az)
	if err != nil {
		return nil, nil, err
	}
	ctx := contextOrBackground(cmd)
	clients, err := kube.ConnectVitistack(ctx, zones, func(e error) { warn(cmd, e) })
	if err != nil {
		return nil, nil, err
	}
	for _, az := range clients {
		targets, err := roll.LoadTargets(ctx, az, s.namespace, name)
		if err == nil {
			return az, targets, nil
		}
		if !errors.Is(err, roll.ErrClusterNotFound) {
			return nil, nil, err
		}
	}
	return nil, nil, fmt.Errorf("%w: %q in any availability zone", roll.ErrClusterNotFound, name)
}

// rolloutOn picks the target and class, then runs the phases in order.
func rolloutOn(cmd *cobra.Command, s *scope, az *kube.VitistackClient, targets []roll.Target, opts rolloutFlags) error {
	ctx := contextOrBackground(cmd)

	target, err := chooseTarget(cmd, targets, opts.nodepool, opts.controlplane)
	if err != nil {
		return err
	}
	class, err := chooseRollClass(cmd, ctx, az, target, opts.class)
	if err != nil {
		return err
	}

	resolver, err := s.resolver()
	if err != nil {
		return err
	}
	resolve := func(ctx context.Context, m *vitiv1alpha1.Machine) (*kube.KubeVirtClient, *kubevirtv1.VirtualMachine, error) {
		kv, err := resolver.For(ctx, az, m.Annotations[kube.AnnotationKubevirtConfig])
		if err != nil {
			return nil, nil, err
		}
		vmObj, err := vm.ResolveVM(ctx, kv, m.Name, m.Namespace)
		return kv, vmObj, err
	}
	plan, err := roll.BuildPlan(ctx, az, target, class, resolve)
	if err != nil {
		return err
	}

	secret, err := guest.FindClusterSecret(ctx, az.Ctrl, plan.Target.Cluster.Namespace, plan.ClusterID)
	if err != nil {
		return err
	}
	clientset, err := guest.Connect(secret)
	if err != nil {
		return err
	}
	g := &guest.Client{Clientset: clientset, DrainTimeout: opts.drainTimeout, ErrOut: cmd.ErrOrStderr()}

	if err := roll.Preflight(ctx, plan, g); err != nil {
		return err
	}

	if !opts.skipConfirm {
		ok, err := confirm(cmd, rollSummary(plan)+" — continue?")
		if err != nil {
			return err
		}
		if !ok {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "aborted")
			return errCancelled
		}
	}

	rep := &cmdReporter{cmd: cmd}
	rollOpts := roll.Options{DrainTimeout: opts.drainTimeout}

	if err := roll.PatchTopology(ctx, az, target, class.Name); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "✅ kubernetescluster %s/%s: %s class %s → %s\n",
		target.Cluster.Namespace, target.Cluster.Name, target.Describe(), dash(target.Class), class.Name)

	rep.Step("waiting for the provider operator to update the machines")
	if err := roll.AwaitPropagation(ctx, az, plan, rollOpts); err != nil {
		return err
	}
	if err := roll.StageVMs(ctx, plan, rep); err != nil {
		return err
	}

	if opts.noRestart {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(),
			"💤 staged, not rolled — each VM picks the new size up when it restarts\n")
		return nil
	}

	restart := func(ctx context.Context, m roll.Member) error {
		return runVirtctl(cmd, m.KV, "restart", m.VM.Namespace, m.VM.Name)
	}
	if err := roll.Roll(ctx, plan, g, restart, rollOpts, rep); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "✅ rolled %d machine(s) of %s/%s %s to class %s\n",
		len(plan.Members), target.Cluster.Namespace, target.Cluster.Name, target.Describe(), class.Name)
	return nil
}

// chooseTarget resolves which part of the cluster to roll: by flag, or by an
// interactive pick over the cluster's controlplane and nodepools.
func chooseTarget(cmd *cobra.Command, targets []roll.Target, nodepool string, controlplane bool) (roll.Target, error) {
	if nodepool != "" || controlplane {
		return resolveRollTarget(targets, nodepool, controlplane)
	}
	if !picker.Interactive() {
		return roll.Target{}, fmt.Errorf(
			"name what to roll with --controlplane or --nodepool (have: %s), "+
				"or run in a terminal to pick interactively", strings.Join(poolNames(targets), ", "))
	}
	items := make([]picker.Item, 0, len(targets))
	for i := range targets {
		t := &targets[i]
		columns := []string{t.Describe(), dash(t.Class), strconv.Itoa(t.Replicas)}
		items = append(items, picker.Item{
			Label:   strings.Join(columns, " "),
			Columns: columns,
			Value:   t,
		})
	}
	chosen, err := picker.Select(" Select what to roll ",
		[]string{"TARGET", "CLASS", "REPLICAS"}, items)
	if err != nil {
		if errors.Is(err, picker.ErrCancelled) {
			return roll.Target{}, errCancelled
		}
		return roll.Target{}, err
	}
	got, ok := chosen.Value.(*roll.Target)
	if !ok {
		return roll.Target{}, fmt.Errorf("picker returned an unexpected item %T", chosen.Value)
	}
	echo(cmd, got.Cluster.Name+" "+got.Describe())
	return *got, nil
}

func resolveRollTarget(targets []roll.Target, nodepool string, controlplane bool) (roll.Target, error) {
	for _, t := range targets {
		if controlplane && t.Kind == roll.KindControlPlane {
			return t, nil
		}
		if nodepool != "" && t.Kind == roll.KindNodePool && t.Pool == nodepool {
			return t, nil
		}
	}
	if controlplane {
		return roll.Target{}, errors.New("the cluster has no control plane in its topology")
	}
	return roll.Target{}, fmt.Errorf("no nodepool named %q (have: %s)",
		nodepool, strings.Join(poolNames(targets), ", "))
}

func poolNames(targets []roll.Target) []string {
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		if t.Kind == roll.KindNodePool {
			out = append(out, t.Pool)
		}
	}
	return out
}

// chooseRollClass picks the class to roll to, mirroring chooseClass but for a
// pool: the current class is a valid choice (a whole-pool re-sync).
func chooseRollClass(cmd *cobra.Command, ctx context.Context, az *kube.VitistackClient, t roll.Target, className string) (*vitiv1alpha1.MachineClass, error) {
	classes, err := vm.ListEnabledClasses(ctx, az)
	if err != nil {
		return nil, err
	}
	if className != "" {
		for i := range classes {
			if classes[i].Name == className {
				if className == t.Class {
					warn(cmd, fmt.Errorf("%s already has class %q — re-syncing and rolling anyway", t.Describe(), className))
				}
				return &classes[i], nil
			}
		}
		return nil, fmt.Errorf("machine class %q is not an enabled kubevirt class in zone %q (valid: %s)",
			className, az.AZ.Name, strings.Join(classNames(classes), ", "))
	}
	if !picker.Interactive() {
		return nil, errors.New("no class given — pass one with --class, " +
			"or run in a terminal to pick one interactively")
	}
	if len(classes) == 0 {
		return nil, fmt.Errorf("zone %q has no enabled kubevirt machine class", az.AZ.Name)
	}
	return pickClass(cmd, classes)
}

// rollSummary renders what the rollout is about to do, for the confirmation.
func rollSummary(p *roll.Plan) string {
	baseline := vm.DesiredResources(&vitiv1alpha1.Machine{}, p.Class)
	s := fmt.Sprintf("Roll %s of %s/%s: %d machines, class %s → %s (%s), one at a time with cordon+drain",
		p.Target.Describe(), p.Target.Cluster.Namespace, p.Target.Cluster.Name,
		len(p.Members), dash(p.Target.Class), p.Class.Name, describeResources(baseline))
	if p.Target.Kind == roll.KindControlPlane && p.Target.Replicas == 1 {
		s += " — the cluster API will be unavailable while its only control-plane node reboots"
	}
	return s
}

// cmdReporter prints rollout progress: steps to stderr so structured stdout
// stays pipeable, warnings through the shared warn helper.
type cmdReporter struct{ cmd *cobra.Command }

func (r *cmdReporter) Step(format string, args ...any) {
	_, _ = fmt.Fprintf(r.cmd.ErrOrStderr(), "▶ "+format+"\n", args...)
}

func (r *cmdReporter) Warn(err error) { warn(r.cmd, err) }
