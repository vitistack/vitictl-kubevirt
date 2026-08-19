package roll

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	kubevirtv1 "kubevirt.io/api/core/v1"
)

// rollableMember is a stagable member whose VMI already reports the desired
// size, so the post-restart wait completes immediately in tests.
func rollableMember(t *testing.T, name string) Member {
	t.Helper()
	vmi := &kubevirtv1.VirtualMachineInstance{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Status: kubevirtv1.VirtualMachineInstanceStatus{
			Phase: kubevirtv1.Running,
			Memory: &kubevirtv1.MemoryStatus{
				GuestAtBoot: ptr.To(resource.MustParse("32Gi")),
			},
		},
	}
	return stagableMember(t, name, vmi)
}

func rollOpts() Options {
	return Options{
		PollInterval: time.Millisecond,
		ReadyTimeout: 50 * time.Millisecond,
	}
}

func TestRollHappyPathOrder(t *testing.T) {
	a, b := rollableMember(t, "wrk0"), rollableMember(t, "wrk1")
	plan := &Plan{Class: sizedClass(), Members: []Member{a, b}}
	g := newFakeGuest("wrk0", "wrk1")
	var restarted []string
	restart := func(_ context.Context, m Member) error {
		g.record("restart " + m.Machine.Name)
		restarted = append(restarted, m.Machine.Name)
		return nil
	}

	if err := Roll(context.Background(), plan, g, restart, rollOpts(), &recordingReporter{}); err != nil {
		t.Fatal(err)
	}
	if len(restarted) != 2 {
		t.Fatalf("restarted %v", restarted)
	}
	got := strings.Join(g.calls, "; ")
	// Everything for wrk0 must complete — through its uncordon — before any
	// call mentions wrk1.
	uncordonA := strings.Index(got, "cordon wrk0 false")
	firstB := strings.Index(got, "wrk1")
	if uncordonA == -1 || firstB == -1 || firstB < uncordonA {
		t.Errorf("wrk1 was touched before wrk0 finished:\n%s", got)
	}
	for _, want := range []string{"cordon wrk0 true", "drain wrk0", "restart wrk0", "cordon wrk0 false"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if i, j := strings.Index(got, "cordon wrk0 true"), strings.Index(got, "drain wrk0"); i > j {
		t.Errorf("drain before cordon:\n%s", got)
	}
	if i, j := strings.Index(got, "drain wrk0"), strings.Index(got, "restart wrk0"); i > j {
		t.Errorf("restart before drain:\n%s", got)
	}
}

func TestRollDrainFailureAbortsAndUncordons(t *testing.T) {
	a, b := rollableMember(t, "wrk0"), rollableMember(t, "wrk1")
	plan := &Plan{Class: sizedClass(), Members: []Member{a, b}}
	g := newFakeGuest("wrk0", "wrk1")
	g.drainErr["wrk0"] = errors.New("cannot evict pod protected by PDB app-pdb")
	restart := func(_ context.Context, m Member) error {
		g.record("restart " + m.Machine.Name)
		return nil
	}

	err := Roll(context.Background(), plan, g, restart, rollOpts(), &recordingReporter{})
	if err == nil || !strings.Contains(err.Error(), "app-pdb") {
		t.Fatalf("err = %v, want the drain error surfaced", err)
	}
	got := strings.Join(g.calls, "; ")
	if !strings.Contains(got, "cordon wrk0 false") {
		t.Errorf("aborted node was not uncordoned:\n%s", got)
	}
	if strings.Contains(got, "restart wrk0") {
		t.Errorf("restarted despite failed drain:\n%s", got)
	}
	if strings.Contains(got, "wrk1") {
		t.Errorf("continued to wrk1 after abort:\n%s", got)
	}
}

func TestRollNodeNeverReadyAborts(t *testing.T) {
	a, b := rollableMember(t, "wrk0"), rollableMember(t, "wrk1")
	plan := &Plan{Class: sizedClass(), Members: []Member{a, b}}
	g := newFakeGuest("wrk0", "wrk1")
	g.nodes["wrk0"].Status.Conditions = []corev1.NodeCondition{
		{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
	}
	restart := func(_ context.Context, m Member) error { return nil }

	err := Roll(context.Background(), plan, g, restart, rollOpts(), &recordingReporter{})
	if err == nil || !strings.Contains(err.Error(), "wrk0") {
		t.Fatalf("err = %v, want ready-timeout naming wrk0", err)
	}
	got := strings.Join(g.calls, "; ")
	if !strings.Contains(got, "cordon wrk0 false") {
		t.Errorf("uncordon was not attempted on abort:\n%s", got)
	}
	if strings.Contains(got, "cordon wrk1 true") {
		t.Errorf("continued to wrk1 after abort:\n%s", got)
	}
}
