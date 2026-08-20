package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/vitistack/vitictl-kubevirt/internal/config"
	"github.com/vitistack/vitictl-kubevirt/internal/kube"
	"github.com/vitistack/vitictl-kubevirt/internal/output"
	"github.com/vitistack/vitictl-kubevirt/internal/vm"
)

// newOrphansCmd is a sibling of newVMListCmd: same scope, same -o handling,
// but anchored on the KubeVirt side rather than the Machine side, so it can
// see what "vm list" cannot by construction — a VM with no Machine at all.
//
// It is not registered here; the controller wires it into the command tree.
func newOrphansCmd(s *scope) *cobra.Command {
	var outputFlag string
	var minAge time.Duration

	cmd := &cobra.Command{
		Use:   "orphans",
		Short: "🧹 Find VMs, VMIs and Machines with no counterpart on the other layer",
		Long: `Finds where the KubeVirt clusters and the Vitistack control plane have
drifted apart.

Three kinds are reported:
  vm-without-machine  a VirtualMachine naming a Machine that does not exist —
                       the dangerous one: real CPU, memory and storage held
                       by something nobody is tracking, typically left behind
                       by a failed or partial cluster teardown. VMs with no
                       vitistack ownership label are never reported; they are
                       none of this tool's business.
  machine-without-vm   a Machine whose KubeVirt cluster answered but has no
                       VirtualMachine for it.
  vmi-without-vm       a running instance with no VirtualMachine object
                       behind it.

A finding here is a candidate for investigation, not a verdict: some are
legitimate, such as a VM deliberately kept running while its Machine is being
recreated. --min-age (default 15m) excludes anything younger, since a Machine
and its VM are reconciled by separate controllers a few seconds apart and a
sweep run mid-provisioning would otherwise flag that as drift.

This command is strictly read-only: it never deletes or changes anything, and
offers no flag that does.

Exits non-zero when any availability zone or KubeVirt cluster could not be
audited, so automation cannot mistake a partial sweep for a clean fleet — a
finding by itself never fails the command.`,
		Example: `  viti kubevirt vm orphans
  viti kubevirt vm orphans --min-age 1h
  viti kubevirt vm orphans -o json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := output.Parse(outputFlag)
			if err != nil {
				return err
			}
			resolver, err := s.resolver()
			if err != nil {
				return err
			}
			zones, err := config.AvailabilityZones(s.az)
			if err != nil {
				return err
			}
			ctx := contextOrBackground(cmd)
			clients, err := kube.ConnectVitistack(ctx, zones, func(e error) { warn(cmd, e) })
			if err != nil {
				return err
			}

			// The user's own locally-configured KubeVirt clusters are
			// unioned in alongside whatever zone discovery finds: a cluster
			// whose last Machine has already been torn down is invisible to
			// discovery, but may still be sitting right here in
			// kubevirt.config.yaml. A missing config file is not an error —
			// config.Load() already treats that as "no clusters configured".
			//
			// --cluster skips the union: the user has already named the one
			// cluster to look at, and every zone group is forced onto it
			// through resolver.Override, so sweeping every other locally
			// configured cluster too would silently widen a scope they
			// deliberately narrowed.
			var localClusters []config.Cluster
			if s.cluster == "" {
				localCfg, err := config.Load()
				if err != nil {
					return err
				}
				localClusters = localCfg.Clusters
			}

			report, err := vm.DetectOrphans(ctx, clients, resolver, localClusters, kube.ConnectKubeVirt,
				s.namespace, minAge, time.Now(), func(e error) { warn(cmd, e) })
			if err != nil {
				return err
			}
			// ConnectVitistack already dropped every zone it could not reach,
			// so DetectOrphans never sees them and cannot count them as
			// configured. Restoring the true configured count here is what
			// stops "2 of 2 zones audited" from reading as complete when a
			// third zone never even got past connecting.
			report.Coverage.ZonesConfigured = len(zones)

			return renderOrphans(cmd, report, format)
		},
	}
	cmd.Flags().StringVarP(&outputFlag, "output", "o", "",
		fmt.Sprintf("output format: %s", strings.Join(output.ValidFormats, ", ")))
	cmd.Flags().DurationVar(&minAge, "min-age", 15*time.Minute,
		"ignore VMs, VMIs and Machines younger than this, to avoid flagging routine provisioning as drift")
	return cmd
}

// orphanRecord is the -o json/yaml shape: the findings plus the coverage
// counts and the suppressed total, because past a pipe the stderr warnings
// and the exit code are both gone — a partial audit must stay detectable in
// the payload itself, not only in signals a pipe throws away.
type orphanRecord struct {
	Orphans    []orphanJSON `json:"orphans"`
	Coverage   coverageJSON `json:"coverage"`
	Suppressed int          `json:"suppressedByMinAge"`
}

type coverageJSON struct {
	ZonesConfigured    int  `json:"zonesConfigured"`
	ZonesChecked       int  `json:"zonesChecked"`
	ClustersConfigured int  `json:"clustersConfigured"`
	ClustersChecked    int  `json:"clustersChecked"`
	Complete           bool `json:"complete"`
}

type orphanJSON struct {
	Kind      string `json:"kind"`
	AZ        string `json:"availabilityZone"`
	Cluster   string `json:"cluster,omitempty"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Detail    string `json:"detail"`
	Age       string `json:"age,omitempty"`
}

func renderOrphans(cmd *cobra.Command, report vm.OrphanReport, format output.Format) error {
	out := cmd.OutOrStdout()
	now := time.Now()

	if format.IsStructured() {
		rec := orphanRecord{
			Orphans: make([]orphanJSON, 0, len(report.Orphans)),
			Coverage: coverageJSON{
				ZonesConfigured:    report.Coverage.ZonesConfigured,
				ZonesChecked:       report.Coverage.ZonesChecked,
				ClustersConfigured: report.Coverage.ClustersConfigured,
				ClustersChecked:    report.Coverage.ClustersChecked,
				Complete:           report.Coverage.Complete(),
			},
			Suppressed: report.Suppressed,
		}
		for _, o := range report.Orphans {
			rec.Orphans = append(rec.Orphans, orphanJSON{
				Kind: string(o.Kind), AZ: o.AZ, Cluster: o.Cluster,
				Namespace: o.Namespace, Name: o.Name, Detail: o.Detail,
				Age: age(o.CreatedAt, now),
			})
		}
		if format == output.FormatJSON {
			if err := output.WriteJSON(out, rec); err != nil {
				return err
			}
		} else if err := output.WriteYAML(out, rec); err != nil {
			return err
		}
		return coverageErr(report)
	}

	if format == output.FormatName {
		for _, o := range report.Orphans {
			_, _ = fmt.Fprintf(out, "%s/%s/%s\n", o.Kind, o.Namespace, o.Name)
		}
		printCoverageSummary(cmd, report)
		return coverageErr(report)
	}

	if len(report.Orphans) == 0 {
		_, _ = fmt.Fprintln(out, "🧹 no orphans found")
	} else {
		rows := make([]string, 0, len(report.Orphans))
		for _, o := range report.Orphans {
			rows = append(rows, strings.Join([]string{
				string(o.Kind), dash(o.AZ), dash(o.Cluster), dash(o.Namespace), dash(o.Name),
				dash(o.Detail), age(o.CreatedAt, now),
			}, "\t"))
		}
		if err := output.WriteTable(out, "KIND\tAZ\tCLUSTER\tNAMESPACE\tNAME\tDETAIL\tAGE", rows); err != nil {
			return err
		}
	}

	printCoverageSummary(cmd, report)
	return coverageErr(report)
}

// printCoverageSummary reports the audit's scope on stderr, so a partial
// sweep is visible even in the plain-text formats where nothing about
// coverage otherwise appears on stdout. It is silent about --min-age only
// when nothing was ever suppressed by it, since a zero worth mentioning is
// still a fact and a nonzero one must never go unmentioned.
func printCoverageSummary(cmd *cobra.Command, report vm.OrphanReport) {
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "🔎 %d/%d availability zone(s), %d/%d kubevirt cluster(s) audited",
		report.Coverage.ZonesChecked, report.Coverage.ZonesConfigured,
		report.Coverage.ClustersChecked, report.Coverage.ClustersConfigured)
	if report.Suppressed > 0 {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), ", %d suppressed by --min-age", report.Suppressed)
	}
	_, _ = fmt.Fprintln(cmd.ErrOrStderr())
}

// coverageErr turns incomplete coverage into the command's exit code.
// Findings themselves never fail the command — they are candidates for a
// human to look at, and automation must not treat a candidate as a crash —
// but coverage that fell short of every configured zone and cluster must,
// since only that distinguishes a clean fleet from an unaudited one.
func coverageErr(report vm.OrphanReport) error {
	if report.Coverage.Complete() {
		return nil
	}
	return fmt.Errorf(
		"incomplete audit: %d/%d availability zone(s) and %d/%d kubevirt cluster(s) checked — "+
			"orphans there are NOT ruled out",
		report.Coverage.ZonesChecked, report.Coverage.ZonesConfigured,
		report.Coverage.ClustersChecked, report.Coverage.ClustersConfigured)
}
