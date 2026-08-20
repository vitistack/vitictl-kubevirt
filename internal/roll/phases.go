package roll

import (
	"context"
	"errors"
	"fmt"

	"github.com/vitistack/vitictl-kubevirt/internal/vm"
)

// Preflight verifies a rollout can finish before it starts: every member's
// node must exist in the guest cluster, so cordon+drain has something to act
// on. Restarting the VM itself needs nothing checked here — it goes through
// KubeVirt's subresource API on the client already held for m.KV, not
// virtctl, so there is no local-kubeconfig precondition left to fail on. A
// rollout that would strand half a pool resized but with nowhere to drain to
// must fail here, with nothing written.
func Preflight(ctx context.Context, plan *Plan, g Guest) error {
	var problems []error
	for _, m := range plan.Members {
		if _, err := g.Node(ctx, m.Machine.Name); err != nil {
			problems = append(problems, fmt.Errorf("machine %q: guest cluster: %w", m.Machine.Name, err))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("preflight failed, nothing was changed:\n%w", errors.Join(problems...))
	}
	return nil
}

// StageVMs writes every member's new size into its VM template. Non-disruptive
// on its own: the running guests keep their size until each restart.
func StageVMs(ctx context.Context, plan *Plan, rep Reporter) error {
	for _, m := range plan.Members {
		res := vm.DesiredResources(m.Machine, plan.Class)
		if err := vm.PatchVMResources(ctx, m.KV, m.VM, res); err != nil {
			return err
		}
		rep.Step("staged %s: %d cores × %d sockets × %d threads, %s memory",
			m.VM.Name, res.Cores, res.Sockets, res.Threads, res.Memory)
	}
	return nil
}
