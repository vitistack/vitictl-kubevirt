package cmd

import (
	"github.com/spf13/cobra"

	"github.com/vitistack/vitictl-kubevirt/internal/kube"
	"github.com/vitistack/vitictl-kubevirt/internal/virtctl"
)

// runVirtctl invokes virtctl against the connected cluster.
//
// The cluster's kubeconfig and context are passed explicitly rather than
// inherited from the environment, so an action can never land on whatever
// cluster the user's KUBECONFIG happened to point at — the whole reason this
// plugin keeps its own cluster list.
func runVirtctl(cmd *cobra.Command, kv *kube.KubeVirtClient, verb, namespace, name string, extra ...string) error {
	kubeconfig, kubecontext, err := kv.VirtctlTarget()
	if err != nil {
		return err
	}
	return virtctl.Run(
		contextOrBackground(cmd),
		virtctl.Streams{In: cmd.InOrStdin(), Out: cmd.OutOrStdout(), Err: cmd.ErrOrStderr()},
		verb,
		virtctl.Target{
			Kubeconfig: kubeconfig,
			Context:    kubecontext,
			Namespace:  namespace,
			Name:       name,
		},
		extra...,
	)
}
