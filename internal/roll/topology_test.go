package roll

import (
	"context"
	"strings"
	"testing"
	"time"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"

	"github.com/vitistack/vitictl-kubevirt/internal/kube"
)

func getCluster(t *testing.T, az *kube.VitistackClient) *vitiv1alpha1.KubernetesCluster {
	t.Helper()
	var kc vitiv1alpha1.KubernetesCluster
	if err := az.Ctrl.Get(context.Background(), ctrlclient.ObjectKey{Namespace: "ns", Name: "c1"}, &kc); err != nil {
		t.Fatal(err)
	}
	return &kc
}

func TestPatchTopologyControlPlane(t *testing.T) {
	az := azClient(t, testCluster())
	target := nodepoolTarget(t, az, "")
	if err := PatchTopology(context.Background(), az, target, "large"); err != nil {
		t.Fatal(err)
	}
	kc := getCluster(t, az)
	if kc.Spec.Topology.ControlPlane.MachineClass != "large" {
		t.Errorf("controlplane class = %q", kc.Spec.Topology.ControlPlane.MachineClass)
	}
	if kc.Spec.Topology.Workers.NodePools[0].MachineClass != "medium" {
		t.Errorf("nodepool clobbered: %q", kc.Spec.Topology.Workers.NodePools[0].MachineClass)
	}
}

func TestPatchTopologyNodePool(t *testing.T) {
	az := azClient(t, testCluster())
	target := nodepoolTarget(t, az, "workers1")
	if err := PatchTopology(context.Background(), az, target, "large"); err != nil {
		t.Fatal(err)
	}
	kc := getCluster(t, az)
	if kc.Spec.Topology.Workers.NodePools[0].MachineClass != "large" {
		t.Errorf("workers1 class = %q", kc.Spec.Topology.Workers.NodePools[0].MachineClass)
	}
	if kc.Spec.Topology.Workers.NodePools[1].MachineClass != "large" && kc.Spec.Topology.Workers.NodePools[1].Name == "gpu" {
		// gpu started as large; ensure it was not touched to something else.
		t.Errorf("gpu pool clobbered: %+v", kc.Spec.Topology.Workers.NodePools[1])
	}
	if kc.Spec.Topology.ControlPlane.MachineClass != "medium" {
		t.Errorf("controlplane clobbered: %q", kc.Spec.Topology.ControlPlane.MachineClass)
	}
}

// A stale PoolIndex must not patch some other pool: the JSON-patch test op
// has to make the whole patch fail instead.
func TestPatchTopologyNodePoolIndexDrift(t *testing.T) {
	az := azClient(t, testCluster())
	target := nodepoolTarget(t, az, "workers1")
	target.PoolIndex = 1 // actually "gpu"
	if err := PatchTopology(context.Background(), az, target, "small"); err == nil {
		t.Fatal("want error when the pool index no longer matches the pool name")
	}
	kc := getCluster(t, az)
	if kc.Spec.Topology.Workers.NodePools[1].MachineClass != "large" {
		t.Errorf("gpu pool was modified despite the failed test op: %q",
			kc.Spec.Topology.Workers.NodePools[1].MachineClass)
	}
}

func fastOpts() Options {
	return Options{PollInterval: 5 * time.Millisecond, PropagationTimeout: 100 * time.Millisecond}
}

func TestAwaitPropagationAlreadyDone(t *testing.T) {
	m := poolMachine("c1-wrk0", "worker", "workers1", "c1-id")
	m.Spec.MachineClass = "large"
	az := azClient(t, testCluster(), m)
	plan := &Plan{Class: largeClass(), Members: []Member{{Machine: m}}}
	if err := AwaitPropagation(context.Background(), az, plan, fastOpts()); err != nil {
		t.Fatal(err)
	}
}

func TestAwaitPropagationTimesOutNamingLaggards(t *testing.T) {
	m := poolMachine("c1-wrk0", "worker", "workers1", "c1-id")
	m.Spec.MachineClass = "medium" // never updated
	az := azClient(t, testCluster(), m)
	plan := &Plan{Class: largeClass(), Members: []Member{{Machine: m}}}
	err := AwaitPropagation(context.Background(), az, plan, fastOpts())
	if err == nil || !strings.Contains(err.Error(), "c1-wrk0") {
		t.Fatalf("err = %v, want timeout naming c1-wrk0", err)
	}
}
