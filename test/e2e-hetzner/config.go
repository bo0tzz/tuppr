//go:build e2e_hetzner

package e2ehetzner

import (
	"fmt"
	"os"
	"os/exec"
)

// Config holds all configuration for the e2e test run.
type Config struct {
	HCloudToken      string
	ServerType       string
	Location         string
	TalosFromVersion string
	TalosToVersion   string
	K8sToVersion     string
	ControllerImage  string // if set, skip build and use this image
}

// LoadConfig reads configuration from environment variables.
func LoadConfig() (*Config, error) {
	cfg := &Config{
		HCloudToken:      os.Getenv("HCLOUD_TOKEN"),
		ServerType:       envOrDefault("HCLOUD_SERVER_TYPE", "cx23"),
		Location:         envOrDefault("HCLOUD_LOCATION", "fsn1"),
		TalosFromVersion: envOrDefault("TALOS_FROM_VERSION", "v1.11.0"),
		TalosToVersion:   envOrDefault("TALOS_TO_VERSION", "v1.12.4"),
		K8sToVersion:     envOrDefault("K8S_TO_VERSION", "v1.34.0"),
		ControllerImage:  os.Getenv("CONTROLLER_IMAGE"),
	}

	if cfg.HCloudToken == "" {
		return nil, fmt.Errorf("HCLOUD_TOKEN environment variable is required")
	}

	return cfg, nil
}

// CheckPrerequisites verifies that all required CLI tools are available.
func CheckPrerequisites() error {
	tools := []string{"talosctl", "kubectl", "helm", "docker"}
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("required tool %q not found in PATH", tool)
		}
	}
	return nil
}

func envOrDefault(key, defaultValue string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultValue
}
