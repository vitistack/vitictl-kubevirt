package kubevirt

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/rest"

	"github.com/vitistack/vitictl-kubevirt/internal/config"
	"github.com/vitistack/vitictl-kubevirt/internal/kube"
)

// testClient points a KubeVirtClient at a test server, the way a real one
// points at a KubeVirt cluster's API endpoint.
func testClient(url string) *kube.KubeVirtClient {
	return &kube.KubeVirtClient{
		Cluster:    config.Cluster{Name: "test-cluster"},
		RESTConfig: &rest.Config{Host: url},
	}
}

// The request must hit the exact subresource path KubeVirt exposes for a
// restart, with PUT — anything else (a different verb, a VirtualMachineInstance
// path, a missing namespace segment) is silently a no-op against the real API.
func TestRestartRequestShape(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	if err := Restart(context.Background(), testClient(srv.URL), "ns1", "vm-1"); err != nil {
		t.Fatalf("Restart() error = %v", err)
	}

	if gotMethod != http.MethodPut {
		t.Errorf("method = %q, want %q", gotMethod, http.MethodPut)
	}
	want := "/apis/subresources.kubevirt.io/v1/namespaces/ns1/virtualmachines/vm-1/restart"
	if gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
}

// A 2xx response, whatever the exact code, is success.
func TestRestartSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := Restart(context.Background(), testClient(srv.URL), "ns1", "vm-1"); err != nil {
		t.Fatalf("Restart() error = %v, want nil", err)
	}
}

// A 404 means the VM does not exist, and the error must say so rather than
// surface a bare "404" that an operator has to go look up.
func TestRestartNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	err := Restart(context.Background(), testClient(srv.URL), "ns1", "missing-vm")
	if err == nil {
		t.Fatal("expected an error for a 404")
	}
	if !apierrors.IsNotFound(err) {
		t.Errorf("err = %v, want it to satisfy apierrors.IsNotFound", err)
	}
	if !strings.Contains(err.Error(), "missing-vm") || !strings.Contains(err.Error(), "no VirtualMachine") {
		t.Errorf("err = %q, want it to name the VM and say it does not exist", err)
	}
}

// A 409 means KubeVirt rejected the restart because the VM is not running,
// which is a distinct, actionable condition from "does not exist".
func TestRestartConflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	defer srv.Close()

	err := Restart(context.Background(), testClient(srv.URL), "ns1", "stopped-vm")
	if err == nil {
		t.Fatal("expected an error for a 409")
	}
	if !apierrors.IsConflict(err) {
		t.Errorf("err = %v, want it to satisfy apierrors.IsConflict", err)
	}
	if !strings.Contains(err.Error(), "stopped-vm") || !strings.Contains(err.Error(), "not running") {
		t.Errorf("err = %q, want it to name the VM and say it is not running", err)
	}
}
