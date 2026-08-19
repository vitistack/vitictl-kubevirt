package roll

import (
	"context"
	"errors"
	"fmt"

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
	return out, nil
}

// findCluster fetches the cluster by name, searching every namespace when
// none was given — the caller types a cluster name, not a namespace.
func findCluster(ctx context.Context, az *kube.VitistackClient, namespace, name string) (*vitiv1alpha1.KubernetesCluster, error) {
	if namespace != "" {
		var cluster vitiv1alpha1.KubernetesCluster
		err := az.Ctrl.Get(ctx, ctrlclient.ObjectKey{Namespace: namespace, Name: name}, &cluster)
		if err == nil {
			return &cluster, nil
		}
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: %q in namespace %q (zone %q)", ErrClusterNotFound, name, namespace, az.AZ.Name)
		}
		return nil, fmt.Errorf("zone %q: reading kubernetescluster %q: %w", az.AZ.Name, name, err)
	}

	var list vitiv1alpha1.KubernetesClusterList
	if err := az.Ctrl.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("zone %q: listing kubernetesclusters: %w", az.AZ.Name, err)
	}
	for i := range list.Items {
		if list.Items[i].Name == name {
			return &list.Items[i], nil
		}
	}
	return nil, fmt.Errorf("%w: %q (zone %q)", ErrClusterNotFound, name, az.AZ.Name)
}
