package roll

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
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
			return fmt.Errorf("cordoning %s: %w — %s", name, err, abortNote)
		}
		rep.Step("%s cordoned", name)

		if err := g.Drain(ctx, name); err != nil {
			if uerr := g.Cordon(ctx, name, false); uerr != nil {
				rep.Warn(fmt.Errorf("uncordoning %s after the failed drain: %w", name, uerr))
			}
			return fmt.Errorf("draining %s: %w — node uncordoned, %s", name, err, abortNote)
		}
		rep.Step("%s drained", name)

		// Captured before the restart request: virtctl restart is
		// asynchronous, and until KubeVirt tears the instance down the OLD
		// VMI is still Running — at the desired size too, whenever the roll
		// is a re-sync or CPU-only change. awaitVMI must see a NEW instance.
		oldUID := currentVMIUID(ctx, m)

		if err := restart(ctx, m); err != nil {
			if uerr := g.Cordon(ctx, name, false); uerr != nil {
				rep.Warn(fmt.Errorf("uncordoning %s after the failed restart: %w", name, uerr))
			}
			return fmt.Errorf("restarting %s: %w — node uncordoned, %s", name, err, abortNote)
		}
		rep.Step("%s restarted", name)

		if err := awaitVMI(ctx, m, plan, opts, oldUID); err != nil {
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

// abortNote is appended to every mid-roll failure. By the time Roll runs, the
// topology patch and StageVMs already put the new size on every member's
// Machine and VM template — an abort stops the restarts, nothing more, and
// saying otherwise invites operators to walk away from nodes that will
// silently resize on their next (even unplanned) reboot.
const abortNote = "rollout aborted: no further machines are restarted, but every member's new size " +
	"is already staged and applies on its next restart — re-run the same command to finish the roll"

// abortUnready handles a member that never came back: uncordon if the guest
// API allows it, and say exactly what state was left behind.
func abortUnready(ctx context.Context, g Guest, rep Reporter, name string, cause error) error {
	if uerr := g.Cordon(ctx, name, false); uerr != nil {
		return fmt.Errorf("%w — the node could not be uncordoned either (%v) and is left cordoned; %s",
			cause, uerr, abortNote)
	}
	rep.Warn(fmt.Errorf("%s was uncordoned after the failure so the cluster owns scheduling again", name))
	return fmt.Errorf("%w — %s", cause, abortNote)
}

// currentVMIUID reads the UID of the member's live VMI, "" when none can be
// read — awaitVMI then trusts the first Running instance it sees, which is the
// best that can be done without a pre-restart observation.
func currentVMIUID(ctx context.Context, m Member) types.UID {
	var vmi kubevirtv1.VirtualMachineInstance
	key := ctrlclient.ObjectKey{Namespace: m.VM.Namespace, Name: m.VM.Name}
	if err := m.KV.Ctrl.Get(ctx, key, &vmi); err != nil {
		return ""
	}
	return vmi.UID
}

// awaitVMI polls until a FRESH instance of the member's VMI (one whose UID
// differs from the pre-restart oldUID) runs with the planned memory. The UID
// check is what keeps the old instance — still Running until KubeVirt tears
// it down, and at the desired size on a re-sync or CPU-only change — from
// counting as done. A fresh VMI that reports no memory status is trusted on
// phase alone.
func awaitVMI(ctx context.Context, m Member, plan *Plan, opts Options, oldUID types.UID) error {
	desired := vm.DesiredResources(m.Machine, plan.Class)
	// Quantities that are equal can render differently ("4096Mi" vs "4Gi"),
	// so the comparison must be numeric, not string equality.
	desiredMem, memErr := resource.ParseQuantity(desired.Memory)
	deadline := time.Now().Add(opts.ReadyTimeout)
	key := ctrlclient.ObjectKey{Namespace: m.VM.Namespace, Name: m.VM.Name}

	var last string
	for {
		var vmi kubevirtv1.VirtualMachineInstance
		err := m.KV.Ctrl.Get(ctx, key, &vmi)
		switch {
		case err != nil:
			last = "no instance"
		case oldUID != "" && vmi.UID == oldUID:
			last = "the pre-restart instance, not yet torn down"
		case vmi.Status.Phase != kubevirtv1.Running:
			last = string(vmi.Status.Phase)
		case vmi.Status.Memory == nil || vmi.Status.Memory.GuestAtBoot == nil,
			memoryMatches(*vmi.Status.Memory.GuestAtBoot, desiredMem, memErr, desired.Memory):
			return nil
		default:
			last = fmt.Sprintf("Running at %s", vmi.Status.Memory.GuestAtBoot)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s: no restarted instance running at %s within %s (last seen: %s)",
				m.Machine.Name, desired.Memory, opts.ReadyTimeout, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(opts.PollInterval):
		}
	}
}

// memoryMatches compares the observed guest memory to the desired quantity,
// falling back to string equality only if the desired value failed to parse
// (which DesiredResources never produces).
func memoryMatches(got, desired resource.Quantity, parseErr error, desiredStr string) bool {
	if parseErr != nil {
		return got.String() == desiredStr
	}
	return got.Cmp(desired) == 0
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
