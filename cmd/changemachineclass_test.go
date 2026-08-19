package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"

	"github.com/vitistack/vitictl-kubevirt/internal/config"
	"github.com/vitistack/vitictl-kubevirt/internal/kube"
	"github.com/vitistack/vitictl-kubevirt/internal/vm"
)

func TestChangeMachineClassIsInTheTree(t *testing.T) {
	out, _, err := run(t, "vm", "--help")
	if err != nil {
		t.Fatalf("run(vm --help) error = %v", err)
	}
	if !strings.Contains(out, "changemachineclass") {
		t.Errorf("vm --help does not list changemachineclass:\n%s", out)
	}
}

func TestChangeMachineClassHelpDocumentsItsFlags(t *testing.T) {
	out, _, err := run(t, "vm", "changemachineclass", "--help")
	if err != nil {
		t.Fatalf("run(vm changemachineclass --help) error = %v", err)
	}
	for _, want := range []string{"--class", "--restart", "--no-restart", "--yes"} {
		if !strings.Contains(out, want) {
			t.Errorf("help does not document %q:\n%s", want, out)
		}
	}
	// The change only takes effect on a reboot; the help must say so.
	if !strings.Contains(strings.ToLower(out), "restart") {
		t.Errorf("help does not explain the restart requirement:\n%s", out)
	}
}

// cmc is what people will actually type.
func TestChangeMachineClassAliases(t *testing.T) {
	for _, alias := range []string{"cmc", "change-class"} {
		out, _, err := run(t, "vm", alias, "--help")
		if err != nil {
			t.Fatalf("run(vm %s --help) error = %v", alias, err)
		}
		if !strings.Contains(out, "changemachineclass") {
			t.Errorf("vm %s --help is not the changemachineclass command:\n%s", alias, out)
		}
	}
}

func TestChangeMachineClassRestartFlagsAreMutuallyExclusive(t *testing.T) {
	isolate(t)
	_, _, err := run(t, "vm", "changemachineclass", "some-vm", "--restart", "--no-restart", "--yes")
	if err == nil {
		t.Fatal("expected an error for --restart together with --no-restart")
	}
}

// The machine class lives in the Vitistack control plane, so the --cluster
// escape hatch (which skips it entirely) cannot serve this command.
func TestChangeMachineClassRefusesClusterFlag(t *testing.T) {
	isolate(t)
	_, _, err := run(t, "vm", "changemachineclass", "some-vm", "--cluster", "kv1", "--yes")
	if err == nil {
		t.Fatal("expected an error when --cluster is given")
	}
	if !strings.Contains(err.Error(), "control plane") {
		t.Errorf("error should explain the control-plane requirement, got: %v", err)
	}
}

func TestChangeSummary(t *testing.T) {
	m := &vitiv1alpha1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "prod"},
		Spec:       vitiv1alpha1.MachineSpec{MachineClass: "small"},
	}
	class := &vitiv1alpha1.MachineClass{
		ObjectMeta: metav1.ObjectMeta{Name: "large"},
		Spec: vitiv1alpha1.MachineClassSpec{
			Memory: vitiv1alpha1.MachineClassMemorySpec{Quantity: resource.MustParse("8Gi")},
			CPU:    vitiv1alpha1.MachineClassCPUSpec{Cores: 4, Sockets: 2, Threads: 2},
		},
	}
	got := changeSummary(m, class, vm.DesiredResources(m, class))
	for _, want := range []string{"prod/web-1", "small", "large", "8Gi", "4"} {
		if !strings.Contains(got, want) {
			t.Errorf("changeSummary = %q, missing %q", got, want)
		}
	}
}

// A machine that never had a class still renders a readable summary.
func TestChangeSummaryWithoutCurrentClass(t *testing.T) {
	m := &vitiv1alpha1.Machine{ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "prod"}}
	class := &vitiv1alpha1.MachineClass{
		ObjectMeta: metav1.ObjectMeta{Name: "large"},
		Spec: vitiv1alpha1.MachineClassSpec{
			Memory: vitiv1alpha1.MachineClassMemorySpec{Quantity: resource.MustParse("8Gi")},
			CPU:    vitiv1alpha1.MachineClassCPUSpec{Cores: 4},
		},
	}
	got := changeSummary(m, class, vm.DesiredResources(m, class))
	if !strings.Contains(got, "large") {
		t.Errorf("changeSummary = %q, missing new class", got)
	}
}

func fakeAZ(t *testing.T, objs ...ctrlclient.Object) *kube.VitistackClient {
	t.Helper()
	s := runtime.NewScheme()
	if err := vitiv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	return &kube.VitistackClient{AZ: config.AvailabilityZone{Name: "az1"}, Ctrl: c}
}

// Naming the machine's current class must re-sync the VM, not exit early:
// a hand-edited spec.machineClass whose VM was never resized is exactly the
// drift this command exists to repair.
func TestChooseClassCurrentClassIsAResync(t *testing.T) {
	class := &vitiv1alpha1.MachineClass{
		ObjectMeta: metav1.ObjectMeta{Name: "large"},
		Spec: vitiv1alpha1.MachineClassSpec{
			Enabled: true,
			Memory:  vitiv1alpha1.MachineClassMemorySpec{Quantity: resource.MustParse("8Gi")},
			CPU:     vitiv1alpha1.MachineClassCPUSpec{Cores: 4},
		},
	}
	found := vm.Located{
		AZ: fakeAZ(t, class),
		Machine: &vitiv1alpha1.Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "prod"},
			Spec:       vitiv1alpha1.MachineSpec{MachineClass: "large"},
		},
	}
	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})

	got, err := chooseClass(cmd, context.Background(), found, "large")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Name != "large" {
		t.Fatalf("chooseClass returned %v, want the current class back for a re-sync", got)
	}
}

// A cluster discovered from the control plane has no local kubeconfig, so
// virtctl cannot restart it — the command must say so and leave the change
// pending rather than fail after already applying it, and must not hint at
// a retry command that would hit the same wall.
func TestMaybeRestartOnDiscoveredClusterExplainsInsteadOfFailing(t *testing.T) {
	kv := &kube.KubeVirtClient{Cluster: config.Cluster{Name: "admin@kv1"}}
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(""))

	if err := maybeRestart(cmd, kv, "prod", "web-1", true, false); err != nil {
		t.Fatalf("maybeRestart error = %v, want nil (the resize is already applied)", err)
	}
	if !strings.Contains(out.String(), "config add") {
		t.Errorf("output does not tell the user how to make the cluster restartable:\n%s", out.String())
	}
}
