package roll

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"

	"github.com/vitistack/vitictl-kubevirt/internal/kube"
)

// Options are a rollout's timing knobs; zero values take the defaults.
type Options struct {
	// DrainTimeout bounds one node's eviction budget.
	DrainTimeout time.Duration
	// ReadyTimeout bounds one node's return after a restart.
	ReadyTimeout time.Duration
	// PropagationTimeout bounds the wait for the provider operator to copy
	// the topology's class onto the member Machines.
	PropagationTimeout time.Duration
	PollInterval       time.Duration
}

func (o Options) withDefaults() Options {
	if o.DrainTimeout == 0 {
		o.DrainTimeout = 5 * time.Minute
	}
	if o.ReadyTimeout == 0 {
		o.ReadyTimeout = 10 * time.Minute
	}
	if o.PropagationTimeout == 0 {
		o.PropagationTimeout = 2 * time.Minute
	}
	if o.PollInterval == 0 {
		o.PollInterval = 5 * time.Second
	}
	return o
}

// PatchTopology writes the new class into the KubernetesCluster — the source
// of truth the provider operators reconcile Machines from. Patching a Machine
// directly would just be reverted on their next reconcile.
func PatchTopology(ctx context.Context, az *kube.VitistackClient, t Target, newClass string) error {
	var patch ctrlclient.Patch
	if t.Kind == KindControlPlane {
		raw, err := json.Marshal(map[string]any{
			"spec": map[string]any{"topology": map[string]any{
				"controlplane": map[string]any{"machineClass": newClass},
			}},
		})
		if err != nil {
			return err
		}
		patch = ctrlclient.RawPatch(types.MergePatchType, raw)
	} else {
		// A merge patch would replace the whole nodePools array; a JSON patch
		// edits one element, and the test op refuses to fire if the index no
		// longer names the pool we planned against.
		base := fmt.Sprintf("/spec/topology/workers/nodePools/%d", t.PoolIndex)
		raw, err := json.Marshal([]map[string]any{
			{"op": "test", "path": base + "/name", "value": t.Pool},
			{"op": "replace", "path": base + "/machineClass", "value": newClass},
		})
		if err != nil {
			return err
		}
		patch = ctrlclient.RawPatch(types.JSONPatchType, raw)
	}
	if err := az.Ctrl.Patch(ctx, t.Cluster, patch); err != nil {
		return fmt.Errorf("setting %s machineClass on kubernetescluster %s/%s: %w",
			t.Describe(), t.Cluster.Namespace, t.Cluster.Name, err)
	}
	return nil
}

// AwaitPropagation polls the member Machines until the provider operator has
// copied the new class onto every one, so the later VM staging cannot race a
// reconcile that still believes the old class.
func AwaitPropagation(ctx context.Context, az *kube.VitistackClient, plan *Plan, opts Options) error {
	opts = opts.withDefaults()
	deadline := time.Now().Add(opts.PropagationTimeout)

	for {
		var laggards []string
		for _, member := range plan.Members {
			var m vitiv1alpha1.Machine
			key := ctrlclient.ObjectKey{Namespace: member.Machine.Namespace, Name: member.Machine.Name}
			if err := az.Ctrl.Get(ctx, key, &m); err != nil {
				return fmt.Errorf("re-reading machine %s: %w", key.Name, err)
			}
			if m.Spec.MachineClass != plan.Class.Name {
				laggards = append(laggards, m.Name)
			}
		}
		if len(laggards) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf(
				"the provider operator did not propagate class %q to %s within %s — is it running? "+
					"The topology is already patched; re-run to continue once the machines update",
				plan.Class.Name, strings.Join(laggards, ", "), opts.PropagationTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(opts.PollInterval):
		}
	}
}
