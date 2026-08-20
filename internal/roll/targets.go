package roll

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"

	"github.com/vitistack/vitictl-kubevirt/internal/kube"
)

// ErrClusterNotFound reports that no KubernetesCluster matched, so callers
// can fall back to other interpretations of the name.
var ErrClusterNotFound = errors.New("kubernetescluster not found")

// LoadTargets reads a KubernetesCluster and returns its rollable targets:
// the control plane first, then the nodepools in spec order. An empty
// namespace searches nowhere in particular — callers pass the namespace they
// are scoped to, or "" to search all (via a list).
func LoadTargets(ctx context.Context, az *kube.VitistackClient, namespace, clusterName string) ([]Target, error) {
	cluster, err := findCluster(ctx, az, namespace, clusterName)
	if err != nil {
		return nil, err
	}
	return Targets(cluster), nil
}

// Targets returns a cluster's rollable targets: the control plane first, then
// the nodepools in spec order.
func Targets(cluster *vitiv1alpha1.KubernetesCluster) []Target {
	out := []Target{{
		Cluster:  cluster,
		Kind:     KindControlPlane,
		Class:    cluster.Spec.Topology.ControlPlane.MachineClass,
		Replicas: cluster.Spec.Topology.ControlPlane.Replicas,
	}}
	for i, p := range cluster.Spec.Topology.Workers.NodePools {
		out = append(out, Target{
			Cluster:   cluster,
			Kind:      KindNodePool,
			Pool:      p.Name,
			Class:     p.MachineClass,
			Replicas:  p.Replicas,
			PoolIndex: i,
		})
	}
	return out
}

// findCluster fetches the cluster the caller means, searching every
// namespace when none was given — the caller types a cluster name, not a
// namespace.
//
// A user reaching for a cluster rarely has its object name in front of them:
// machine names embed the clusterId (t-x-vexr-ctp0 is clusterId t-x-vexr plus
// a role suffix), so the string actually visible when looking at a machine is
// the clusterId, not the name `viti kc list` shows in its NAME column. So the
// object name is tried first — one Get, the common case — and the clusterId
// is tried second, never the other way round: the object name is what the API
// server itself keys on, and preferring a fuzzy match over an exact one is
// how the wrong cluster gets acted on.
func findCluster(ctx context.Context, az *kube.VitistackClient, namespace, name string) (*vitiv1alpha1.KubernetesCluster, error) {
	if namespace != "" {
		var cluster vitiv1alpha1.KubernetesCluster
		err := az.Ctrl.Get(ctx, ctrlclient.ObjectKey{Namespace: namespace, Name: name}, &cluster)
		if err == nil {
			return &cluster, nil
		}
		if !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("zone %q: reading kubernetescluster %q: %w", az.AZ.Name, name, err)
		}
		// Not found by name — fall through to the clusterId search below,
		// scoped to this same namespace.
	}

	var opts []ctrlclient.ListOption
	if namespace != "" {
		opts = append(opts, ctrlclient.InNamespace(namespace))
	}
	var list vitiv1alpha1.KubernetesClusterList
	if err := az.Ctrl.List(ctx, &list, opts...); err != nil {
		return nil, fmt.Errorf("zone %q: listing kubernetesclusters: %w", az.AZ.Name, err)
	}

	// Exact name match, checked again here for the namespace=="" path (which
	// never called Get above) and re-checked harmlessly for the
	// namespace!="" path. Still first, per the resolution order above.
	for i := range list.Items {
		if list.Items[i].Name == name {
			return &list.Items[i], nil
		}
	}

	var matches []*vitiv1alpha1.KubernetesCluster
	for i := range list.Items {
		if list.Items[i].Spec.Cluster.ClusterId == name {
			matches = append(matches, &list.Items[i])
		}
	}
	switch len(matches) {
	case 0:
		return nil, notFoundError(az, namespace, name, list.Items)
	case 1:
		return matches[0], nil
	default:
		// clusterIds are supposed to be unique, so this should be
		// impossible — which is exactly why it is reported instead of
		// silently resolved to whichever one was listed first.
		where := make([]string, 0, len(matches))
		for _, m := range matches {
			where = append(where, m.Namespace+"/"+m.Name)
		}
		return nil, fmt.Errorf("clusterId %q is ambiguous — it matches %s", name, strings.Join(where, ", "))
	}
}

// roleSuffix matches a trailing "-<role><index>" segment such as "-ctp0" or
// "-wrk3", the pattern machine names append to their cluster's clusterId.
var roleSuffix = regexp.MustCompile(`^(.+)-[a-zA-Z]+[0-9]+$`)

// notFoundError reports that neither identifier matched, and — when the
// search string looks like a machine name (clusterId plus a role suffix) and
// stripping that suffix would have hit a real cluster — says so directly.
// That turns what would otherwise be two failed attempts (the machine name,
// then the guessed clusterId) into one correction.
func notFoundError(az *kube.VitistackClient, namespace, name string, items []vitiv1alpha1.KubernetesCluster) error {
	where := fmt.Sprintf("(zone %q)", az.AZ.Name)
	if namespace != "" {
		where = fmt.Sprintf("in namespace %q %s", namespace, where)
	}

	if m := roleSuffix.FindStringSubmatch(name); m != nil {
		stripped := m[1]
		for i := range items {
			if items[i].Spec.Cluster.ClusterId == stripped {
				return fmt.Errorf(
					"%w: %q as a name or clusterId %s — %q looks like a machine name; "+
						"its cluster is %q",
					ErrClusterNotFound, name, where, name, items[i].Name)
			}
		}
	}

	return fmt.Errorf("%w: %q as a name or clusterId %s", ErrClusterNotFound, name, where)
}
