// Package viticli shells out to the parent viti CLI.
//
// Opening a Talos node's dashboard needs more than the VM: the machine's
// owning cluster, that cluster's credentials secret, the cert-valid
// control-plane endpoints, and the one node address that appears in the
// certificate's SANs — picking the wrong address yields "x509: certificate is
// valid for X, not Y". viti already resolves all of it in "viti machine
// console", including the KubeVirt-aware filtering that discards the CNI and
// overlay addresses the guest agent reports alongside the real NIC.
//
// Copying that here would duplicate several hundred lines of subtle logic
// across two repositories, to drift apart the first time either changes. This
// plugin drives the command that owns it instead, the same way it drives
// virtctl rather than reimplementing KubeVirt's subresource API.
//
// The exec plumbing and failure classification (cancelled context, killed
// child, ordinary non-zero exit) live in the kit's
// github.com/vitistack/vitictl/pkg/plugin/viticli package, which every
// plugin shares; this package keeps only the domain layer (Target, Args) and
// the dashboard-specific bits the kit cannot know about.
package viticli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"

	kitcli "github.com/vitistack/vitictl/pkg/plugin/viticli"
)

// Binary is the executable this package's own Path preflight looks up. A
// variable so tests can point it at a stub instead of the real thing. The
// actual child process is resolved and run by the kit through its own
// Binary variable (kitcli.Binary).
var Binary = "viti"

// ErrNotInstalled is returned when viti is not on PATH.
var ErrNotInstalled = errors.New("the viti CLI was not found on PATH")

// Target identifies the machine to open a dashboard for.
type Target struct {
	// Name is the Machine name, which is what viti resolves against.
	Name             string
	Namespace        string
	AvailabilityZone string
}

// Streams are the caller's I/O. The dashboard is a full-screen terminal UI, so
// these are wired straight through rather than captured.
type Streams struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer
}

// Path resolves the viti binary, purely as a preflight check that gives the
// Talos-dashboard-specific hint the kit's own generic Path error doesn't
// know.
//
// The plugin is normally reached as "viti kubevirt ...", so viti is already
// there; a standalone viti-kubevirt on a machine without it is the case worth
// naming, hence the actionable error.
func Path() (string, error) {
	p, err := exec.LookPath(Binary)
	if err != nil {
		return "", fmt.Errorf(
			"%w — the Talos dashboard is opened through it. Install viti, "+
				"or run 'talosctl dashboard' yourself against the node", ErrNotInstalled)
	}
	return p, nil
}

// MachineConsole runs `viti machine console <name>` attached to the caller's
// terminal, replacing this process's foreground for the dashboard's lifetime.
// A normal failure is wrapped loudly with the machine's name — the dashboard
// is interactive, and a silent non-zero exit would read as it simply closing.
func MachineConsole(ctx context.Context, s Streams, t Target) error {
	if _, err := Path(); err != nil {
		return err
	}
	return kitcli.Run(ctx,
		kitcli.Streams{In: s.In, Out: s.Out, Err: s.Err},
		Args(t),
		func(_ context.Context, _ string, childErr error) error {
			return fmt.Errorf("viti machine console %s: %w", t.Name, childErr)
		})
}

// Args builds the viti argument list. Split out from MachineConsole so tests
// can assert on it without executing anything.
func Args(t Target) []string {
	args := []string{"machine", "console", t.Name}
	if t.Namespace != "" {
		args = append(args, "--namespace", t.Namespace)
	}
	// Passing the zone through saves viti re-searching every configured zone
	// for a machine this plugin has already located.
	if t.AvailabilityZone != "" {
		args = append(args, "--availabilityzone", t.AvailabilityZone)
	}
	return args
}
