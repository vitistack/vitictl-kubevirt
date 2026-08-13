package cmd

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/vitistack/vitictl-kubevirt/internal/release"
)

func newUpgradeCmd() *cobra.Command {
	var (
		run    bool
		assume bool
	)

	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "⬆️  Check for a newer viti-kubevirt release and upgrade",
		Long: `Checks GitHub for the latest released version of the viti-kubevirt plugin
and, if a newer release is available, prints the command that upgrades it.

viti-kubevirt ships no installer of its own. It is a viti plugin, so upgrades go
through "viti plugin upgrade kubevirt", which downloads the release, verifies its
SHA-256 checksum and (when cosign is installed) its Sigstore signature, and
replaces the binary atomically. Pass --run to have this command invoke that
for you.

vitistack/vitictl-kubevirt is a private repository, so both the check and the
upgrade need a GitHub token: set GH_TOKEN (or GITHUB_TOKEN), or run
"gh auth login".`,
		Example: `  viti kubevirt upgrade
  viti kubevirt upgrade --run
  viti kubevirt upgrade --run --yes`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			latest, err := release.FetchLatest(cmd.Context(), release.Repo)
			if err != nil {
				return fmt.Errorf("could not check for updates: %w", err)
			}
			_, _ = fmt.Fprintf(out, "installed: %s\n", version)
			_, _ = fmt.Fprintf(out, "latest:    %s\n", latest.Tag)

			switch release.Compare(version, latest.Tag) {
			case release.StatusUpToDate:
				_, _ = fmt.Fprintln(out, "✅ already on the latest release — nothing to do")
				return nil
			case release.StatusAhead:
				_, _ = fmt.Fprintln(out, "🧪 local build is ahead of the latest release — nothing to do")
				return nil
			case release.StatusDevelopment:
				_, _ = fmt.Fprintln(out, "🛠  development build — switch to the latest release with:")
			case release.StatusOutdated:
				_, _ = fmt.Fprintln(out, "🆕 a newer release is available")
			}

			hint := release.UpgradeHint()
			_, _ = fmt.Fprintf(out, "   release notes: %s\n", latest.URL)
			_, _ = fmt.Fprintf(out, "   upgrade with:  %s\n", hint)

			if !run {
				return nil
			}
			if runtime.GOOS == "windows" {
				// The upgrade replaces this very binary. Unix keeps our
				// running image alive when the file is renamed over; Windows
				// refuses to replace a running .exe, so the upgrade has to be
				// started from a viti process rather than from inside us.
				return fmt.Errorf("--run is not supported on Windows; run %q yourself", hint)
			}
			if !assume {
				ok, err := confirm(cmd, fmt.Sprintf("Run %q to upgrade to %s?", hint, latest.Tag))
				if err != nil {
					return err
				}
				if !ok {
					_, _ = fmt.Fprintln(out, "aborted")
					return nil
				}
			}
			return runPluginUpgrade(cmd)
		},
	}
	cmd.Flags().BoolVar(&run, "run", false,
		"run `viti plugin upgrade kubevirt` after printing instructions")
	cmd.Flags().BoolVarP(&assume, "yes", "y", false,
		"skip the confirmation prompt when used with --run")
	return cmd
}

// confirm asks for a yes/no answer on the command's stdin.
//
// Non-interactive stdin is refused rather than assumed either way, so a
// piped or CI invocation never replaces a binary without having been told
// to. --yes is the documented way through.
func confirm(cmd *cobra.Command, prompt string) (bool, error) {
	// Ask the terminal directly rather than inferring from the file mode:
	// /dev/null is a character device but is nobody's terminal, so a
	// mode-based check waves `upgrade --run < /dev/null` through and then
	// fails on the read with a bare "EOF".
	if in, ok := cmd.InOrStdin().(*os.File); ok && !term.IsTerminal(int(in.Fd())) {
		return false, fmt.Errorf("stdin is not a terminal; re-run with --yes to confirm non-interactively")
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s [y/N]: ", prompt)

	// A final line without a trailing newline still counts as an answer.
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && line == "" {
		return false, err
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

// runPluginUpgrade hands the upgrade to viti's plugin manager, which owns
// downloading, verifying and replacing plugin binaries.
//
// Nothing goes through a shell: unlike vitictl's own `upgrade --run`, which
// has to pipe curl into bash, there is no pipe here, so exec is used
// directly and there is no quoting or injection surface at all.
func runPluginUpgrade(cmd *cobra.Command) error {
	viti, err := exec.LookPath("viti")
	if err != nil {
		return fmt.Errorf("viti was not found on PATH; run %q from a shell where viti is installed",
			release.UpgradeHint())
	}
	// #nosec G204 -- viti is resolved from PATH and invoked with fixed arguments.
	c := exec.CommandContext(cmd.Context(), viti, "plugin", "upgrade", release.PluginName)
	c.Stdout = cmd.OutOrStdout()
	c.Stderr = cmd.ErrOrStderr()
	c.Stdin = cmd.InOrStdin()
	return c.Run()
}
