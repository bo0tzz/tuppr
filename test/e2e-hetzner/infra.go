//go:build e2e_hetzner

package e2ehetzner

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net"
	"sync"
	"time"

	"log"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"golang.org/x/crypto/ssh"
)

const (
	// defaultSchematicID is the vanilla Talos schematic (no extensions).
	defaultSchematicID = "376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba"
	nodeCount          = 3
	sshRetryInterval   = 5 * time.Second
	sshConnectTimeout  = 5 * time.Minute
)

// HetznerCluster manages the lifecycle of Hetzner Cloud VMs for e2e testing.
type HetznerCluster struct {
	client     *hcloud.Client
	config     *Config
	RunID      string
	sshKey     *hcloud.SSHKey
	privateKey ed25519.PrivateKey
	servers    []*hcloud.Server
}

// NewHetznerCluster creates a new cluster manager.
func NewHetznerCluster(cfg *Config) *HetznerCluster {
	return &HetznerCluster{
		client: hcloud.NewClient(hcloud.WithToken(cfg.HCloudToken)),
		config: cfg,
		RunID:  fmt.Sprintf("tuppr-e2e-%d", time.Now().Unix()),
	}
}

// Create provisions VMs, flashes Talos, and reboots into Talos.
// All servers are provisioned concurrently.
func (h *HetznerCluster) Create(ctx context.Context) error {
	if err := h.createSSHKey(ctx); err != nil {
		return fmt.Errorf("creating SSH key: %w", err)
	}

	var wg sync.WaitGroup
	errs := make([]error, nodeCount)
	h.servers = make([]*hcloud.Server, nodeCount)

	for i := range nodeCount {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			server, err := h.createAndFlashServer(ctx, idx)
			if err != nil {
				errs[idx] = fmt.Errorf("server %d: %w", idx, err)
				return
			}
			h.servers[idx] = server
		}(i)
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return err
		}
	}

	return nil
}

// Destroy tears down all Hetzner resources (servers + SSH key).
func (h *HetznerCluster) Destroy(ctx context.Context) error {
	var errs []error

	for _, server := range h.servers {
		if server == nil {
			continue
		}
		h.logf("deleting server %s", server.Name)
		if _, err := h.client.Server.Delete(ctx, server); err != nil {
			errs = append(errs, fmt.Errorf("deleting server %s: %w", server.Name, err))
		}
	}

	if h.sshKey != nil {
		h.logf("deleting SSH key %s", h.sshKey.Name)
		if _, err := h.client.SSHKey.Delete(ctx, h.sshKey); err != nil {
			errs = append(errs, fmt.Errorf("deleting SSH key: %w", err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("cleanup errors: %v", errs)
	}
	return nil
}

// ServerIPs returns the public IPv4 addresses of all servers.
func (h *HetznerCluster) ServerIPs() []string {
	ips := make([]string, len(h.servers))
	for i, s := range h.servers {
		if s != nil {
			ips[i] = s.PublicNet.IPv4.IP.String()
		}
	}
	return ips
}

func (h *HetznerCluster) createSSHKey(ctx context.Context) error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("generating ED25519 key: %w", err)
	}
	h.privateKey = priv

	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return fmt.Errorf("creating SSH public key: %w", err)
	}

	key, _, err := h.client.SSHKey.Create(ctx, hcloud.SSHKeyCreateOpts{
		Name:      h.RunID,
		PublicKey: string(ssh.MarshalAuthorizedKey(sshPub)),
		Labels:   h.labels(),
	})
	if err != nil {
		return fmt.Errorf("uploading SSH key to Hetzner: %w", err)
	}
	h.sshKey = key

	return nil
}

func (h *HetznerCluster) logf(format string, args ...any) {
	log.Printf("[hetzner] "+format, args...)
}

func (h *HetznerCluster) createAndFlashServer(ctx context.Context, index int) (*hcloud.Server, error) {
	name := fmt.Sprintf("%s-node-%d", h.RunID, index)

	h.logf("%s: creating server (type=%s, location=%s)", name, h.config.ServerType, h.config.Location)

	// Create server with any Linux image (we'll overwrite the disk)
	result, _, err := h.client.Server.Create(ctx, hcloud.ServerCreateOpts{
		Name:       name,
		ServerType: &hcloud.ServerType{Name: h.config.ServerType},
		Image:      &hcloud.Image{Name: "ubuntu-24.04"},
		Location:   &hcloud.Location{Name: h.config.Location},
		SSHKeys:    []*hcloud.SSHKey{h.sshKey},
		Labels:     h.labels(),
	})
	if err != nil {
		return nil, fmt.Errorf("creating server %s: %w", name, err)
	}

	server := result.Server
	ip := server.PublicNet.IPv4.IP.String()
	h.logf("%s: server created (ip=%s), waiting for actions", name, ip)

	// Wait for creation action to complete
	if err := h.client.Action.WaitFor(ctx, result.Action); err != nil {
		return nil, fmt.Errorf("waiting for server %s creation: %w", name, err)
	}
	for _, a := range result.NextActions {
		if err := h.client.Action.WaitFor(ctx, a); err != nil {
			return nil, fmt.Errorf("waiting for server %s next action: %w", name, err)
		}
	}

	h.logf("%s: enabling rescue mode", name)
	rescueResult, _, err := h.client.Server.EnableRescue(ctx, server, hcloud.ServerEnableRescueOpts{
		Type:    hcloud.ServerRescueTypeLinux64,
		SSHKeys: []*hcloud.SSHKey{h.sshKey},
	})
	if err != nil {
		return nil, fmt.Errorf("enabling rescue on %s: %w", name, err)
	}
	if err := h.client.Action.WaitFor(ctx, rescueResult.Action); err != nil {
		return nil, fmt.Errorf("waiting for rescue enable on %s: %w", name, err)
	}

	h.logf("%s: resetting server to boot into rescue", name)
	resetAction, _, err := h.client.Server.Reset(ctx, server)
	if err != nil {
		return nil, fmt.Errorf("resetting %s for rescue: %w", name, err)
	}
	if err := h.client.Action.WaitFor(ctx, resetAction); err != nil {
		return nil, fmt.Errorf("waiting for reset on %s: %w", name, err)
	}

	h.logf("%s: waiting for SSH on %s", name, ip)
	if err := h.waitForSSH(ctx, ip); err != nil {
		return nil, fmt.Errorf("waiting for SSH on %s (%s): %w", name, ip, err)
	}

	h.logf("%s: flashing Talos %s (this takes 2-5 minutes)", name, h.config.TalosFromVersion)
	if err := h.flashTalos(ctx, ip); err != nil {
		return nil, fmt.Errorf("flashing Talos on %s (%s): %w", name, ip, err)
	}

	h.logf("%s: flash complete, rebooting into Talos", name)
	rebootAction, _, err := h.client.Server.Reset(ctx, server)
	if err != nil {
		return nil, fmt.Errorf("rebooting %s into Talos: %w", name, err)
	}
	if err := h.client.Action.WaitFor(ctx, rebootAction); err != nil {
		return nil, fmt.Errorf("waiting for Talos reboot on %s: %w", name, err)
	}

	h.logf("%s: ready (ip=%s)", name, ip)
	return server, nil
}

func (h *HetznerCluster) waitForSSH(ctx context.Context, ip string) error {
	deadline := time.After(sshConnectTimeout)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("SSH not available at %s after %v", ip, sshConnectTimeout)
		case <-time.After(sshRetryInterval):
			conn, err := net.DialTimeout("tcp", ip+":22", 5*time.Second)
			if err == nil {
				conn.Close()
				// Give the SSH service a moment to fully initialize
				time.Sleep(2 * time.Second)
				return nil
			}
		}
	}
}

func (h *HetznerCluster) flashTalos(ctx context.Context, ip string) error {
	signer, err := ssh.NewSignerFromKey(h.privateKey)
	if err != nil {
		return fmt.Errorf("creating SSH signer: %w", err)
	}

	sshConfig := &ssh.ClientConfig{
		User:            "root",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // ephemeral test VMs
		Timeout:         10 * time.Second,
	}

	client, err := ssh.Dial("tcp", ip+":22", sshConfig)
	if err != nil {
		return fmt.Errorf("SSH dial to %s: %w", ip, err)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("SSH session: %w", err)
	}
	defer session.Close()

	imageURL := fmt.Sprintf(
		"https://factory.talos.dev/image/%s/%s/hcloud-amd64.raw.xz",
		defaultSchematicID,
		h.config.TalosFromVersion,
	)

	cmd := fmt.Sprintf("curl -fsSL %s | xz -d | dd of=/dev/sda bs=4M && sync", imageURL)
	output, err := session.CombinedOutput(cmd)
	if err != nil {
		return fmt.Errorf("flash command failed: %w\nOutput: %s", err, string(output))
	}

	return nil
}

func (h *HetznerCluster) labels() map[string]string {
	return map[string]string{
		"managed-by": "tuppr-e2e",
		"run-id":     h.RunID,
	}
}
