package guest

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
)

func coreClient(t *testing.T, objs ...ctrlclient.Object) ctrlclient.Client {
	t.Helper()
	s := runtime.NewScheme()
	if err := scheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

func TestFindClusterSecretByName(t *testing.T) {
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "d-stackops-1010-qjxq", Namespace: "ns"}}
	got, err := FindClusterSecret(context.Background(), coreClient(t, sec), "ns", "d-stackops-1010-qjxq")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "d-stackops-1010-qjxq" {
		t.Errorf("got secret %q", got.Name)
	}
}

func TestFindClusterSecretByLabel(t *testing.T) {
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: "prefixed-d-stackops-1010-qjxq", Namespace: "ns",
		Labels: map[string]string{LabelClusterID: "d-stackops-1010-qjxq"},
	}}
	got, err := FindClusterSecret(context.Background(), coreClient(t, sec), "ns", "d-stackops-1010-qjxq")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "prefixed-d-stackops-1010-qjxq" {
		t.Errorf("got secret %q", got.Name)
	}
}

func TestFindClusterSecretMissing(t *testing.T) {
	if _, err := FindClusterSecret(context.Background(), coreClient(t), "ns", "nope"); err == nil {
		t.Fatal("want error for a missing secret")
	}
}

const miniKubeconfig = `apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: "https://127.0.0.1:1"}}]
contexts: [{name: c, context: {cluster: c, user: u}}]
users: [{name: u, user: {}}]
current-context: c
`

func TestConnectBuildsAClientset(t *testing.T) {
	sec := &corev1.Secret{Data: map[string][]byte{KeyKubeConfig: []byte(miniKubeconfig)}}
	if _, err := Connect(sec); err != nil {
		t.Fatal(err)
	}
}

func TestConnectRejectsMissingKey(t *testing.T) {
	_, err := Connect(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s"}})
	if err == nil || !strings.Contains(err.Error(), KeyKubeConfig) {
		t.Fatalf("want error naming %s, got %v", KeyKubeConfig, err)
	}
}
