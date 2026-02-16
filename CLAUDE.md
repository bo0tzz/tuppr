# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Tuppr is a Kubernetes controller for managing automated upgrades of Talos Linux nodes and Kubernetes clusters. Built with Kubebuilder (go.kubebuilder.io/v4) and controller-runtime. It defines two cluster-scoped CRDs: `TalosUpgrade` and `KubernetesUpgrade` under API group `tuppr.home-operations.com/v1alpha1`.

## Build & Development Commands

```bash
make build              # Build manager binary (runs manifests, generate, fmt, vet, helm-crds)
make test               # Unit tests (excludes e2e and integration)
make test-integration   # Integration tests with envtest
make test-e2e           # E2E tests using Kind cluster (creates/destroys automatically)
make lint               # Run golangci-lint (v2, config in .golangci.yml)
make lint-fix           # Lint with auto-fix
make manifests          # Generate CRDs, RBAC, webhook configs
make generate           # Generate DeepCopy methods
make fmt                # go fmt
make vet                # go vet
```

Run a single unit test:
```bash
go test ./internal/controller/talosupgrade/ -run TestSpecificName -v
```

Run a single integration test:
```bash
go test ./test/integration/ -v -ginkgo.v -ginkgo.focus="test description"
```

## Architecture

### Two Controllers, One Coordination Model

Both controllers follow the same pattern: reconcile loop → check suspend/reset annotations → check generation change → check phase → maintenance window → coordination → process upgrade.

- **TalosUpgrade controller** (`internal/controller/talosupgrade/`): Upgrades Talos OS on nodes sequentially (one node at a time). Creates Jobs that run `talosctl upgrade` against each node. Supports per-node version/schematic overrides via annotations on Node objects.
- **KubernetesUpgrade controller** (`internal/controller/kubernetesupgrade/`): Upgrades Kubernetes by running `talosctl upgrade-k8s` from a controller node. Single Job execution.

Cross-upgrade coordination (`internal/controller/coordination/`): Only one upgrade type (Talos or Kubernetes) can be InProgress at a time. Multiple TalosUpgrade plans are queued FIFO.

### Upgrade Lifecycle Phases

Defined in `internal/constants/constants.go`: `Pending` → `InProgress` → `Completed` | `Failed`

### Key Internal Packages

- **`internal/healthcheck/`**: CEL-based health check evaluator. Fetches arbitrary K8s resources and evaluates CEL expressions against them. Variables available in CEL: `object` (full resource) and `status` (status subresource).
- **`internal/controller/maintenance/`**: Cron-based maintenance window evaluation.
- **`internal/metrics/`**: Prometheus metrics reporter (`tuppr_*` prefix).
- **`internal/webhook/`**: Validating webhooks for both CRDs. TalosUpgrade webhook also checks for node selector overlaps between plans. KubernetesUpgrade webhook enforces singleton constraint.
- **`internal/webhook/validation/`**: Shared validation logic used by both webhooks.
- **`internal/talos/`**: Talos API client wrapper.
- **`internal/image/`**: Container image existence checker.
- **`internal/controller/nodeutil/`**: Node filtering and clock utilities.

### Testing Patterns

- **Unit tests**: Standard Go tests alongside source files, using gomega matchers. Controller tests use fake client.
- **Integration tests** (`test/integration/`): Ginkgo/Gomega with envtest (real API server, no kubelet). Uses a job simulator goroutine that auto-completes Jobs. Mock interfaces for TalosClient, HealthChecker, and VersionGetter are injected into reconcilers.
- **E2E tests** (`test/e2e/`): Run against Kind cluster with real controller deployment.

### CRD Design Notes

- Both CRDs are **cluster-scoped** (not namespaced).
- Status updates use JSON merge patches via `client.RawPatch` rather than full status updates.
- Controllers use finalizers for cleanup.
- Annotations control operational behavior: `tuppr.home-operations.com/suspend`, `tuppr.home-operations.com/reset`, `tuppr.home-operations.com/version` (node override), `tuppr.home-operations.com/schematic` (node override).

### Webhook Certificate Management

The controller manages its own webhook TLS certificates using `cert-controller` (rotator). Certificates are self-signed and rotated automatically. The webhook server starts only after cert rotation setup completes (gated by a channel).
