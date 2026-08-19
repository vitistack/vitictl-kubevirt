package roll

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	kubevirtv1 "kubevirt.io/api/core/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/vitistack/vitictl-kubevirt/internal/vm"
)

// Roll applies a staged plan to the running guests, strictly one member at a
// time: cordon → drain → restart → wait for the VMI to come back at the new
// size → wait for the node to be Ready → uncordon → next. A member that
// cannot complete aborts the rollout — the members already rolled keep the
// new size, and re-running the same command resumes idempotently.
func Roll(ctx context.Context, plan *Plan, g Guest, restart Restarter, opts Options, rep Reporter) error {
	opts = opts.withDefaults()

	for i, m := range plan.Members {
		name := m.Machine.Name
		rep.Step("rolling %s (%d/%d)", name, i+1, len(plan.Members))

		if err := g.Cordon(ctx, name, true); err != nil {
			return fmt.Errorf("cordoning %s: %w — rollout aborted, remaining members untouched", name, err)
		}
		rep.Step("%s cordoned", name)

		if err := g.Drain(ctx, name); err != nil {
			if uerr := g.Cordon(ctx, name, false); uerr != nil {
				rep.Warn(fmt.Errorf("uncordoning %s after the failed drain: %w", name, uerr))
			}
			return fmt.Errorf("draining %s: %w — node uncordoned, rollout aborted, remaining members untouched",
				name, err)
		}
		rep.Step("%s drained", name)

		if err := restart(ctx, m); err != nil {
			if uerr := g.Cordon(ctx, name, false); uerr != nil {
				rep.Warn(fmt.Errorf("uncordoning %s after the failed restart: %w", name, uerr))
			}
			return fmt.Errorf("restarting %s: %w — node uncordoned, rollout aborted", name, err)
		}
		rep.Step("%s restarted", name)

		if err := awaitVMI(ctx, m, plan, opts); err != nil {
			return abortUnready(ctx, g, rep, name, err)
		}
		if err := awaitNodeReady(ctx, g, name, opts); err != nil {
			return abortUnready(ctx, g, rep, name, err)
		}

		if err := g.Cordon(ctx, name, false); err != nil {
			return fmt.Errorf("uncordoning %s: %w — the node is up but still cordoned; "+
				"uncordon it yourself, then re-run to continue", name, err)
		}
		rep.Step("%s back and uncordoned", name)
	}
	return nil
}

// abortUnready handles a member that never came back: uncordon if the guest
// API allows it, and say exactly what state was left behind.
func abortUnready(ctx context.Context, g Guest, rep Reporter, name string, cause error) error {
	if uerr := g.Cordon(ctx, name, false); uerr != nil {
		return fmt.Errorf("%w — the node could not be uncordoned either (%v); "+
			"it is left cordoned, remaining members untouched", cause, uerr)
	}
	rep.Warn(fmt.Errorf("%s was uncordoned after the failure so the cluster owns scheduling again", name))
	return fmt.Errorf("%w — rollout aborted, remaining members untouched", cause)
}

// awaitVMI polls until the member's VMI runs with the planned memory. The
// memory check keeps a pre-restart "Running" (the old instance, not yet torn
// down) from counting as done; a VMI that reports no memory status is trusted
// on phase alone.
func awaitVMI(ctx context.Context, m Member, plan *Plan, opts Options) error {
	desired := vm.DesiredResources(m.Machine, plan.Class)
	deadline := time.Now().Add(opts.ReadyTimeout)
	key := ctrlclient.ObjectKey{Namespace: m.VM.Namespace, Name: m.VM.Name}

	var last string
	for {
		var vmi kubevirtv1.VirtualMachineInstance
		err := m.KV.Ctrl.Get(ctx, key, &vmi)
		switch {
		case err != nil:
			last = "no instance"
		case vmi.Status.Phase != kubevirtv1.Running:
			last = string(vmi.Status.Phase)
		case vmi.Status.Memory == nil || vmi.Status.Memory.GuestAtBoot == nil,
			vmi.Status.Memory != nil && vmi.Status.Memory.GuestAtBoot != nil &&
				vmi.Status.Memory.GuestAtBoot.String() == desired.Memory:
			return nil
		default:
			last = fmt.Sprintf("Running at %s", vmi.Status.Memory.GuestAtBoot)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s: instance not running at %s within %s (last seen: %s)",
				m.Machine.Name, desired.Memory, opts.ReadyTimeout, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(opts.PollInterval):
		}
	}
}

// awaitNodeReady polls the guest cluster until the node reports Ready. Errors
// count as not-ready rather than fatal: while a control plane's only node
// reboots, its own API is down, and that is expected.
func awaitNodeReady(ctx context.Context, g Guest, name string, opts Options) error {
	deadline := time.Now().Add(opts.ReadyTimeout)
	for {
		node, err := g.Node(ctx, name)
		if err == nil && nodeReady(node) {
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("node %s: not reachable within %s (last error: %v)", name, opts.ReadyTimeout, err)
			}
			return fmt.Errorf("node %s: not Ready within %s", name, opts.ReadyTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(opts.PollInterval):
		}
	}
}

func nodeReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
