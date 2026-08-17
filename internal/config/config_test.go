package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte("watches:\n  - apiVersion: k8s.nginx.org/v1\n    resources:\n      - policies\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.Watches) != 1 || cfg.Watches[0].APIVersion != "k8s.nginx.org/v1" || len(cfg.Watches[0].Resources) != 1 || cfg.Watches[0].Resources[0] != "policies" {
		t.Fatalf("unexpected config: %#v", cfg)
	}
	if cfg.Policy.CascadeDeletion != CascadeDeletionDelete {
		t.Fatalf("cascadeDeletion = %q, want default %q", cfg.Policy.CascadeDeletion, CascadeDeletionDelete)
	}
	if cfg.Policy.ExistingTarget != ExistingTargetPreserve {
		t.Fatalf("existingTarget = %q, want default %q", cfg.Policy.ExistingTarget, ExistingTargetPreserve)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("watches: []\nunknown: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load() succeeded with an unknown field")
	}
}

func TestLoadPolicies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte("policy:\n  cascadeDeletion: Retain\n  existingTarget: Overwrite\nwatches:\n  - apiVersion: v1\n    resources: [secrets]\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Policy.CascadeDeletion != CascadeDeletionRetain {
		t.Fatalf("cascadeDeletion = %q, want %q", cfg.Policy.CascadeDeletion, CascadeDeletionRetain)
	}
	if cfg.Policy.ExistingTarget != ExistingTargetOverwrite {
		t.Fatalf("existingTarget = %q, want %q", cfg.Policy.ExistingTarget, ExistingTargetOverwrite)
	}
}

func TestLoadRejectsInvalidWatch(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "no watches", data: "watches: []\n", want: "at least one watch"},
		{name: "empty apiVersion", data: "watches:\n  - apiVersion: '  '\n    resources: [secrets]\n", want: "apiVersion must not be empty"},
		{name: "no resources", data: "watches:\n  - apiVersion: v1\n    resources: []\n", want: "at least one resource"},
		{name: "empty resource", data: "watches:\n  - apiVersion: v1\n    resources: ['  ']\n", want: "resources[0] must not be empty"},
		{name: "invalid cascade deletion", data: "policy:\n  cascadeDeletion: Keep\nwatches:\n  - apiVersion: v1\n    resources: [secrets]\n", want: "cascadeDeletion must be"},
		{name: "invalid existing target", data: "policy:\n  existingTarget: Replace\nwatches:\n  - apiVersion: v1\n    resources: [secrets]\n", want: "existingTarget must be"},
		{name: "legacy behavior field", data: "behavior:\n  cascadeDeletionPolicy: Retain\nwatches:\n  - apiVersion: v1\n    resources: [secrets]\n", want: "unknown field \"behavior\""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tt.data), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}
