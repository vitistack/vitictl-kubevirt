package kube

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"

	"github.com/vitistack/vitictl-kubevirt/internal/config"
)

// kubeconfigFor is a minimal but valid kubeconfig, of the shape the operators
// store in the Secret a KubevirtConfig points at.
func kubeconfigFor(cluster, kubecontext string) []byte {
	return []byte(`apiVersion: v1
kind: Config
clusters:
- name: ` + cluster + `
  cluster:
    server: https://10.0.0.1:6443
contexts:
- name: ` + kubecontext + `
  context: {cluster: ` + cluster + `, user: admin}
current-context: ` + kubecontext + `
users:
- name: admin
  user: {token: secret-token}
`)
}

func azScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := scheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := vitiv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func azWith(t *testing.T, name string, objs ...ctrlclient.Object) *VitistackClient {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(azScheme(t)).WithObjects(objs...).Build()
	return &VitistackClient{AZ: config.AvailabilityZone{Name: name}, Ctrl: c}
}

// kubevirtConfigCR is cluster-scoped and names the Secret holding the
// kubeconfig for the KubeVirt cluster it fronts.
func kubevirtConfigCR(name, secretNamespace, secretName string) *vitiv1alpha1.KubevirtConfig {
	return &vitiv1alpha1.KubevirtConfig{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: vitiv1alpha1.KubevirtConfigSpec{
			Name:                name,
			SecretNamespace:     secretNamespace,
			KubeconfigSecretRef: secretName,
		},
	}
}

func kubeconfigSecret(namespace, name string, raw []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data:       map[string][]byte{SecretKeyKubeconfig: raw},
	}
}

// The point of discovery: the management cluster already knows which KubeVirt
// cluster backs its machines, so the user need not maintain a mapping.
func TestDiscovererWalksConfigToSecretToClient(t *testing.T) {
	az := azWith(t, "az1",
		kubevirtConfigCR("kubevirt-provider", "vitistack", "kubevirt-provider"),
		kubeconfigSecret("vitistack", "kubevirt-provider", kubeconfigFor("ptr1-kv-cl01", "admin@ptr1-kv-cl01")),
	)

	var d Discoverer
	kv, err := d.For(context.Background(), az, "kubevirt-provider")
	if err != nil {
		t.Fatalf("For() error = %v", err)
	}
	if kv.Cluster.Name != "admin@ptr1-kv-cl01" {
		t.Errorf("cluster name = %q, want the discovered context", kv.Cluster.Name)
	}
	if kv.Ctrl == nil || kv.RESTConfig == nil {
		t.Error("expected a usable client and REST config")
	}
}

// A discovered cluster the user also has locally must adopt that entry, so
// virtctl — which needs a kubeconfig path — keeps working for it.
func TestDiscovererAdoptsAMatchingLocalCluster(t *testing.T) {
	az := azWith(t, "az1",
		kubevirtConfigCR("kubevirt-provider", "vitistack", "kubevirt-provider"),
		kubeconfigSecret("vitistack", "kubevirt-provider", kubeconfigFor("ptr1-kv-cl01", "admin@ptr1-kv-cl01")),
	)
	d := Discoverer{Local: []config.Cluster{
		{Name: "other", Kubeconfig: "/k/other", Context: "admin@other"},
		{Name: "ptr1-kv-cl01", Kubeconfig: "/k/ptr", Context: "admin@ptr1-kv-cl01"},
	}}

	kv, err := d.For(context.Background(), az, "kubevirt-provider")
	if err != nil {
		t.Fatalf("For() error = %v", err)
	}
	if kv.Cluster.Name != "ptr1-kv-cl01" || kv.Cluster.Kubeconfig != "/k/ptr" {
		t.Errorf("cluster = %+v, want the local ptr1-kv-cl01 entry", kv.Cluster)
	}
	kubeconfig, kubecontext, err := kv.VirtctlTarget()
	if err != nil {
		t.Fatalf("VirtctlTarget() error = %v", err)
	}
	if kubeconfig != "/k/ptr" || kubecontext != "admin@ptr1-kv-cl01" {
		t.Errorf("VirtctlTarget() = %q, %q; want the local path and context", kubeconfig, kubecontext)
	}
}

// Without a local entry there is no kubeconfig path to hand virtctl. Refusing
// with a hint is the point: passing neither flag would let virtctl fall back
// to the ambient KUBECONFIG and act on the wrong cluster.
func TestVirtctlTargetRefusesADiscoveredOnlyCluster(t *testing.T) {
	kv := &KubeVirtClient{Cluster: config.Cluster{Name: "admin@new-kv-cl01"}}

	_, _, err := kv.VirtctlTarget()
	if err == nil {
		t.Fatal("expected an error for a cluster with no local kubeconfig")
	}
	for _, want := range []string{"admin@new-kv-cl01", "config add", "new-kv-cl01"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

// An unannotated machine is answered with the zone's only cluster.
func TestDiscovererFallsBackToTheOnlyConfig(t *testing.T) {
	az := azWith(t, "az1",
		kubevirtConfigCR("kubevirt-provider", "vitistack", "kubevirt-provider"),
		kubeconfigSecret("vitistack", "kubevirt-provider", kubeconfigFor("kv", "admin@kv")),
	)

	var d Discoverer
	kv, err := d.For(context.Background(), az, "")
	if err != nil {
		t.Fatalf("For() error = %v", err)
	}
	if kv.Cluster.Name != "admin@kv" {
		t.Errorf("cluster = %q, want admin@kv", kv.Cluster.Name)
	}
}

// Guessing between several would pair a machine with a stranger's VM.
func TestDiscovererRefusesToGuessBetweenConfigs(t *testing.T) {
	az := azWith(t, "az1",
		kubevirtConfigCR("kv-a", "vitistack", "kv-a"),
		kubevirtConfigCR("kv-b", "vitistack", "kv-b"),
	)

	var d Discoverer
	_, err := d.For(context.Background(), az, "")
	if err == nil {
		t.Fatal("expected an error when the zone publishes several configs")
	}
	for _, want := range []string{"ambiguous", "kv-a", "kv-b"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func TestDiscovererReportsAMissingSecret(t *testing.T) {
	az := azWith(t, "az1", kubevirtConfigCR("kubevirt-provider", "vitistack", "absent"))

	var d Discoverer
	_, err := d.For(context.Background(), az, "kubevirt-provider")
	if err == nil {
		t.Fatal("expected an error for a missing secret")
	}
	if !strings.Contains(err.Error(), "vitistack/absent") {
		t.Errorf("error %q should name the secret it could not read", err)
	}
}

// --cluster must win everywhere, and is the path that keeps working when the
// control plane is down.
func TestDiscovererOverrideWinsWithoutTouchingTheZone(t *testing.T) {
	// A zone with nothing in it: reaching for discovery here would error.
	az := azWith(t, "az1")
	pinned := &KubeVirtClient{Cluster: config.Cluster{Name: "pinned", Context: "admin@pinned"}}
	d := Discoverer{Override: pinned}

	kv, err := d.For(context.Background(), az, "anything")
	if err != nil {
		t.Fatalf("For() error = %v", err)
	}
	if kv != pinned {
		t.Errorf("For() = %+v, want the pinned cluster", kv.Cluster)
	}
}

// A cluster that is down should cost one timeout per listing, not one per
// machine, so failures are cached alongside successes.
func TestDiscovererCachesBothOutcomes(t *testing.T) {
	ok := azWith(t, "good",
		kubevirtConfigCR("kubevirt-provider", "vitistack", "kubevirt-provider"),
		kubeconfigSecret("vitistack", "kubevirt-provider", kubeconfigFor("kv", "admin@kv")),
	)
	bad := azWith(t, "bad")

	var d Discoverer
	first, err := d.For(context.Background(), ok, "kubevirt-provider")
	if err != nil {
		t.Fatalf("For() error = %v", err)
	}
	again, err := d.For(context.Background(), ok, "kubevirt-provider")
	if err != nil {
		t.Fatalf("For() second call error = %v", err)
	}
	if first != again {
		t.Error("a repeated lookup rebuilt the client instead of reusing it")
	}

	if _, err := d.For(context.Background(), bad, ""); err == nil {
		t.Fatal("expected an error for a zone with no KubevirtConfig")
	}
	if _, err := d.For(context.Background(), bad, ""); err == nil {
		t.Fatal("expected the cached error to be returned again")
	}
}
