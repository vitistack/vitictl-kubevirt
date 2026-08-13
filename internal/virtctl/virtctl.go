// Package virtctl shells out to the virtctl binary.
//
// The VM lifecycle verbs and the VNC console are KubeVirt *subresource*
// endpoints, not ordinary object writes: they cannot be expressed through a
// controller-runtime client, and reimplementing them would mean speaking
// KubeVirt's subresource API and, for VNC, proxying a websocket to a local
// viewer. virtctl already does exactly that and ships with KubeVirt, so this
// plugin drives it rather than duplicating it.
package virtctl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// Binary is the executable to invoke. A variable so tests can point it at a
// stub instead of the real thing.
var Binary = "virtctl"

// Target identifies the VM to act on, and how to reach its cluster.
type Target struct {
	Kubeconfig string
	Context    string
	Namespace  string
	// Name is the KubeVirt VM name, which is not always the Machine name.
	Name string
}

// Streams are the caller's I/O. VNC and console are interactive, so these are
// wired straight through to the terminal rather than captured.
type Streams struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer
}

// ErrNotInstalled is returned when virtctl is not on PATH.
var ErrNotInstalled = errors.New("virtctl was not found on PATH")

// Path resolves the virtctl binary, with an actionable error when it is
// absent — this plugin is useless without it, so say so plainly.
func Path() (string, error) {
	p, err := exec.LookPath(Binary)
	if err != nil {
		return "", fmt.Errorf(
			"%w — install it from https://kubevirt.io/user-guide/operations/virtctl_client_tool/ "+
				"(brew install virtctl, or download the release matching your cluster)", ErrNotInstalled)
	}
	return p, nil
}

// Run invokes `virtctl <verb> <name>` against the target's cluster.
//
// Cluster selection is passed explicitly rather than through the ambient
// KUBECONFIG, so acting on a VM can never land on whatever cluster the user's
// shell happened to be pointing at.
func Run(ctx context.Context, s Streams, verb string, t Target, extra ...string) error {
	bin, err := Path()
	if err != nil {
		return err
	}
	args := Args(verb, t, extra...)

	// #nosec G204 -- bin is resolved from PATH; args are a fixed verb plus
	// values that came from the cluster's own API, never a shell string.
	c := exec.CommandContext(ctx, bin, args...)
	c.Stdin = s.In
	c.Stdout = s.Out
	c.Stderr = s.Err
	if c.Stdin == nil {
		c.Stdin = os.Stdin
	}
	if err := c.Run(); err != nil {
		return fmt.Errorf("virtctl %s %s: %w", verb, t.Name, err)
	}
	return nil
}

// Args builds the virtctl argument list. Split out from Run so tests can
// assert on it without executing anything.
func Args(verb string, t Target, extra ...string) []string {
	args := []string{verb, t.Name}
	args = append(args, extra...)
	if t.Namespace != "" {
		args = append(args, "--namespace", t.Namespace)
	}
	if t.Kubeconfig != "" {
		args = append(args, "--kubeconfig", t.Kubeconfig)
	}
	if t.Context != "" {
		args = append(args, "--context", t.Context)
	}
	return args
}
