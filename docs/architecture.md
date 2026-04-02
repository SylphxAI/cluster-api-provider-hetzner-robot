# Architecture: Why Bare Metal CAPI Providers Are OS-Specific

This document explains why CAPHR is Talos-specific, why this is the correct design, and how it relates to the broader CAPI ecosystem.

## The CAPI Layering Model

CAPI (Cluster API) separates concerns into three pluggable providers:

```
CAPI Core (OS-agnostic)
├── Infrastructure Provider — manages hardware (create/destroy/reset machines)
├── Bootstrap Provider     — generates OS config (machineconfig / cloud-init)
└── Control Plane Provider — manages control plane lifecycle (scaling, upgrades)
```

In theory, the infrastructure provider should be OS-agnostic. You should be able to pair any infra provider with any bootstrap provider.

**In practice, this only works for cloud VMs.**

## Cloud vs Bare Metal: The Metadata Service Gap

### Cloud VMs

Cloud providers expose a metadata service (169.254.169.254) that the OS reads on boot:

```
Infra Provider                           Bootstrap Provider
  │                                        │
  ├─ API: create VM                        ├─ Generate cloud-init userdata
  ├─ API: attach userdata ◄────────────────┘
  ├─ VM boots
  │                                        cloud-init reads metadata service
  │                                        cloud-init applies userdata
  └─ Report: VM running                   └─ Report: bootstrap complete
```

The infra provider writes userdata as an **opaque blob** — it never parses or modifies it. The OS (via cloud-init) pulls config from the metadata service autonomously. Clean separation.

### Bare Metal

No metadata service exists. The infra provider must **directly interact with the OS** to deliver configuration:

```
Infra Provider                           Bootstrap Provider
  │                                        │
  ├─ Robot API: activate rescue            ├─ Generate machineconfig
  ├─ SSH: write OS image to disk           ├─ Store in K8s Secret
  ├─ Robot API: reboot                     │
  ├─ Detect OS is booted ◄─── OS-specific  │  (CABPT does NOT connect
  ├─ Push config to OS    ◄─── OS-specific  │   to any machine)
  └─ Report: provisioned                   └─ Done
```

The "detect OS is booted" and "push config to OS" steps are inherently OS-specific:

| OS | Detection | Config Delivery |
|----|-----------|-----------------|
| Talos | gRPC port 50000 in maintenance mode | `ApplyConfiguration` gRPC call |
| Ubuntu/kubeadm | SSH port 22 open | SSH + `kubeadm init` |
| Flatcar | SSH port 22 open | SSH + write Ignition |

There is no abstraction that covers all three. The infra provider **must** know the OS.

## Why This Is Not an Antipattern

Sidero Metal (by Siderolabs, creators of Talos) is the canonical bare metal provider for Talos. It is equally Talos-specific — it speaks the Talos gRPC API directly, generates Talos machineconfigs, and manages Talos-specific lifecycle states.

CAPH (syself) is equally kubeadm-specific — it relies on SSH + `installimage` + cloud-init throughout the provisioning flow.

**Every production bare metal CAPI provider is OS-specific.** This is not a design flaw in any individual provider — it is a fundamental property of bare metal provisioning.

## The SoC Boundary That Matters

Since OS coupling is unavoidable, the meaningful separation of concerns is **what the infra provider injects into the config**.

### Correct: Hardware Facts

CAPHR injects information discovered at provisioning time that CABPT cannot know when generating the machineconfig:

- **MAC address** — detected via `ip link` in rescue. NIC names differ between rescue (`eth0`) and Talos (`enp193s0f0np0`); MAC is stable.
- **Gateway IP** — detected via `ip route` in rescue. Hetzner-assigned, varies per server.
- **Install disk** — resolved to stable `/dev/disk/by-id/` path in rescue. NVMe enumeration order differs between boots.
- **Hostname** — deterministic from server ID + datacenter: `compute-fsn1-2938104`.
- **VLAN IP + static routes** — from HetznerRobotHost spec. Per-server network assignment.
- **IPv6 address + routes** — from Robot API `server_ipv6_net`. Per-server allocation.
- **Provider ID** — `hetzner-robot://{serverID}`. Required for CAPI Machine-to-Node matching.
- **Cluster-level secrets** — secretbox encryption key and service account key. CABPT generates unique keys per Machine, but all control plane nodes must share the same keys for etcd and token validation.

### Incorrect: Application Config

The infra provider should **never** inject:

- CNI configuration (Cilium taints, network policies)
- Container runtime settings (Kata, gVisor)
- Workload-specific config (resource limits, scheduling)
- Monitoring/observability agents

These belong in the TalosControlPlane or TalosConfigTemplate specs, managed by CABPT/CACPPT — the layer that **owns** the desired OS configuration.

## CABPT/CACPPT: Config Generators, Not Config Deliverers

A common misconception is that the bootstrap provider should deliver config to the machine. In CAPI's model, bootstrap providers are **config generators only**:

1. CACPPT/CABPT watches Machine objects
2. Generates machineconfig YAML from the TalosControlPlane/TalosConfigTemplate spec
3. Stores the config in a Kubernetes Secret
4. Sets `Machine.Spec.Bootstrap.DataSecretName`
5. **Done.** It never connects to any machine.

The infrastructure provider reads the Secret and handles delivery. For cloud VMs, "delivery" means writing userdata to the cloud API. For bare metal, "delivery" means pushing config to the OS API.

This is why moving config delivery out of CAPHR into CABPT was never a viable option — CABPT is architecturally incapable of it.

## The Lifecycle Ordering Problem

A related question: why can't CABPT generate a complete machineconfig that includes hardware facts?

Because hardware facts are discovered **after** the machineconfig is generated:

```
T1: CABPT generates machineconfig → Secret
    (MAC, gateway, disk: unknown — machine hasn't booted yet)

T2: CAPHR activates rescue, boots server

T3: CAPHR SSHes into rescue, discovers:
    MAC = aa:bb:cc:dd:ee:ff
    Gateway = 138.199.242.129
    Disk = /dev/disk/by-id/nvme-Samsung_SSD_990_PRO_...

T4: CAPHR injects facts into machineconfig from T1

T5: CAPHR delivers augmented config to Talos
```

The HetznerRobotHost CR pre-stores some facts (serverIP, internalIP) so they're known before provisioning. But MAC, gateway, and stable disk path can only be discovered by SSHing into the running rescue environment.

## Multi-Document Config: Clean Augmentation

Talos supports multi-document YAML configs. CAPHR uses this for storage-specific config (VolumeConfig, RawVolumeConfig) — appending separate YAML documents rather than modifying the base machineconfig from CABPT.

Hardware facts that must modify the base document (MAC in deviceSelector, hostname, routes) use structured YAML manipulation via `modifyFirstDocument()`. Each inject function:

- Operates on a single concern
- Is independently unit-tested
- Uses `ensureMap()` for safe nested access
- Never does string replacement on YAML

## Summary

| Concern | Owner | Rationale |
|---------|-------|-----------|
| Machineconfig generation | CABPT/CACPPT | Cluster-level config: certs, etcd, kubelet, networking baseline |
| Hardware lifecycle | CAPHR | Hetzner Robot API: rescue, reset, power, server info |
| Hardware fact discovery | CAPHR | SSH in rescue: MAC, gateway, disk, EFI |
| Hardware fact injection | CAPHR | Must augment CABPT config with runtime-discovered facts |
| Config delivery | CAPHR | Bare metal has no metadata service; direct gRPC push required |
| Application config | CABPT/CACPPT templates | CNI, runtime, workload settings belong in TalosControlPlane spec |
| Day-2 config changes | CAPI in-place updates | Future: CAPI v1.12 RuntimeSDK extension (ADR-034) |
