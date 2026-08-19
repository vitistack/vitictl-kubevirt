package vm

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"

	"k8s.io/apimachinery/pkg/types"
	kubevirtv1 "kubevirt.io/api/core/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"

	"github.com/vitistack/vitictl-kubevirt/internal/kube"
)

// Resources is what a machine's VM should be sized to: the shape the
// kubevirt-operator computes from a MachineClass when it first creates the VM.
type Resources struct {
	// Memory is a Kubernetes quantity string ("8Gi"), the form the VM's
	// memory.guest field takes.
	Memory  string
	Cores   uint32
	Sockets uint32
	Threads uint32
}

// DesiredResources computes the resources a machine's VM should run with under
// the given class.
//
// This mirrors the kubevirt-operator's calculateResourceRequirements exactly —
// class values first, then the Machine's own spec.cpu/spec.memory overrides on
// top — because the operator never reconciles an existing VM: whatever this
// returns is written to the VM directly, and it must be what the operator
// itself would have produced, or a future re-provisioning would disagree with
// what the user saw applied.
func DesiredResources(m *vitiv1alpha1.Machine, class *vitiv1alpha1.MachineClass) Resources {
	r := Resources{
		Memory:  class.Spec.Memory.Quantity.String(),
		Cores:   uintToUint32(class.Spec.CPU.Cores, 2),
		Sockets: 1,
		Threads: 1,
	}
	if class.Spec.CPU.Sockets > 0 {
		r.Sockets = uintToUint32(class.Spec.CPU.Sockets, 1)
	}
	if class.Spec.CPU.Threads > 0 {
		r.Threads = uintToUint32(class.Spec.CPU.Threads, 1)
	}

	if m.Spec.Memory > 0 {
		r.Memory = fmt.Sprintf("%dMi", m.Spec.Memory/1024/1024)
	}
	if m.Spec.CPU.Cores > 0 {
		r.Cores = intToUint32(m.Spec.CPU.Cores, r.Cores)
	}
	if m.Spec.CPU.Sockets > 0 {
		r.Sockets = intToUint32(m.Spec.CPU.Sockets, r.Sockets)
	}
	if m.Spec.CPU.ThreadsPerCore > 0 {
		r.Threads = intToUint32(m.Spec.CPU.ThreadsPerCore, r.Threads)
	}
	return r
}

// HasResourceOverrides reports whether the Machine carries its own
// spec.cpu/spec.memory values, which win over any class. A user changing the
// class of such a machine should be told the class alone will not resize it.
func HasResourceOverrides(m *vitiv1alpha1.Machine) bool {
	return m.Spec.Memory > 0 || m.Spec.CPU.Cores > 0 ||
		m.Spec.CPU.Sockets > 0 || m.Spec.CPU.ThreadsPerCore > 0
}

// ListEnabledClasses returns the machine classes a KubeVirt machine can be
// changed to, sorted by name.
//
// Disabled classes are excluded because the operator refuses them outright;
// classes naming only other providers are excluded because assigning one to a
// KubeVirt machine could never be honoured. A class naming no providers is
// kept — the field is meant to be required, but the operator itself never
// checks it.
func ListEnabledClasses(ctx context.Context, az *kube.VitistackClient) ([]vitiv1alpha1.MachineClass, error) {
	var list vitiv1alpha1.MachineClassList
	if err := az.Ctrl.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("availability zone %q: listing machine classes: %w", az.AZ.Name, err)
	}
	out := make([]vitiv1alpha1.MachineClass, 0, len(list.Items))
	for _, c := range list.Items {
		if !c.Spec.Enabled || !supportsKubevirt(c.Spec.MachineProviders) {
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func supportsKubevirt(providers []vitiv1alpha1.MachineProviderType) bool {
	if len(providers) == 0 {
		return true
	}
	for _, p := range providers {
		if p == vitiv1alpha1.MachineProviderTypeKubevirt {
			return true
		}
	}
	return false
}

// PatchMachineClass sets the Machine's spec.machineClass in its availability
// zone. A merge patch rather than an update, so a Machine modified between
// read and write costs nothing but this one field.
func PatchMachineClass(ctx context.Context, az *kube.VitistackClient, m *vitiv1alpha1.Machine, className string) error {
	patch, err := json.Marshal(map[string]any{
		"spec": map[string]any{"machineClass": className},
	})
	if err != nil {
		return err
	}
	if err := az.Ctrl.Patch(ctx, m, ctrlclient.RawPatch(types.MergePatchType, patch)); err != nil {
		return fmt.Errorf("availability zone %q: setting machine class on %s/%s: %w",
			az.AZ.Name, m.Namespace, m.Name, err)
	}
	return nil
}

// PatchVMResources writes the computed resources into the VM's template, which
// is what the guest boots with on its next restart. The running VMI is
// deliberately left alone — KubeVirt applies the template on restart, and
// live-resizing is a different feature with different failure modes.
func PatchVMResources(ctx context.Context, kv *kube.KubeVirtClient, v *kubevirtv1.VirtualMachine, r Resources) error {
	if v.Spec.Template == nil {
		return fmt.Errorf("virtual machine %s/%s has no template to size — not created by kubevirt-operator?",
			v.Namespace, v.Name)
	}
	patch, err := json.Marshal(map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{
					"domain": map[string]any{
						"cpu": map[string]any{
							"cores":   r.Cores,
							"sockets": r.Sockets,
							"threads": r.Threads,
						},
						"memory": map[string]any{"guest": r.Memory},
					},
				},
			},
		},
	})
	if err != nil {
		return err
	}
	if err := kv.Ctrl.Patch(ctx, v, ctrlclient.RawPatch(types.MergePatchType, patch)); err != nil {
		return fmt.Errorf("kubevirt cluster %q: sizing %s/%s: %w",
			kv.Cluster.Name, v.Namespace, v.Name, err)
	}
	return nil
}

// uintToUint32 and intToUint32 mirror the operator's bounds-checked
// conversions exactly, falling back rather than wrapping on out-of-range
// values. uintToUint32 passes zero through, as safeUintToUint32 does — a
// class with cores: 0 must size the VM the way the operator would, not the
// way a default would. The comparisons go through uint64/int64 so the
// MaxUint32 constant stays representable on 32-bit platforms.
func uintToUint32(v uint, fallback uint32) uint32 {
	if uint64(v) > math.MaxUint32 {
		return fallback
	}
	return uint32(v) // #nosec G115 -- bounds checked above
}

func intToUint32(v int, fallback uint32) uint32 {
	if v <= 0 || int64(v) > math.MaxUint32 {
		return fallback
	}
	return uint32(v) // #nosec G115 -- bounds checked above
}
