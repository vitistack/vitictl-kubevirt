package vm

import (
	"strings"
	"testing"

	k8sv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubevirtv1 "kubevirt.io/api/core/v1"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
)

func vmiWithCondition(status k8sv1.ConditionStatus, reason, message string) *kubevirtv1.VirtualMachineInstance {
	vmi := &kubevirtv1.VirtualMachineInstance{
		ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "vm-a"},
	}
	vmi.Status.Conditions = []kubevirtv1.VirtualMachineInstanceCondition{{
		Type: kubevirtv1.VirtualMachineInstanceIsMigratable, Status: status,
		Reason: reason, Message: message,
	}}
	return vmi
}

func TestNotMigratableReasonPassesKubeVirtsOwnExplanation(t *testing.T) {
	vmi := vmiWithCondition(k8sv1.ConditionFalse, "DisksNotLiveMigratable",
		"cannot migrate VMI which does not use shared storage")
	got := NotMigratableReason(vmi)
	if !strings.Contains(got, "DisksNotLiveMigratable") || !strings.Contains(got, "shared storage") {
		t.Errorf("NotMigratableReason() = %q, want KubeVirt's reason and message", got)
	}
}

func TestNotMigratableReasonEmptyWhenMigratable(t *testing.T) {
	if got := NotMigratableReason(vmiWithCondition(k8sv1.ConditionTrue, "", "")); got != "" {
		t.Errorf("NotMigratableReason() = %q, want empty for a migratable instance", got)
	}
}

// An absent condition must not read as permission. KubeVirt publishes it
// shortly after an instance starts, and guessing "probably fine" about a
// safety check is how a preflight becomes decoration.
func TestNotMigratableReasonTreatsAnAbsentConditionAsUnknown(t *testing.T) {
	bare := &kubevirtv1.VirtualMachineInstance{ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "vm-a"}}
	if got := NotMigratableReason(bare); got == "" {
		t.Error("an instance with no LiveMigratable condition must not be reported as migratable")
	}
}

func TestNotMigratableReasonCopesWithASilentCondition(t *testing.T) {
	if got := NotMigratableReason(vmiWithCondition(k8sv1.ConditionFalse, "", "")); got == "" {
		t.Error("a False condition with no reason or message must still refuse")
	}
}

func machineWithRole(namespace, name, cluster, role string) *vitiv1alpha1.Machine {
	m := &vitiv1alpha1.Machine{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
	if role != "" {
		m.Labels = map[string]string{vitiv1alpha1.NodeRoleAnnotation: role}
	}
	if cluster != "" {
		m.OwnerReferences = []metav1.OwnerReference{{Kind: "KubernetesCluster", Name: cluster}}
	}
	return m
}

// The role comes from the operator's label, never the "-ctp0" name suffix:
// the operator derives names independently, so a naming change would silently
// turn every control-plane check into a no-op.
func TestIsControlPlaneReadsTheLabelNotTheName(t *testing.T) {
	labelled := machineWithRole("prod", "c1-abc-wrk0", "c1", RoleControlPlane)
	if !IsControlPlane(labelled) {
		t.Error("a labelled machine is a control plane whatever its name suggests")
	}
	namedLikeOne := machineWithRole("prod", "c1-abc-ctp0", "c1", "")
	if IsControlPlane(namedLikeOne) {
		t.Error("a -ctp0 name without the label must not be taken for a control plane")
	}
	if IsControlPlane(nil) {
		t.Error("nil is not a control plane")
	}
}

func TestOwningClusterFromOwnerReferences(t *testing.T) {
	if got := owningCluster(machineWithRole("prod", "m1", "my-cluster", RoleControlPlane)); got != "my-cluster" {
		t.Errorf("owningCluster() = %q, want my-cluster", got)
	}
	if got := owningCluster(machineWithRole("prod", "m1", "", RoleControlPlane)); got != "" {
		t.Errorf("owningCluster() = %q, want empty when nothing owns it", got)
	}
}

// Counting live machines, not spec.topology.controlPlane.replicas: replicas is
// what the topology asks for, and what decides whether an API server survives
// one node pausing is how many exist right now.
func TestControlPlanePeersCountsLiveMachines(t *testing.T) {
	az := azClient(t,
		machineWithRole("prod", "c1-ctp0", "c1", RoleControlPlane),
		machineWithRole("prod", "c1-ctp1", "c1", RoleControlPlane),
		machineWithRole("prod", "c1-wrk0", "c1", "worker"),
		machineWithRole("prod", "other-ctp0", "other-cluster", RoleControlPlane), // different cluster
		machineWithRole("dev", "c1-ctp9", "c1", RoleControlPlane),                // different namespace
	)
	target := machineWithRole("prod", "c1-ctp0", "c1", RoleControlPlane)

	n, cluster, err := ControlPlanePeers(t.Context(), az, target)
	if err != nil {
		t.Fatalf("ControlPlanePeers() error = %v", err)
	}
	if n != 2 {
		t.Errorf("peers = %d, want 2 — only this cluster's control planes in this namespace", n)
	}
	if cluster != "c1" {
		t.Errorf("cluster = %q, want c1", cluster)
	}
}

func TestControlPlanePeersFindsTheLoneControlPlane(t *testing.T) {
	az := azClient(t,
		machineWithRole("prod", "c1-ctp0", "c1", RoleControlPlane),
		machineWithRole("prod", "c1-wrk0", "c1", "worker"),
		machineWithRole("prod", "c1-wrk1", "c1", "worker"),
	)
	n, _, err := ControlPlanePeers(t.Context(), az, machineWithRole("prod", "c1-ctp0", "c1", RoleControlPlane))
	if err != nil {
		t.Fatalf("ControlPlanePeers() error = %v", err)
	}
	if n != 1 {
		t.Fatalf("peers = %d, want 1 — this is the case the warning exists for", n)
	}
}

// An unowned machine is "could not tell", not "no peers": the difference
// matters when the answer gates a warning about cluster availability.
func TestControlPlanePeersReportsUnknownForAnUnownedMachine(t *testing.T) {
	az := azClient(t, machineWithRole("prod", "loose", "", RoleControlPlane))
	n, cluster, err := ControlPlanePeers(t.Context(), az, machineWithRole("prod", "loose", "", RoleControlPlane))
	if err != nil {
		t.Fatalf("ControlPlanePeers() error = %v", err)
	}
	if n != 0 || cluster != "" {
		t.Errorf("got peers=%d cluster=%q, want 0 and empty so the caller treats it as unknown", n, cluster)
	}
}
