package virtctl

import (
	"strings"
	"testing"
)

// The cluster must be selected explicitly on every invocation: inheriting the
// ambient KUBECONFIG would let "stop" land on whatever cluster the user's
// shell happened to point at.
func TestArgsAlwaysCarriesTheClusterSelection(t *testing.T) {
	got := Args("stop", Target{
		Kubeconfig: "/k/config",
		Context:    "ctx",
		Namespace:  "vms",
		Name:       "vm-1",
	})
	want := []string{"stop", "vm-1", "--namespace", "vms", "--kubeconfig", "/k/config", "--context", "ctx"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("Args() = %v, want %v", got, want)
	}
}

// An unset kubeconfig or context must be omitted rather than passed empty,
// which virtctl would read as "use this empty path".
func TestArgsOmitsUnsetSelectors(t *testing.T) {
	got := Args("start", Target{Name: "vm-1"})
	if strings.Join(got, " ") != "start vm-1" {
		t.Errorf("Args() = %v, want just the verb and name", got)
	}
	for _, unwanted := range []string{"--kubeconfig", "--context", "--namespace"} {
		if strings.Contains(strings.Join(got, " "), unwanted) {
			t.Errorf("Args() should not carry an empty %s", unwanted)
		}
	}
}

func TestArgsAppendsExtrasBeforeTheSelectors(t *testing.T) {
	got := Args("stop", Target{Namespace: "vms", Name: "vm-1"}, "--force", "--grace-period=0")
	joined := strings.Join(got, " ")
	if !strings.HasPrefix(joined, "stop vm-1 --force --grace-period=0") {
		t.Errorf("Args() = %q, want the extras right after the name", joined)
	}
	if !strings.Contains(joined, "--namespace vms") {
		t.Errorf("Args() = %q, want the namespace still applied", joined)
	}
}

// virtctl is a hard requirement, so its absence must say how to fix it.
func TestPathWithoutVirtctlIsActionable(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	_, err := Path()
	if err == nil {
		t.Fatal("expected an error when virtctl is not on PATH")
	}
	for _, want := range []string{"virtctl", "kubevirt.io"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}
