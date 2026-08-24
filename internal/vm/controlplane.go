package vm

import (
	"context"
	"fmt"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"

	"github.com/vitistack/vitictl-kubevirt/internal/kube"
)

// IsControlPlane reports whether a Machine runs a control-plane node.
//
// Read from the operator's own label, never from the name. Machine names do
// carry a role suffix — "-ctp0", "-wrk3" — and matching on it would work
// almost always, which is the problem: the operator derives those names
// independently, and a single naming change would silently turn every
// control-plane check in this package into a no-op. Same reasoning as
// LabelSourceMachine.
func IsControlPlane(m *vitiv1alpha1.Machine) bool {
	return m != nil && m.Labels[vitiv1alpha1.NodeRoleAnnotation] == RoleControlPlane
}

// RoleControlPlane is NodeRoleAnnotation's value for control-plane machines.
// Mirrors internal/roll's constant of the same name; duplicated rather than
// imported to keep internal/vm from depending on the rollout package.
const RoleControlPlane = "control-plane"

// ControlPlanePeers counts the control-plane Machines of the cluster owning
// the given Machine, that Machine included.
//
// Counting live Machines rather than reading spec.topology.controlPlane
// .replicas is deliberate: replicas is what the topology ASKS for, and what
// decides whether an API server survives one node pausing is how many exist
// right now. A cluster mid-scale-out, or one whose third control plane never
// came up, would otherwise be reported as safe on the strength of an
// intention.
//
// Returns 0 when the Machine names no owning cluster, which callers must treat
// as unknown rather than as "no peers" — the difference between "this is the
// only control plane" and "we could not tell" matters when the answer gates a
// warning about cluster availability.
func ControlPlanePeers(ctx context.Context, az *kube.VitistackClient, m *vitiv1alpha1.Machine) (int, string, error) {
	cluster := owningCluster(m)
	if cluster == "" {
		return 0, "", nil
	}

	var list vitiv1alpha1.MachineList
	if err := az.Ctrl.List(ctx, &list, ctrlclient.InNamespace(m.Namespace)); err != nil {
		return 0, cluster, fmt.Errorf("zone %q: listing machines in %s: %w", az.AZ.Name, m.Namespace, err)
	}

	n := 0
	for i := range list.Items {
		peer := &list.Items[i]
		if owningCluster(peer) == cluster && IsControlPlane(peer) {
			n++
		}
	}
	return n, cluster, nil
}

// owningCluster is the name of the KubernetesCluster that owns a Machine, from
// its owner references — the same link internal/roll's ownedBy follows.
func owningCluster(m *vitiv1alpha1.Machine) string {
	if m == nil {
		return ""
	}
	for _, ref := range m.OwnerReferences {
		if ref.Kind == "KubernetesCluster" {
			return ref.Name
		}
	}
	return ""
}
