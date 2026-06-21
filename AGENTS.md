# Repository Agent Instructions

This repository follows the central doctrine in
[SylphxAI/doctrine](https://github.com/SylphxAI/doctrine).

Before changing behavior, read [PROJECT.md](./PROJECT.md) and
[.doctrine/project.json](./.doctrine/project.json). Keep enterprise policy in
doctrine; keep only repo-local CAPHR facts here.

Useful validation for provider changes:

- `gofmt -w` on changed Go files, then confirm no gofmt drift
- `go test ./...`
- CRD/RBAC/generated manifest freshness checks for API or controller contract
  changes
- Image digest or GHCR readback for release-impacting changes

Do not mutate shared infrastructure manually from this repo. Desired provider
state, CRDs, RBAC, examples, image releases, and recovery notes must flow through
Git and the documented delivery path. Do not add product-specific cluster
topology, workload policy, backup policy, or GitOps environment assumptions to
the provider core.
