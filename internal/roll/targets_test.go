package roll

import (
	"context"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubevirtv1 "kubevirt.io/api/core/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"

	"github.com/vitistack/vitictl-kubevirt/internal/config"
	"github.com/vitistack/vitictl-kubevirt/internal/kube"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := vitiv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := kubevirtv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func azClient(t *testing.T, objs ...ctrlclient.Object) *kube.VitistackClient {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	return &kube.VitistackClient{AZ: config.AvailabilityZone{Name: "az1"}, Ctrl: c}
}

func testCluster() *vitiv1alpha1.KubernetesCluster {
	return &vitiv1alpha1.KubernetesCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "ns"},
		Spec: vitiv1alpha1.KubernetesClusterSpec{
			Topology: vitiv1alpha1.KubernetesClusterSpecTopology{
				ControlPlane: vitiv1alpha1.KubernetesClusterSpecControlPlane{
					Replicas: 1, MachineClass: "medium",
				},
				Workers: vitiv1alpha1.KubernetesClusterWorkers{
					NodePools: []vitiv1alpha1.KubernetesClusterNodePool{
						{Name: "workers1", MachineClass: "medium", Replicas: 3},
						{Name: "gpu", MachineClass: "large", Replicas: 1},
					},
				},
			},
		},
	}
}

func TestLoadTargets(t *testing.T) {
	got, err := LoadTargets(context.Background(), azClient(t, testCluster()), "ns", "c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d targets, want 3", len(got))
	}
	cp := got[0]
	if cp.Kind != KindControlPlane || cp.Class != "medium" || cp.Replicas != 1 {
		t.Errorf("controlplane target = %+v", cp)
	}
	w := got[1]
	if w.Kind != KindNodePool || w.Pool != "workers1" || w.Class != "medium" || w.Replicas != 3 || w.PoolIndex != 0 {
		t.Errorf("workers1 target = %+v", w)
	}
	g := got[2]
	if g.Pool != "gpu" || g.PoolIndex != 1 {
		t.Errorf("gpu target = %+v", g)
	}
	if cp.Cluster == nil || cp.Cluster.Name != "c1" {
		t.Errorf("target does not carry its cluster")
	}
}

func TestLoadTargetsClusterNotFound(t *testing.T) {
	_, err := LoadTargets(context.Background(), azClient(t), "ns", "nope")
	if !errors.Is(err, ErrClusterNotFound) {
		t.Fatalf("err = %v, want ErrClusterNotFound", err)
	}
}

// clusterWithID returns a cluster fixture naming both its object name and its
// clusterId, so tests can tell which identifier a lookup actually matched on.
func clusterWithID(name, namespace, clusterID string) *vitiv1alpha1.KubernetesCluster {
	c := testCluster()
	c.Name = name
	c.Namespace = namespace
	c.Spec.Cluster.ClusterId = clusterID
	return c
}

func TestFindClusterByExactName(t *testing.T) {
	c := clusterWithID("c1", "ns", "c1-id")
	got, err := findCluster(context.Background(), azClient(t, c), "ns", "c1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "c1" {
		t.Errorf("got cluster %q, want c1", got.Name)
	}
}

func TestFindClusterByClusterId(t *testing.T) {
	c := clusterWithID("c1", "ns", "t-jraviti-123-vexr")
	got, err := findCluster(context.Background(), azClient(t, c), "ns", "t-jraviti-123-vexr")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "c1" {
		t.Errorf("got cluster %q, want c1", got.Name)
	}
}

func TestFindClusterByClusterIdSearchesAllNamespaces(t *testing.T) {
	c := clusterWithID("c1", "other-ns", "t-jraviti-123-vexr")
	got, err := findCluster(context.Background(), azClient(t, c), "", "t-jraviti-123-vexr")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "c1" {
		t.Errorf("got cluster %q, want c1", got.Name)
	}
}

// TestFindClusterNameWinsOverClusterIdCollision is the important case: if one
// cluster's object name happens to equal a different cluster's clusterId, the
// name must win. The object name is what the API server itself keys on, so
// silently preferring the fuzzy clusterId match would risk acting on the
// wrong cluster.
func TestFindClusterNameWinsOverClusterIdCollision(t *testing.T) {
	byName := clusterWithID("dupe", "ns", "dupe-id")
	byClusterID := clusterWithID("other", "ns", "dupe")

	got, err := findCluster(context.Background(), azClient(t, byName, byClusterID), "ns", "dupe")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "dupe" {
		t.Errorf("got cluster %q, want the exact name match \"dupe\"", got.Name)
	}
}

func TestFindClusterNotFoundMentionsBothIdentifiers(t *testing.T) {
	c := clusterWithID("c1", "ns", "c1-id")
	_, err := findCluster(context.Background(), azClient(t, c), "ns", "nope")
	if !errors.Is(err, ErrClusterNotFound) {
		t.Fatalf("err = %v, want ErrClusterNotFound", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "name") || !strings.Contains(msg, "clusterId") {
		t.Errorf("err = %q, want it to mention both name and clusterId were tried", msg)
	}
}

// TestFindClusterNotFoundHintsStrippedSuffix covers the machine-name mistake
// from the bug report: a user types a machine name (clusterId plus a role
// suffix) instead of the cluster's clusterId or object name.
func TestFindClusterNotFoundHintsStrippedSuffix(t *testing.T) {
	c := clusterWithID("c1", "ns", "t-jraviti-123-vexr")
	_, err := findCluster(context.Background(), azClient(t, c), "ns", "t-jraviti-123-vexr-ctp0")
	if !errors.Is(err, ErrClusterNotFound) {
		t.Fatalf("err = %v, want ErrClusterNotFound", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "c1") {
		t.Errorf("err = %q, want it to name the matching cluster c1", msg)
	}
}

func TestFindClusterAmbiguousClusterId(t *testing.T) {
	a := clusterWithID("a", "ns", "dupe-id")
	b := clusterWithID("b", "ns", "dupe-id")
	_, err := findCluster(context.Background(), azClient(t, a, b), "ns", "dupe-id")
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "ambiguous") || !strings.Contains(msg, "ns/a") || !strings.Contains(msg, "ns/b") {
		t.Errorf("err = %q, want it to name both candidates as ambiguous", msg)
	}
}

func TestTargetDescribe(t *testing.T) {
	if d := (Target{Kind: KindControlPlane}).Describe(); d != "controlplane" {
		t.Errorf("Describe = %q", d)
	}
	if d := (Target{Kind: KindNodePool, Pool: "workers1"}).Describe(); d != "nodepool workers1" {
		t.Errorf("Describe = %q", d)
	}
}
