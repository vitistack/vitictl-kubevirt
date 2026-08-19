package cmd

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"

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
