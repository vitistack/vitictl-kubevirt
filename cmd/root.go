// Package cmd wires the viti-kubevirt plugin's cobra command tree.
//
// The binary is named viti-kubevirt so that vitictl's plugin dispatcher
// exposes it as "viti kubevirt ..."; it also runs standalone as
// "viti-kubevirt ...". Dispatch is by binary name, so the "kv" shorthand is a
// viti-kv symlink alongside it rather than a cobra alias — see the Makefile.
package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/vitistack/vitictl/pkg/plugin/selfupgrade"
)

// version is set by main from the -ldflags-injected build version.
var version = "dev"

const rootLong = `🖥️  viti-kubevirt adds KubeVirt commands to the viti CLI.

It lists Vitistack Machines and enriches each one with the state of the
KubeVirt VirtualMachine and VirtualMachineInstance backing it, then lets you
act on them: start, stop, restart, pause, unpause, reset, and open a VNC
console.

Two clusters are involved. Machines live in the Vitistack availability zones
that viti itself is configured with, and are read from there. The virtual
machines live in a KubeVirt cluster, configured separately with
"viti kubevirt config add" (~/.vitistack/kubevirt.config.yaml).

Installed as a viti plugin (a viti-kubevirt binary on PATH) it is invoked as
"viti kubevirt ..." — or "viti kv ..." via the viti-kv symlink.`

// NewRootCmd builds a fresh command tree. Tests construct their own instance
// so flag state is never shared between runs.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "viti-kubevirt",
		Short:         "🖥️  KubeVirt commands for viti",
		Long:          rootLong,
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetVersionTemplate("viti-kubevirt version {{.Version}}\n")
	root.AddCommand(newVMCmd())
	// migrations and orphans read the same two layers as "vm", so they take
	// the same scope flags (--cluster, -n, -z). Each owns its own scope: the
	// flags are per-command, and sharing one instance across sibling commands
	// would let one invocation's parsed values leak into another's.
	root.AddCommand(withScope(newMigrationsCmd))
	root.AddCommand(withScope(newOrphansCmd))
	root.AddCommand(newConfigCmd())
	o := selfupgrade.Options{
		Name:    "kubevirt",
		Repo:    "vitistack/vitictl-kubevirt",
		Version: version,
	}
	root.AddCommand(selfupgrade.NewVersionCmd(o))
	root.AddCommand(selfupgrade.NewUpgradeCmd(o))
	return root
}

// SetVersion wires the build version in before the tree is constructed.
func SetVersion(v string) {
	if v != "" {
		version = v
	}
}

// Execute runs the plugin, printing errors in viti's style.
func Execute() error {
	root := NewRootCmd()
	if err := root.Execute(); err != nil {
		if errors.Is(err, errCancelled) {
			return err
		}
		_, _ = fmt.Fprintln(root.ErrOrStderr(), "❌ Error:", err)
		return err
	}
	return nil
}

// errCancelled unwinds a declined confirmation without printing an error.
var errCancelled = errors.New("cancelled")

// warn reports a non-fatal problem on stderr, so structured stdout stays
// pipeable even when one availability zone is unreachable.
func warn(cmd *cobra.Command, err error) {
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "⚠️  %v\n", err)
}

// withScope builds a top-level command that needs the shared availability-zone
// and cluster flags, registering them on the command the constructor returns.
func withScope(build func(*scope) *cobra.Command) *cobra.Command {
	s := &scope{}
	cmd := build(s)
	s.register(cmd)
	return cmd
}

// confirm asks for a yes/no answer on the command's stdin.
//
// Non-interactive stdin is refused rather than assumed either way, so a
// piped or CI invocation never performs a destructive action without having
// been told to. --yes is the documented way through, wherever a caller offers
// one.
func confirm(cmd *cobra.Command, prompt string) (bool, error) {
	// Ask the terminal directly rather than inferring from the file mode:
	// /dev/null is a character device but is nobody's terminal, so a
	// mode-based check waves a non-interactive invocation through and then
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
