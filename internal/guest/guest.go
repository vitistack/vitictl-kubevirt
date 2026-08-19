// Package guest reaches the workload (guest) cluster that a KubernetesCluster
// provisions, using the kubeconfig the control plane itself stores — the same
// secret vitictl's extract command reads. Draining a node needs the guest
// API, and requiring the user to configure every guest cluster locally would
// make rolling changes unusable at fleet scale.
package guest

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// KeyKubeConfig is the secret key holding the guest cluster's kubeconfig.
	KeyKubeConfig = "kube.config"
	// LabelClusterID links secrets and machines to their cluster.
	LabelClusterID = "vitistack.io/clusterid"
)

// FindClusterSecret locates the Secret holding a cluster's config artifacts:
// named exactly <clusterID>, else labelled vitistack.io/clusterid=<clusterID>
// (which covers operators configured with a SECRET_PREFIX).
func FindClusterSecret(ctx context.Context, c ctrlclient.Client, namespace, clusterID string) (*corev1.Secret, error) {
	var direct corev1.Secret
	err := c.Get(ctx, ctrlclient.ObjectKey{Namespace: namespace, Name: clusterID}, &direct)
	if err == nil {
		return &direct, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("reading secret %s/%s: %w", namespace, clusterID, err)
	}

	var list corev1.SecretList
	if err := c.List(ctx, &list, ctrlclient.InNamespace(namespace),
		ctrlclient.MatchingLabels{LabelClusterID: clusterID}); err != nil {
		return nil, fmt.Errorf("listing secrets for cluster %q: %w", clusterID, err)
	}
	if len(list.Items) == 0 {
		return nil, fmt.Errorf(
			"no secret for cluster %q in namespace %q — cannot reach the guest cluster to drain nodes",
			clusterID, namespace)
	}
	return &list.Items[0], nil
}

// Connect builds a clientset from the secret's kube.config key.
func Connect(secret *corev1.Secret) (kubernetes.Interface, error) {
	raw, ok := secret.Data[KeyKubeConfig]
	if !ok {
		return nil, fmt.Errorf("secret %q has no %q key", secret.Name, KeyKubeConfig)
	}
	cfg, err := clientcmd.RESTConfigFromKubeConfig(raw)
	if err != nil {
		return nil, fmt.Errorf("parsing guest kubeconfig: %w", err)
	}
	return kubernetes.NewForConfig(cfg)
}
