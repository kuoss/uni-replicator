package config

import (
	"fmt"
	"os"
	"strings"

	"sigs.k8s.io/yaml"
)

type Config struct {
	Watches  []Watch  `json:"watches"`
	Behavior Behavior `json:"behavior,omitempty"`
}

const (
	CascadeDeletionDelete = "Delete"
	CascadeDeletionRetain = "Retain"
)

type Behavior struct {
	CascadeDeletionPolicy string `json:"cascadeDeletionPolicy,omitempty"`
}

type Watch struct {
	APIVersion string   `json:"apiVersion"`
	Resources  []string `json:"resources"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	var cfg Config
	if err := yaml.UnmarshalStrict(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	if len(cfg.Watches) == 0 {
		return nil, fmt.Errorf("config must contain at least one watch")
	}
	cfg.Behavior.CascadeDeletionPolicy = strings.TrimSpace(cfg.Behavior.CascadeDeletionPolicy)
	if cfg.Behavior.CascadeDeletionPolicy == "" {
		cfg.Behavior.CascadeDeletionPolicy = CascadeDeletionDelete
	}
	if cfg.Behavior.CascadeDeletionPolicy != CascadeDeletionDelete && cfg.Behavior.CascadeDeletionPolicy != CascadeDeletionRetain {
		return nil, fmt.Errorf("behavior.cascadeDeletionPolicy must be %q or %q", CascadeDeletionDelete, CascadeDeletionRetain)
	}
	for i := range cfg.Watches {
		watch := &cfg.Watches[i]
		watch.APIVersion = strings.TrimSpace(watch.APIVersion)
		if watch.APIVersion == "" {
			return nil, fmt.Errorf("watches[%d].apiVersion must not be empty", i)
		}
		if len(watch.Resources) == 0 {
			return nil, fmt.Errorf("watches[%d].resources must contain at least one resource", i)
		}
		for j := range watch.Resources {
			watch.Resources[j] = strings.TrimSpace(watch.Resources[j])
			if watch.Resources[j] == "" {
				return nil, fmt.Errorf("watches[%d].resources[%d] must not be empty", i, j)
			}
		}
	}
	return &cfg, nil
}
