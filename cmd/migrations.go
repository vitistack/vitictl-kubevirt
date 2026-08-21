package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/vitistack/vitictl-kubevirt/internal/config"
	"github.com/vitistack/vitictl-kubevirt/internal/kube"
	"github.com/vitistack/vitictl/pkg/plugin/output"
	"github.com/vitistack/vitictl-kubevirt/internal/vm"
)

// newMigrationsCmd lists VirtualMachineInstanceMigrations, the resource this
// plugin previously left to "kubectl get vmim -w | grep" during a rollout.
//
// It is a sibling of newVMListCmd rather than a vm subcommand: a migration
// belongs to a KubeVirt cluster, not to any one Machine, so it is listed
// fleet-wide the same way vm list is, but joined back to a Machine name
// rather than to a full Machine row.
func newMigrationsCmd(s *scope) *cobra.Command {
	var outputFlag string
	var all bool
	var watch bool
	var interval time.Duration

	cmd := &cobra.Command{
		Use:     "migrations",
		Aliases: []string{"mig"},
		Short:   "🚚 List VirtualMachineInstanceMigrations in flight",
		Long: `List VirtualMachineInstanceMigrations across the KubeVirt clusters backing
your availability zones, joined back to the Machine each one belongs to.

This replaces watching "kubectl get vmim -w" by hand against each KubeVirt
cluster during a rolling operation such as "vm changemachineclass" or a Talos
upgrade.

Only active migrations show by default — during a rollout you care about what
is in flight, and finished migrations accumulate fast enough to bury the ones
that matter. Pass --all to include Succeeded and Failed migrations too.`,
		Example: `  viti kubevirt migrations
  viti kubevirt mig --all
  viti kubevirt migrations --watch
  viti kubevirt migrations -o wide`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := output.Parse(outputFlag)
			if err != nil {
				return err
			}
			// json/yaml/name exist to be piped into something else, and a
			// repeating stream of documents on the same stdout is not a shape
			// any consumer of those formats expects — it would need framing
			// this command does not provide.
			if watch && (format == output.FormatJSON || format == output.FormatYAML || format == output.FormatName) {
				return fmt.Errorf("--watch prints a repeating table and cannot be combined with -o %s", outputFlag)
			}

			if !watch {
				migs, err := collectMigrations(contextOrBackground(cmd), cmd, s)
				if err != nil {
					return err
				}
				if !all {
					migs = activeMigrations(migs)
				}
				return renderMigrations(cmd, migs, format, all)
			}
			return watchMigrations(cmd, s, format, all, interval)
		},
	}
	cmd.Flags().StringVarP(&outputFlag, "output", "o", "",
		fmt.Sprintf("output format: %s", strings.Join(output.ValidFormats, ", ")))
	cmd.Flags().BoolVar(&all, "all", false,
		"include finished migrations (Succeeded/Failed) as well as active ones")
	cmd.Flags().BoolVar(&watch, "watch", false,
		"re-poll and reprint until interrupted (Ctrl-C)")
	cmd.Flags().DurationVar(&interval, "interval", 2*time.Second,
		"poll interval for --watch")
	return cmd
}

// watchMigrations re-polls and reprints until the process is interrupted.
//
// This polls on a plain ticker rather than opening the Kubernetes watch API
// against every KubeVirt cluster in play. A migration runs for minutes, this
// command already spans several clusters, and a real watch would mean
// managing a resourceVersion and a reconnect per cluster for a responsiveness
// gain nobody watching a table with their eyes would notice.
func watchMigrations(cmd *cobra.Command, s *scope, format output.Format, all bool, interval time.Duration) error {
	// Nothing upstream of this command wires SIGINT into cmd.Context(), so a
	// bare Ctrl-C would kill the process mid-write instead of ending the loop
	// cleanly. Trapping it here, only for the duration of --watch, is what
	// lets the loop exit on its own terms.
	ctx, stop := signal.NotifyContext(contextOrBackground(cmd), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		if ctx.Err() != nil {
			return nil
		}

		migs, err := collectMigrations(ctx, cmd, s)
		if ctx.Err() != nil {
			// Cancelled during the collect: printing what was gathered so far
			// would be a half-written frame, so the loop ends here instead.
			return nil
		}
		if err != nil {
			return err
		}
		if !all {
			migs = activeMigrations(migs)
		}

		printWatchHeader(cmd, interval)
		if err := renderMigrations(cmd, migs, format, all); err != nil {
			return err
		}

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// printWatchHeader separates one frame from the next with a timestamp, so a
// terminal scrolling past several polls stays readable without a TUI: the
// reader can tell which table belongs to which moment at a glance.
func printWatchHeader(cmd *cobra.Command, interval time.Duration) {
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "\n── %s (every %s, Ctrl-C to stop) ──\n",
		time.Now().Format("15:04:05"), interval)
}

// collectMigrations connects to the availability zones and their KubeVirt
// clusters and joins them, the migrations.go equivalent of collect() in
// cmd/vm.go. It takes ctx explicitly, rather than deriving it from cmd, so
// --watch can pass the same cancellable context on every poll instead of one
// that stops observing Ctrl-C after the first tick.
func collectMigrations(ctx context.Context, cmd *cobra.Command, s *scope) ([]vm.Migration, error) {
	resolver, err := s.resolver()
	if err != nil {
		return nil, err
	}
	zones, err := config.AvailabilityZones(s.az)
	if err != nil {
		return nil, err
	}
	clients, err := kube.ConnectVitistack(ctx, zones, func(e error) { warn(cmd, e) })
	if err != nil {
		return nil, err
	}
	return vm.CollectMigrations(ctx, clients, resolver, s.namespace, func(e error) { warn(cmd, e) })
}

// activeMigrations narrows a listing to migrations still in flight, which is
// the default view — see newMigrationsCmd's --all flag for why.
func activeMigrations(migs []vm.Migration) []vm.Migration {
	out := make([]vm.Migration, 0, len(migs))
	for _, m := range migs {
		if m.Active() {
			out = append(out, m)
		}
	}
	return out
}

func renderMigrations(cmd *cobra.Command, migs []vm.Migration, format output.Format, all bool) error {
	out := cmd.OutOrStdout()
	switch format {
	case output.FormatJSON:
		return output.WriteJSON(out, structuredMigrations(migs))
	case output.FormatYAML:
		return output.WriteYAML(out, structuredMigrations(migs))
	case output.FormatName:
		for _, m := range migs {
			_, _ = fmt.Fprintf(out, "virtualmachineinstancemigration/%s/%s\n", m.Namespace(), m.Name())
		}
		return nil
	}

	if len(migs) == 0 {
		msg := "🤷 no active migrations"
		if all {
			msg = "🤷 no migrations found"
		}
		_, err := fmt.Fprintln(out, msg)
		return err
	}

	wide := format == output.FormatWide
	header := "AZ\tCLUSTER\tNAMESPACE\tVMI\tMACHINE\tPHASE\tNODE\tAGE"
	if wide {
		header = "AZ\tCLUSTER\tNAMESPACE\tVMI\tMACHINE\tNAME\tPHASE\tMODE\tNODE\tAGE"
	}
	rows := make([]string, 0, len(migs))
	now := time.Now()
	for _, m := range migs {
		node := dash(migrationNode(m))
		if wide {
			rows = append(rows, strings.Join([]string{
				dash(m.AZ), dash(m.Cluster), dash(m.Namespace()), dash(m.VMIName()), dash(m.Machine),
				dash(m.Name()), dash(m.Phase()), dash(m.Mode()), node,
				age(m.VMIM.CreationTimestamp, now),
			}, "\t"))
			continue
		}
		rows = append(rows, strings.Join([]string{
			dash(m.AZ), dash(m.Cluster), dash(m.Namespace()), dash(m.VMIName()), dash(m.Machine),
			dash(m.Phase()), node, age(m.VMIM.CreationTimestamp, now),
		}, "\t"))
	}
	return output.WriteTable(out, header, rows)
}

// migrationNode renders the source and target as one column: what a rollout
// operator wants to know is where the guest is moving from and to, in one
// glance, not two separate columns to line up by eye.
func migrationNode(m vm.Migration) string {
	src, dst := m.SourceNode(), m.TargetNode()
	if src == "" && dst == "" {
		return ""
	}
	return dash(src) + "→" + dash(dst)
}

// migrationJSON is the machine-readable shape: the join fields alongside the
// migration object unflattened, so a consumer can reach anything KubeVirt
// carries on it without this package needing an accessor for every field.
type migrationJSON struct {
	AvailabilityZone string `json:"availabilityZone"`
	KubeVirtCluster  string `json:"kubevirtCluster,omitempty"`
	// Machine is omitted rather than emitted empty, so a consumer can treat
	// its absence as "unattributable" without special-casing an empty string.
	Machine   string `json:"machine,omitempty"`
	Migration any    `json:"migration"`
}

func structuredMigrations(migs []vm.Migration) []migrationJSON {
	out := make([]migrationJSON, 0, len(migs))
	for _, m := range migs {
		out = append(out, migrationJSON{
			AvailabilityZone: m.AZ,
			KubeVirtCluster:  m.Cluster,
			Machine:          m.Machine,
			Migration:        m.VMIM,
		})
	}
	return out
}
