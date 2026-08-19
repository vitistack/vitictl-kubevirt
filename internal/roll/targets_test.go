package roll

import (
	"context"
	"errors"
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

func TestTargetDescribe(t *testing.T) {
	if d := (Target{Kind: KindControlPlane}).Describe(); d != "controlplane" {
		t.Errorf("Describe = %q", d)
	}
	if d := (Target{Kind: KindNodePool, Pool: "workers1"}).Describe(); d != "nodepool workers1" {
		t.Errorf("Describe = %q", d)
	}
}
