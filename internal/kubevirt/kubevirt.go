// Package kubevirt speaks a handful of KubeVirt's subresource endpoints
// directly over REST, for the one action this plugin can do itself instead
// of shelling out to virtctl: restarting a VM.
//
// Most lifecycle verbs and both consoles stay on virtctl (see
// internal/virtctl) because they are either VirtualMachineInstance
// subresources with their own paths and semantics, or streaming connections
// a plain HTTP call cannot replace. Restart is a VirtualMachine subresource
// reachable with a single PUT, so it does not need virtctl's binary or its
// kubeconfig-path requirement — see KubeVirtClient.VirtctlTarget for why that
// requirement exists and why it does not apply here.
package kubevirt

import (
	"context"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	kubevirtv1 "kubevirt.io/api/core/v1"

	"github.com/vitistack/vitictl-kubevirt/internal/kube"
)

// virtualMachines is the resource name in the subresource API path.
const virtualMachines = "virtualmachines"

// Restart triggers a VirtualMachine restart through KubeVirt's subresource
// API: PUT /apis/subresources.kubevirt.io/v1/namespaces/{namespace}/virtualmachines/{name}/restart.
//
// This exists so restarting a VM needs nothing beyond the client the plugin
// already holds for kv.Cluster — no virtctl binary, and no local kubeconfig
// for clusters that were only ever discovered from the Vitistack control
// plane. See the package doc for why the other lifecycle verbs stay on
// virtctl.
func Restart(ctx context.Context, kv *kube.KubeVirtClient, namespace, name string) error {
	client, err := subresourceClient(kv.RESTConfig)
	if err != nil {
		return fmt.Errorf("kubevirt cluster %q: building subresource client: %w", kv.Cluster.Name, err)
	}

	// An empty body: KubeVirt defaults GracePeriodSeconds to the VMI's own
	// terminationGracePeriodSeconds when nil, which is the right default
	// absent any reason from a caller to override it — nothing here has one.
	body, err := json.Marshal(&kubevirtv1.RestartOptions{})
	if err != nil {
		return fmt.Errorf("encoding restart options: %w", err)
	}

	err = client.Put().
		Namespace(namespace).
		Resource(virtualMachines).
		Name(name).
		SubResource("restart").
		SetHeader("Content-Type", runtime.ContentTypeJSON).
		Body(body).
		Do(ctx).
		Error()

	return mapRestartError(err, kv.Cluster.Name, namespace, name)
}

// subresourceClient builds a rest.Interface scoped to KubeVirt's subresource
// API group and version.
//
// There is no KubeVirt client-go in this module, and adding one just for a
// single PUT would be a heavy dependency for what is otherwise a plain HTTP
// call — so this builds directly off the rest.Config every KubeVirtClient
// already carries, the same way ctrlclient.New does in package kube.
//
// The NegotiatedSerializer is client-go's own generated Kubernetes scheme,
// not one built here: KubeVirt's aggregated API server reports errors as
// ordinary metav1.Status objects, and that scheme already knows how to
// decode those, which is what lets mapRestartError use apierrors.IsNotFound
// and apierrors.IsConflict instead of comparing raw status codes.
func subresourceClient(cfg *rest.Config) (rest.Interface, error) {
	c := rest.CopyConfig(cfg)
	c.APIPath = "/apis"
	gv := kubevirtv1.SubresourceGroupVersions[0]
	c.GroupVersion = &gv
	c.NegotiatedSerializer = scheme.Codecs
	return rest.RESTClientFor(c)
}

// mapRestartError turns the subresource call's error into something an
// operator can act on, rather than a bare HTTP status.
//
// KubeVirt's aggregated API server follows ordinary Kubernetes conventions
// here: a missing VirtualMachine is a 404, and restarting one that is not
// currently running is a 409 — the subresource handler refuses because there
// is no running VirtualMachineInstance to restart.
func mapRestartError(err error, cluster, namespace, name string) error {
	switch {
	case err == nil:
		return nil
	case apierrors.IsNotFound(err):
		return fmt.Errorf("kubevirt cluster %q: no VirtualMachine %s/%s: %w", cluster, namespace, name, err)
	case apierrors.IsConflict(err):
		return fmt.Errorf(
			"kubevirt cluster %q: VirtualMachine %s/%s cannot be restarted right now — "+
				"it is most likely not running: %w",
			cluster, namespace, name, err)
	default:
		return fmt.Errorf("kubevirt cluster %q: restarting VirtualMachine %s/%s: %w", cluster, namespace, name, err)
	}
}
