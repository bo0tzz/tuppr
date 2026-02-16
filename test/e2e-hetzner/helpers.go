//go:build e2e_hetzner

package e2ehetzner

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"
)

// kubectlOutput runs kubectl and returns stdout.
func kubectlOutput(ctx context.Context, kubeconfig string, args ...string) (string, error) {
	fullArgs := append([]string{"--kubeconfig", kubeconfig}, args...)
	cmd := exec.CommandContext(ctx, "kubectl", fullArgs...)
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("kubectl %s: %w\nstderr: %s", args[0], err, string(ee.Stderr))
		}
		return "", fmt.Errorf("kubectl %s: %w", args[0], err)
	}
	return strings.TrimSpace(string(out)), nil
}

// kubectlApply applies a YAML manifest via kubectl.
func kubectlApply(ctx context.Context, kubeconfig string, yaml string) error {
	cmd := exec.CommandContext(ctx, "kubectl",
		"--kubeconfig", kubeconfig,
		"apply", "-f", "-",
	)
	cmd.Stdin = strings.NewReader(yaml)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("kubectl apply: %w\n%s", err, string(out))
	}
	log.Printf("[kubectl] %s", strings.TrimSpace(string(out)))
	return nil
}

// kubectlDelete deletes a resource via kubectl (ignores not-found errors).
func kubectlDelete(ctx context.Context, kubeconfig string, resource, name string) error {
	_, err := kubectlOutput(ctx, kubeconfig,
		"delete", resource, name, "--ignore-not-found",
	)
	return err
}

// getCRStatus fetches a CR's status using kubectl and returns it as a map.
func getCRStatus(ctx context.Context, kubeconfig string, resource, name string) (map[string]any, error) {
	out, err := kubectlOutput(ctx, kubeconfig,
		"get", resource, name,
		"-o", "jsonpath={.status}",
	)
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	var status map[string]any
	if err := json.Unmarshal([]byte(out), &status); err != nil {
		return nil, fmt.Errorf("parsing status JSON: %w\nraw: %s", err, out)
	}
	return status, nil
}

// getCRPhase returns the .status.phase of a CR.
func getCRPhase(ctx context.Context, kubeconfig string, resource, name string) (string, error) {
	return kubectlOutput(ctx, kubeconfig,
		"get", resource, name,
		"-o", "jsonpath={.status.phase}",
	)
}

// talosctlOutput runs talosctl with the given talosconfig and returns stdout.
func talosctlOutput(ctx context.Context, talosconfig string, args ...string) (string, error) {
	fullArgs := append([]string{"--talosconfig", talosconfig}, args...)
	cmd := exec.CommandContext(ctx, "talosctl", fullArgs...)
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("talosctl %s: %w\nstderr: %s", args[0], err, string(ee.Stderr))
		}
		return "", fmt.Errorf("talosctl %s: %w", args[0], err)
	}
	return strings.TrimSpace(string(out)), nil
}

// streamLogs starts streaming kubectl logs for a deployment in the background.
// Automatically restarts if the connection drops (e.g. during node upgrades).
func streamLogs(ctx context.Context, kubeconfig, namespace, deployment, prefix string) {
	go func() {
		for ctx.Err() == nil {
			runStreamLogs(ctx, kubeconfig, namespace, deployment, prefix)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
				log.Printf("%s reconnecting...", prefix)
			}
		}
	}()
}

func runStreamLogs(ctx context.Context, kubeconfig, namespace, deployment, prefix string) {
	cmd := exec.CommandContext(ctx, "kubectl",
		"--kubeconfig", kubeconfig,
		"--namespace", namespace,
		"logs", fmt.Sprintf("deployment/%s", deployment),
		"--follow", "--tail=0",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("%s failed to create pipe: %v", prefix, err)
		return
	}
	if err := cmd.Start(); err != nil {
		log.Printf("%s failed to start: %v", prefix, err)
		return
	}
	log.Printf("%s streaming started", prefix)
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		log.Printf("%s %s", prefix, scanner.Text())
	}
	_ = cmd.Wait()
}

// watchResource starts a background `kubectl get -w` for a resource type.
// Automatically restarts if the connection drops (e.g. during node upgrades).
func watchResource(ctx context.Context, kubeconfig, resource, prefix string) {
	go func() {
		for ctx.Err() == nil {
			runWatch(ctx, kubeconfig, resource, prefix)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
				log.Printf("%s reconnecting...", prefix)
			}
		}
	}()
}

func runWatch(ctx context.Context, kubeconfig, resource, prefix string) {
	cmd := exec.CommandContext(ctx, "kubectl",
		"--kubeconfig", kubeconfig,
		"get", resource,
		"--watch", "--output-watch-events",
		"-o", "json",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("%s failed to create pipe: %v", prefix, err)
		return
	}
	if err := cmd.Start(); err != nil {
		log.Printf("%s failed to start: %v", prefix, err)
		return
	}
	log.Printf("%s watching started", prefix)
	dec := json.NewDecoder(stdout)
	for {
		var event json.RawMessage
		if err := dec.Decode(&event); err != nil {
			if ctx.Err() == nil {
				log.Printf("%s decode error: %v", prefix, err)
			}
			break
		}
		log.Printf("%s %s", prefix, event)
	}
	_ = cmd.Wait()
}

// getTalosNodeVersions returns a map of node IP -> Talos version string.
func getTalosNodeVersions(ctx context.Context, talosconfig string, nodeIPs []string) (map[string]string, error) {
	versions := make(map[string]string, len(nodeIPs))
	for _, ip := range nodeIPs {
		out, err := talosctlOutput(ctx, talosconfig,
			"version", "--nodes", ip, "--short",
		)
		if err != nil {
			return nil, fmt.Errorf("getting version for %s: %w", ip, err)
		}
		// talosctl version --short outputs two lines:
		//   Client:
		//     Tag: v1.12.4
		//   Server:
		//     Tag: v1.11.0
		// We want the server tag.
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "Tag:") {
				// The second "Tag:" line is the server version
				versions[ip] = strings.TrimSpace(strings.TrimPrefix(line, "Tag:"))
			}
		}
		if _, ok := versions[ip]; !ok {
			return nil, fmt.Errorf("could not parse Talos version for %s from:\n%s", ip, out)
		}
	}
	return versions, nil
}
