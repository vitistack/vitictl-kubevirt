package roll

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubevirtv1 "kubevirt.io/api/core/v1"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"

	"github.com/vitistack/vitictl-kubevirt/internal/kube"
)

func poolMachine(name, role, pool, clusterID string) *vitiv1alpha1.Machine {
	m := &vitiv1alpha1.Machine{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "ns",
		Labels:          map[string]string{LabelClusterID: clusterID, LabelNodeRole: role},
		OwnerReferences: []metav1.OwnerReference{{Kind: "KubernetesCluster", Name: "c1"}},
	}}
	if pool != "" {
		m.Annotations = map[string]string{AnnotationNodepool: pool}
	}
	return m
}

func okResolver(ctx context.Context, m *vitiv1alpha1.Machine) (*kube.KubeVirtClient, *kubevirtv1.VirtualMachine, error) {
	return &kube.KubeVirtClient{},
		&kubevirtv1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: m.Name, Namespace: m.Namespace}}, nil
}

func largeClass() *vitiv1alpha1.MachineClass {
	return &vitiv1alpha1.MachineClass{ObjectMeta: metav1.ObjectMeta{Name: "large"}}
}

func clusterMachines() []*vitiv1alpha1.Machine {
	return []*vitiv1alpha1.Machine{
		poolMachine("c1-wrk1", "worker", "workers1", "c1-id"),
		poolMachine("c1-wrk0", "worker", "workers1", "c1-id"),
		poolMachine("c1-gpu0", "worker", "gpu", "c1-id"),
		poolMachine("c1-ctp0", RoleControlPlane, "", "c1-id"),
	}
}

func azWithMachines(t *testing.T) *kube.VitistackClient {
	t.Helper()
	ms := clusterMachines()
	other := poolMachine("other-cluster-wrk0", "worker", "workers1", "other-id")
	other.OwnerReferences[0].Name = "other-cluster"
	return azClient(t, testCluster(), ms[0], ms[1], ms[2], ms[3], other)
}

func nodepoolTarget(t *testing.T, az *kube.VitistackClient, pool string) Target {
	t.Helper()
	targets, err := LoadTargets(context.Background(), az, "ns", "c1")
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range targets {
		if target.Pool == pool {
			return target
		}
		if pool == "" && target.Kind == KindControlPlane {
			return target
		}
	}
	t.Fatalf("no target for pool %q", pool)
	return Target{}
}

func TestBuildPlanSelectsPoolMembers(t *testing.T) {
	az := azWithMachines(t)
	plan, err := BuildPlan(context.Background(), az, nodepoolTarget(t, az, "workers1"), largeClass(), okResolver)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Members) != 2 {
		t.Fatalf("got %d members, want 2", len(plan.Members))
	}
	// Sorted by name, only c1's workers1 machines.
	if plan.Members[0].Machine.Name != "c1-wrk0" || plan.Members[1].Machine.Name != "c1-wrk1" {
		t.Errorf("members = %s, %s", plan.Members[0].Machine.Name, plan.Members[1].Machine.Name)
	}
	if plan.ClusterID != "c1-id" {
		t.Errorf("ClusterID = %q", plan.ClusterID)
	}
	if plan.Members[0].VM == nil || plan.Members[0].KV == nil {
		t.Error("members not resolved")
	}
}

func TestBuildPlanControlPlane(t *testing.T) {
	az := azWithMachines(t)
	plan, err := BuildPlan(context.Background(), az, nodepoolTarget(t, az, ""), largeClass(), okResolver)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Members) != 1 || plan.Members[0].Machine.Name != "c1-ctp0" {
		t.Fatalf("members = %+v", plan.Members)
	}
}

func TestBuildPlanFailsWhenAResolveFails(t *testing.T) {
	az := azWithMachines(t)
	bad := func(ctx context.Context, m *vitiv1alpha1.Machine) (*kube.KubeVirtClient, *kubevirtv1.VirtualMachine, error) {
		return nil, nil, errors.New("boom")
	}
	if _, err := BuildPlan(context.Background(), az, nodepoolTarget(t, az, "workers1"), largeClass(), bad); err == nil {
		t.Fatal("want error when a member's VM cannot be resolved")
	}
}

func TestBuildPlanFailsOnZeroMembers(t *testing.T) {
	az := azClient(t, testCluster()) // cluster exists, no machines
	if _, err := BuildPlan(context.Background(), az, nodepoolTarget(t, az, "workers1"), largeClass(), okResolver); err == nil {
		t.Fatal("want error for a pool with no machines")
	}
}
