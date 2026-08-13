package config

import (
	"fmt"
	"os"
	"path/filepath"

	yaml "go.yaml.in/yaml/v3"
)

// EnvVitiConfig is set by vitictl for every plugin it dispatches, pointing at
// the active ctl.config.yaml. Reading it means "viti kubevirt ..." honours
// whatever config the parent viti was using, without this plugin having to
// re-derive it.
const EnvVitiConfig = "VITI_CONFIG"

// EnvAvailabilityZone is vitictl's -z/--availabilityzone, likewise forwarded
// to plugins so a zone-scoped invocation stays zone-scoped.
const EnvAvailabilityZone = "VITI_AVAILABILITYZONE"

// VitistackConfigFileName is vitictl's own config file.
const VitistackConfigFileName = "ctl.config.yaml"

// AvailabilityZone is one Vitistack control-plane cluster, where Machine
// resources live. The shape mirrors vitictl's own config so the same file
// parses here.
type AvailabilityZone struct {
	Name       string `yaml:"name"`
	Kubeconfig string `yaml:"kubeconfig,omitempty"`
	Context    string `yaml:"context,omitempty"`
}

// vitistackConfig is the subset of ctl.config.yaml this plugin reads.
type vitistackConfig struct {
	AvailabilityZones []AvailabilityZone `yaml:"availabilityzones"`
}

// VitistackConfigPath returns vitictl's config file: $VITI_CONFIG when viti
// dispatched us, otherwise ~/.vitistack/ctl.config.yaml for standalone runs.
func VitistackConfigPath() (string, error) {
	if p := os.Getenv(EnvVitiConfig); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home directory: %w", err)
	}
	return filepath.Join(home, ConfigDirName, VitistackConfigFileName), nil
}

// AvailabilityZones returns the Vitistack availability zones vitictl is
// configured with, optionally narrowed to one by name.
//
// The zones are read rather than redefined: configuring them twice would let
// the two CLIs drift, and vitictl already owns "viti config add".
func AvailabilityZones(name string) ([]AvailabilityZone, error) {
	path, err := VitistackConfigPath()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- path is vitictl's own config location
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf(
				"no Vitistack configuration at %s — run 'viti config init' (or 'viti config add <name> --kubeconfig <path>') first",
				path)
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var cfg vitistackConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if len(cfg.AvailabilityZones) == 0 {
		return nil, fmt.Errorf(
			"no availability zones configured in %s — add one with 'viti config add <name> --kubeconfig <path>'",
			path)
	}
	if name == "" {
		name = os.Getenv(EnvAvailabilityZone)
	}
	if name == "" {
		return cfg.AvailabilityZones, nil
	}
	for _, z := range cfg.AvailabilityZones {
		if z.Name == name {
			return []AvailabilityZone{z}, nil
		}
	}
	return nil, fmt.Errorf("no availability zone named %q in %s", name, path)
}
