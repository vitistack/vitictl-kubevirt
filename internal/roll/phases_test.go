package roll

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubevirtv1 "kubevirt.io/api/core/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"

	"github.com/vitistack/vitictl-kubevirt/internal/config"
	"github.com/vitistack/vitictl-kubevirt/internal/kube"
)

func objKey(namespace, name string) ctrlclient.ObjectKey {
	return ctrlclient.ObjectKey{Namespace: namespace, Name: name}
}

// fakeGuest scripts the workload cluster for tests and records every call.
type fakeGuest struct {
	mu    sync.Mutex
	nodes map[string]*corev1.Node
	// drainErr, if set for a node, fails its Drain call.
	drainErr map[string]error
	calls    []string
}

func newFakeGuest(nodeNames ...string) *fakeGuest {
	g := &fakeGuest{nodes: map[string]*corev1.Node{}, drainErr: map[string]error{}}
	for _, n := range nodeNames {
		g.nodes[n] = readyNode(n)
	}
	return g
}

func readyNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
		}},
	}
}

func (g *fakeGuest) record(s string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, s)
}

func (g *fakeGuest) Node(_ context.Context, name string) (*corev1.Node, error) {
	g.record("node " + name)
	n, ok := g.nodes[name]
	if !ok {
		return nil, errors.New("node not found: " + name)
	}
	return n, nil
}

func (g *fakeGuest) Cordon(_ context.Context, name string, desired bool) error {
	g.record(fmt.Sprintf("cordon %s %v", name, desired))
	return nil
}

func (g *fakeGuest) Drain(_ context.Context, name string) error {
	g.record("drain " + name)
	return g.drainErr[name]
}

type recordingReporter struct{ steps []string }

func (r *recordingReporter) Step(format string, args ...any) {
	r.steps = append(r.steps, fmt.Sprintf(format, args...))
}
func (r *recordingReporter) Warn(error) {}

// stagableMember builds a member whose VM lives in a fake KubeVirt cluster.
func stagableMember(t *testing.T, name string) Member {
	t.Helper()
	m := poolMachine(name, "worker", "workers1", "c1-id")
	vmObj := &kubevirtv1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec: kubevirtv1.VirtualMachineSpec{
			Template: &kubevirtv1.VirtualMachineInstanceTemplateSpec{
				Spec: kubevirtv1.VirtualMachineInstanceSpec{
					Domain: kubevirtv1.DomainSpec{CPU: &kubevirtv1.CPU{Cores: 2}},
				},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(vmObj).Build()
	kv := &kube.KubeVirtClient{
		Cluster: config.Cluster{Name: "kv1", Kubeconfig: "/tmp/kc", Context: "ctx"},
		Ctrl:    c,
	}
	return Member{Machine: m, VM: vmObj, KV: kv}
}

func sizedClass() *vitiv1alpha1.MachineClass {
	c := largeClass()
	c.Spec.Enabled = true
	c.Spec.Memory = vitiv1alpha1.MachineClassMemorySpec{Quantity: resource.MustParse("32Gi")}
	c.Spec.CPU = vitiv1alpha1.MachineClassCPUSpec{Cores: 4, Sockets: 1, Threads: 1}
	return c
}

func TestPreflightPasses(t *testing.T) {
	plan := &Plan{Class: sizedClass(), Members: []Member{stagableMember(t, "wrk0")}}
	if err := Preflight(context.Background(), plan, newFakeGuest("wrk0")); err != nil {
		t.Fatal(err)
	}
}

func TestPreflightCatchesUnrestartableCluster(t *testing.T) {
	m := stagableMember(t, "wrk0")
	m.KV.Cluster = config.Cluster{Name: "discovered-only"} // no kubeconfig → virtctl impossible
	plan := &Plan{Class: sizedClass(), Members: []Member{m}}
	err := Preflight(context.Background(), plan, newFakeGuest("wrk0"))
	if err == nil || !strings.Contains(err.Error(), "discovered-only") {
		t.Fatalf("err = %v, want virtctl-target failure naming the cluster", err)
	}
}

func TestPreflightCatchesMissingNode(t *testing.T) {
	plan := &Plan{Class: sizedClass(), Members: []Member{stagableMember(t, "wrk0")}}
	err := Preflight(context.Background(), plan, newFakeGuest()) // no nodes
	if err == nil || !strings.Contains(err.Error(), "wrk0") {
		t.Fatalf("err = %v, want missing-node failure naming wrk0", err)
	}
}

func TestStageVMsPatchesEveryMember(t *testing.T) {
	a, b := stagableMember(t, "wrk0"), stagableMember(t, "wrk1")
	plan := &Plan{Class: sizedClass(), Members: []Member{a, b}}
	if err := StageVMs(context.Background(), plan, &recordingReporter{}); err != nil {
		t.Fatal(err)
	}
	for _, m := range []Member{a, b} {
		var got kubevirtv1.VirtualMachine
		if err := m.KV.Ctrl.Get(context.Background(), objKey("ns", m.VM.Name), &got); err != nil {
			t.Fatal(err)
		}
		if guest := got.Spec.Template.Spec.Domain.Memory.Guest; guest == nil || guest.String() != "32Gi" {
			t.Errorf("%s memory = %v, want 32Gi", m.VM.Name, guest)
		}
	}
}
