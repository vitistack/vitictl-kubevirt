package roll

import (
	"context"
	"errors"
	"fmt"

	"github.com/vitistack/vitictl-kubevirt/internal/vm"
)

// Preflight verifies a rollout can finish before it starts: every member's
// VM must be restartable (virtctl needs a local kubeconfig) and its node must
// exist in the guest cluster. A rollout that would strand half a pool resized
// but unrebootable must fail here, with nothing written.
func Preflight(ctx context.Context, plan *Plan, g Guest) error {
	var problems []error
	for _, m := range plan.Members {
		if _, _, err := m.KV.VirtctlTarget(); err != nil {
			problems = append(problems, fmt.Errorf("machine %q cannot be restarted: %w", m.Machine.Name, err))
			continue
		}
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
