// Package config loads and persists the viti-kubevirt plugin's own
// configuration (~/.vitistack/kubevirt.config.yaml), which lists the KubeVirt
// clusters the plugin talks to.
//
// It is deliberately separate from vitictl's ctl.config.yaml. Those
// availability zones hold the Vitistack control plane — where Machine
// resources live — while these are the clusters the virtual machines actually
// run on. They are frequently different kubeconfigs, and a listing joins the
// two.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

const (
	// EnvConfigPath overrides the config file location outright.
	EnvConfigPath = "VITI_KUBEVIRT_CONFIG"

	// EnvKubeconfig and EnvContext define a single ad-hoc cluster without a
	// config file, so CI can point the plugin at a cluster inline.
	EnvKubeconfig = "VITI_KUBEVIRT_KUBECONFIG"
	EnvContext    = "VITI_KUBEVIRT_CONTEXT"
	EnvNamespace  = "VITI_KUBEVIRT_NAMESPACE"

	// EnvClusterName is the name given to the cluster defined by the
	// environment, so it can be referred to like any other.
	EnvClusterName = "env"

	// ConfigDirName and ConfigFileName mirror vitictl's ~/.vitistack layout.
	ConfigDirName  = ".vitistack"
	ConfigFileName = "kubevirt.config.yaml"
)

// Cluster points at one KubeVirt cluster. An empty Kubeconfig falls back to
// $KUBECONFIG or ~/.kube/config; an empty Context uses the kubeconfig's
// current-context — the same rules vitictl applies to availability zones.
type Cluster struct {
	Name       string `yaml:"name"`
	Kubeconfig string `yaml:"kubeconfig,omitempty"`
	Context    string `yaml:"context,omitempty"`
	// Namespace limits listings when none is given on the command line.
	Namespace string `yaml:"namespace,omitempty"`
	// Default marks the cluster used when --cluster is not given.
	Default bool `yaml:"default,omitempty"`
}

// Config is the on-disk shape of kubevirt.config.yaml.
type Config struct {
	Clusters []Cluster `yaml:"clusters"`
}

// Path returns the config file location: $VITI_KUBEVIRT_CONFIG if set,
// otherwise ~/.vitistack/kubevirt.config.yaml.
func Path() (string, error) {
	if p := os.Getenv(EnvConfigPath); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home directory: %w", err)
	}
	return filepath.Join(home, ConfigDirName, ConfigFileName), nil
}

// Load reads the config file. A missing file is not an error — the
// environment alone is a valid configuration, and Select reports what is
// missing with an actionable hint.
func Load() (Config, error) {
	var cfg Config
	path, err := Path()
	if err != nil {
		return cfg, err
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- path is the user's own config location
	switch {
	case err == nil:
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return cfg, fmt.Errorf("parsing %s: %w", path, err)
		}
	case os.IsNotExist(err):
		// Not an error: the environment may define a cluster on its own.
	default:
		return cfg, fmt.Errorf("reading %s: %w", path, err)
	}
	cfg.applyEnv()
	return cfg, nil
}

// applyEnv folds a cluster defined by the environment into the config. It
// replaces any same-named entry and becomes the default, so an explicit
// environment always wins over the file.
func (c *Config) applyEnv() {
	kubeconfig := strings.TrimSpace(os.Getenv(EnvKubeconfig))
	context := strings.TrimSpace(os.Getenv(EnvContext))
	if kubeconfig == "" && context == "" {
		return
	}
	env := Cluster{
		Name:       EnvClusterName,
		Kubeconfig: kubeconfig,
		Context:    context,
		Namespace:  strings.TrimSpace(os.Getenv(EnvNamespace)),
		Default:    true,
	}
	for i := range c.Clusters {
		c.Clusters[i].Default = false
		if c.Clusters[i].Name == env.Name {
			c.Clusters[i] = env
			return
		}
	}
	c.Clusters = append(c.Clusters, env)
}

// Select resolves the cluster to operate on. An explicit name wins; otherwise
// the one marked default, or the only configured one.
func (c Config) Select(name string) (Cluster, error) {
	if len(c.Clusters) == 0 {
		return Cluster{}, errNoClusters()
	}
	if name != "" {
		for _, cl := range c.Clusters {
			if cl.Name == name {
				return cl, nil
			}
		}
		return Cluster{}, fmt.Errorf("no KubeVirt cluster named %q configured (have: %s)",
			name, strings.Join(c.Names(), ", "))
	}
	for _, cl := range c.Clusters {
		if cl.Default {
			return cl, nil
		}
	}
	if len(c.Clusters) == 1 {
		return c.Clusters[0], nil
	}
	return Cluster{}, fmt.Errorf(
		"several KubeVirt clusters are configured (%s) and none is marked default — "+
			"pass --cluster <name>, or run 'viti kubevirt config default <name>'",
		strings.Join(c.Names(), ", "))
}

// Names lists the configured cluster names, for error messages and completion.
func (c Config) Names() []string {
	out := make([]string, 0, len(c.Clusters))
	for _, cl := range c.Clusters {
		out = append(out, cl.Name)
	}
	return out
}

// Add appends or replaces a cluster by name. Marking one default clears the
// flag on the others, so exactly one can ever hold it.
func (c *Config) Add(cl Cluster) error {
	if cl.Name == "" {
		return errors.New("cluster name is required")
	}
	if cl.Kubeconfig == "" && cl.Context == "" {
		return errors.New("a cluster needs at least a kubeconfig or a context")
	}
	if cl.Default {
		for i := range c.Clusters {
			c.Clusters[i].Default = false
		}
	}
	for i := range c.Clusters {
		if c.Clusters[i].Name == cl.Name {
			c.Clusters[i] = cl
			return nil
		}
	}
	c.Clusters = append(c.Clusters, cl)
	return nil
}

// Remove deletes a cluster by name.
func (c *Config) Remove(name string) error {
	filtered := c.Clusters[:0]
	found := false
	for _, cl := range c.Clusters {
		if cl.Name == name {
			found = true
			continue
		}
		filtered = append(filtered, cl)
	}
	if !found {
		return fmt.Errorf("no KubeVirt cluster named %q", name)
	}
	c.Clusters = filtered
	return nil
}

// SetDefault marks one cluster as the default and clears the rest.
func (c *Config) SetDefault(name string) error {
	found := false
	for i := range c.Clusters {
		if c.Clusters[i].Name == name {
			found = true
		}
		c.Clusters[i].Default = c.Clusters[i].Name == name
	}
	if !found {
		return fmt.Errorf("no KubeVirt cluster named %q", name)
	}
	return nil
}

// Save writes cfg to the config path, creating the directory if needed.
//
// The file holds kubeconfig paths rather than secrets, but those paths point
// at credentials, so it is written owner-only like the rest of ~/.vitistack.
func Save(cfg Config) error {
	path, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encoding config: %w", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	// WriteFile only applies the mode when it creates the file; an existing
	// file keeps its old, possibly wider, permissions.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("securing %s: %w", path, err)
	}
	return nil
}

func errNoClusters() error {
	return errors.New(strings.TrimSpace(`
no KubeVirt clusters configured.

Add one with:
  viti kubevirt config add <name> --kubeconfig <path>
  viti kubevirt config add <name> --context <kubectl-context>

Or point at one for a single command with ` + EnvKubeconfig + `.`))
}
