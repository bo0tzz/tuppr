//go:build e2e_hetzner

package e2ehetzner

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	controllerNamespace = "tuppr-system"
	helmReleaseName     = "tuppr"
	controllerReadyTimeout = 5 * time.Minute
	controllerPollInterval = 10 * time.Second
)

// BuildAndPushImage builds the controller image and pushes it to ttl.sh.
// If cfg.ControllerImage is set, it returns that directly (skipping the build).
func BuildAndPushImage(ctx context.Context, cfg *Config, runID string) (string, error) {
	if cfg.ControllerImage != "" {
		log.Printf("[deploy] using pre-built image: %s", cfg.ControllerImage)
		return cfg.ControllerImage, nil
	}

	image := fmt.Sprintf("ttl.sh/tuppr-%s:2h", runID)
	log.Printf("[deploy] building image: %s", image)

	// Find the repo root (one level up from test/e2e-hetzner/)
	repoRoot, err := repoRootDir()
	if err != nil {
		return "", err
	}

	buildCmd := exec.CommandContext(ctx, "docker", "build", "-t", image, ".")
	buildCmd.Dir = repoRoot
	buildCmd.Stdout = os.Stdout
	buildCmd.Stderr = os.Stderr
	if err := buildCmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("docker build: %w", err)
	}

	log.Printf("[deploy] pushing image: %s", image)
	pushCmd := exec.CommandContext(ctx, "docker", "push", image)
	pushCmd.Stdout = os.Stdout
	pushCmd.Stderr = os.Stderr
	if err := pushCmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("docker push: %w", err)
	}

	return image, nil
}

// DeployController installs the controller via Helm and creates the talosconfig secret.
func DeployController(ctx context.Context, kubeconfig string, image string, talosConfigData []byte) error {
	repoRoot, err := repoRootDir()
	if err != nil {
		return err
	}

	// Copy CRDs to chart dir (they're gitignored)
	log.Printf("[deploy] copying CRDs to Helm chart")
	crdSrc := filepath.Join(repoRoot, "config", "crd", "bases")
	crdDst := filepath.Join(repoRoot, "charts", "tuppr", "crds")
	if err := copyCRDs(crdSrc, crdDst); err != nil {
		return fmt.Errorf("copying CRDs: %w", err)
	}

	// Create namespace with pod security labels before Helm install
	log.Printf("[deploy] creating namespace %s", controllerNamespace)
	if err := kubectl(ctx, kubeconfig,
		"create", "namespace", controllerNamespace,
	); err != nil {
		return fmt.Errorf("creating namespace: %w", err)
	}
	if err := kubectl(ctx, kubeconfig,
		"label", "namespace", controllerNamespace,
		"pod-security.kubernetes.io/enforce=privileged",
		"pod-security.kubernetes.io/warn=privileged",
	); err != nil {
		return fmt.Errorf("labeling namespace: %w", err)
	}

	// Create the talosconfig secret BEFORE helm install so the deployment
	// can mount it immediately when pods start.
	log.Printf("[deploy] creating talosconfig secret")
	if err := createTalosConfigSecret(ctx, kubeconfig, talosConfigData); err != nil {
		return fmt.Errorf("creating talosconfig secret: %w", err)
	}

	// Split image into repository:tag
	repo, tag := splitImage(image)

	chartPath := filepath.Join(repoRoot, "charts", "tuppr")
	log.Printf("[deploy] helm install %s (image=%s)", helmReleaseName, image)

	helmArgs := []string{
		"install", helmReleaseName, chartPath,
		"--namespace", controllerNamespace,
		"--set", fmt.Sprintf("image.repository=%s", repo),
		"--set", fmt.Sprintf("image.tag=%s", tag),
		"--set", "image.pullPolicy=Always",
		"--wait",
		"--timeout", controllerReadyTimeout.String(),
		"--kubeconfig", kubeconfig,
	}

	cmd := exec.CommandContext(ctx, "helm", helmArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("helm install: %w", err)
	}

	log.Printf("[deploy] controller deployed successfully")
	return nil
}

// WaitForController waits for the controller pod to be ready.
func WaitForController(ctx context.Context, kubeconfig string) error {
	deadline := time.After(controllerReadyTimeout)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("controller not ready after %v", controllerReadyTimeout)
		case <-time.After(controllerPollInterval):
			cmd := exec.CommandContext(ctx, "kubectl",
				"--kubeconfig", kubeconfig,
				"--namespace", controllerNamespace,
				"rollout", "status", "deployment/tuppr",
				"--timeout=5s",
			)
			if err := cmd.Run(); err == nil {
				return nil
			}
		}
	}
}

func createTalosConfigSecret(ctx context.Context, kubeconfig string, data []byte) error {
	// The chart mounts a secret named <sa-name>-talosconfig.
	// With default naming, the SA is "tuppr", so the secret is "tuppr-talosconfig".
	secretName := "tuppr-talosconfig"

	cmd := exec.CommandContext(ctx, "kubectl",
		"--kubeconfig", kubeconfig,
		"--namespace", controllerNamespace,
		"create", "secret", "generic", secretName,
		fmt.Sprintf("--from-literal=config=%s", string(data)),
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func copyCRDs(srcDir, dstDir string) error {
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(srcDir, e.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dstDir, e.Name()), data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func splitImage(image string) (repo, tag string) {
	// Handle ttl.sh/tuppr-xxx:2h or ghcr.io/org/repo:tag
	if idx := strings.LastIndex(image, ":"); idx != -1 {
		return image[:idx], image[idx+1:]
	}
	return image, "latest"
}

func kubectl(ctx context.Context, kubeconfig string, args ...string) error {
	fullArgs := append([]string{"--kubeconfig", kubeconfig}, args...)
	log.Printf("[deploy] kubectl %s", strings.Join(fullArgs, " "))
	cmd := exec.CommandContext(ctx, "kubectl", fullArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("kubectl %s: %w", args[0], err)
	}
	return nil
}

func repoRootDir() (string, error) {
	// We're in test/e2e-hetzner/, so repo root is two levels up.
	// Use git to find it reliably.
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("finding repo root: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}
