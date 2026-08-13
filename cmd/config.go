package cmd

import (
	"context"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/vitistack/vitictl-kubevirt/internal/config"
	"github.com/vitistack/vitictl-kubevirt/internal/kube"
	"github.com/vitistack/vitictl-kubevirt/internal/virtctl"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "config",
		Short:   "⚙️  Manage the KubeVirt cluster configuration",
		Aliases: []string{"cfg"},
		Long: `Manage the KubeVirt clusters this plugin talks to
(~/.vitistack/kubevirt.config.yaml).

This is separate from viti's own availability zones. Those hold the Vitistack
control plane, where Machine resources live, and are configured with
"viti config add" — this plugin reads them. The clusters configured here are
where the virtual machines actually run.`,
	}
	cmd.AddCommand(
		newConfigAddCmd(),
		newConfigListCmd(),
		newConfigRemoveCmd(),
		newConfigDefaultCmd(),
		newConfigPathCmd(),
		newConfigTestCmd(),
	)
	return cmd
}

func newConfigAddCmd() *cobra.Command {
	var kubeconfig, kubecontext, namespace string
	var makeDefault bool

	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Add or update a KubeVirt cluster",
		Long: `Register a KubeVirt cluster by kubeconfig path and/or context.

An omitted --kubeconfig falls back to $KUBECONFIG or ~/.kube/config; an
omitted --context uses that kubeconfig's current-context.`,
		Example: `  viti kubevirt config add kv-osl-01 --kubeconfig ~/kubeconfig/kv-osl-01
  viti kubevirt config add kv-trd-02 --context admin@kv-trd-02 --default`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			cluster := config.Cluster{
				Name:       args[0],
				Kubeconfig: kubeconfig,
				Context:    kubecontext,
				Namespace:  namespace,
				// The first cluster configured is the default, so a
				// single-cluster setup never needs --cluster.
				Default: makeDefault || len(cfg.Clusters) == 0,
			}
			if err := cfg.Add(cluster); err != nil {
				return err
			}
			if err := config.Save(cfg); err != nil {
				return err
			}
			path, err := config.Path()
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "✅ added %s to %s\n", cluster.Name, path)
			return err
		},
	}
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "path to the cluster's kubeconfig")
	cmd.Flags().StringVar(&kubecontext, "context", "", "kubeconfig context to use")
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "", "default namespace for listings on this cluster")
	cmd.Flags().BoolVar(&makeDefault, "default", false, "use this cluster when --cluster is not given")
	return cmd
}

func newConfigListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls", "view"},
		Short:   "List the configured KubeVirt clusters",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if len(cfg.Clusters) == 0 {
				_, err := fmt.Fprintln(cmd.OutOrStdout(),
					"no KubeVirt clusters configured — add one with 'viti kubevirt config add <name> --kubeconfig <path>'")
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)
			_, _ = fmt.Fprintln(tw, "NAME\tDEFAULT\tKUBECONFIG\tCONTEXT\tNAMESPACE")
			for _, c := range cfg.Clusters {
				def := ""
				if c.Default {
					def = "*"
				}
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
					c.Name, def, orDash(c.Kubeconfig), orDash(c.Context), orDash(c.Namespace))
			}
			return tw.Flush()
		},
	}
}

func newConfigRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "remove <name>",
		Aliases: []string{"rm", "delete"},
		Short:   "Remove a KubeVirt cluster",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if err := cfg.Remove(args[0]); err != nil {
				return err
			}
			if err := config.Save(cfg); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "✅ removed %s\n", args[0])
			return err
		},
	}
}

func newConfigDefaultCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "default <name>",
		Short: "Set the cluster used when --cluster is not given",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if err := cfg.SetDefault(args[0]); err != nil {
				return err
			}
			if err := config.Save(cfg); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "✅ %s is now the default cluster\n", args[0])
			return err
		},
	}
}

func newConfigPathCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Print the configuration file paths",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := config.Path()
			if err != nil {
				return err
			}
			viti, err := config.VitistackConfigPath()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "kubevirt clusters:   %s\n", path)
			_, err = fmt.Fprintf(out, "vitistack (viti):    %s\n", viti)
			return err
		},
	}
}

func newConfigTestCmd() *cobra.Command {
	var clusterName string

	cmd := &cobra.Command{
		Use:   "test",
		Short: "Verify that virtctl, the KubeVirt cluster and the Vitistack zones are reachable",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			ctx := cmd.Context()

			// Check every leg, then report — a user fixing their setup wants
			// the whole picture, not one failure at a time.
			var failed bool
			report := func(label string, err error) {
				if err != nil {
					failed = true
					_, _ = fmt.Fprintf(out, "❌ %s: %v\n", label, err)
					return
				}
				_, _ = fmt.Fprintf(out, "✅ %s\n", label)
			}

			if path, err := virtctl.Path(); err != nil {
				report("virtctl", err)
			} else {
				report("virtctl ("+path+")", nil)
			}

			cfg, err := config.Load()
			if err != nil {
				return err
			}
			cluster, err := cfg.Select(clusterName)
			if err != nil {
				report("kubevirt cluster", err)
			} else {
				kv, err := kube.ConnectKubeVirt(cluster)
				if err != nil {
					report("kubevirt cluster "+cluster.Name, err)
				} else {
					report("kubevirt cluster "+cluster.Name+" ("+kv.RESTConfig.Host+")", nil)
				}
			}

			zones, err := config.AvailabilityZones("")
			if err != nil {
				report("vitistack availability zones", err)
			} else {
				clients, err := kube.ConnectVitistack(ctx, zones, nil)
				if err != nil {
					report("vitistack availability zones", err)
				} else {
					report(fmt.Sprintf("vitistack availability zones (%d/%d reachable)", len(clients), len(zones)), nil)
				}
			}

			if failed {
				return errConfigIncomplete
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&clusterName, "cluster", "", "KubeVirt cluster to test (default: the configured default)")
	return cmd
}

// errConfigIncomplete makes "config test" exit non-zero without printing a
// second error underneath the per-check report above.
var errConfigIncomplete = fmt.Errorf("one or more checks failed")

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// contextOrBackground keeps helpers usable from tests that build a bare
// command without calling Execute.
func contextOrBackground(cmd *cobra.Command) context.Context {
	if ctx := cmd.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}
