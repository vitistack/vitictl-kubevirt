package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolate points the config at a temp file and clears the environment, so a
// developer's real ~/.vitistack never influences a test.
func isolate(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubevirt.config.yaml")
	t.Setenv(EnvConfigPath, path)
	t.Setenv(EnvKubeconfig, "")
	t.Setenv(EnvContext, "")
	t.Setenv(EnvNamespace, "")
	return path
}

func TestLoadMissingFileIsNotAnError(t *testing.T) {
	isolate(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.Clusters) != 0 {
		t.Errorf("got %d clusters, want none", len(cfg.Clusters))
	}
}

func TestSaveThenLoadRoundTrips(t *testing.T) {
	isolate(t)

	var cfg Config
	if err := cfg.Add(Cluster{Name: "kv-01", Kubeconfig: "/k/one", Namespace: "vms", Default: true}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Add(Cluster{Name: "kv-02", Context: "ctx-two"}); err != nil {
		t.Fatal(err)
	}
	if err := Save(cfg); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	got, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(got.Clusters) != 2 {
		t.Fatalf("got %d clusters, want 2", len(got.Clusters))
	}
	if got.Clusters[0].Kubeconfig != "/k/one" || got.Clusters[0].Namespace != "vms" {
		t.Errorf("first cluster round-tripped as %+v", got.Clusters[0])
	}
	if got.Clusters[1].Context != "ctx-two" {
		t.Errorf("second cluster round-tripped as %+v", got.Clusters[1])
	}
}

func TestAddRequiresANameAndSomewhereToConnect(t *testing.T) {
	var cfg Config
	if err := cfg.Add(Cluster{Kubeconfig: "/k"}); err == nil {
		t.Error("expected an error when the name is missing")
	}
	if err := cfg.Add(Cluster{Name: "kv"}); err == nil {
		t.Error("expected an error when neither kubeconfig nor context is given")
	}
}

func TestAddReplacesByNameAndKeepsOneDefault(t *testing.T) {
	var cfg Config
	_ = cfg.Add(Cluster{Name: "a", Kubeconfig: "/a", Default: true})
	_ = cfg.Add(Cluster{Name: "b", Kubeconfig: "/b", Default: true})
	_ = cfg.Add(Cluster{Name: "a", Kubeconfig: "/a2"})

	if len(cfg.Clusters) != 2 {
		t.Fatalf("got %d clusters, want 2 after replacing one", len(cfg.Clusters))
	}
	var defaults int
	for _, c := range cfg.Clusters {
		if c.Default {
			defaults++
		}
	}
	if defaults != 1 {
		t.Errorf("got %d defaults, want exactly 1", defaults)
	}
	sel, err := cfg.Select("a")
	if err != nil {
		t.Fatal(err)
	}
	if sel.Kubeconfig != "/a2" {
		t.Errorf("cluster a = %q, want the replacement", sel.Kubeconfig)
	}
}

func TestSelect(t *testing.T) {
	t.Run("no clusters explains how to add one", func(t *testing.T) {
		var cfg Config
		_, err := cfg.Select("")
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "config add") {
			t.Errorf("error %q should say how to add a cluster", err)
		}
	})

	t.Run("a single cluster needs no default", func(t *testing.T) {
		cfg := Config{Clusters: []Cluster{{Name: "only", Kubeconfig: "/k"}}}
		got, err := cfg.Select("")
		if err != nil {
			t.Fatalf("Select() error = %v", err)
		}
		if got.Name != "only" {
			t.Errorf("Select() = %q, want only", got.Name)
		}
	})

	t.Run("several with no default is refused, not guessed", func(t *testing.T) {
		cfg := Config{Clusters: []Cluster{{Name: "a"}, {Name: "b"}}}
		_, err := cfg.Select("")
		if err == nil {
			t.Fatal("expected an error rather than an arbitrary pick")
		}
		if !strings.Contains(err.Error(), "--cluster") {
			t.Errorf("error %q should point at --cluster", err)
		}
	})

	t.Run("the default wins", func(t *testing.T) {
		cfg := Config{Clusters: []Cluster{{Name: "a"}, {Name: "b", Default: true}}}
		got, err := cfg.Select("")
		if err != nil {
			t.Fatal(err)
		}
		if got.Name != "b" {
			t.Errorf("Select() = %q, want b", got.Name)
		}
	})

	t.Run("an unknown name lists what is configured", func(t *testing.T) {
		cfg := Config{Clusters: []Cluster{{Name: "a"}, {Name: "b"}}}
		_, err := cfg.Select("nope")
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "a, b") {
			t.Errorf("error %q should list the configured clusters", err)
		}
	})
}

// An explicit environment must beat the file, so CI can point at a cluster
// without rewriting anyone's config.
func TestEnvironmentDefinesAClusterAndWins(t *testing.T) {
	isolate(t)
	cfg := Config{Clusters: []Cluster{{Name: "from-file", Kubeconfig: "/file", Default: true}}}
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvKubeconfig, "/from/env")
	t.Setenv(EnvNamespace, "envns")

	got, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	sel, err := got.Select("")
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if sel.Name != EnvClusterName || sel.Kubeconfig != "/from/env" || sel.Namespace != "envns" {
		t.Errorf("Select() = %+v, want the environment-defined cluster", sel)
	}
	// The file entry is still listed, just no longer the default.
	if len(got.Clusters) != 2 {
		t.Errorf("got %d clusters, want the file entry kept alongside the env one", len(got.Clusters))
	}
}

func TestRemoveAndSetDefault(t *testing.T) {
	cfg := Config{Clusters: []Cluster{{Name: "a", Default: true}, {Name: "b"}}}

	if err := cfg.SetDefault("b"); err != nil {
		t.Fatal(err)
	}
	if cfg.Clusters[0].Default || !cfg.Clusters[1].Default {
		t.Error("SetDefault should move the flag, not add a second one")
	}
	if err := cfg.SetDefault("nope"); err == nil {
		t.Error("expected an error for an unknown cluster")
	}
	if err := cfg.Remove("a"); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Clusters) != 1 || cfg.Clusters[0].Name != "b" {
		t.Errorf("Remove() left %+v", cfg.Clusters)
	}
	if err := cfg.Remove("a"); err == nil {
		t.Error("removing a missing cluster should error")
	}
}

// The file records paths to credentials, so it must not be world-readable.
func TestSaveWritesOwnerOnly(t *testing.T) {
	path := isolate(t)
	if err := Save(Config{Clusters: []Cluster{{Name: "a", Kubeconfig: "/k"}}}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config mode = %o, want 600", perm)
	}
}
