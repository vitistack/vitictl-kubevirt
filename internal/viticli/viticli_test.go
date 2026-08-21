package viticli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	kitcli "github.com/vitistack/vitictl/pkg/plugin/viticli"
)

func TestArgsCarriesTheScopeThroughToViti(t *testing.T) {
	got := Args(Target{Name: "wrk0", Namespace: "t-test004", AvailabilityZone: "test-south-az1"})
	want := []string{
		"machine", "console", "wrk0",
		"--namespace", "t-test004",
		"--availabilityzone", "test-south-az1",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("Args() = %v, want %v", got, want)
	}
}

// An unset namespace or zone must be omitted rather than passed empty, which
// viti would read as "the namespace named empty string" and find nothing.
func TestArgsOmitsEmptyScope(t *testing.T) {
	got := Args(Target{Name: "wrk0"})
	if strings.Join(got, " ") != "machine console wrk0" {
		t.Errorf("Args() = %v, want just the subcommand and name", got)
	}
	for _, unwanted := range []string{"--namespace", "--availabilityzone"} {
		for _, a := range got {
			if a == unwanted {
				t.Errorf("Args() passed %q with no value set", unwanted)
			}
		}
	}
}

// The plugin is normally reached through viti, so a missing binary is the
// standalone case — it must say what to do rather than fail obscurely.
func TestPathReportsAMissingViti(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	old := Binary
	Binary = "viti-does-not-exist"
	defer func() { Binary = old }()

	_, err := Path()
	if err == nil {
		t.Fatal("expected an error when viti is absent")
	}
	if !errors.Is(err, ErrNotInstalled) {
		t.Errorf("error %v should wrap ErrNotInstalled", err)
	}
	if !strings.Contains(err.Error(), "talosctl dashboard") {
		t.Errorf("error %q should name the manual fallback", err)
	}
}

// MachineConsole must hand the caller's streams to the child unchanged: the
// dashboard is a full-screen UI and would be unusable through a pipe.
func TestMachineConsoleRunsTheBinaryWithTheRightArgs(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "viti")
	script := "#!/bin/sh\necho \"$@\"\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil { // #nosec G306 -- test stub must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	oldBinary, oldKitBinary := Binary, kitcli.Binary
	Binary, kitcli.Binary = stub, stub
	defer func() { Binary, kitcli.Binary = oldBinary, oldKitBinary }()

	var out strings.Builder
	err := MachineConsole(context.Background(),
		Streams{In: strings.NewReader(""), Out: &out, Err: &out},
		Target{Name: "wrk0", Namespace: "ns", AvailabilityZone: "az1"})
	if err != nil {
		t.Fatalf("MachineConsole() error = %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "machine console wrk0 --namespace ns --availabilityzone az1" {
		t.Errorf("viti was invoked as %q", got)
	}
}

// A dashboard that exits non-zero must surface, naming the machine.
func TestMachineConsoleReportsAFailure(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "viti")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 3\n"), 0o700); err != nil { // #nosec G306 -- test stub must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	oldBinary, oldKitBinary := Binary, kitcli.Binary
	Binary, kitcli.Binary = stub, stub
	defer func() { Binary, kitcli.Binary = oldBinary, oldKitBinary }()

	err := MachineConsole(context.Background(), Streams{In: strings.NewReader("")}, Target{Name: "wrk0"})
	if err == nil {
		t.Fatal("expected an error when viti exits non-zero")
	}
	if !strings.Contains(err.Error(), "wrk0") {
		t.Errorf("error %q should name the machine", err)
	}
}
