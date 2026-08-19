package roll

import (
	"context"
	"fmt"
	"sort"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"

	"github.com/vitistack/vitictl-kubevirt/internal/kube"
)

// BuildPlan enumerates a target's member machines and resolves each one's VM,
// producing everything a rollout needs before anything is written.
//
// Membership is the intersection of ownership and role: owned by the target's
// KubernetesCluster (controller relationships survive renames where
// name-prefix guessing would not), and carrying the nodepool annotation or
// control-plane role label the provider operators stamp on what they create.
func BuildPlan(ctx context.Context, az *kube.VitistackClient, t Target, class *vitiv1alpha1.MachineClass, resolve VMResolver) (*Plan, error) {
	var list vitiv1alpha1.MachineList
	if err := az.Ctrl.List(ctx, &list, ctrlclient.InNamespace(t.Cluster.Namespace)); err != nil {
		return nil, fmt.Errorf("zone %q: listing machines: %w", az.AZ.Name, err)
	}

	plan := &Plan{Target: t, Class: class}
	for i := range list.Items {
		m := &list.Items[i]
		if !ownedBy(m, t.Cluster.Name) || !inTarget(m, t) {
			continue
		}
		id := m.Labels[LabelClusterID]
		if id == "" {
			return nil, fmt.Errorf("machine %s/%s has no %s label — cannot locate the guest cluster's kubeconfig secret",
				m.Namespace, m.Name, LabelClusterID)
		}
		if plan.ClusterID != "" && plan.ClusterID != id {
			return nil, fmt.Errorf("members disagree on %s (%q vs %q) — refusing to guess which guest cluster to drain",
				LabelClusterID, plan.ClusterID, id)
		}
		plan.ClusterID = id
		plan.Members = append(plan.Members, Member{Machine: m})
	}
	if len(plan.Members) == 0 {
		return nil, fmt.Errorf("cluster %q has no machines in its %s — nothing to roll",
			t.Cluster.Name, t.Describe())
	}
	sort.Slice(plan.Members, func(i, j int) bool {
		return plan.Members[i].Machine.Name < plan.Members[j].Machine.Name
	})

	for i := range plan.Members {
		m := plan.Members[i].Machine
		kv, vmObj, err := resolve(ctx, m)
		if err != nil {
			return nil, fmt.Errorf("machine %s/%s: %w", m.Namespace, m.Name, err)
		}
		plan.Members[i].KV = kv
		plan.Members[i].VM = vmObj
	}
	return plan, nil
}

func ownedBy(m *vitiv1alpha1.Machine, clusterName string) bool {
	for _, ref := range m.OwnerReferences {
		if ref.Kind == "KubernetesCluster" && ref.Name == clusterName {
			return true
		}
	}
	return false
}

func inTarget(m *vitiv1alpha1.Machine, t Target) bool {
	if t.Kind == KindControlPlane {
		return m.Labels[LabelNodeRole] == RoleControlPlane
	}
	return m.Annotations[AnnotationNodepool] == t.Pool
}
