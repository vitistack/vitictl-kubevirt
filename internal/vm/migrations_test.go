package vm

import (
	"context"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubevirtv1 "kubevirt.io/api/core/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/vitistack/vitictl-kubevirt/internal/kube"
)

// migration builds a VirtualMachineInstanceMigration naming the given VMI,
// the way the fake clients in vm_test.go build the other kubevirt types.
func migration(namespace, name, vmiName string, phase kubevirtv1.VirtualMachineInstanceMigrationPhase) *kubevirtv1.VirtualMachineInstanceMigration {
	return &kubevirtv1.VirtualMachineInstanceMigration{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       kubevirtv1.VirtualMachineInstanceMigrationSpec{VMIName: vmiName},
		Status:     kubevirtv1.VirtualMachineInstanceMigrationStatus{Phase: phase},
	}
}

// migrating returns a VMI carrying the live migrationState KubeVirt maintains
// while a migration runs, tagged with the migration object's UID the way
// KubeVirt tags it.
func migrating(namespace, name string, migUID types.UID, source, target string) *kubevirtv1.VirtualMachineInstance {
	vmi := instance(namespace, name, source)
	vmi.Status.MigrationState = &kubevirtv1.VirtualMachineInstanceMigrationState{
		MigrationUID: migUID,
		SourceNode:   source,
		TargetNode:   target,
	}
	return vmi
}

// withUID stamps a migration object with a UID so the VMI's MigrationUID has
// something to match, as the API server would.
func withUID(m *kubevirtv1.VirtualMachineInstanceMigration, uid types.UID) *kubevirtv1.VirtualMachineInstanceMigration {
	m.UID = uid
	return m
}

// vmiListErrClient fails only VirtualMachineInstance listings, so a test can
// prove the live-state lookup degrades the NODE column rather than hiding the
// migrations themselves.
type vmiListErrClient struct {
	ctrlclient.Client
	err error
}

func (c vmiListErrClient) List(ctx context.Context, list ctrlclient.ObjectList, opts ...ctrlclient.ListOption) error {
	if _, ok := list.(*kubevirtv1.VirtualMachineInstanceList); ok {
		return c.err
	}
	return c.Client.List(ctx, list, opts...)
}

// migListErrClient fails only VirtualMachineInstanceMigration listings,
// leaving VirtualMachine listings — the other half of the join — to succeed,
// so a test can tell CollectMigrations apart from indexKubeVirt's failure
// path.
type migListErrClient struct {
	ctrlclient.Client
	err error
}

func (c migListErrClient) List(ctx context.Context, list ctrlclient.ObjectList, opts ...ctrlclient.ListOption) error {
	if _, ok := list.(*kubevirtv1.VirtualMachineInstanceMigrationList); ok {
		return c.err
	}
	return c.Client.List(ctx, list, opts...)
}

// Succeeded and Failed are the only phases KubeVirt itself treats as
// terminal; everything else, including the unset phase a brand-new migration
// briefly has, must still count as active.
func TestMigrationActiveClassifiesTheRealPhases(t *testing.T) {
	tests := []struct {
		phase kubevirtv1.VirtualMachineInstanceMigrationPhase
		want  bool
	}{
		{kubevirtv1.MigrationPhaseUnset, true},
		{kubevirtv1.MigrationPending, true},
		{kubevirtv1.MigrationScheduling, true},
		{kubevirtv1.MigrationScheduled, true},
		{kubevirtv1.MigrationPreparingTarget, true},
		{kubevirtv1.MigrationTargetReady, true},
		{kubevirtv1.MigrationRunning, true},
		{kubevirtv1.MigrationWaitingForSync, true},
		{kubevirtv1.MigrationSynchronizing, true},
		{kubevirtv1.MigrationSucceeded, false},
		{kubevirtv1.MigrationFailed, false},
	}
	for _, tc := range tests {
		t.Run(string(tc.phase), func(t *testing.T) {
			m := Migration{VMIM: migration("prod", "mig-1", "vm-1", tc.phase)}
			if got := m.Active(); got != tc.want {
				t.Errorf("Active() for phase %q = %v, want %v", tc.phase, got, tc.want)
			}
		})
	}
}

// The join goes VMI -> VM -> Machine through the source-machine label, the
// same key Collect uses, and for the same reason: the operator names the VM
// (and so the VMI, which shares its name) independently of the Machine.
func TestCollectMigrationsJoinsThroughTheSourceMachineLabel(t *testing.T) {
	kv := kvClient(t,
		virtualMachine("prod", "vm-abc123", "web-01"),
		migration("prod", "mig-1", "vm-abc123", kubevirtv1.MigrationRunning),
	)
	az := azClient(t, machine("prod", "web-01"))

	migs, err := CollectMigrations(context.Background(), []*kube.VitistackClient{az}, fixedResolver{kv}, "", nil)
	if err != nil {
		t.Fatalf("CollectMigrations() error = %v", err)
	}
	if len(migs) != 1 {
		t.Fatalf("got %d migrations, want 1", len(migs))
	}
	m := migs[0]
	if m.Machine != "web-01" {
		t.Errorf("Machine = %q, want the Machine resolved through the label", m.Machine)
	}
	if m.VMIName() != "vm-abc123" {
		t.Errorf("VMIName() = %q, want vm-abc123", m.VMIName())
	}
	if m.Cluster != "kv-test" {
		t.Errorf("Cluster = %q, want kv-test", m.Cluster)
	}
}

// A hand-made VM, or one predating the label, still resolves under its own
// name — the same fallback indexKubeVirt uses.
func TestCollectMigrationsFallsBackToMatchingOnName(t *testing.T) {
	kv := kvClient(t,
		virtualMachine("prod", "web-01", ""),
		migration("prod", "mig-1", "web-01", kubevirtv1.MigrationRunning),
	)
	az := azClient(t, machine("prod", "web-01"))

	migs, err := CollectMigrations(context.Background(), []*kube.VitistackClient{az}, fixedResolver{kv}, "", nil)
	if err != nil {
		t.Fatalf("CollectMigrations() error = %v", err)
	}
	if len(migs) != 1 || migs[0].Machine != "web-01" {
		t.Fatalf("an unlabelled VM sharing the VMI name should still resolve the Machine, got %+v", migs)
	}
}

// A migration whose VMI has no matching VM at all — the VM was deleted out
// from under it, say — must still be shown. Dropping it would hide a live
// migration; showing it with an empty Machine is the honest answer.
func TestCollectMigrationsKeepsUnattributableMigrations(t *testing.T) {
	kv := kvClient(t,
		migration("prod", "mig-1", "ghost-vm", kubevirtv1.MigrationRunning),
	)
	az := azClient(t, machine("prod", "web-01"))

	migs, err := CollectMigrations(context.Background(), []*kube.VitistackClient{az}, fixedResolver{kv}, "", nil)
	if err != nil {
		t.Fatalf("CollectMigrations() error = %v", err)
	}
	if len(migs) != 1 {
		t.Fatalf("got %d migrations, want the unattributable one still listed", len(migs))
	}
	if migs[0].Machine != "" {
		t.Errorf("Machine = %q, want empty for an unresolvable owner", migs[0].Machine)
	}
	if migs[0].VMIName() != "ghost-vm" {
		t.Errorf("VMIName() = %q, want ghost-vm", migs[0].VMIName())
	}
}

// Deterministic ordering: namespace, then VMI name, then the migration's own
// name, so a fleet listing does not reshuffle between runs.
func TestCollectMigrationsOrdering(t *testing.T) {
	kv := kvClient(t,
		migration("b-ns", "mig-1", "vmi-z", kubevirtv1.MigrationRunning),
		migration("a-ns", "mig-2", "vmi-b", kubevirtv1.MigrationRunning),
		migration("a-ns", "mig-1", "vmi-a", kubevirtv1.MigrationRunning),
		migration("a-ns", "mig-3", "vmi-a", kubevirtv1.MigrationPending),
	)
	az := azClient(t, machine("prod", "irrelevant"))

	migs, err := CollectMigrations(context.Background(), []*kube.VitistackClient{az}, fixedResolver{kv}, "", nil)
	if err != nil {
		t.Fatalf("CollectMigrations() error = %v", err)
	}
	var got []string
	for _, m := range migs {
		got = append(got, m.Namespace()+"/"+m.VMIName()+"/"+m.Name())
	}
	want := "a-ns/vmi-a/mig-1,a-ns/vmi-a/mig-3,a-ns/vmi-b/mig-2,b-ns/vmi-z/mig-1"
	if strings.Join(got, ",") != want {
		t.Errorf("order = %v, want %v", got, want)
	}
}

// A KubeVirt cluster that cannot be reached must be warned about and
// skipped, not turned into a fatal error for the whole command — the same
// contract Collect follows for an unreachable cluster.
func TestCollectMigrationsWarnsAndSkipsAnUnreachableCluster(t *testing.T) {
	az := namedAZ(t, "az1", machine("prod", "a"))
	r := &mapResolver{} // every lookup misses, simulating an unresolvable cluster

	var warnings []error
	migs, err := CollectMigrations(context.Background(),
		[]*kube.VitistackClient{az}, r, "", func(e error) { warnings = append(warnings, e) })
	if err != nil {
		t.Fatalf("CollectMigrations() error = %v", err)
	}
	if len(migs) != 0 {
		t.Errorf("got %d migrations from an unresolvable cluster, want 0", len(migs))
	}
	if len(warnings) != 1 {
		t.Fatalf("got %d warnings, want exactly 1: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0].Error(), "az1") {
		t.Errorf("warning %q should name the zone", warnings[0])
	}
}

// A cluster that resolves but fails to list migrations must be warned about
// and skipped too, without losing any other cluster's rows.
func TestCollectMigrationsWarnsAndSkipsAFailingClusterList(t *testing.T) {
	broken := namedKV(t, "broken-kv")
	broken.Ctrl = migListErrClient{Client: broken.Ctrl, err: errors.New("dial tcp: connection refused")}
	good := namedKV(t, "good-kv", migration("prod", "mig-1", "vm-1", kubevirtv1.MigrationRunning))

	az := namedAZ(t, "az1", machineOn("prod", "m-broken", "cfg-broken"), machineOn("prod", "m-good", "cfg-good"))
	r := &mapResolver{byKey: map[string]*kube.KubeVirtClient{
		"az1/cfg-broken": broken,
		"az1/cfg-good":   good,
	}}

	var warnings []error
	migs, err := CollectMigrations(context.Background(),
		[]*kube.VitistackClient{az}, r, "", func(e error) { warnings = append(warnings, e) })
	if err != nil {
		t.Fatalf("CollectMigrations() error = %v", err)
	}
	if len(migs) != 1 || migs[0].Cluster != "good-kv" {
		t.Fatalf("got %v, want only the healthy cluster's migration", migs)
	}
	if len(warnings) != 1 {
		t.Fatalf("got %d warnings, want exactly 1 for the broken cluster: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0].Error(), "broken-kv") {
		t.Errorf("warning %q should name the cluster", warnings[0])
	}
}

// A KubeVirt cluster reachable from two zone groups must be listed once, not
// once per group: migrations carry no per-zone identity to de-duplicate by,
// so a second listing would double every row rather than merely wasting a
// call.
func TestCollectMigrationsListsASharedClusterOnce(t *testing.T) {
	shared := namedKV(t, "shared-kv", migration("prod", "mig-1", "vm-1", kubevirtv1.MigrationRunning))

	west := namedAZ(t, "west", machine("prod", "web-01"))
	east := namedAZ(t, "east", machine("prod", "web-02"))
	r := &mapResolver{byKey: map[string]*kube.KubeVirtClient{"west/": shared, "east/": shared}}

	migs, err := CollectMigrations(context.Background(),
		[]*kube.VitistackClient{west, east}, r, "", nil)
	if err != nil {
		t.Fatalf("CollectMigrations() error = %v", err)
	}
	if len(migs) != 1 {
		t.Fatalf("got %d migrations from a cluster shared by two zones, want 1", len(migs))
	}
	if len(r.asked) != 2 {
		t.Errorf("resolver asked %d times, want one attempt per zone group (2): %v", len(r.asked), r.asked)
	}
}

// A zone whose Machine listing fails outright is a real problem, distinct
// from the "no CRDs" case, and must still be reported.
func TestCollectMigrationsWarnsOnAZoneListFailure(t *testing.T) {
	good := namedAZ(t, "good", machine("prod", "web-01"))
	kv := namedKV(t, "kv", migration("prod", "mig-1", "web-01", kubevirtv1.MigrationRunning))
	r := &mapResolver{byKey: map[string]*kube.KubeVirtClient{"good/": kv}}

	var warnings []error
	migs, err := CollectMigrations(context.Background(),
		[]*kube.VitistackClient{failingAZ("broken", errors.New("dial tcp: connection refused")), good},
		r, "", func(e error) { warnings = append(warnings, e) })
	if err != nil {
		t.Fatalf("CollectMigrations() error = %v", err)
	}
	if len(migs) != 1 {
		t.Errorf("got %d migrations, want the healthy zone's still listed", len(migs))
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0].Error(), "broken") {
		t.Errorf("warnings = %v, want exactly one naming the broken zone", warnings)
	}
}

// The bug this closes: KubeVirt fills in the migration object's own
// migrationState only when the migration finishes, so a --watch built on it
// showed an empty NODE column for the entire migration and populated it only
// once there was nothing left to watch. Observed live on ptr1-kv-cl01, where
// mid-flight the VMI already read wrk02->wrk14 while the VMIM's state was nil.
func TestMigrationPrefersTheInstancesLiveStateWhileInFlight(t *testing.T) {
	const uid = types.UID("8ac3aab6-dffb-4706-8b09-8889328a1b91")
	kv := kvClient(t,
		virtualMachine("prod", "vm-abc123", "web-01"),
		migrating("prod", "vm-abc123", uid, "wrk02", "wrk14"),
		// Running, and with no migrationState of its own yet — the real
		// mid-flight shape.
		withUID(migration("prod", "mig-1", "vm-abc123", kubevirtv1.MigrationRunning), uid),
	)
	az := azClient(t, machine("prod", "web-01"))

	migs, err := CollectMigrations(context.Background(), []*kube.VitistackClient{az}, fixedResolver{kv}, "", nil)
	if err != nil {
		t.Fatalf("CollectMigrations() error = %v", err)
	}
	if len(migs) != 1 {
		t.Fatalf("got %d migrations, want 1", len(migs))
	}
	if got, want := migs[0].SourceNode(), "wrk02"; got != want {
		t.Errorf("SourceNode() = %q, want %q while the migration is still running", got, want)
	}
	if got, want := migs[0].TargetNode(), "wrk14"; got != want {
		t.Errorf("TargetNode() = %q, want %q — an empty NODE column mid-flight is the bug", got, want)
	}
}

// The correctness trap in preferring the live state: a VMI keeps the state of
// its most RECENT migration, so an older finished one must not be re-labelled
// with the newer one's nodes. t-jraviti-123's wrk3 has exactly this history —
// Failed to wrk18, then Succeeded to wrk14 — and the Failed row has to keep
// reporting wrk18. The MigrationUID is what makes the distinction exact.
func TestMigrationDoesNotBorrowAnotherMigrationsState(t *testing.T) {
	const (
		failedUID    = types.UID("8ac3aab6-dffb-4706-8b09-8889328a1b91")
		succeededUID = types.UID("22330df9-7c64-486b-9947-82a136e89c0a")
	)
	// The finished migration carries its own state, as KubeVirt leaves it.
	failed := withUID(migration("prod", "mig-failed", "vm-abc123", kubevirtv1.MigrationFailed), failedUID)
	failed.Status.MigrationState = &kubevirtv1.VirtualMachineInstanceMigrationState{
		MigrationUID: failedUID, SourceNode: "wrk02", TargetNode: "wrk18", Failed: true,
	}
	succeeded := withUID(migration("prod", "mig-ok", "vm-abc123", kubevirtv1.MigrationSucceeded), succeededUID)
	succeeded.Status.MigrationState = &kubevirtv1.VirtualMachineInstanceMigrationState{
		MigrationUID: succeededUID, SourceNode: "wrk02", TargetNode: "wrk14",
	}

	kv := kvClient(t,
		virtualMachine("prod", "vm-abc123", "web-01"),
		// The VMI holds only the LATER migration's state.
		migrating("prod", "vm-abc123", succeededUID, "wrk02", "wrk14"),
		failed, succeeded,
	)
	az := azClient(t, machine("prod", "web-01"))

	migs, err := CollectMigrations(context.Background(), []*kube.VitistackClient{az}, fixedResolver{kv}, "", nil)
	if err != nil {
		t.Fatalf("CollectMigrations() error = %v", err)
	}
	byName := map[string]Migration{}
	for _, m := range migs {
		byName[m.Name()] = m
	}
	if got, want := byName["mig-failed"].TargetNode(), "wrk18"; got != want {
		t.Errorf("failed migration TargetNode() = %q, want %q — it must not inherit the later migration's target", got, want)
	}
	if got, want := byName["mig-ok"].TargetNode(), "wrk14"; got != want {
		t.Errorf("succeeded migration TargetNode() = %q, want %q", got, want)
	}
}

// With no live state to consult, the migration object's own remains the
// answer — the previous behaviour, unchanged for finished migrations.
func TestMigrationFallsBackToItsOwnStateWithoutAnInstance(t *testing.T) {
	m := withUID(migration("prod", "mig-1", "vm-gone", kubevirtv1.MigrationSucceeded), types.UID("u1"))
	m.Status.MigrationState = &kubevirtv1.VirtualMachineInstanceMigrationState{
		MigrationUID: types.UID("u1"), SourceNode: "wrk01", TargetNode: "wrk02",
	}
	mig := Migration{VMIM: m} // VMIState nil: the VMI is gone, as it is after a delete
	if got, want := mig.SourceNode(), "wrk01"; got != want {
		t.Errorf("SourceNode() = %q, want %q", got, want)
	}
	if got, want := mig.TargetNode(), "wrk02"; got != want {
		t.Errorf("TargetNode() = %q, want %q", got, want)
	}

	// And with neither, the columns are empty rather than panicking.
	bare := Migration{VMIM: migration("prod", "mig-2", "vm-x", kubevirtv1.MigrationPending)}
	if bare.SourceNode() != "" || bare.TargetNode() != "" || bare.Mode() != "" {
		t.Error("a migration with no state anywhere must report empty, not fabricate")
	}
}

// Losing the live lookup must degrade the NODE column, never hide in-flight
// migrations — the same contract the owning-Machine lookup follows.
func TestCollectMigrationsWarnsAndStillListsWhenInstancesCannotBeListed(t *testing.T) {
	const uid = types.UID("u-live")
	kv := kvClient(t,
		virtualMachine("prod", "vm-abc123", "web-01"),
		migrating("prod", "vm-abc123", uid, "wrk02", "wrk14"),
		withUID(migration("prod", "mig-1", "vm-abc123", kubevirtv1.MigrationRunning), uid),
	)
	kv.Ctrl = vmiListErrClient{Client: kv.Ctrl, err: errors.New("boom")}
	az := azClient(t, machine("prod", "web-01"))

	var warnings []string
	migs, err := CollectMigrations(context.Background(), []*kube.VitistackClient{az}, fixedResolver{kv}, "",
		func(e error) { warnings = append(warnings, e.Error()) })
	if err != nil {
		t.Fatalf("CollectMigrations() error = %v", err)
	}
	if len(migs) != 1 {
		t.Fatalf("got %d migrations, want the migration listed anyway", len(migs))
	}
	if migs[0].TargetNode() != "" {
		t.Errorf("TargetNode() = %q, want empty: the live state was unavailable", migs[0].TargetNode())
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "VirtualMachineInstances") {
		t.Errorf("warnings = %v, want one naming the failed VirtualMachineInstance listing", warnings)
	}
}
