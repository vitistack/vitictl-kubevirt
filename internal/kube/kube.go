// Package kube builds Kubernetes clients for the two kinds of cluster this
// plugin talks to: the Vitistack availability zones that hold Machine
// resources, and the KubeVirt clusters the virtual machines actually run on.
package kube

import (
	"context"
	"fmt"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	kubevirtv1 "kubevirt.io/api/core/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"

	"github.com/vitistack/vitictl-kubevirt/internal/config"
)

// KubeVirtClient is a connection to one KubeVirt cluster.
type KubeVirtClient struct {
	Cluster    config.Cluster
	Ctrl       ctrlclient.Client
	RESTConfig *rest.Config
}

// VirtctlTarget returns the kubeconfig path and context virtctl must be given
// to reach this cluster.
//
// virtctl is a separate binary that takes a kubeconfig *path*, so a cluster
// discovered from a management cluster can only be driven when the user also
// has it configured locally. The alternative — writing the discovered
// credentials to a temporary file — would put a cluster-admin kubeconfig on
// disk for the duration of a VNC session, so it is refused with a hint here
// instead.
//
// Returning an error rather than an empty pair matters: virtctl falls back to
// the ambient KUBECONFIG when given neither flag, which is exactly the "act on
// whatever cluster the shell was pointing at" accident this plugin exists to
// prevent.
func (k *KubeVirtClient) VirtctlTarget() (kubeconfig, kubecontext string, err error) {
	if k.Cluster.Kubeconfig == "" && k.Cluster.Context == "" {
		return "", "", fmt.Errorf(
			"KubeVirt cluster %q was discovered from the Vitistack control plane, but it is "+
				"not in your local configuration and virtctl needs a kubeconfig file.\n"+
				"Add it with:\n  viti kubevirt config add %s --kubeconfig <path> --context %s",
			k.Cluster.Name, localName(k.Cluster.Name), k.Cluster.Name)
	}
	return k.Cluster.Kubeconfig, k.Cluster.Context, nil
}

// localName suggests a config entry name from a context name, which is
// conventionally "admin@cluster".
func localName(kubecontext string) string {
	if _, cluster, ok := strings.Cut(kubecontext, "@"); ok && cluster != "" {
		return cluster
	}
	return kubecontext
}

// VitistackClient is a connection to one Vitistack availability zone.
type VitistackClient struct {
	AZ   config.AvailabilityZone
	Ctrl ctrlclient.Client
}

// ConnectKubeVirt builds a client for a KubeVirt cluster.
//
// The scheme carries the core types plus KubeVirt's, so VirtualMachine and
// VirtualMachineInstance can be read with the typed client. A cluster without
// KubeVirt installed fails on the first List with a clear "no matches for
// kind" rather than here, because discovery costs a round trip that every
// other command would pay for nothing.
func ConnectKubeVirt(cl config.Cluster) (*KubeVirtClient, error) {
	cfg, err := RESTConfig(cl.Kubeconfig, cl.Context)
	if err != nil {
		return nil, fmt.Errorf("kubevirt cluster %q: %w", cl.Name, err)
	}
	sch := runtime.NewScheme()
	if err := scheme.AddToScheme(sch); err != nil {
		return nil, fmt.Errorf("adding core scheme: %w", err)
	}
	if err := kubevirtv1.AddToScheme(sch); err != nil {
		return nil, fmt.Errorf("adding kubevirt scheme: %w", err)
	}
	c, err := ctrlclient.New(cfg, ctrlclient.Options{Scheme: sch})
	if err != nil {
		return nil, fmt.Errorf("kubevirt cluster %q: building kube client: %w", cl.Name, err)
	}
	return &KubeVirtClient{Cluster: cl, Ctrl: c, RESTConfig: cfg}, nil
}

// ConnectKubeVirtFromKubeconfig builds a client from raw kubeconfig bytes,
// which is the form a discovered cluster arrives in: its credentials come out
// of a Secret in the management cluster rather than a file on disk.
//
// local is the user's own configured clusters. A discovered cluster that the
// user also has locally adopts that entry wholesale, so virtctl — which needs
// a kubeconfig path — keeps working for it. One with no local counterpart is
// named after its context and left without a path, which VirtctlTarget then
// reports rather than falling back to the ambient KUBECONFIG.
func ConnectKubeVirtFromKubeconfig(raw []byte, local []config.Cluster) (*KubeVirtClient, error) {
	apiCfg, err := clientcmd.Load(raw)
	if err != nil {
		return nil, fmt.Errorf("parsing discovered kubeconfig: %w", err)
	}
	kubecontext := apiCfg.CurrentContext
	cfg, err := clientcmd.NewNonInteractiveClientConfig(
		*apiCfg, kubecontext, &clientcmd.ConfigOverrides{}, nil).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("building client config for %q: %w", kubecontext, err)
	}

	sch := runtime.NewScheme()
	if err := scheme.AddToScheme(sch); err != nil {
		return nil, fmt.Errorf("adding core scheme: %w", err)
	}
	if err := kubevirtv1.AddToScheme(sch); err != nil {
		return nil, fmt.Errorf("adding kubevirt scheme: %w", err)
	}
	c, err := ctrlclient.New(cfg, ctrlclient.Options{Scheme: sch})
	if err != nil {
		return nil, fmt.Errorf("building kube client for %q: %w", kubecontext, err)
	}

	cluster := config.Cluster{Name: kubecontext}
	for _, l := range local {
		if l.Context == kubecontext {
			cluster = l
			break
		}
	}
	return &KubeVirtClient{Cluster: cluster, Ctrl: c, RESTConfig: cfg}, nil
}

// ConnectVitistack builds clients for the given availability zones. A zone
// that cannot be reached is reported to warn and skipped, so one unreachable
// zone does not hide the machines in every other one.
func ConnectVitistack(_ context.Context, zones []config.AvailabilityZone, warn func(error)) ([]*VitistackClient, error) {
	sch := runtime.NewScheme()
	if err := scheme.AddToScheme(sch); err != nil {
		return nil, fmt.Errorf("adding core scheme: %w", err)
	}
	if err := vitiv1alpha1.AddToScheme(sch); err != nil {
		return nil, fmt.Errorf("adding vitistack scheme: %w", err)
	}

	out := make([]*VitistackClient, 0, len(zones))
	var firstErr error
	for _, z := range zones {
		cfg, err := RESTConfig(z.Kubeconfig, z.Context)
		if err == nil {
			var c ctrlclient.Client
			c, err = ctrlclient.New(cfg, ctrlclient.Options{Scheme: sch})
			if err == nil {
				out = append(out, &VitistackClient{AZ: z, Ctrl: c})
				continue
			}
		}
		if firstErr == nil {
			firstErr = err
		}
		if warn != nil {
			warn(fmt.Errorf("availability zone %q unreachable: %w", z.Name, err))
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf(
			"cannot reach any Vitistack availability zone (0/%d). "+
				"This is a kubeconfig or network problem, not a KubeVirt one. First error: %v",
			len(zones), firstErr)
	}
	return out, nil
}

// RESTConfig loads a kubeconfig, honouring an explicit path and context and
// otherwise falling back to $KUBECONFIG and ~/.kube/config.
func RESTConfig(kubeconfig, kubecontext string) (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		if _, err := os.Stat(kubeconfig); err != nil {
			return nil, fmt.Errorf("kubeconfig %q is not readable: %w", kubeconfig, err)
		}
		rules.ExplicitPath = kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{}
	if kubecontext != "" {
		overrides.CurrentContext = kubecontext
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kube client config: %w", err)
	}
	return cfg, nil
}
