package vm

import (
	"context"
	"fmt"
	"time"

	k8sv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubevirtv1 "kubevirt.io/api/core/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/vitistack/vitictl-kubevirt/internal/kube"
)

// GetVMI fetches the running instance of a VirtualMachine. A VMI shares its
// VM's name, so the VM's name is what to ask for.
//
// A missing VMI is returned as a plain not-found error rather than a nil
// instance: every caller here needs the instance, and "the VM is not running"
// is a better thing to say than a nil dereference three lines later.
func GetVMI(ctx context.Context, kv *kube.KubeVirtClient, namespace, name string) (*kubevirtv1.VirtualMachineInstance, error) {
	var vmi kubevirtv1.VirtualMachineInstance
	key := ctrlclient.ObjectKey{Namespace: namespace, Name: name}
	if err := kv.Ctrl.Get(ctx, key, &vmi); err != nil {
		return nil, fmt.Errorf("kubevirt cluster %q: reading instance %s/%s: %w",
			kv.Cluster.Name, namespace, name, err)
	}
	return &vmi, nil
}

// NotMigratableReason returns KubeVirt's own explanation of why an instance
// cannot be live-migrated, or "" when it can.
//
// KubeVirt evaluates this continuously and publishes it as the LiveMigratable
// condition — a VM on non-shared storage, for instance, is permanently
// unmigratable and says so. Reading it costs one GET and turns a migration
// that would have failed a minute later, after a target pod was scheduled and
// a domain created, into an immediate refusal carrying the real reason.
//
// An absent condition is treated as unknown rather than as permission: KubeVirt
// populates it shortly after the instance starts, and guessing "probably fine"
// about a safety check is how a preflight becomes decoration.
func NotMigratableReason(vmi *kubevirtv1.VirtualMachineInstance) string {
	for _, c := range vmi.Status.Conditions {
		if c.Type != kubevirtv1.VirtualMachineInstanceIsMigratable {
			continue
		}
		if c.Status == k8sv1.ConditionTrue {
			return ""
		}
		switch {
		case c.Message != "" && c.Reason != "":
			return fmt.Sprintf("%s: %s", c.Reason, c.Message)
		case c.Message != "":
			return c.Message
		case c.Reason != "":
			return c.Reason
		default:
			return "KubeVirt reports it as not live-migratable but gave no reason"
		}
	}
	return "KubeVirt has not published a LiveMigratable condition for this instance yet"
}

// StartMigration asks KubeVirt to live-migrate an instance.
//
// A VirtualMachineInstanceMigration is an ordinary object, not a subresource,
// so this is a plain create through the client the plugin already holds — no
// virtctl, and no local kubeconfig. KubeVirt does the rest: it schedules a
// target pod, hands the domain over, and reports progress on the instance.
//
// targetNode, when set, becomes an addedNodeSelector pinning the destination.
// Without it KubeVirt chooses, which is usually what you want — pinning is for
// steering away from a suspect host or onto a known-good one. The field needs
// KubeVirt 1.5 or newer; an older cluster rejects the object at create time,
// which is the right moment to find out.
//
// The name is generated rather than derived from the instance: a VMI can be
// migrated any number of times, and a deterministic name would collide with
// its own history.
func StartMigration(ctx context.Context, kv *kube.KubeVirtClient, namespace, vmiName, targetNode string) (*kubevirtv1.VirtualMachineInstanceMigration, error) {
	m := &kubevirtv1.VirtualMachineInstanceMigration{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "viti-" + vmiName + "-",
			Namespace:    namespace,
		},
		Spec: kubevirtv1.VirtualMachineInstanceMigrationSpec{VMIName: vmiName},
	}
	if targetNode != "" {
		m.Spec.AddedNodeSelector = map[string]string{k8sv1.LabelHostname: targetNode}
	}
	if err := kv.Ctrl.Create(ctx, m); err != nil {
		return nil, fmt.Errorf("kubevirt cluster %q: starting migration of %s/%s: %w",
			kv.Cluster.Name, namespace, vmiName, err)
	}
	return m, nil
}

// AwaitMigration polls a migration until KubeVirt marks it Succeeded or
// Failed, and returns it in that terminal state.
//
// Polling rather than watching, for the same reason migrations --watch polls:
// a migration runs for tens of seconds to minutes, and a watch would buy a
// resourceVersion and a reconnect path for responsiveness nobody waiting on a
// single object would notice.
//
// A cancelled context returns the last state read alongside the error, so a
// caller interrupted mid-migration can still say where it had got to. The
// migration itself keeps running — KubeVirt owns it now, and abandoning the
// poll does not abandon the work.
func AwaitMigration(ctx context.Context, kv *kube.KubeVirtClient, namespace, name string, interval time.Duration) (*kubevirtv1.VirtualMachineInstanceMigration, error) {
	key := ctrlclient.ObjectKey{Namespace: namespace, Name: name}
	var last *kubevirtv1.VirtualMachineInstanceMigration

	for {
		var m kubevirtv1.VirtualMachineInstanceMigration
		if err := kv.Ctrl.Get(ctx, key, &m); err != nil {
			return last, fmt.Errorf("kubevirt cluster %q: reading migration %s/%s: %w",
				kv.Cluster.Name, namespace, name, err)
		}
		last = &m
		if isMigrationFinished(&m) {
			return &m, nil
		}

		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(interval):
		}
	}
}
