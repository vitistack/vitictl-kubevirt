package vm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vitistack/vitictl-kubevirt/internal/config"
	"github.com/vitistack/vitictl-kubevirt/internal/kube"
)

// findOrphan returns the single orphan of the given kind, failing the test
// if there is not exactly one — assertions read better against one row than
// against a slice index that shifts as other kinds are added to a fixture.
func findOrphan(t *testing.T, orphans []Orphan, kind OrphanKind) Orphan {
	t.Helper()
	var hits []Orphan
	for _, o := range orphans {
		if o.Kind == kind {
			hits = append(hits, o)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("got %d findings of kind %q, want 1: %+v", len(hits), kind, orphans)
	}
	return hits[0]
}

// A VM naming a Machine that was never created — or has since been deleted —
// is the dangerous case this command exists for: real compute nobody is
// tracking. A legitimate machine sharing the cluster is what lets the
// cluster be discovered at all, since discovery is anchored on Machines.
func TestDetectOrphansReportsVMWithoutMachine(t *testing.T) {
	kv := kvClient(t,
		virtualMachine("prod", "vm-real", "real-machine"),
		virtualMachine("prod", "vm-ghost", "ghost-machine"),
	)
	az := azClient(t, machine("prod", "real-machine"))

	report, err := DetectOrphans(context.Background(),
		[]*kube.VitistackClient{az}, fixedResolver{kv}, "", 15*time.Minute, time.Now(), nil)
	if err != nil {
		t.Fatalf("DetectOrphans() error = %v", err)
	}

	o := findOrphan(t, report.Orphans, KindVMWithoutMachine)
	if o.Name != "vm-ghost" || o.Namespace != "prod" {
		t.Errorf("got %s/%s, want prod/vm-ghost", o.Namespace, o.Name)
	}
	if !strings.Contains(o.Detail, "ghost-machine") {
		t.Errorf("Detail = %q, should name the missing machine", o.Detail)
	}
	if o.Cluster != "kv-test" {
		t.Errorf("Cluster = %q, want kv-test", o.Cluster)
	}
	if !report.Coverage.Complete() {
		t.Errorf("Coverage = %+v, want complete", report.Coverage)
	}
}

// A Machine whose cluster answered but holds no VM is the same state
// "vm list" already shows as a blank column, reported here as a finding.
func TestDetectOrphansReportsMachineWithoutVM(t *testing.T) {
	kv := kvClient(t)
	az := azClient(t, machine("prod", "solo"))

	report, err := DetectOrphans(context.Background(),
		[]*kube.VitistackClient{az}, fixedResolver{kv}, "", 15*time.Minute, time.Now(), nil)
	if err != nil {
		t.Fatalf("DetectOrphans() error = %v", err)
	}

	o := findOrphan(t, report.Orphans, KindMachineWithoutVM)
	if o.Name != "solo" || o.Namespace != "prod" {
		t.Errorf("got %s/%s, want prod/solo", o.Namespace, o.Name)
	}
}

// A running instance with no VirtualMachine behind it should not happen
// through the ordinary lifecycle — KubeVirt only ever creates a VMI from a
// VM — so one appearing alone means the VM was removed out from under it.
func TestDetectOrphansReportsVMIWithoutVM(t *testing.T) {
	kv := kvClient(t,
		virtualMachine("prod", "vm-real", "real-machine"),
		instance("prod", "vm-real", "node-1"),
		instance("prod", "vmi-ghost", "node-2"),
	)
	az := azClient(t, machine("prod", "real-machine"))

	report, err := DetectOrphans(context.Background(),
		[]*kube.VitistackClient{az}, fixedResolver{kv}, "", 15*time.Minute, time.Now(), nil)
	if err != nil {
		t.Fatalf("DetectOrphans() error = %v", err)
	}

	o := findOrphan(t, report.Orphans, KindVMIWithoutVM)
	if o.Name != "vmi-ghost" || o.Namespace != "prod" {
		t.Errorf("got %s/%s, want prod/vmi-ghost", o.Namespace, o.Name)
	}
}

// A VM without the source-machine label may be someone's hand-made workload
// on a shared cluster. It is none of this tool's business, and reporting it
// would train people to ignore the command.
func TestDetectOrphansIgnoresUnlabelledVMs(t *testing.T) {
	kv := kvClient(t,
		virtualMachine("prod", "real-machine", ""), // legit machine's own VM
		virtualMachine("prod", "hand-made", ""),    // unlabelled, someone's own
	)
	az := azClient(t, machine("prod", "real-machine"))

	report, err := DetectOrphans(context.Background(),
		[]*kube.VitistackClient{az}, fixedResolver{kv}, "", 15*time.Minute, time.Now(), nil)
	if err != nil {
		t.Fatalf("DetectOrphans() error = %v", err)
	}
	for _, o := range report.Orphans {
		if o.Kind == KindVMWithoutMachine {
			t.Errorf("unlabelled VM was reported as an orphan: %+v", o)
		}
	}
}

// A labelled VM whose named Machine exists is exactly the healthy case and
// must never be reported.
func TestDetectOrphansIgnoresVMsWithAnExistingMachine(t *testing.T) {
	kv := kvClient(t, virtualMachine("prod", "vm-abc", "web-01"))
	az := azClient(t, machine("prod", "web-01"))

	report, err := DetectOrphans(context.Background(),
		[]*kube.VitistackClient{az}, fixedResolver{kv}, "", 15*time.Minute, time.Now(), nil)
	if err != nil {
		t.Fatalf("DetectOrphans() error = %v", err)
	}
	if len(report.Orphans) != 0 {
		t.Errorf("got %d findings, want none: %+v", len(report.Orphans), report.Orphans)
	}
	if !report.Coverage.Complete() {
		t.Errorf("Coverage = %+v, want complete", report.Coverage)
	}
}

// A VM created moments ago may have a Machine still being reconciled.
// --min-age holds it back, and the suppression is counted rather than
// silently dropped so the number is never a hidden zero.
func TestDetectOrphansSuppressesYoungFindings(t *testing.T) {
	young := virtualMachine("prod", "vm-ghost", "ghost-machine")
	young.CreationTimestamp = metav1.NewTime(time.Now())
	old := virtualMachine("prod", "vm-old-ghost", "another-ghost")
	old.CreationTimestamp = metav1.NewTime(time.Now().Add(-1 * time.Hour))

	kv := kvClient(t,
		virtualMachine("prod", "vm-real", "real-machine"),
		young,
		old,
	)
	az := azClient(t, machine("prod", "real-machine"))

	report, err := DetectOrphans(context.Background(),
		[]*kube.VitistackClient{az}, fixedResolver{kv}, "", 15*time.Minute, time.Now(), nil)
	if err != nil {
		t.Fatalf("DetectOrphans() error = %v", err)
	}

	if report.Suppressed != 1 {
		t.Errorf("Suppressed = %d, want 1", report.Suppressed)
	}
	o := findOrphan(t, report.Orphans, KindVMWithoutMachine)
	if o.Name != "vm-old-ghost" {
		t.Errorf("reported finding = %q, want the old one; the young one should have been suppressed", o.Name)
	}
}

// A cluster that cannot be queried leaves "no orphans there" unproven, and
// that must be visible in the coverage rather than silently reading as a
// clean sweep.
func TestDetectOrphansReportsIncompleteCoverageWhenAClusterFails(t *testing.T) {
	broken := &kube.KubeVirtClient{
		Cluster: config.Cluster{Name: "broken-kv"},
		Ctrl:    listErrClient{err: errors.New("connection refused")},
	}
	az := azClient(t, machine("prod", "web-01"))

	var warnings []error
	report, err := DetectOrphans(context.Background(),
		[]*kube.VitistackClient{az}, fixedResolver{broken}, "", 15*time.Minute, time.Now(),
		func(e error) { warnings = append(warnings, e) })
	if err != nil {
		t.Fatalf("DetectOrphans() error = %v", err)
	}

	if report.Coverage.Complete() {
		t.Error("Coverage.Complete() = true, want false when a cluster could not be queried")
	}
	if report.Coverage.ClustersConfigured != 1 || report.Coverage.ClustersChecked != 0 {
		t.Errorf("Coverage = %+v, want 1 configured / 0 checked cluster", report.Coverage)
	}
	if len(warnings) != 1 {
		t.Fatalf("got %d warnings, want 1: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0].Error(), "broken-kv") {
		t.Errorf("warning %q should name the cluster", warnings[0])
	}
}

// A zone whose Machine listing itself fails is likewise incomplete, and must
// not be conflated with the no-vitistack-CRDs case, which is silent because
// it is confirmed information rather than a gap.
func TestDetectOrphansReportsIncompleteCoverageWhenAZoneFails(t *testing.T) {
	broken := failingAZ("broken", errors.New("dial tcp: connection refused"))

	report, err := DetectOrphans(context.Background(),
		[]*kube.VitistackClient{broken}, fixedResolver{kvClient(t)}, "", 15*time.Minute, time.Now(), nil)
	if err != nil {
		t.Fatalf("DetectOrphans() error = %v", err)
	}
	if report.Coverage.Complete() {
		t.Error("Coverage.Complete() = true, want false when a zone could not be listed")
	}
	if report.Coverage.ZonesChecked != 0 || report.Coverage.ZonesConfigured != 1 {
		t.Errorf("Coverage = %+v, want 0 checked / 1 configured zone", report.Coverage)
	}
}

// A zone with no vitistack CRDs holds no Machines by definition — that is
// confirmed, not missing, information, so it must count as checked.
func TestDetectOrphansTreatsNoCRDsZoneAsChecked(t *testing.T) {
	bare := failingAZ("no-crds", noCRDErr())

	report, err := DetectOrphans(context.Background(),
		[]*kube.VitistackClient{bare}, fixedResolver{kvClient(t)}, "", 15*time.Minute, time.Now(), nil)
	if err != nil {
		t.Fatalf("DetectOrphans() error = %v", err)
	}
	if !report.Coverage.Complete() {
		t.Errorf("Coverage = %+v, want complete for a zone with no vitistack CRDs", report.Coverage)
	}
}

// Sanity check that a fully healthy, single-zone, single-cluster fleet finds
// nothing and reports complete coverage — the everyday case.
func TestDetectOrphansCleanFleetHasNoFindings(t *testing.T) {
	kv := kvClient(t,
		virtualMachine("prod", "vm-abc", "web-01"),
		instance("prod", "vm-abc", "node-1"),
	)
	az := azClient(t, machine("prod", "web-01"))

	report, err := DetectOrphans(context.Background(),
		[]*kube.VitistackClient{az}, fixedResolver{kv}, "", 15*time.Minute, time.Now(), nil)
	if err != nil {
		t.Fatalf("DetectOrphans() error = %v", err)
	}
	if len(report.Orphans) != 0 {
		t.Errorf("got %d findings, want none: %+v", len(report.Orphans), report.Orphans)
	}
	if report.Suppressed != 0 {
		t.Errorf("Suppressed = %d, want 0", report.Suppressed)
	}
	if !report.Coverage.Complete() {
		t.Errorf("Coverage = %+v, want complete", report.Coverage)
	}
	if report.Coverage.ClustersConfigured != 1 || report.Coverage.ClustersChecked != 1 {
		t.Errorf("Coverage = %+v, want exactly 1 cluster configured and checked", report.Coverage)
	}
}
