package kube

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"

	"github.com/vitistack/vitictl-kubevirt/internal/config"
)

// AnnotationKubevirtConfig is stamped on a Machine by the operators, naming the
// KubevirtConfig it was provisioned through — and so, transitively, the KubeVirt
// cluster its VM actually runs on.
//
// This is what makes the lookup per-machine rather than per-zone. A zone that
// fronts two KubeVirt clusters resolves each machine to the right one instead
// of pairing half of them against the wrong cluster.
const AnnotationKubevirtConfig = "vitistack.io/kubevirt-config"

// SecretKeyKubeconfig is the key holding the kubeconfig in the Secret a
// KubevirtConfig points at.
const SecretKeyKubeconfig = "kubeconfig"

// Discoverer resolves the KubeVirt cluster backing a machine by asking that
// machine's own management cluster, rather than requiring the user to maintain
// a zone-to-cluster mapping by hand.
//
// Connections are cached per (zone, KubevirtConfig), so listing a fleet opens
// one connection per KubeVirt cluster no matter how many machines it holds.
// Failures are cached too: a cluster that is down should cost one timeout per
// listing, not one per machine.
type Discoverer struct {
	// Override short-circuits discovery entirely. When the user passes
	// --cluster they have said which cluster to act on, and that must win —
	// it is also the path that keeps working when the control plane is down.
	Override *KubeVirtClient

	// Local is the user's own configured clusters, consulted so a discovered
	// cluster they also have on disk can still be driven by virtctl.
	Local []config.Cluster

	mu    sync.Mutex
	cache map[string]cachedClient
}

type cachedClient struct {
	client *KubeVirtClient
	err    error
}

// For returns the KubeVirt cluster named by configName in the given zone.
//
// An empty configName means the Machine carried no annotation. The zone's sole
// KubevirtConfig is then used; a zone publishing several is reported as an
// error rather than guessed at, because guessing wrong pairs a machine with a
// stranger's VM.
func (d *Discoverer) For(ctx context.Context, az *VitistackClient, configName string) (*KubeVirtClient, error) {
	if d.Override != nil {
		return d.Override, nil
	}

	key := az.AZ.Name + "/" + configName
	d.mu.Lock()
	defer d.mu.Unlock()
	if c, ok := d.cache[key]; ok {
		return c.client, c.err
	}
	client, err := d.connect(ctx, az, configName)
	if d.cache == nil {
		d.cache = make(map[string]cachedClient)
	}
	d.cache[key] = cachedClient{client: client, err: err}
	return client, err
}

// connect walks KubevirtConfig -> Secret -> kubeconfig -> client.
func (d *Discoverer) connect(ctx context.Context, az *VitistackClient, configName string) (*KubeVirtClient, error) {
	kvc, err := d.kubevirtConfig(ctx, az, configName)
	if err != nil {
		return nil, err
	}

	ref := ctrlclient.ObjectKey{Namespace: kvc.Spec.SecretNamespace, Name: kvc.Spec.KubeconfigSecretRef}
	var secret corev1.Secret
	if err := az.Ctrl.Get(ctx, ref, &secret); err != nil {
		return nil, fmt.Errorf("reading kubeconfig secret %s/%s named by KubevirtConfig %q: %w",
			ref.Namespace, ref.Name, kvc.Name, err)
	}
	raw := secret.Data[SecretKeyKubeconfig]
	if len(raw) == 0 {
		return nil, fmt.Errorf("secret %s/%s named by KubevirtConfig %q has no %q key",
			ref.Namespace, ref.Name, kvc.Name, SecretKeyKubeconfig)
	}
	return ConnectKubeVirtFromKubeconfig(raw, d.Local)
}

// kubevirtConfig resolves the named KubevirtConfig, or the zone's only one when
// the machine did not name it. KubevirtConfig is cluster-scoped, so the lookup
// carries no namespace.
func (d *Discoverer) kubevirtConfig(ctx context.Context, az *VitistackClient, configName string) (
	*vitiv1alpha1.KubevirtConfig, error,
) {
	if configName != "" {
		var kvc vitiv1alpha1.KubevirtConfig
		if err := az.Ctrl.Get(ctx, ctrlclient.ObjectKey{Name: configName}, &kvc); err != nil {
			return nil, fmt.Errorf("reading KubevirtConfig %q: %w", configName, err)
		}
		return &kvc, nil
	}

	var list vitiv1alpha1.KubevirtConfigList
	if err := az.Ctrl.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("listing KubevirtConfigs: %w", err)
	}
	switch len(list.Items) {
	case 0:
		return nil, fmt.Errorf(
			"no KubevirtConfig is published, so there is nothing to say which "+
				"KubeVirt cluster these machines run on (expected one, or a %s annotation on the machine)",
			AnnotationKubevirtConfig)
	case 1:
		return &list.Items[0], nil
	default:
		names := make([]string, 0, len(list.Items))
		for i := range list.Items {
			names = append(names, list.Items[i].Name)
		}
		sort.Strings(names)
		return nil, fmt.Errorf(
			"machine carries no %s annotation and the zone publishes several KubevirtConfigs (%s), "+
				"so which KubeVirt cluster it runs on is ambiguous",
			AnnotationKubevirtConfig, strings.Join(names, ", "))
	}
}
