package guest

import (
	"context"
	"io"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubectl/pkg/drain"
)

// Client is the workload cluster as a rollout drives it. It satisfies
// roll.Guest. Cordon and drain go through k8s.io/kubectl's drain package —
// the exact code `kubectl drain` runs — rather than a hand-rolled eviction
// loop, because PDB retry semantics, DaemonSet filtering and the timeout
// behavior are all subtle and already solved there.
type Client struct {
	Clientset kubernetes.Interface
	// DrainTimeout bounds one node's eviction budget.
	DrainTimeout time.Duration
	// ErrOut receives the drain package's own diagnostics — the "cannot
	// delete ..." pod lists a stuck drain prints.
	ErrOut io.Writer
}

func (c *Client) Node(ctx context.Context, name string) (*corev1.Node, error) {
	return c.Clientset.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
}

func (c *Client) Cordon(ctx context.Context, name string, desired bool) error {
	node, err := c.Node(ctx, name)
	if err != nil {
		return err
	}
	return drain.RunCordonOrUncordon(c.helper(ctx), node, desired)
}

func (c *Client) Drain(ctx context.Context, name string) error {
	return drain.RunNodeDrain(c.helper(ctx), name)
}

func (c *Client) helper(ctx context.Context) *drain.Helper {
	errOut := c.ErrOut
	if errOut == nil {
		errOut = io.Discard
	}
	return &drain.Helper{
		Ctx:    ctx,
		Client: c.Clientset,
		// DaemonSet pods restart with the node and cannot be evicted anyway;
		// emptyDir data is by definition node-local scratch. Force stays off:
		// an unmanaged pod aborts the rollout visibly instead of dying silently.
		IgnoreAllDaemonSets: true,
		DeleteEmptyDirData:  true,
		Force:               false,
		Timeout:             c.DrainTimeout,
		Out:                 io.Discard,
		ErrOut:              errOut,
	}
}
