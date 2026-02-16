//go:build e2e_hetzner

package e2ehetzner

import (
	"fmt"
	"log"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Upgrades run in a single Ordered container to ensure TalosUpgrade completes
// before KubernetesUpgrade starts (K8s v1.35 requires Talos v1.12).
var _ = Describe("Upgrades", Ordered, func() {
	Describe("TalosUpgrade", func() {
		It("should upgrade all nodes to the target Talos version", func(ctx SpecContext) {
			By("Verifying all nodes are on the initial Talos version")
			versions, err := getTalosNodeVersions(ctx, talosCluster.TalosConfig, talosCluster.ServerIPs)
			Expect(err).NotTo(HaveOccurred())
			for ip, v := range versions {
				log.Printf("[talos-upgrade] node %s: %s", ip, v)
				Expect(v).To(Equal(cfg.TalosFromVersion), "node %s should be on %s", ip, cfg.TalosFromVersion)
			}

			By("Creating TalosUpgrade CR")
			manifest := fmt.Sprintf(`apiVersion: tuppr.home-operations.com/v1alpha1
kind: TalosUpgrade
metadata:
  name: e2e-talos-upgrade
spec:
  talos:
    version: %q
  policy:
    debug: true
    rebootMode: default
`, cfg.TalosToVersion)
			Expect(kubectlApply(ctx, talosCluster.Kubeconfig, manifest)).To(Succeed())

			By("Waiting for TalosUpgrade to start")
			Eventually(func(g Gomega) {
				phase, err := getCRPhase(ctx, talosCluster.Kubeconfig, "talosupgrade", "e2e-talos-upgrade")
				g.Expect(err).NotTo(HaveOccurred())
				log.Printf("[talos-upgrade] phase: %s", phase)
				g.Expect(phase).To(Or(Equal("InProgress"), Equal("Completed")))
			}, 2*time.Minute, 10*time.Second).Should(Succeed())

			By("Waiting for TalosUpgrade to complete")
			Eventually(func(g Gomega) {
				phase, err := getCRPhase(ctx, talosCluster.Kubeconfig, "talosupgrade", "e2e-talos-upgrade")
				g.Expect(err).NotTo(HaveOccurred())
				log.Printf("[talos-upgrade] phase: %s", phase)
				g.Expect(phase).NotTo(Equal("Failed"), "upgrade entered Failed phase ")
				g.Expect(phase).To(Equal("Completed"))
			}, 20*time.Minute, 30*time.Second).Should(Succeed())

			By("Verifying all nodes are on the target Talos version")
			versions, err = getTalosNodeVersions(ctx, talosCluster.TalosConfig, talosCluster.ServerIPs)
			Expect(err).NotTo(HaveOccurred())
			for ip, v := range versions {
				log.Printf("[talos-upgrade] node %s: %s", ip, v)
				Expect(v).To(Equal(cfg.TalosToVersion), "node %s should be on %s", ip, cfg.TalosToVersion)
			}

			By("Verifying status details")
			status, err := getCRStatus(ctx, talosCluster.Kubeconfig, "talosupgrade", "e2e-talos-upgrade")
			Expect(err).NotTo(HaveOccurred())

			completedNodes, ok := status["completedNodes"].([]any)
			Expect(ok).To(BeTrue(), "completedNodes should be a list")
			Expect(completedNodes).To(HaveLen(len(talosCluster.ServerIPs)), "all nodes should be completed")

			failedNodes, _ := status["failedNodes"].([]any)
			Expect(failedNodes).To(BeEmpty(), "no nodes should have failed")

			log.Printf("[talos-upgrade] upgrade completed successfully")
		}, NodeTimeout(25*time.Minute))
	})

	Describe("KubernetesUpgrade", func() {
		It("should upgrade Kubernetes to the target version", func(ctx SpecContext) {
			By("Getting the current Kubernetes version")
			currentVersion, err := kubectlOutput(ctx, talosCluster.Kubeconfig,
				"version", "-o", "json",
			)
			Expect(err).NotTo(HaveOccurred())
			log.Printf("[k8s-upgrade] current cluster version info:\n%s", currentVersion)

			By("Creating KubernetesUpgrade CR")
			manifest := fmt.Sprintf(`apiVersion: tuppr.home-operations.com/v1alpha1
kind: KubernetesUpgrade
metadata:
  name: e2e-k8s-upgrade
spec:
  kubernetes:
    version: %q
`, cfg.K8sToVersion)
			Expect(kubectlApply(ctx, talosCluster.Kubeconfig, manifest)).To(Succeed())

			By("Waiting for KubernetesUpgrade to start")
			Eventually(func(g Gomega) {
				phase, err := getCRPhase(ctx, talosCluster.Kubeconfig, "kubernetesupgrade", "e2e-k8s-upgrade")
				g.Expect(err).NotTo(HaveOccurred())
				log.Printf("[k8s-upgrade] phase: %s", phase)
				g.Expect(phase).To(Or(Equal("InProgress"), Equal("Completed")))
			}, 2*time.Minute, 10*time.Second).Should(Succeed())

			By("Waiting for KubernetesUpgrade to complete")
			Eventually(func(g Gomega) {
				phase, err := getCRPhase(ctx, talosCluster.Kubeconfig, "kubernetesupgrade", "e2e-k8s-upgrade")
				g.Expect(err).NotTo(HaveOccurred())
				log.Printf("[k8s-upgrade] phase: %s", phase)
				g.Expect(phase).NotTo(Equal("Failed"), "upgrade entered Failed phase ")
				g.Expect(phase).To(Equal("Completed"))
			}, 15*time.Minute, 30*time.Second).Should(Succeed())

			By("Verifying Kubernetes version on all nodes")
			nodesVersion, err := kubectlOutput(ctx, talosCluster.Kubeconfig,
				"get", "nodes",
				"-o", "jsonpath={.items[*].status.nodeInfo.kubeletVersion}",
			)
			Expect(err).NotTo(HaveOccurred())
			for _, v := range strings.Fields(nodesVersion) {
				log.Printf("[k8s-upgrade] node kubelet version: %s", v)
				Expect(v).To(Equal(cfg.K8sToVersion), "kubelet should be on %s", cfg.K8sToVersion)
			}

			By("Verifying status details")
			status, err := getCRStatus(ctx, talosCluster.Kubeconfig, "kubernetesupgrade", "e2e-k8s-upgrade")
			Expect(err).NotTo(HaveOccurred())

			phase, _ := status["phase"].(string)
			Expect(phase).To(Equal("Completed"))

			lastError, _ := status["lastError"].(string)
			Expect(lastError).To(BeEmpty(), "should have no error")

			log.Printf("[k8s-upgrade] upgrade completed successfully")
		}, NodeTimeout(20*time.Minute))
	})
})
