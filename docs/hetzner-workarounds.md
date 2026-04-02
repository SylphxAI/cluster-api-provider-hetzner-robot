# Hetzner Bare Metal Workarounds

Hetzner dedicated servers have networking and hardware behaviors that require specific workarounds. This document explains each issue and how CAPHR addresses it.

## L2 Isolation and Static /32 Routing

### Problem

Hetzner DHCP assigns /25 or /26 prefixes to dedicated servers. When two servers share the same subnet (e.g., 138.199.242.217/25 and 138.199.242.218/25), the Linux kernel creates an on-link route for the entire /25. Traffic to the other server goes via ARP — but Hetzner **blocks direct L2 traffic between servers**. Result: SSH, kubelet, and all inter-node communication fails silently.

### Solution

Configure static /32 addresses instead of DHCP. This eliminates on-link routes entirely:

```yaml
# Talos machineconfig (injected by CAPHR)
machine:
  network:
    interfaces:
      - deviceSelector:
          hardwareAddr: "aa:bb:cc:dd:ee:ff"
        addresses:
          - "138.199.242.217/32"          # /32 = no on-link route
        routes:
          - network: "0.0.0.0/0"
            gateway: "138.199.242.129"    # Default route via gateway
          - network: "138.199.242.129/32" # On-link route for gateway itself
```

The on-link route for the gateway (`138.199.242.129/32` with no gateway field) tells the kernel the next hop is directly reachable on the interface. Without it, the default route fails with "network unreachable" because with a /32 address, there's no path to the gateway.

### How CAPHR Implements This

`injectVLANConfig()` replaces any DHCP config on the primary NIC with static /32 addressing. The gateway IP is auto-detected via `ip route | grep default` during rescue and stored in `HetznerRobotMachine.Status.GatewayIP`.

## NVMe Device Name Instability

### Problem

NVMe device names (`/dev/nvme0n1`, `nvme1n1`, etc.) depend on PCI probe order, which can differ between:
- Hetzner rescue Linux and Talos
- Reboots (rare but possible)
- Firmware updates

A disk installed as `/dev/nvme0n1` in rescue might appear as `/dev/nvme1n1` in Talos, causing Talos to mount the wrong disk or fail to boot.

### Solution

During rescue, resolve the device name to a stable `/dev/disk/by-id/` path using `readlink -f`:

```bash
# In rescue: /dev/nvme0n1 → /dev/disk/by-id/nvme-Samsung_SSD_990_PRO_4TB_S7KGNU0X123456
readlink -f /dev/disk/by-id/nvme-Samsung_SSD_990_PRO_*
```

This path is hardware-serial-based and stable across reboots, rescue/Talos transitions, and PCI reordering.

### How CAPHR Implements This

`sshrescue.ResolveStableDiskPath()` maps the spec-provided device name to a `/dev/disk/by-id/` path during rescue. The result is stored in `HetznerRobotMachine.Status.ResolvedInstallDisk` and used in both the `dd` command and the machineconfig `machine.install.disk`.

## EFI Boot Order

### Problem

After installing Talos via `dd`, the EFI boot order may still have PXE (network boot) as the first entry. On the next reboot, the server network-boots into Hetzner's PXE environment instead of Talos.

This is particularly common after rescue mode, which activates PXE boot.

### Solution

After writing the Talos image but before rebooting, use `efibootmgr` to:
1. Identify the Talos boot entry and PXE boot entry
2. Delete stale entries (old OS installs, duplicate entries)
3. Set boot order: Talos first, PXE last

```bash
# Delete non-Talos, non-PXE entries
efibootmgr -B -b 0003  # Old Ubuntu entry

# Set boot order: Talos (0001) first, PXE (0002) last
efibootmgr -o 0001,0002
```

### How CAPHR Implements This

EFI boot order manipulation runs in rescue after the `dd` image write and before the final reboot. Handles both UEFI and legacy BIOS firmware (some Hetzner servers have hybrid firmware).

## Primary NIC Identification

### Problem

Network interface names differ between environments:
- Hetzner rescue Linux: `eth0`
- Talos: `enp193s0f0np0` (PCI slot-based)
- Other Hetzner hardware: varies by NIC model

Hardcoding interface names breaks across environments and hardware models.

### Solution

Use MAC address for NIC identification. MAC is hardware-burned and stable across all environments:

```yaml
# Instead of: interface: enp193s0f0np0 (breaks in rescue)
deviceSelector:
  hardwareAddr: "aa:bb:cc:dd:ee:ff"   # Works everywhere
  physical: true
```

### How CAPHR Implements This

During rescue, `sshrescue` detects the primary NIC MAC via:
```bash
ip route show default | awk '{print $5}'  # Get default route interface
ip link show eth0 | grep ether            # Get MAC of that interface
```

The MAC is stored in `HetznerRobotMachine.Status.PrimaryMAC` and used in all Talos network config injections (VLAN, IPv6, static routing).

## Ceph OSD Coexistence

### Problem

Storage servers may have multiple NVMe drives. Some are used as Ceph OSDs (with BlueStore signature), some should be used for the Talos OS install. Installing Talos on a Ceph OSD disk destroys cluster data.

Additionally, Talos's EPHEMERAL partition (for kubelet data) defaults to consuming all remaining disk space, leaving no room for Ceph OSDs on the OS disk.

### Solution

**Disk selection**: Check for Ceph BlueStore signatures before install:
```bash
blkid /dev/nvme0n1* | grep ceph_bluestore
```
Refuse to install on disks with active Ceph data.

**Partition layout**: Use Talos v1.12+ VolumeConfig to limit EPHEMERAL and create OSD partitions:
```yaml
apiVersion: v1alpha1
kind: VolumeConfig
name: EPHEMERAL
provisioning:
  maxSize: 100GiB
---
apiVersion: v1alpha1
kind: RawVolumeConfig
name: osd-data
provisioning:
  diskSelector:
    match: system_disk
  minSize: 50GB
```

The OSD partition appears at `/dev/disk/by-partlabel/r-osd-data`, which Ceph/Rook can consume. The partition name must **not** contain "ceph" — Ceph's device inventory rejects disks with "ceph" in the PARTLABEL.

### How CAPHR Implements This

- `sshrescue.ResolveInstallDisk()` checks `blkid` for BlueStore signatures
- `HetznerRobotMachine.Spec.EphemeralSize` triggers VolumeConfig + RawVolumeConfig document generation
- These documents are appended to the multi-document machineconfig (not injected into the base config)

## IPv6 Dual-Stack

### Problem

Each Hetzner dedicated server gets a /64 IPv6 subnet. Without explicit configuration, Talos only uses IPv4, and Kubernetes doesn't know the node has IPv6 connectivity.

### Solution

Assign `{prefix}::1/64` and route via `fe80::1` (Hetzner's link-local gateway). Set kubelet `--node-ip` to both IPv4 and IPv6:

```yaml
machine:
  network:
    interfaces:
      - deviceSelector:
          hardwareAddr: "aa:bb:cc:dd:ee:ff"
        addresses:
          - "2a01:4f8:271:3b49::1/64"
        routes:
          - network: "::/0"
            gateway: "fe80::1"
  kubelet:
    extraArgs:
      node-ip: "10.10.0.6,2a01:4f8:271:3b49::1"
  sysctls:
    net.ipv6.conf.all.forwarding: "1"
```

### How CAPHR Implements This

`injectIPv6Config()` reads `serverIPv6Net` from the HetznerRobotHost spec (auto-detected from Robot API), strips the prefix length, constructs the `::1/64` address, and injects the full dual-stack config including kubelet nodeIP and sysctl.
