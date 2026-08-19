package guest

import (
	"context"
	"io"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func testNode(name string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func TestClientNode(t *testing.T) {
	c := &Client{Clientset: k8sfake.NewClientset(testNode("n1")), ErrOut: io.Discard}
	got, err := c.Node(context.Background(), "n1")
	if err != nil || got.Name != "n1" {
		t.Fatalf("Node = %v, %v", got, err)
	}
	if _, err := c.Node(context.Background(), "missing"); err == nil {
		t.Fatal("want error for a missing node")
	}
}

func TestClientCordonAndUncordon(t *testing.T) {
	cs := k8sfake.NewClientset(testNode("n1"))
	c := &Client{Clientset: cs, ErrOut: io.Discard}

	if err := c.Cordon(context.Background(), "n1", true); err != nil {
		t.Fatal(err)
	}
	n, _ := cs.CoreV1().Nodes().Get(context.Background(), "n1", metav1.GetOptions{})
	if !n.Spec.Unschedulable {
		t.Error("node not cordoned")
	}

	if err := c.Cordon(context.Background(), "n1", false); err != nil {
		t.Fatal(err)
	}
	n, _ = cs.CoreV1().Nodes().Get(context.Background(), "n1", metav1.GetOptions{})
	if n.Spec.Unschedulable {
		t.Error("node not uncordoned")
	}
}

func TestClientDrainEmptyNode(t *testing.T) {
	c := &Client{Clientset: k8sfake.NewClientset(testNode("n1")), ErrOut: io.Discard}
	if err := c.Drain(context.Background(), "n1"); err != nil {
		t.Fatalf("draining an empty node: %v", err)
	}
}
