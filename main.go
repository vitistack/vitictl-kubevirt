// viti-kubevirt adds KubeVirt commands to the viti CLI. vitictl discovers any
// viti-* binary on PATH as a subcommand, so this binary is reachable as
// "viti kubevirt ..." — and as "viti kv ..." through the viti-kv symlink the
// Makefile installs alongside it.
package main

import (
	"os"

	"github.com/vitistack/vitictl-kubevirt/cmd"
)

// Injected via -ldflags at build time; see the Makefile.
var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	cmd.SetVersion(version)
	_ = commit

	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
