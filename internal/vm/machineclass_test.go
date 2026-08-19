package vm

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	kubevirtv1 "kubevirt.io/api/core/v1"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
)

func objKey(namespace, name string) ctrlclient.ObjectKey {
	return ctrlclient.ObjectKey{Namespace: namespace, Name: name}
}

func machineClass(name string, enabled bool, providers ...vitiv1alpha1.MachineProviderType) *vitiv1alpha1.MachineClass {
	return &vitiv1alpha1.MachineClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: vitiv1alpha1.MachineClassSpec{
			Enabled:          enabled,
			Memory:           vitiv1alpha1.MachineClassMemorySpec{Quantity: resource.MustParse("8Gi")},
			CPU:              vitiv1alpha1.MachineClassCPUSpec{Cores: 4, Sockets: 2, Threads: 2},
			MachineProviders: providers,
		},
	}
}

func TestDesiredResourcesFromClass(t *testing.T) {
	m := &vitiv1alpha1.Machine{}
	got := DesiredResources(m, machineClass("large", true, vitiv1alpha1.MachineProviderTypeKubevirt))
	want := Resources{Memory: "8Gi", Cores: 4, Sockets: 2, Threads: 2}
	if got != want {
		t.Errorf("DesiredResources = %+v, want %+v", got, want)
	}
}

func TestDesiredResourcesDefaultsSocketsAndThreads(t *testing.T) {
	class := machineClass("small", true)
	class.Spec.CPU.Sockets = 0
	class.Spec.CPU.Threads = 0
	got := DesiredResources(&vitiv1alpha1.Machine{}, class)
	if got.Sockets != 1 || got.Threads != 1 {
		t.Errorf("sockets/threads = %d/%d, want 1/1", got.Sockets, got.Threads)
	}
}

// The kubevirt-operator lets a Machine's own spec.cpu / spec.memory override
// its class; the patch this command writes must agree with what the operator
// would compute, or the next reconcile would fight it.
func TestDesiredResourcesMachineOverridesWin(t *testing.T) {
	m := &vitiv1alpha1.Machine{
		Spec: vitiv1alpha1.MachineSpec{
			Memory: 4 * 1024 * 1024 * 1024, // bytes
			CPU:    vitiv1alpha1.MachineCPU{Cores: 8, Sockets: 4, ThreadsPerCore: 1},
		},
	}
	got := DesiredResources(m, machineClass("large", true))
	want := Resources{Memory: "4096Mi", Cores: 8, Sockets: 4, Threads: 1}
	if got != want {
		t.Errorf("DesiredResources = %+v, want %+v", got, want)
	}
}

func TestHasResourceOverrides(t *testing.T) {
	if HasResourceOverrides(&vitiv1alpha1.Machine{}) {
		t.Error("machine without cpu/memory reported overrides")
	}
	m := &vitiv1alpha1.Machine{Spec: vitiv1alpha1.MachineSpec{Memory: 1024}}
	if !HasResourceOverrides(m) {
		t.Error("machine with spec.memory not reported as overriding")
	}
}

func TestListEnabledClasses(t *testing.T) {
	az := azClient(t,
		machineClass("b-enabled", true, vitiv1alpha1.MachineProviderTypeKubevirt),
		machineClass("a-enabled", true, vitiv1alpha1.MachineProviderTypeKubevirt),
		machineClass("disabled", false, vitiv1alpha1.MachineProviderTypeKubevirt),
		machineClass("proxmox-only", true, vitiv1alpha1.MachineProviderTypeProxmox),
		machineClass("no-providers", true),
	)
	got, err := ListEnabledClasses(context.Background(), az)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range got {
		names = append(names, c.Name)
	}
	// Sorted; disabled and other-provider classes are excluded, a class that
	// names no providers is kept.
	want := []string{"a-enabled", "b-enabled", "no-providers"}
	if len(names) != len(want) {
		t.Fatalf("classes = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("classes = %v, want %v", names, want)
		}
	}
}

func TestPatchMachineClass(t *testing.T) {
	m := &vitiv1alpha1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "prod"},
		Spec: vitiv1alpha1.MachineSpec{
			Name:         "web-1",
			MachineClass: "small",
			Provider:     vitiv1alpha1.MachineProviderTypeKubevirt,
		},
	}
	az := azClient(t, m)

	if err := PatchMachineClass(context.Background(), az, m, "large"); err != nil {
		t.Fatal(err)
	}

	var got vitiv1alpha1.Machine
	if err := az.Ctrl.Get(context.Background(), objKey("prod", "web-1"), &got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.MachineClass != "large" {
		t.Errorf("spec.machineClass = %q, want %q", got.Spec.MachineClass, "large")
	}
	if got.Spec.Provider != vitiv1alpha1.MachineProviderTypeKubevirt {
		t.Errorf("merge patch clobbered spec.provider: %q", got.Spec.Provider)
	}
}

func TestPatchVMResources(t *testing.T) {
	v := &kubevirtv1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "prod"},
		Spec: kubevirtv1.VirtualMachineSpec{
			Template: &kubevirtv1.VirtualMachineInstanceTemplateSpec{
				Spec: kubevirtv1.VirtualMachineInstanceSpec{
					Domain: kubevirtv1.DomainSpec{
						CPU:    &kubevirtv1.CPU{Cores: 2, Sockets: 1, Threads: 1, Model: "host-passthrough"},
						Memory: &kubevirtv1.Memory{Guest: ptr.To(resource.MustParse("2Gi"))},
						Firmware: &kubevirtv1.Firmware{
							Bootloader: &kubevirtv1.Bootloader{EFI: &kubevirtv1.EFI{SecureBoot: ptr.To(false)}},
						},
					},
				},
			},
		},
	}
	kv := kvClient(t, v)

	r := Resources{Memory: "8Gi", Cores: 4, Sockets: 2, Threads: 2}
	if err := PatchVMResources(context.Background(), kv, v, r); err != nil {
		t.Fatal(err)
	}

	var got kubevirtv1.VirtualMachine
	if err := kv.Ctrl.Get(context.Background(), objKey("prod", "web-1"), &got); err != nil {
		t.Fatal(err)
	}
	cpu := got.Spec.Template.Spec.Domain.CPU
	if cpu.Cores != 4 || cpu.Sockets != 2 || cpu.Threads != 2 {
		t.Errorf("cpu = %d/%d/%d, want 4/2/2", cpu.Cores, cpu.Sockets, cpu.Threads)
	}
	if cpu.Model != "host-passthrough" {
		t.Errorf("merge patch clobbered cpu.model: %q", cpu.Model)
	}
	if guest := got.Spec.Template.Spec.Domain.Memory.Guest; guest == nil || guest.String() != "8Gi" {
		t.Errorf("memory.guest = %v, want 8Gi", guest)
	}
	if got.Spec.Template.Spec.Domain.Firmware == nil {
		t.Error("merge patch clobbered domain.firmware")
	}
}

func TestPatchVMResourcesWithoutTemplate(t *testing.T) {
	v := &kubevirtv1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: "bare", Namespace: "prod"}}
	kv := kvClient(t, v)
	err := PatchVMResources(context.Background(), kv, v, Resources{Memory: "8Gi", Cores: 4, Sockets: 1, Threads: 1})
	if err == nil {
		t.Fatal("expected an error for a VM without a template, got nil")
	}
}

// The operator's safeUintToUint32 only guards overflow, so a class with
// cores: 0 yields 0 — this must not "helpfully" substitute a default the
// operator would not use.
func TestDesiredResourcesZeroCoresMirrorsOperator(t *testing.T) {
	class := machineClass("odd", true)
	class.Spec.CPU.Cores = 0
	got := DesiredResources(&vitiv1alpha1.Machine{}, class)
	if got.Cores != 0 {
		t.Errorf("cores = %d, want 0 (the operator passes 0 through)", got.Cores)
	}
}
