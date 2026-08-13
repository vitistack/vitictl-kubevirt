package cmd

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vitistack/vitictl-kubevirt/internal/config"
)

// run executes the plugin's command tree against in-memory streams.
func run(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errBuf bytes.Buffer
	root := NewRootCmd()
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs(args)
	err = root.Execute()
	return out.String(), errBuf.String(), err
}

// isolate keeps a developer's real ~/.vitistack out of the tests.
//
// Both configs must be pinned, not just this plugin's. Listings resolve their
// KubeVirt clusters from the availability zones in vitictl's ctl.config.yaml,
// so leaving VITI_CONFIG unset lets a test fall back to ~/.vitistack and open
// connections to whatever live fleet the developer happens to be running.
func isolate(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(config.EnvConfigPath, filepath.Join(dir, "kubevirt.config.yaml"))
	t.Setenv(config.EnvVitiConfig, filepath.Join(dir, "ctl.config.yaml"))
	t.Setenv(config.EnvAvailabilityZone, "")
	t.Setenv(config.EnvKubeconfig, "")
	t.Setenv(config.EnvContext, "")
	t.Setenv(config.EnvNamespace, "")
}

func TestVersionSubcommandMatchesTheVersionFlag(t *testing.T) {
	sub, _, err := run(t, "version")
	if err != nil {
		t.Fatalf("run(version) error = %v", err)
	}
	flag, _, err := run(t, "--version")
	if err != nil {
		t.Fatalf("run(--version) error = %v", err)
	}
	if sub != flag {
		t.Errorf("version subcommand = %q, --version = %q; they must agree", sub, flag)
	}
	if !strings.HasPrefix(sub, "viti-kubevirt version ") {
		t.Errorf("version output = %q", sub)
	}
}

// Every capability the plugin exists for must be reachable from the tree.
func TestCommandTree(t *testing.T) {
	out, _, err := run(t, "--help")
	if err != nil {
		t.Fatalf("run(--help) error = %v", err)
	}
	for _, want := range []string{"vm", "config", "version", "upgrade"} {
		if !strings.Contains(out, want) {
			t.Errorf("--help does not list %q:\n%s", want, out)
		}
	}

	vmHelp, _, err := run(t, "vm", "--help")
	if err != nil {
		t.Fatalf("run(vm --help) error = %v", err)
	}
	for _, want := range []string{"list", "get", "start", "stop", "restart", "pause", "unpause", "reset", "vnc", "console"} {
		if !strings.Contains(vmHelp, want) {
			t.Errorf("vm --help does not list %q:\n%s", want, vmHelp)
		}
	}
}

// "vm" is what the command is called, but people say machine too.
func TestVMAliases(t *testing.T) {
	for _, alias := range []string{"vms", "machine", "machines", "m"} {
		if _, _, err := run(t, alias, "--help"); err != nil {
			t.Errorf("run(%s --help) error = %v", alias, err)
		}
	}
}

// With nothing configured the user must be told how to configure it, rather
// than getting a connection error against some accidental cluster.
func TestActionsWithoutAClusterExplainHowToAddOne(t *testing.T) {
	isolate(t)

	for _, args := range [][]string{
		{"vm", "list"},
		{"vm", "start", "some-vm"},
	} {
		_, _, err := run(t, args...)
		if err == nil {
			t.Fatalf("run(%v) = nil error, want one", args)
		}
		if !strings.Contains(err.Error(), "config add") {
			t.Errorf("run(%v) error %q should say how to add a cluster", args, err)
		}
	}
}

// The picker takes over the terminal, so a piped or CI invocation must be told
// to name its machine rather than hang on a UI that cannot be drawn. This is
// checked before any cluster is contacted, which is why it reports the missing
// argument rather than the missing config.
func TestNoMachineWithoutATerminalIsRefused(t *testing.T) {
	isolate(t)

	for _, args := range [][]string{
		{"vm", "start"},
		{"vm", "reboot"},
		{"vm", "vnc"},
		{"vm", "get"},
	} {
		_, _, err := run(t, args...)
		if err == nil {
			t.Fatalf("run(%v) = nil error, want one", args)
		}
		if !strings.Contains(err.Error(), "run in a terminal") {
			t.Errorf("run(%v) error %q should explain the picker needs a terminal", args, err)
		}
	}
}

// reboot is what people say; restart is what KubeVirt calls it.
func TestRebootIsAnAliasForRestart(t *testing.T) {
	out, _, err := run(t, "vm", "reboot", "--help")
	if err != nil {
		t.Fatalf("run(vm reboot --help) error = %v", err)
	}
	if !strings.Contains(out, "Restart a virtual machine") {
		t.Errorf("vm reboot --help does not describe restart:\n%s", out)
	}
}

// Leaving the machine out must be accepted by the parser, so the picker gets a
// chance to run; only the absent terminal stops it above.
func TestSingleMachineCommandsAcceptNoArgument(t *testing.T) {
	for _, verb := range []string{
		"get", "start", "stop", "restart", "reboot", "pause", "unpause",
		"reset", "soft-reboot", "vnc", "console",
	} {
		_, _, err := run(t, "vm", verb, "--help")
		if err != nil {
			t.Errorf("run(vm %s --help) error = %v", verb, err)
		}
	}
}

func TestConfigAddListRemove(t *testing.T) {
	isolate(t)

	if _, _, err := run(t, "config", "add", "kv-01", "--kubeconfig", "/k/one"); err != nil {
		t.Fatalf("config add error = %v", err)
	}
	out, _, err := run(t, "config", "list")
	if err != nil {
		t.Fatalf("config list error = %v", err)
	}
	if !strings.Contains(out, "kv-01") || !strings.Contains(out, "/k/one") {
		t.Errorf("config list = %q, want the added cluster", out)
	}
	// The first cluster added becomes the default, so --cluster is optional
	// for anyone with a single cluster.
	if !strings.Contains(out, "*") {
		t.Errorf("config list = %q, want the first cluster marked default", out)
	}

	if _, _, err := run(t, "config", "remove", "kv-01"); err != nil {
		t.Fatalf("config remove error = %v", err)
	}
	out, _, err = run(t, "config", "list")
	if err != nil {
		t.Fatalf("config list error = %v", err)
	}
	if strings.Contains(out, "kv-01") {
		t.Errorf("config list = %q, want the cluster gone", out)
	}
}

func TestConfigPathShowsBothFiles(t *testing.T) {
	isolate(t)

	out, _, err := run(t, "config", "path")
	if err != nil {
		t.Fatalf("config path error = %v", err)
	}
	for _, want := range []string{"kubevirt.config.yaml", "ctl.config.yaml"} {
		if !strings.Contains(out, want) {
			t.Errorf("config path = %q, want it to mention %q", out, want)
		}
	}
}

// Destructive verbs must confirm; the prompt is refused without a terminal.
func TestDestructiveActionsRequireConfirmation(t *testing.T) {
	for _, a := range vmActions() {
		if !a.destructive {
			continue
		}
		cmd := NewRootCmd()
		found, _, err := cmd.Find([]string{"vm", a.verb})
		if err != nil {
			t.Fatalf("vm %s not registered: %v", a.verb, err)
		}
		if found.Flags().Lookup("yes") == nil {
			t.Errorf("vm %s should offer --yes to skip confirmation", a.verb)
		}
	}
}

// Starting or unpausing cannot lose data, so they must not nag.
func TestSafeActionsAreNotGatedOnConfirmation(t *testing.T) {
	safe := map[string]bool{"start": true, "unpause": true}
	for _, a := range vmActions() {
		if safe[a.verb] && a.destructive {
			t.Errorf("vm %s should not require confirmation", a.verb)
		}
	}
}
