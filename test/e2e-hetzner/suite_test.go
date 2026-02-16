//go:build e2e_hetzner

package e2ehetzner

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var (
	cfg     *Config
	cluster *HetznerCluster
	testCtx context.Context
	cancel  context.CancelFunc
)

func TestE2EHetzner(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "E2E Hetzner Suite")
}

var _ = BeforeSuite(func() {
	testCtx, cancel = context.WithTimeout(context.Background(), 40*time.Minute)

	var err error

	By("Loading configuration")
	cfg, err = LoadConfig()
	Expect(err).NotTo(HaveOccurred())

	By("Checking prerequisites")
	Expect(CheckPrerequisites()).To(Succeed())

	By("Creating Hetzner cluster")
	cluster = NewHetznerCluster(cfg)

	// Ensure cleanup runs even if Create fails partway through
	DeferCleanup(func() {
		if cluster == nil {
			return
		}
		By("Destroying Hetzner cluster")
		// Use a fresh context for cleanup in case the test context was cancelled
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cleanupCancel()
		err := cluster.Destroy(cleanupCtx)
		if err != nil {
			GinkgoWriter.Printf("WARNING: cleanup failed: %v\n", err)
		}
	})

	Expect(cluster.Create(testCtx)).To(Succeed())

	By("Cluster created successfully")
	for i, ip := range cluster.ServerIPs() {
		GinkgoWriter.Printf("  Node %d: %s\n", i, ip)
	}
})

var _ = AfterSuite(func() {
	if cancel != nil {
		cancel()
	}
})

var _ = Describe("Infrastructure", Ordered, func() {
	It("should have created 3 servers with public IPs", func() {
		Expect(cluster).NotTo(BeNil())
		ips := cluster.ServerIPs()
		Expect(ips).To(HaveLen(3))
		for i, ip := range ips {
			Expect(ip).NotTo(BeEmpty(), "server %d has no IP", i)
			GinkgoWriter.Printf("Server %d IP: %s\n", i, ip)
		}
	})
})
