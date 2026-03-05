# cluster-api-provider-hetzner-robot (CAPHR)

A [Cluster API](https://cluster-api.sigs.k8s.io/) infrastructure provider for **Hetzner Robot bare metal servers** running **Talos Linux**.

## Why CAPHR?

[CAPH (syself/cluster-api-provider-hetzner)](https://github.com/syself/cluster-api-provider-hetzner) uses SSH + cloud-init for bare metal provisioning, which is fundamentally incompatible with Talos Linux (no SSH, no cloud-init).

CAPHR is purpose-built for Talos on Hetzner dedicated servers:
- Uses Hetzner Robot API for server lifecycle (rescue mode, reset, power)
- Uses SSH **only in rescue mode** to write the Talos raw disk image
- Uses the **Talos gRPC API (port 50000)** to deliver machineconfig — no cloud-init needed
- Works alongside [CAPT (siderolabs/cluster-api-bootstrap-provider-talos)](https://github.com/siderolabs/cluster-api-bootstrap-provider-talos)

## Architecture

```
CAPI (cluster-api)                    — core lifecycle
  └── CAPT (bootstrap/controlplane)   — machineconfig generation
  └── CAPHR (infrastructure)          — Hetzner Robot bare metal
        ├── Robot API → rescue mode → SSH → dd Talos image → reboot
        └── Talos API (port 50000) → apply machineconfig → node Ready
```

## Provisioning Flow

1. `HetznerRobotMachine` created → CAPHR calls Robot API to activate rescue mode
2. Hardware reset → server boots into Hetzner rescue Linux
3. SSH into rescue → `curl talos-image | xzcat | dd of=/dev/nvme0n1`
4. Reboot into Talos maintenance mode (gRPC port 50000)
5. Get bootstrap machineconfig from CAPT bootstrap secret
6. `talosctl apply-config --insecure --nodes <ip>` → Talos configures itself
7. Talos joins K8s cluster → `status.ready = true`

## CRDs

- `HetznerRobotCluster` — cluster-level config (Robot API secret, SSH secret)
- `HetznerRobotMachine` — per-server config (serverID, Talos schematic, version, disk)
- `HetznerRobotMachineTemplate` — template for `MachineDeployment`

## Status

🚧 **Under active development** — built by [Sylphx AI](https://sylphx.com)

## License

Apache 2.0
