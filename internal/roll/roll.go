// Package roll orchestrates a machineclass change across a whole nodepool or
// control plane: patch the KubernetesCluster topology (the source of truth
// the provider operators reconcile Machines from), wait for propagation,
// stage every VM's new size, then roll the machines one at a time behind a
// cordon+drain, each fully back before the next.
//
// Everything here is pure orchestration behind small interfaces — the cmd
// layer supplies the picker, prompts, virtctl and drain implementations — so
// each phase unit-tests against fakes.
package roll

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	kubevirtv1 "kubevirt.io/api/core/v1"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"

	"github.com/vitistack/vitictl-kubevirt/internal/kube"
)

const (
	// LabelClusterID links machines and secrets to their cluster.
	LabelClusterID = "vitistack.io/clusterid"
	// AnnotationNodepool names the nodepool a machine belongs to.
	AnnotationNodepool = "vitistack.io/nodepool"
	// LabelNodeRole distinguishes control-plane machines from workers.
	LabelNodeRole = "vitistack.io/node-role"
	// RoleControlPlane is LabelNodeRole's value for control-plane machines.
	RoleControlPlane = "control-plane"
)

// TargetKind is what part of a cluster a rollout addresses.
type TargetKind string

const (
	KindControlPlane TargetKind = "controlplane"
	KindNodePool     TargetKind = "nodepool"
)

// Target is one rollable part of a KubernetesCluster.
type Target struct {
	Cluster *vitiv1alpha1.KubernetesCluster
	Kind    TargetKind
	// Pool is the nodepool name, empty for the control plane.
	Pool  string
	Class string
	// Replicas is what the topology asks for, shown in the picker; the
	// machines actually found are what a rollout acts on.
	Replicas int
	// PoolIndex is the pool's position in spec.topology.workers.nodePools,
	// guarded by a JSON-patch test op when patching.
	PoolIndex int
}

// Describe renders the target the way prompts and errors name it.
func (t Target) Describe() string {
	if t.Kind == KindControlPlane {
		return "controlplane"
	}
	return "nodepool " + t.Pool
}

// Member is one machine of a rollout, with the VM and cluster acting on it.
type Member struct {
	Machine *vitiv1alpha1.Machine
	VM      *kubevirtv1.VirtualMachine
	KV      *kube.KubeVirtClient
}

// VMResolver finds the KubeVirt cluster and VM backing a machine. Supplied by
// the cmd layer, which owns discovery configuration.
type VMResolver func(ctx context.Context, m *vitiv1alpha1.Machine) (*kube.KubeVirtClient, *kubevirtv1.VirtualMachine, error)

// Plan is a fully-resolved rollout, ready to confirm and execute.
type Plan struct {
	Target  Target
	Class   *vitiv1alpha1.MachineClass
	Members []Member
	// ClusterID is the members' shared vitistack.io/clusterid label, which
	// names the secret holding the guest cluster's kubeconfig.
	ClusterID string
}

// Guest is the workload cluster as a rollout needs it.
type Guest interface {
	Node(ctx context.Context, name string) (*corev1.Node, error)
	Cordon(ctx context.Context, name string, desired bool) error
	Drain(ctx context.Context, name string) error
}

// Restarter reboots one member's VM.
type Restarter func(ctx context.Context, m Member) error

// Reporter receives progress; the cmd layer prints it.
type Reporter interface {
	Step(format string, args ...any)
	Warn(err error)
}
