package cmd

import (
	"strings"
	"testing"

	k8sv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubevirtv1 "kubevirt.io/api/core/v1"
)

func runningVMI(node string) *kubevirtv1.VirtualMachineInstance {
	vmi := &kubevirtv1.VirtualMachineInstance{
		ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "vm-a"},
	}
	vmi.Status.Phase = kubevirtv1.Running
	vmi.Status.NodeName = node
	vmi.Status.Conditions = []kubevirtv1.VirtualMachineInstanceCondition{{
		Type: kubevirtv1.VirtualMachineInstanceIsMigratable, Status: k8sv1.ConditionTrue,
	}}
	return vmi
}

// The point of the preflight: refuse before creating anything, carrying
// KubeVirt's own reason, rather than letting a migration fail a minute later
// with a target pod already scheduled.
func TestCheckMigratableRefusesWithKubeVirtsReason(t *testing.T) {
	vmi := runningVMI("wrk01")
	vmi.Status.Conditions[0].Status = k8sv1.ConditionFalse
	vmi.Status.Conditions[0].Reason = "DisksNotLiveMigratable"
	vmi.Status.Conditions[0].Message = "cannot migrate VMI which does not use shared storage"

	err := checkMigratable(vmi)
	if err == nil {
		t.Fatal("want a refusal for a non-migratable instance")
	}
	if !strings.Contains(err.Error(), "shared storage") {
		t.Errorf("error = %v, want KubeVirt's own explanation", err)
	}
}

func TestCheckMigratableRefusesAnInstanceThatIsNotRunning(t *testing.T) {
	vmi := runningVMI("wrk01")
	vmi.Status.Phase = kubevirtv1.Succeeded // stopped
	err := checkMigratable(vmi)
	if err == nil || !strings.Contains(err.Error(), "not Running") {
		t.Errorf("error = %v, want a refusal naming the phase", err)
	}
}

func TestCheckMigratableAcceptsARunningMigratableInstance(t *testing.T) {
	if err := checkMigratable(runningVMI("wrk01")); err != nil {
		t.Errorf("checkMigratable() = %v, want nil", err)
	}
}

// KubeVirt would accept a migration to the node the instance already runs on
// and then achieve nothing, which is a confusing way to spend a minute.
func TestCheckTargetRefusesTheCurrentHost(t *testing.T) {
	err := checkTarget(runningVMI("wrk01"), "wrk01")
	if err == nil || !strings.Contains(err.Error(), "already on") {
		t.Errorf("error = %v, want a refusal for a no-op pin", err)
	}
}

func TestCheckTargetAllowsADifferentHostOrNoPin(t *testing.T) {
	if err := checkTarget(runningVMI("wrk01"), "wrk14"); err != nil {
		t.Errorf("checkTarget(different host) = %v, want nil", err)
	}
	if err := checkTarget(runningVMI("wrk01"), ""); err != nil {
		t.Errorf("checkTarget(no pin) = %v, want nil — KubeVirt chooses", err)
	}
}

func TestPinnedToRendersOnlyWhenPinned(t *testing.T) {
	if got := pinnedTo(""); got != "" {
		t.Errorf("pinnedTo(\"\") = %q, want empty", got)
	}
	if got := pinnedTo("wrk14"); !strings.Contains(got, "wrk14") {
		t.Errorf("pinnedTo(wrk14) = %q, want it to name the node", got)
	}
}

// The --cluster path never consults the Vitistack control plane, so the
// guest's shape is genuinely unknown there. Saying nothing is right; implying
// the cluster was checked and found safe would not be.
func TestControlPlaneWarningSilentWithoutAMachine(t *testing.T) {
	if got := controlPlaneWarning(nil, target{}); got != "" {
		t.Errorf("controlPlaneWarning() = %q, want silence when the Machine is unknown", got)
	}
}
