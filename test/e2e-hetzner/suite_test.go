//go:build e2e_hetzner

package e2ehetzner

import (
	"context"
	"log"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var (
	cfg          *Config
	cluster      *HetznerCluster
	talosCluster *TalosCluster

	// bgCtx is a long-lived context for background goroutines (log streaming,
	// resource watches) that must outlive the BeforeSuite SpecContext.
	// Cancelled in DeferCleanup.
	bgCtx    context.Context
	bgCancel context.CancelFunc
)

func TestE2EHetzner(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "E2E Hetzner Suite")
}

// BeforeSuite accepts SpecContext which is automatically cancelled by Ginkgo on
// interrupt (ctrl+c). This ensures all in-flight API calls and SSH sessions
// are terminated promptly so DeferCleanup can run Destroy.
var _ = BeforeSuite(func(ctx SpecContext) {
	var err error

	By("Loading configuration")
	cfg, err = LoadConfig()
	Expect(err).NotTo(HaveOccurred())

	By("Checking prerequisites")
	Expect(CheckPrerequisites()).To(Succeed())

	By("Creating Hetzner cluster")
	cluster = NewHetznerCluster(cfg)

	// Ensure cleanup runs even if Create fails partway through.
	// Uses a fresh context since the spec context will be cancelled by then.
	DeferCleanup(func() {
		if cluster == nil {
			return
		}
		log.Println("[hetzner] cleaning up Hetzner resources...")
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cleanupCancel()
		if err := cluster.Destroy(cleanupCtx); err != nil {
			log.Printf("[hetzner] WARNING: cleanup failed: %v", err)
		}
	})

	Expect(cluster.Create(ctx)).To(Succeed())

	By("Cluster created successfully")
	for i, ip := range cluster.ServerIPs() {
		log.Printf("[hetzner] node %d: %s", i, ip)
	}

	By("Bootstrapping Talos cluster and building controller image in parallel")
	var err2 error
	talosCluster, err2 = NewTalosCluster(cluster.RunID, cfg.TalosFromVersion, cfg.K8sFromVersion, cluster.ServerIPs())
	Expect(err2).NotTo(HaveOccurred())

	DeferCleanup(func() {
		if talosCluster != nil {
			talosCluster.Cleanup()
		}
	})

	// Run bootstrap and image build concurrently — they're independent.
	type imageResult struct {
		image string
		err   error
	}
	imageCh := make(chan imageResult, 1)
	go func() {
		img, err := BuildAndPushImage(ctx, cfg, cluster.RunID)
		imageCh <- imageResult{img, err}
	}()

	Expect(talosCluster.Bootstrap(ctx)).To(Succeed())
	log.Printf("[talos] kubeconfig: %s", talosCluster.Kubeconfig)
	log.Printf("[talos] talosconfig: %s", talosCluster.TalosConfig)

	By("Waiting for controller image build to finish")
	imgRes := <-imageCh
	Expect(imgRes.err).NotTo(HaveOccurred(), "building controller image")
	image := imgRes.image
	log.Printf("[deploy] image: %s", image)

	By("Deploying controller via Helm")
	talosConfigData, err4 := talosCluster.TalosConfigData()
	Expect(err4).NotTo(HaveOccurred())
	Expect(DeployController(ctx, talosCluster.Kubeconfig, image, talosConfigData)).To(Succeed())

	By("Waiting for controller to be ready")
	Expect(WaitForController(ctx, talosCluster.Kubeconfig)).To(Succeed())
	log.Printf("[deploy] controller is ready")

	By("Starting background log streaming")
	bgCtx, bgCancel = context.WithCancel(context.Background())
	DeferCleanup(bgCancel)
	streamLogs(bgCtx, talosCluster.Kubeconfig, controllerNamespace, "tuppr", "[tuppr]")
	watchResource(bgCtx, talosCluster.Kubeconfig, "talosupgrade", "[watch/talosupgrade]")
	watchResource(bgCtx, talosCluster.Kubeconfig, "kubernetesupgrade", "[watch/k8supgrade]")
}, NodeTimeout(40*time.Minute))
