package config

import (
	"fmt"
	"os"
	"strings"

	"sigs.k8s.io/yaml"
)

type Config struct {
	Watches []Watch `json:"watches"`
	Policy  Policy  `json:"policy,omitempty"`
}

const (
	CascadeDeletionDelete   = "Delete"
	CascadeDeletionRetain   = "Retain"
	ExistingTargetPreserve  = "Preserve"
	ExistingTargetOverwrite = "Overwrite"
)

type Policy struct {
	CascadeDeletion string `json:"cascadeDeletion,omitempty"`
	ExistingTarget  string `json:"existingTarget,omitempty"`
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
	cfg.Policy.CascadeDeletion = strings.TrimSpace(cfg.Policy.CascadeDeletion)
	if cfg.Policy.CascadeDeletion == "" {
		cfg.Policy.CascadeDeletion = CascadeDeletionDelete
	}
	if cfg.Policy.CascadeDeletion != CascadeDeletionDelete && cfg.Policy.CascadeDeletion != CascadeDeletionRetain {
		return nil, fmt.Errorf("policy.cascadeDeletion must be %q or %q", CascadeDeletionDelete, CascadeDeletionRetain)
	}
	cfg.Policy.ExistingTarget = strings.TrimSpace(cfg.Policy.ExistingTarget)
	if cfg.Policy.ExistingTarget == "" {
		cfg.Policy.ExistingTarget = ExistingTargetPreserve
	}
	if cfg.Policy.ExistingTarget != ExistingTargetPreserve && cfg.Policy.ExistingTarget != ExistingTargetOverwrite {
		return nil, fmt.Errorf("policy.existingTarget must be %q or %q", ExistingTargetPreserve, ExistingTargetOverwrite)
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
