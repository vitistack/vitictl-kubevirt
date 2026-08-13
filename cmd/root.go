// Package cmd wires the viti-kubevirt plugin's cobra command tree.
//
// The binary is named viti-kubevirt so that vitictl's plugin dispatcher
// exposes it as "viti kubevirt ..."; it also runs standalone as
// "viti-kubevirt ...". Dispatch is by binary name, so the "kv" shorthand is a
// viti-kv symlink alongside it rather than a cobra alias — see the Makefile.
package cmd

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
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
	root.AddCommand(newConfigCmd())
	root.AddCommand(newVersionCmd())
	root.AddCommand(newUpgradeCmd())
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
