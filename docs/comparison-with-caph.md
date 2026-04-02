# CAPHR vs CAPH: Design Comparison

## Background

[CAPH (syself/cluster-api-provider-hetzner)](https://github.com/syself/cluster-api-provider-hetzner) is the community Hetzner CAPI provider. It supports both HCloud (cloud VMs) and Robot (dedicated servers).

CAPHR was built because CAPH's bare metal provisioning is incompatible with Talos Linux.

## Provisioning Architecture

### CAPH Bare Metal Flow

```
Robot API: activate rescue
  → SSH: installimage (Debian/Ubuntu)
  → SSH: configure cloud-init
  → Reboot
  → SSH: verify boot
  → cloud-init: kubeadm init/join
  → Node joins cluster
```

SSH is used **throughout the entire flow** — from OS installation through bootstrap. cloud-init handles userdata delivery. This couples the infra provider to SSH-capable, cloud-init-capable operating systems.

### CAPHR Flow

```
Robot API: activate rescue
  → SSH: curl | dd (raw Talos image)    ← SSH only for disk write
  → Reboot
  → Talos gRPC: detect maintenance mode ← OS-specific detection
  → Talos gRPC: ApplyConfiguration      ← OS-specific delivery
  → Node joins cluster
```

SSH is used **only in rescue** to write the disk image. After reboot, all interaction is via the Talos gRPC API. This is a narrower, more explicit coupling.

## Key Differences

| Aspect | CAPH | CAPHR |
|--------|------|-------|
| **Target OS** | Ubuntu/Debian (kubeadm + cloud-init) | Talos Linux |
| **SSH usage** | Throughout lifecycle | Rescue only (disk write) |
| **Config delivery** | cloud-init userdata | Talos gRPC `ApplyConfiguration` |
| **NIC identification** | `nic-info.sh` script (fragile) | MAC-based `deviceSelector` (stable) |
| **IP detection** | SSH script in rescue | Robot API `server_ip` / `server_ipv6_net` |
| **Static routing** | Not addressed | /32 address + explicit gateway (L2 isolation fix) |
| **Disk path** | Device name (`/dev/sda`) | Stable `/dev/disk/by-id/` path |
| **EFI management** | Not addressed | `efibootmgr` boot order fix |
| **State machine** | 9 states (map-of-handlers) | 11 states (switch-based) |
| **Error handling** | Uniform retry | Transient vs permanent distinction + exponential backoff |
| **Status storage** | In Spec (CAPI `clusterctl move` workaround) | In Status (K8s convention) |
| **Host claiming** | Label-based pool | `hostRef` (static) or `hostSelector` (pool) |
| **Remediation** | Reboot via SSH | Hardware reset via Robot API |
| **IPv6** | Not supported on bare metal | Dual-stack: IPv4 VLAN + IPv6 public |
| **Storage nodes** | Not addressed | `ephemeralSize` → VolumeConfig + OSD partition |
| **Codebase** | ~15k LOC (bare metal portion) | ~3.4k LOC total |

## What Could Be Contributed Upstream

Standalone improvements that benefit CAPH without requiring architectural changes:

1. **Robot API IP detection** (CAPH Issue #1642) — Read `server_ip`/`server_ipv6_net` from Robot API instead of fragile SSH scripts.

2. **`hrobot-go` client improvements** — Retry logic, rate-limit handling, IPv6 parsing, rescue mode verification.

3. **MAC-based NIC identification** — Replace `nic-info.sh` with `deviceSelector.hardwareAddr` for stable identification across reboots.

4. **Exponential backoff with error classification** — Distinguish transient (network, SSH timeout) from permanent (missing secret, invalid config) errors.

5. **MachineHealthCheck remediation patterns** — Hardware-reset-based remediation with retry limits.

6. **Documentation** — Hetzner bare metal operational knowledge: L2 isolation, EFI boot order, NVMe device instability, Ceph OSD coexistence.

## What Cannot Be Contributed

These require fundamental architecture changes incompatible with CAPH's SSH + cloud-init model:

- Talos gRPC provisioning
- Maintenance mode detection
- Multi-document machineconfig
- VLAN injection via structured YAML manipulation
- Static /32 routing (addresses CAPH's assumption of DHCP)

## Why Not Merge Into CAPH

1. **Talos support explicitly declined** — CAPH Issue #133 (closed Aug 2024). Maintainers: "We currently see no benefit in supporting Talos."

2. **Single maintainer bottleneck** — ~95% of CAPH commits by one person (guettli). Complex changes would sit in review for weeks.

3. **Incompatible provisioning model** — CAPH's SSH-throughout flow vs CAPHR's rescue-only-SSH + gRPC. These are parallel architectures, not incremental improvements.

4. **Architectural debt in CAPH** — Status-in-Spec (#1333), Update-vs-Patch conflicts (#1587), planned refactoring (#1681). Clean integration would require waiting for these to be resolved.

5. **Business misalignment** — Syself sells managed Kubernetes on Hetzner. A provider that enables self-managed Talos clusters competes with their offering.

## Recommended Strategy

1. **Cherry-pick standalone improvements** to CAPH — good citizenship, builds reputation.
2. **Keep CAPHR as an independent provider** — fills a gap the CAPH maintainers won't fill.
3. **Potential open-source publication** — as the canonical Talos-on-Hetzner-Robot solution.
