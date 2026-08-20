package roll

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	kubevirtv1 "kubevirt.io/api/core/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/vitistack/vitictl-kubevirt/internal/config"
	"github.com/vitistack/vitictl-kubevirt/internal/kube"
)

// vmiHolder serves a member's VMI reads mutably, so a test can swap the
// instance mid-roll the way a real restart does (the fake client's typed
// tracker cannot Create a VMI whose status carries KubeVirt's exotic types).
type vmiHolder struct {
	mu  sync.Mutex
	vmi *kubevirtv1.VirtualMachineInstance
}

func (h *vmiHolder) set(v *kubevirtv1.VirtualMachineInstance) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.vmi = v
}

func (h *vmiHolder) get() *kubevirtv1.VirtualMachineInstance {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.vmi
}

func runningVMI(name, uid, memory string) *kubevirtv1.VirtualMachineInstance {
	return &kubevirtv1.VirtualMachineInstance{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", UID: types.UID(uid)},
		Status: kubevirtv1.VirtualMachineInstanceStatus{
			Phase: kubevirtv1.Running,
			Memory: &kubevirtv1.MemoryStatus{
				GuestAtBoot: ptr.To(resource.MustParse(memory)),
			},
		},
	}
}

// rollableMember is a stagable member whose (pre-restart) VMI already runs at
// the desired size — the situation a re-sync or CPU-only roll starts from.
// VMI reads go through the returned holder, which tests mutate to simulate
// the restart completing.
func rollableMember(t *testing.T, name string) (Member, *vmiHolder) {
	t.Helper()
	holder := &vmiHolder{}
	holder.set(runningVMI(name, "uid-old-"+name, "32Gi"))

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
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(vmObj).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl ctrlclient.WithWatch, key ctrlclient.ObjectKey,
				obj ctrlclient.Object, opts ...ctrlclient.GetOption) error {
				if v, ok := obj.(*kubevirtv1.VirtualMachineInstance); ok && key.Name == name {
					cur := holder.get()
					if cur == nil {
						return apierrors.NewNotFound(
							kubevirtv1.Resource("virtualmachineinstances"), key.Name)
					}
					cur.DeepCopyInto(v)
					return nil
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	kv := &kube.KubeVirtClient{
		Cluster: config.Cluster{Name: "kv1", Kubeconfig: "/tmp/kc", Context: "ctx"},
		Ctrl:    c,
	}
	return Member{Machine: m, VM: vmObj, KV: kv}, holder
}

func rollOpts() Options {
	return Options{
		PollInterval: time.Millisecond,
		ReadyTimeout: 50 * time.Millisecond,
	}
}

// replacingRestart is a Restarter that records the call and simulates the
// restart completing: the member's holder swaps to a fresh instance (new UID)
// running at the desired 32Gi.
func replacingRestart(g *fakeGuest, holders map[string]*vmiHolder, restarted *[]string) Restarter {
	return func(_ context.Context, m Member) error {
		g.record("restart " + m.Machine.Name)
		if restarted != nil {
			*restarted = append(*restarted, m.Machine.Name)
		}
		holders[m.Machine.Name].set(runningVMI(m.VM.Name, "uid-new-"+m.VM.Name, "32Gi"))
		return nil
	}
}

// virtctl restart is asynchronous: right after it returns, the OLD instance is
// still Running — at the desired memory too, whenever the roll is a same-size
// re-sync or a CPU-only change. Trusting it would uncordon and move on while
// the node is actually about to go down, rebooting a whole pool (or control
// plane) nearly simultaneously. The roll must wait for a NEW instance.
func TestRollWaitsForAFreshVMIAfterRestart(t *testing.T) {
	a, _ := rollableMember(t, "wrk0") // old VMI: Running, already at the desired 32Gi
	plan := &Plan{Class: sizedClass(), Members: []Member{a}}
	g := newFakeGuest("wrk0")
	restart := func(_ context.Context, m Member) error {
		g.record("restart " + m.Machine.Name)
		return nil // asynchronous: the old VMI stays in place
	}

	err := Roll(context.Background(), plan, g, restart, rollOpts(), &recordingReporter{})
	if err == nil {
		t.Fatal("Roll accepted the old, pre-restart VMI as the restarted instance")
	}
	if !strings.Contains(err.Error(), "wrk0") {
		t.Errorf("err = %v, want the stuck member named", err)
	}
}

// By the time Roll runs, the topology patch and StageVMs have already put the
// new size on EVERY member's Machine and VM template — an abort stops the
// restarts, nothing more. Claiming members are "untouched" invites operators
// to walk away from nodes that will silently resize on their next reboot.
func TestRollAbortSaysSizesAreAlreadyStaged(t *testing.T) {
	a, _ := rollableMember(t, "wrk0")
	b, _ := rollableMember(t, "wrk1")
	plan := &Plan{Class: sizedClass(), Members: []Member{a, b}}
	g := newFakeGuest("wrk0", "wrk1")
	g.drainErr["wrk0"] = errors.New("cannot evict pod protected by PDB app-pdb")
	restart := func(_ context.Context, m Member) error { return nil }

	err := Roll(context.Background(), plan, g, restart, rollOpts(), &recordingReporter{})
	if err == nil {
		t.Fatal("expected the drain failure to abort")
	}
	if strings.Contains(err.Error(), "untouched") {
		t.Errorf("abort claims members are untouched, but their sizes are staged: %v", err)
	}
	if !strings.Contains(err.Error(), "staged") {
		t.Errorf("abort does not say the new sizes are already staged: %v", err)
	}
}

// Quantities that are equal can render differently — a Machine's 4GiB memory
// override yields the string "4096Mi" while the VMI's guestAtBoot serializes
// canonically as "4Gi". The wait must compare quantities, not their strings,
// or a healthy restart reads as never-completing and falsely aborts the roll.
func TestAwaitVMIComparesQuantitiesNotStrings(t *testing.T) {
	m, holder := rollableMember(t, "wrk0")
	m.Machine.Spec.Memory = 4 * 1024 * 1024 * 1024 // desired renders as "4096Mi"
	holder.set(runningVMI("wrk0", "uid-new-wrk0", "4Gi"))

	plan := &Plan{Class: sizedClass(), Members: []Member{m}}
	if err := awaitVMI(context.Background(), m, plan, rollOpts(), types.UID("uid-old-wrk0")); err != nil {
		t.Fatalf("awaitVMI = %v, want nil: 4Gi and 4096Mi are the same quantity", err)
	}
}

func TestRollHappyPathOrder(t *testing.T) {
	a, ha := rollableMember(t, "wrk0")
	b, hb := rollableMember(t, "wrk1")
	plan := &Plan{Class: sizedClass(), Members: []Member{a, b}}
	g := newFakeGuest("wrk0", "wrk1")
	var restarted []string
	restart := replacingRestart(g, map[string]*vmiHolder{"wrk0": ha, "wrk1": hb}, &restarted)

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
	a, _ := rollableMember(t, "wrk0")
	b, _ := rollableMember(t, "wrk1")
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
	a, ha := rollableMember(t, "wrk0")
	b, hb := rollableMember(t, "wrk1")
	plan := &Plan{Class: sizedClass(), Members: []Member{a, b}}
	g := newFakeGuest("wrk0", "wrk1")
	g.nodes["wrk0"].Status.Conditions = []corev1.NodeCondition{
		{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
	}
	restart := replacingRestart(g, map[string]*vmiHolder{"wrk0": ha, "wrk1": hb}, nil)

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
