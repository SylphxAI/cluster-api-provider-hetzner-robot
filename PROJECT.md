# CAPHR

`SylphxAI/cluster-api-provider-hetzner-robot` provides CAPHR, a Cluster API
infrastructure provider for Hetzner Robot bare-metal servers running Talos
Linux.

## Lifecycle

- State: `active`
- Layer: `integration`
- Machine manifest: [`.doctrine/project.json`](./.doctrine/project.json)

## Goals

- Provide the Go controller, APIs, CRDs, RBAC, examples, and container image for
  Hetzner Robot bare-metal Cluster API infrastructure.
- Own the Talos-specific provisioning flow that discovers hardware facts,
  injects them into machine config, and applies config via Talos APIs.
- Keep Host ownership, destructive provisioning policy, remediation, generated
  CRDs, tests, docs, and release artifacts coherent.

## Non-Goals

- This repo does not own generic Hetzner Cloud support, kubeadm/Ubuntu flows,
  cloud-init, application workloads, or product cluster policy outside the
  provider contract.
- This repo does not own external storage safety decisions except through the
  narrow `HetznerRobotHostRelease` authorization surface documented here.

## Boundary

CAPHR owns the infrastructure-provider contract for Hetzner Robot bare metal and
Talos. Consumers use its CRDs, controller image, examples, docs, and Go module
as public surfaces. Product-specific cluster topology, workload policy,
backups, Ceph health, and GitOps environment ownership live outside this repo
unless explicitly expressed through CAPHR's documented CRDs.

## Public Surfaces

- Go module: `go.mod`
- CRDs and API types: `api/v1alpha1/`, `config/crd/bases/`
- Controller and policies: `controllers/`, `pkg/`
- Container image workflow: `.github/workflows/build.yml`
- CI workflow: `.github/workflows/ci.yml`
- Docs and examples: `README.md`, `docs/`, `examples/`

## Delivery

Pull requests and merge queue refs run Go CI with gofmt checks and `go test
./...`. Main/tag builds push the controller image to GHCR and create GitHub
releases for tags. Production proof for behavior changes requires CI, generated
CRD/RBAC freshness, image digest or registry readback, and cluster reconciliation
or smoke evidence for infrastructure-impacting changes.
