package cmd

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubevirtv1 "kubevirt.io/api/core/v1"

	"github.com/vitistack/vitictl-kubevirt/internal/vm"
)

// failedMigration builds a Failed migration carrying the given reason, the
// shape KubeVirt leaves behind once it has given up.
func failedMigration(namespace, vmiName, reason string) vm.Migration {
	m := &kubevirtv1.VirtualMachineInstanceMigration{
		ObjectMeta: metav1.ObjectMeta{Name: "mig-" + vmiName, Namespace: namespace},
		Spec:       kubevirtv1.VirtualMachineInstanceMigrationSpec{VMIName: vmiName},
		Status: kubevirtv1.VirtualMachineInstanceMigrationStatus{
			Phase: kubevirtv1.MigrationFailed,
			MigrationState: &kubevirtv1.VirtualMachineInstanceMigrationState{
				Failed: true, FailureReason: reason,
			},
		},
	}
	return vm.Migration{AZ: "az1", Cluster: "kv-test", VMIM: m}
}

// The gap this closes: PHASE=Failed was the whole story the tool told, and the
// reason was a kubectl round-trip away into
// status.migrationState.failureReason.
func TestPrintFailureReasonsExplainsFailedRows(t *testing.T) {
	const reason = "virError(Code=1, Domain=7, Message='internal error: client socket is closed')"
	var b strings.Builder
	printFailureReasons(&b, []vm.Migration{failedMigration("prod", "vm-a", reason)})

	got := b.String()
	if !strings.Contains(got, reason) {
		t.Errorf("output = %q, want the full libvirt reason", got)
	}
	// The useful half of a libvirt error is at the END, so it must not be
	// truncated away — that is why this is a footnote and not a column.
	if !strings.Contains(got, "client socket is closed") {
		t.Error("the message tail was lost; truncating it keeps the error codes and drops the answer")
	}
	if !strings.Contains(got, "prod/vm-a") {
		t.Errorf("output = %q, want the VMI identified by namespace/name", got)
	}
}

// A migration that failed before it was ever scheduled carries a
// migrationState with little more than a sourcePod and no reason at all —
// observed on pos1-kv-cl01. Saying nothing there would read as "the tool did
// not look".
func TestPrintFailureReasonsHandlesAFailureWithNoRecordedReason(t *testing.T) {
	var b strings.Builder
	printFailureReasons(&b, []vm.Migration{failedMigration("prod", "vm-b", "")})

	got := b.String()
	if !strings.Contains(got, "no reason recorded") {
		t.Errorf("output = %q, want it to say KubeVirt recorded nothing", got)
	}
	if !strings.Contains(got, "events") {
		t.Error("want a pointer to where the answer actually lives")
	}
}

// Silent in the normal case: a rollout with nothing failing must not grow a
// footnote saying so.
func TestPrintFailureReasonsSaysNothingWhenNothingFailed(t *testing.T) {
	ok := &kubevirtv1.VirtualMachineInstanceMigration{
		ObjectMeta: metav1.ObjectMeta{Name: "mig-ok", Namespace: "prod"},
		Spec:       kubevirtv1.VirtualMachineInstanceMigrationSpec{VMIName: "vm-c"},
		Status:     kubevirtv1.VirtualMachineInstanceMigrationStatus{Phase: kubevirtv1.MigrationSucceeded},
	}
	running := &kubevirtv1.VirtualMachineInstanceMigration{
		ObjectMeta: metav1.ObjectMeta{Name: "mig-run", Namespace: "prod"},
		Spec:       kubevirtv1.VirtualMachineInstanceMigrationSpec{VMIName: "vm-d"},
		Status:     kubevirtv1.VirtualMachineInstanceMigrationStatus{Phase: kubevirtv1.MigrationRunning},
	}
	var b strings.Builder
	printFailureReasons(&b, []vm.Migration{
		{AZ: "az1", VMIM: ok}, {AZ: "az1", VMIM: running},
	})
	if b.String() != "" {
		t.Errorf("output = %q, want nothing when no migration failed", b.String())
	}
}

func TestPrintFailureReasonsCountsEveryFailure(t *testing.T) {
	var b strings.Builder
	printFailureReasons(&b, []vm.Migration{
		failedMigration("prod", "vm-a", "broken pipe"),
		failedMigration("prod", "vm-b", ""),
	})
	got := b.String()
	if !strings.Contains(got, "2 failed") {
		t.Errorf("output = %q, want a count of 2", got)
	}
	if strings.Count(got, "❌") != 2 {
		t.Errorf("output = %q, want one line per failure", got)
	}
}
