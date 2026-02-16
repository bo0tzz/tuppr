//go:build e2e_hetzner

package e2ehetzner

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	talosBootstrapTimeout = 10 * time.Minute
	nodeReadyTimeout      = 10 * time.Minute
	nodeReadyPollInterval = 10 * time.Second
)

// TalosCluster manages a Talos Linux cluster bootstrapped via talosctl.
type TalosCluster struct {
	Name         string
	TalosVersion string // e.g. "v1.11.0" — config contract compatibility
	K8sVersion   string // e.g. "v1.33.0" — Kubernetes version to bootstrap
	ServerIPs    []string
	ConfigDir    string // temp dir for generated configs
	TalosConfig  string // path to talosconfig file
	Kubeconfig   string // path to kubeconfig file
}

// NewTalosCluster creates a new TalosCluster manager.
func NewTalosCluster(name string, talosVersion string, k8sVersion string, ips []string) (*TalosCluster, error) {
	dir, err := os.MkdirTemp("", "tuppr-e2e-talos-*")
	if err != nil {
		return nil, fmt.Errorf("creating temp dir: %w", err)
	}
	return &TalosCluster{
		Name:         name,
		TalosVersion: talosVersion,
		K8sVersion:   k8sVersion,
		ServerIPs:    ips,
		ConfigDir:    dir,
	}, nil
}

// Bootstrap generates configs, applies them to each node, and bootstraps
// the Talos cluster. After this returns, Kubernetes should be accessible.
func (tc *TalosCluster) Bootstrap(ctx context.Context) error {
	if err := tc.genConfig(ctx); err != nil {
		return fmt.Errorf("generating config: %w", err)
	}

	for i, ip := range tc.ServerIPs {
		log.Printf("[talos] waiting for Talos API on node %d (%s)", i, ip)
		if err := tc.waitForTalosAPI(ctx, ip); err != nil {
			return fmt.Errorf("waiting for Talos API on node %d (%s): %w", i, ip, err)
		}
		log.Printf("[talos] applying config to node %d (%s)", i, ip)
		if err := tc.applyConfig(ctx, i, ip); err != nil {
			return fmt.Errorf("applying config to node %d (%s): %w", i, ip, err)
		}
	}

	log.Printf("[talos] bootstrapping cluster on %s", tc.ServerIPs[0])
	if err := tc.bootstrap(ctx); err != nil {
		return fmt.Errorf("bootstrapping: %w", err)
	}

	log.Printf("[talos] waiting for Kubernetes API to be ready")
	if err := tc.waitForKubernetesReady(ctx); err != nil {
		return fmt.Errorf("waiting for Kubernetes: %w", err)
	}

	log.Printf("[talos] fetching kubeconfig")
	if err := tc.fetchKubeconfig(ctx); err != nil {
		return fmt.Errorf("fetching kubeconfig: %w", err)
	}

	log.Printf("[talos] waiting for all nodes to be Ready")
	if err := tc.waitForNodesReady(ctx); err != nil {
		return fmt.Errorf("waiting for nodes ready: %w", err)
	}

	log.Printf("[talos] cluster bootstrap complete")
	return nil
}

// TalosConfigData returns the raw talosconfig content for creating a K8s Secret.
func (tc *TalosCluster) TalosConfigData() ([]byte, error) {
	return os.ReadFile(tc.TalosConfig)
}

// KubeconfigData returns the raw kubeconfig content.
func (tc *TalosCluster) KubeconfigData() ([]byte, error) {
	return os.ReadFile(tc.Kubeconfig)
}

// Cleanup removes the temporary config directory.
func (tc *TalosCluster) Cleanup() {
	if tc.ConfigDir != "" {
		os.RemoveAll(tc.ConfigDir)
	}
}

func (tc *TalosCluster) genConfig(ctx context.Context) error {
	endpoint := fmt.Sprintf("https://%s:6443", tc.ServerIPs[0])

	// Build config patches — each must be passed as a separate --config-patch
	// flag (a JSON array is interpreted as JSON6902 which isn't supported).
	patches := []map[string]any{
		// Allow scheduling on control plane nodes (we only have control planes)
		{
			"cluster": map[string]any{
				"allowSchedulingOnControlPlanes": true,
			},
		},
		// Enable Kubernetes Talos API access for the tuppr controller
		{
			"machine": map[string]any{
				"features": map[string]any{
					"kubernetesTalosAPIAccess": map[string]any{
						"enabled": true,
						"allowedRoles": []string{
							"os:admin",
						},
						"allowedKubernetesNamespaces": []string{
							"tuppr-system",
						},
					},
				},
			},
		},
	}

	args := []string{
		"gen", "config",
		tc.Name,
		endpoint,
		"--output", tc.ConfigDir,
		"--force",
		"--talos-version", tc.TalosVersion,
		"--kubernetes-version", tc.K8sVersion,
	}
	for _, p := range patches {
		pj, err := json.Marshal(p)
		if err != nil {
			return fmt.Errorf("marshaling patch: %w", err)
		}
		args = append(args, "--config-patch", string(pj))
	}

	if err := tc.talosctl(ctx, args...); err != nil {
		return err
	}

	tc.TalosConfig = filepath.Join(tc.ConfigDir, "talosconfig")

	// Set real endpoints in the talosconfig (gen config defaults to 127.0.0.1)
	endpointArgs := append([]string{"config", "endpoint"}, tc.ServerIPs...)
	if err := tc.talosctlWithConfig(ctx, endpointArgs...); err != nil {
		return fmt.Errorf("setting endpoints: %w", err)
	}
	if err := tc.talosctlWithConfig(ctx, "config", "node", tc.ServerIPs[0]); err != nil {
		return fmt.Errorf("setting default node: %w", err)
	}

	return nil
}

func (tc *TalosCluster) applyConfig(ctx context.Context, index int, ip string) error {
	configFile := filepath.Join(tc.ConfigDir, "controlplane.yaml")

	// Set a unique hostname per node (must be a JSON object, not array,
	// to be detected as a strategic merge patch instead of JSON6902)
	hostnamePatch := fmt.Sprintf(`{"machine": {"network": {"hostname": "%s-node-%d"}}}`, tc.Name, index)

	return tc.talosctl(ctx,
		"apply-config",
		"--nodes", ip,
		"--file", configFile,
		"--insecure",
		"--config-patch", hostnamePatch,
	)
}

func (tc *TalosCluster) bootstrap(ctx context.Context) error {
	return tc.talosctlWithConfig(ctx,
		"bootstrap",
		"--nodes", tc.ServerIPs[0],
	)
}

func (tc *TalosCluster) waitForKubernetesReady(ctx context.Context) error {
	deadline, cancel := context.WithTimeout(ctx, talosBootstrapTimeout)
	defer cancel()

	// Use talosctl health to wait for the cluster to converge.
	// This waits for etcd, Kubernetes API, and all nodes to be healthy.
	endpoints := strings.Join(tc.ServerIPs, ",")
	return tc.talosctlWithConfig(deadline,
		"health",
		"--nodes", tc.ServerIPs[0],
		"--control-plane-nodes", endpoints,
		"--wait-timeout", talosBootstrapTimeout.String(),
	)
}

func (tc *TalosCluster) fetchKubeconfig(ctx context.Context) error {
	kubeconfigPath := filepath.Join(tc.ConfigDir, "kubeconfig")

	if err := tc.talosctlWithConfig(ctx,
		"kubeconfig",
		"--nodes", tc.ServerIPs[0],
		"--force",
		kubeconfigPath,
	); err != nil {
		return err
	}

	tc.Kubeconfig = kubeconfigPath
	return nil
}

func (tc *TalosCluster) waitForTalosAPI(ctx context.Context, ip string) error {
	deadline := time.After(sshConnectTimeout)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("Talos API not available at %s:50000 after %v", ip, sshConnectTimeout)
		case <-time.After(sshRetryInterval):
			conn, err := net.DialTimeout("tcp", ip+":50000", 5*time.Second)
			if err == nil {
				conn.Close()
				return nil
			}
		}
	}
}

func (tc *TalosCluster) waitForNodesReady(ctx context.Context) error {
	deadline := time.After(nodeReadyTimeout)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("nodes not ready after %v", nodeReadyTimeout)
		case <-time.After(nodeReadyPollInterval):
			ready, err := tc.allNodesReady(ctx)
			if err != nil {
				log.Printf("[talos] checking node readiness: %v", err)
				continue
			}
			if ready {
				return nil
			}
		}
	}
}

func (tc *TalosCluster) allNodesReady(ctx context.Context) (bool, error) {
	cmd := exec.CommandContext(ctx, "kubectl",
		"--kubeconfig", tc.Kubeconfig,
		"get", "nodes",
		"-o", "jsonpath={.items[*].status.conditions[?(@.type==\"Ready\")].status}",
	)
	out, err := cmd.Output()
	if err != nil {
		return false, err
	}
	statuses := strings.Fields(string(out))
	if len(statuses) < len(tc.ServerIPs) {
		return false, nil
	}
	for _, s := range statuses {
		if s != "True" {
			return false, nil
		}
	}
	return true, nil
}

// talosctl runs a talosctl command without any config file (for pre-bootstrap commands).
func (tc *TalosCluster) talosctl(ctx context.Context, args ...string) error {
	log.Printf("[talos] talosctl %s", strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, "talosctl", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("talosctl %s: %w", args[0], err)
	}
	return nil
}

// talosctlWithConfig runs a talosctl command using the generated talosconfig.
func (tc *TalosCluster) talosctlWithConfig(ctx context.Context, args ...string) error {
	fullArgs := append([]string{"--talosconfig", tc.TalosConfig}, args...)
	return tc.talosctl(ctx, fullArgs...)
}
