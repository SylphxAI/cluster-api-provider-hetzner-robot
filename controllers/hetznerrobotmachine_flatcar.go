package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util"

	infrav1 "github.com/SylphxAI/cluster-api-provider-hetzner-robot/api/v1alpha1"
	"github.com/SylphxAI/cluster-api-provider-hetzner-robot/pkg/robot"
	"github.com/SylphxAI/cluster-api-provider-hetzner-robot/pkg/sshrescue"
)

// stateInstallFlatcar SSHes into rescue and installs Flatcar Container Linux.
// Unlike Talos (which uses gRPC ApplyConfig after boot), Flatcar gets its entire
// config via Ignition written to the OEM partition BEFORE first boot. This means
// the install phase does ALL config injection — there's no separate ApplyConfig state.
func (r *HetznerRobotMachineReconciler) stateInstallFlatcar(
	ctx context.Context,
	hrm *infrav1.HetznerRobotMachine,
	machine *clusterv1.Machine,
	hrc *infrav1.HetznerRobotCluster,
	hrh *infrav1.HetznerRobotHost,
	robotClient *robot.Client,
	serverID int,
	serverIP string,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// ── Pre-flight: check bootstrap data BEFORE any destructive operations ──
	// Without bootstrap data, we can't build Ignition → wiping disks wastes time
	// and leaves the server in a broken state until bootstrap data appears.
	bootstrapData, err := r.getBootstrapData(ctx, machine)
	if err != nil {
		logger.Info("Bootstrap data not ready yet, deferring Flatcar install (no disk wipe)", "error", err)
		return ctrl.Result{RequeueAfter: requeueAfterShort}, nil
	}

	// Derive SSH public key for Ignition injection — core user needs it for
	// CAPHR to SSH in and verify boot + check bootstrap completion.
	sshPubKey, err := r.getSSHPublicKey(ctx, hrc)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get SSH public key for Flatcar: %w", err)
	}

	// If port 22 is not accessible, the node may have already rebooted
	// from a previous install attempt. Activate rescue again and retry.
	if !sshrescue.IsReachable(serverIP) {
		logger.Info("Rescue SSH not reachable in InRescue state, activating rescue again", "ip", serverIP)
		hrm.Status.ProvisioningState = infrav1.StateNone
		return ctrl.Result{RequeueAfter: requeueAfterShort}, nil
	}

	// Verify rescue mode — same safety check as Talos install.
	privateKey, err := r.getSSHPrivateKey(ctx, hrc)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get SSH private key for rescue check: %w", err)
	}
	isRescue, rescueErr := sshrescue.IsRescueMode(serverIP, privateKey)
	if rescueErr != nil {
		logger.Info("Could not verify rescue mode, will retry", "ip", serverIP, "error", rescueErr)
		return ctrl.Result{RequeueAfter: requeueAfterLong}, nil
	}
	if !isRescue {
		logger.Info("Server running normal OS instead of rescue, re-activating rescue",
			"serverID", serverID, "ip", serverIP)
		return r.stateActivateRescue(ctx, hrm, hrc, robotClient, serverID, serverIP)
	}

	logger.Info("Rescue mode verified, installing Flatcar via rescue SSH", "ip", serverIP)

	sshClient := sshrescue.New(serverIP, privateKey)
	if err := sshClient.Connect(); err != nil {
		return ctrl.Result{}, fmt.Errorf("SSH connect to rescue %s: %w", serverIP, err)
	}
	defer sshClient.Close()

	// Detect hardware (same as Talos — reuse completely).
	hw, err := sshClient.DetectHardware()
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("detect hardware on %s: %w", serverIP, err)
	}
	hrm.Status.PrimaryMAC = hw.PrimaryMAC
	hrm.Status.GatewayIP = hw.GatewayIP
	logger.Info("Hardware detected",
		"mac", hw.PrimaryMAC, "gateway", hw.GatewayIP,
		"disks", hw.NVMeDisks, "cephDisks", len(hw.CephDisks),
		"ip", serverIP)

	// Resolve install disk (same as Talos).
	configuredDisk := hrm.Spec.InstallDisk
	if configuredDisk == "" {
		configuredDisk = "/dev/nvme0n1"
	}
	installDisk, err := sshrescue.ResolveInstallDiskFromInfo(hw, configuredDisk)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolve install disk on %s: %w", serverIP, err)
	}
	if installDisk != configuredDisk {
		logger.Info("Install disk resolved to different device",
			"configured", configuredDisk, "resolved", installDisk, "ip", serverIP)
	}
	stableDisk := installDisk
	if byID, ok := hw.ByIDPaths[installDisk]; ok {
		logger.Info("Resolved install disk to stable by-id path",
			"bare", installDisk, "stable", byID, "ip", serverIP)
		stableDisk = byID
	}
	hrm.Status.ResolvedInstallDisk = stableDisk

	// Wipe all NVMe disks (same as Talos — clean slate).
	if len(hw.NVMeDisks) == 0 {
		return ctrl.Result{}, fmt.Errorf("no NVMe disks found on %s", serverIP)
	}
	logger.Info("Wiping all NVMe disks for clean provision",
		"ip", serverIP, "installDisk", stableDisk, "disks", hw.NVMeDisks)
	if out, err := sshClient.WipeAllDisks(hw.NVMeDisks); err != nil {
		return ctrl.Result{}, fmt.Errorf("wipe all disks on %s: %w\nOutput: %s", serverIP, err, out)
	} else {
		logger.Info("All NVMe disks wiped", "ip", serverIP, "output", out)
	}

	// Inject per-machine config into the Ignition JSON.
	providerID := fmt.Sprintf("hetzner-robot://%d", serverID)
	internalIP := hrh.Spec.InternalIP
	if hrc.Spec.VLANConfig == nil {
		internalIP = "" // only use when VLAN configured
	}

	ignitionJSON, err := injectFlatcarConfig(
		bootstrapData,
		providerID,
		internalIP,
		hrh.Spec.ServerIPv6Net,
		hw.PrimaryMAC,
		hrc.Spec.VLANConfig,
		sshPubKey,
		serverIP,
		hw.GatewayIP,
	)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("inject Flatcar config: %w", err)
	}
	logger.Info("Ignition config built with injections",
		"providerID", providerID,
		"internalIP", internalIP,
		"ipv6Net", hrh.Spec.ServerIPv6Net,
		"mac", hw.PrimaryMAC,
		"sshKeyInjected", sshPubKey != "")

	// Install Flatcar: DD image + create partitions + write Ignition.
	channel := hrm.Spec.FlatcarChannel
	if channel == "" {
		channel = "stable"
	}
	if err := sshClient.InstallFlatcar(channel, installDisk, hrm.Spec.CustomImageURL, ignitionJSON); err != nil {
		return ctrl.Result{}, fmt.Errorf("install Flatcar on %s: %w", serverIP, err)
	}

	// Write pre-boot static networkd config to the ROOT partition so the
	// machine has a working network address before Ignition runs on first boot.
	// Hetzner dedicated servers have no DHCP — without this, network-online.target
	// never reaches ONLINE state and Ignition times out trying to download sysexts.
	// Non-fatal: if the mount fails we log and continue; the machine will fail to
	// network-online but that's a provisioning failure, not a controller failure.
	logger.Info("Flatcar installed, fixing EFI boot order", "ip", serverIP)

	// EFI boot order: same as Talos — delete stale non-PXE entries, set PXE first.
	// The firmware auto-discovers the Flatcar ESP on reboot and boots from it
	// after PXE fails (rescue deactivated). No efibootmgr -c needed.
	efiScript := `
		if command -v efibootmgr > /dev/null 2>&1; then
			mount -o remount,rw /sys/firmware/efi/efivars 2>/dev/null || \
			mount -t efivarfs efivarfs /sys/firmware/efi/efivars 2>/dev/null || true

			echo "Before:"
			efibootmgr

			# Delete ALL non-PXE boot entries (stale Flatcar, Talos, UEFI OS, etc)
			for entry in $(efibootmgr 2>/dev/null | grep '^Boot[0-9A-Fa-f]' | grep -iv 'pxe\|network\|ipv4\|ipv6' | grep -o '^Boot[0-9A-Fa-f]*' | sed 's/Boot//'); do
				efibootmgr -b "$entry" -B 2>/dev/null || true
			done

			# Set boot order to PXE only — firmware will fallback to disk ESP
			PXE_NUMS=$(efibootmgr | grep -i 'PXE\|Network\|IPv4' | grep -oP 'Boot\K[0-9A-Fa-f]+' | paste -sd,)
			if [ -n "$PXE_NUMS" ]; then
				efibootmgr -o "${PXE_NUMS}" 2>&1
			fi

			echo "After:"
			efibootmgr
		fi
	`
	if out, err := sshClient.Run(efiScript); err != nil {
		logger.Info("Post-install EFI fix failed (non-fatal)", "error", err, "output", out)
	} else {
		logger.Info("EFI boot order fix applied", "output", out)
	}

	// Deactivate rescue before reboot.
	if err := robotClient.DeactivateRescue(ctx, serverID); err != nil {
		logger.Error(err, "Failed to deactivate rescue, server may boot back to rescue",
			"serverID", serverID)
	}

	// Set providerID on HRM spec (propagates to Machine via CAPI).
	hrm.Spec.ProviderID = &providerID

	// Reboot into Flatcar.
	sshClient.Run("reboot") //nolint:errcheck // reboot disconnects SSH
	hrm.Status.ProvisioningState = infrav1.StateInstalling
	// Flatcar first boot on Hetzner: PXE timeout ~2-3 min (rescue deactivated,
	// PXE tries both NICs then fails) + GRUB + kernel + Ignition = ~5 min total.
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

// stateWaitFlatcarInstall waits for Flatcar to boot after image install.
// Detects Flatcar by SSH connectivity as `core` user (not root/rescue).
func (r *HetznerRobotMachineReconciler) stateWaitFlatcarInstall(
	ctx context.Context,
	hrm *infrav1.HetznerRobotMachine,
	hrc *infrav1.HetznerRobotCluster,
	robotClient *robot.Client,
	serverID int,
	serverIP string,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	privateKey, err := r.getSSHPrivateKey(ctx, hrc)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get SSH private key: %w", err)
	}

	// Check if Flatcar is up (SSH as `core` user).
	if sshrescue.IsFlatcarUp(serverIP, privateKey) {
		logger.Info("Flatcar booted, SSH as core user successful", "ip", serverIP)
		hrm.Status.ProvisioningState = infrav1.StateBootingFlatcar
		return ctrl.Result{RequeueAfter: requeueAfterShort}, nil
	}

	// Check if Talos booted instead (shouldn't happen, but defensive).
	// Talos is detected by port 50000 — if we see it, the Flatcar install failed
	// and an old Talos is still on disk. Re-rescue.
	// (Import talos package would create circular dep — just check port 50000 directly.)

	// Check if rescue is still up (server didn't reboot yet or booted back to rescue).
	if sshrescue.IsReachable(serverIP) {
		// Could be rescue still running (reboot didn't happen) or rescue re-activated.
		// Don't check rescue mode here — the SSH port may be closing during reboot.
		logger.Info("SSH port open but Flatcar not yet up, still booting", "ip", serverIP)
	}

	logger.Info("Waiting for Flatcar to boot after install", "ip", serverIP)
	return ctrl.Result{RequeueAfter: requeueAfterMedium}, nil
}

// stateWaitFlatcarBoot waits for Flatcar to complete bootstrap (kubeadm join).
// Checks for the CAPI bootstrap sentinel file.
func (r *HetznerRobotMachineReconciler) stateWaitFlatcarBoot(
	ctx context.Context,
	hrm *infrav1.HetznerRobotMachine,
	machine *clusterv1.Machine,
	hrc *infrav1.HetznerRobotCluster,
	serverIP string,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	privateKey, err := r.getSSHPrivateKey(ctx, hrc)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get SSH private key: %w", err)
	}

	// Check if bootstrap is complete.
	complete, err := sshrescue.CheckFlatcarBootstrapComplete(serverIP, privateKey)
	if err != nil {
		// SSH might be temporarily unavailable during first boot services.
		logger.Info("Could not check bootstrap sentinel, will retry", "ip", serverIP, "error", err)
		return ctrl.Result{RequeueAfter: requeueAfterMedium}, nil
	}

	if !complete {
		logger.Info("Flatcar booted but bootstrap not complete (kubeadm join in progress)", "ip", serverIP)
		return ctrl.Result{RequeueAfter: requeueAfterMedium}, nil
	}

	// Bootstrap complete — node has joined the cluster.
	logger.Info("Flatcar bootstrap complete, node joined cluster", "ip", serverIP)
	hrm.Status.ProvisioningState = infrav1.StateProvisioned
	hrm.Status.Ready = true

	if util.IsControlPlaneMachine(machine) {
		logger.Info("Control plane Flatcar machine provisioned", "ip", serverIP)
	} else {
		logger.Info("Worker Flatcar machine provisioned", "ip", serverIP)
	}
	return ctrl.Result{}, nil
}

// ─── Ignition Config Injection ──────────────────────────────────────────────

// IgnitionConfig is a minimal representation of Ignition 3.x for JSON manipulation.
// Only the fields we need to inject into are modeled.
type IgnitionConfig struct {
	Ignition IgnitionMeta      `json:"ignition"`
	Storage  *IgnitionStorage  `json:"storage,omitempty"`
	Systemd  *IgnitionSystemd  `json:"systemd,omitempty"`
	// Passwd and other fields pass through via rawFields.
	rawFields map[string]json.RawMessage
}

type IgnitionMeta struct {
	Version string `json:"version"`
}

type IgnitionStorage struct {
	Files       []IgnitionFile      `json:"files,omitempty"`
	Directories []json.RawMessage   `json:"directories,omitempty"`
	Links       []json.RawMessage   `json:"links,omitempty"`
	Filesystems []json.RawMessage   `json:"filesystems,omitempty"`
	Luks        []json.RawMessage   `json:"luks,omitempty"`
	Raid        []json.RawMessage   `json:"raid,omitempty"`
	Disks       []json.RawMessage   `json:"disks,omitempty"`
}

type IgnitionFile struct {
	Path      string              `json:"path"`
	Contents  *IgnitionFileSource `json:"contents,omitempty"`
	Mode      *int                `json:"mode,omitempty"`
	Overwrite *bool               `json:"overwrite,omitempty"`
	User      *IgnitionFileUser   `json:"user,omitempty"`
	Group     *IgnitionFileGroup  `json:"group,omitempty"`
}

type IgnitionFileSource struct {
	Source       string `json:"source,omitempty"`
	Compression  string `json:"compression,omitempty"`
	Verification *struct {
		Hash string `json:"hash,omitempty"`
	} `json:"verification,omitempty"`
}

type IgnitionFileUser struct {
	ID   *int   `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

type IgnitionFileGroup struct {
	ID   *int   `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

type IgnitionSystemd struct {
	Units []IgnitionUnit `json:"units,omitempty"`
}

type IgnitionUnit struct {
	Name     string              `json:"name"`
	Enabled  *bool               `json:"enabled,omitempty"`
	Mask     *bool               `json:"mask,omitempty"`
	Contents *string             `json:"contents,omitempty"`
	Dropins  []IgnitionDropin    `json:"dropins,omitempty"`
}

type IgnitionDropin struct {
	Name     string `json:"name"`
	Contents string `json:"contents"`
}

// injectFlatcarConfig takes CABPK-generated Ignition JSON and injects
// CAPHR per-machine config: providerID, nodeIP, VLAN, IPv6, devmapper setup,
// Hetzner static network config, and SSH authorized keys for the core user.
func injectFlatcarConfig(
	bootstrapData []byte,
	providerID string,
	internalIP string,
	ipv6Net string,
	primaryMAC string,
	vlanConfig *infrav1.VLANConfig,
	sshPublicKey string,
	serverIP string,
	gatewayIP string,
) ([]byte, error) {
	// Parse the Ignition JSON from CABPK.
	var ign map[string]interface{}
	if err := json.Unmarshal(bootstrapData, &ign); err != nil {
		return nil, fmt.Errorf("parse Ignition JSON: %w", err)
	}

	// Fix CABPK compatibility: convert "inline" → "source: data:,..." in file contents.
	// CABPK generates Ignition with "inline" field (spec 3.4.0+), but Flatcar's Ignition
	// engine uses version 3.3.0 which doesn't support "inline". Without this conversion,
	// Flatcar rejects the ENTIRE config — no files, no SSH keys, no network.
	fixIgnitionInlineFields(ign)

	// Remove URL file downloads from Ignition. Ignition uses its own DHCP client
	// for downloads, which doesn't work on Hetzner (anti-spoofing). Files with
	// http/https sources (like sysext raw images) must be downloaded post-boot
	// via preKubeadmCommands instead, when systemd-networkd is configured.
	removeIgnitionURLDownloads(ign)

	// Ensure top-level structure exists.
	if _, ok := ign["storage"]; !ok {
		ign["storage"] = map[string]interface{}{}
	}
	storage := ign["storage"].(map[string]interface{})
	if _, ok := storage["files"]; !ok {
		storage["files"] = []interface{}{}
	}

	if _, ok := ign["systemd"]; !ok {
		ign["systemd"] = map[string]interface{}{}
	}
	systemd := ign["systemd"].(map[string]interface{})
	if _, ok := systemd["units"]; !ok {
		systemd["units"] = []interface{}{}
	}

	// ── 1. Inject kubelet extra args (providerID + nodeIP) ───────────────
	// Devmapper, Kata, sysext are handled by the bootstrap template's
	// preKubeadmCommands — NOT by CAPHR. CAPHR only injects per-machine
	// config that varies between hosts (providerID, nodeIP, network).

	kubeletArgs := buildKubeletExtraArgs(providerID, internalIP, ipv6Net)
	if kubeletArgs != "" {
		// Drop-in for kubelet to add extra args.
		addIgnitionUnit(systemd, "kubelet.service", false, "")
		addKubeletDropin(systemd, kubeletArgs)
	}

	// ── 4. Inject Hetzner network config as storage files ───────────────
	// Ignition 3.x requires networkd config as storage.files.
	// Hetzner dedicated servers need static /32 IP + Peer=gateway. Custom
	// networkd files that match by MAC override the default DHCP, so we
	// MUST include the Hetzner-style static config explicitly.

	if serverIP != "" && gatewayIP != "" {
		// VLAN config is NOT included in the initial Ignition. Adding VLAN=vlanX
		// to the primary NIC causes systemd-networkd to hang if the server is not
		// yet added to the Hetzner vSwitch. VLAN is configured post-provision
		// once the node has joined the cluster and is confirmed working.
		addHetznerPrimaryNetwork(storage, primaryMAC, serverIP, gatewayIP, ipv6Net, nil)
	}

	// ── 5. Inject SSH authorized keys for core user ─────────────────────
	// CAPHR needs SSH access as `core` to verify Flatcar boot and check
	// bootstrap completion. The CABPK Ignition may or may not include SSH
	// keys (depends on KubeadmConfig), so we always inject our own.
	if sshPublicKey != "" {
		addSSHAuthorizedKey(ign, "core", sshPublicKey)
	}

	// Marshal back to JSON.
	result, err := json.MarshalIndent(ign, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal Ignition JSON: %w", err)
	}
	return result, nil
}

// ─── Helper Functions ────────────────────────────────────────────────────────

// fixIgnitionInlineFields converts all "inline" fields in file contents to
// "source: data:,..." URIs. CABPK generates Ignition with "inline" (spec 3.4.0),
// but Flatcar uses Ignition 3.3.0 which only supports "source".
func fixIgnitionInlineFields(ign map[string]interface{}) {
	storage, ok := ign["storage"].(map[string]interface{})
	if !ok {
		return
	}
	files, ok := storage["files"].([]interface{})
	if !ok {
		return
	}
	for i, f := range files {
		file, ok := f.(map[string]interface{})
		if !ok {
			continue
		}
		contents, ok := file["contents"].(map[string]interface{})
		if !ok {
			continue
		}
		inline, hasInline := contents["inline"]
		_, hasSource := contents["source"]
		if hasInline && !hasSource {
			contents["source"] = "data:," + urlEncode(fmt.Sprintf("%v", inline))
			delete(contents, "inline")
			file["contents"] = contents
			files[i] = file
		}
	}
	storage["files"] = files
}

// removeIgnitionURLDownloads removes files with http/https source URLs from
// Ignition. Also removes storage.links whose targets are removed files.
// These files must be downloaded post-boot when networkd is configured.
func removeIgnitionURLDownloads(ign map[string]interface{}) {
	storage, ok := ign["storage"].(map[string]interface{})
	if !ok {
		return
	}

	// Remove files with URL sources
	removedPaths := map[string]bool{}
	if files, ok := storage["files"].([]interface{}); ok {
		var kept []interface{}
		for _, f := range files {
			file, ok := f.(map[string]interface{})
			if !ok {
				kept = append(kept, f)
				continue
			}
			contents, _ := file["contents"].(map[string]interface{})
			src, _ := contents["source"].(string)
			if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
				removedPaths[file["path"].(string)] = true
				continue // skip this file
			}
			kept = append(kept, f)
		}
		storage["files"] = kept
	}

	// Remove links whose targets were removed
	if links, ok := storage["links"].([]interface{}); ok && len(removedPaths) > 0 {
		var kept []interface{}
		for _, l := range links {
			link, ok := l.(map[string]interface{})
			if !ok {
				kept = append(kept, l)
				continue
			}
			target, _ := link["target"].(string)
			if removedPaths[target] {
				continue // skip dangling link
			}
			kept = append(kept, l)
		}
		storage["links"] = kept
	}
}

func addIgnitionFile(storage map[string]interface{}, path, content string, mode int) {
	files := storage["files"].([]interface{})
	trueVal := true
	file := map[string]interface{}{
		"path": path,
		"contents": map[string]interface{}{
			"source": "data:," + urlEncode(content),
		},
		"mode":      mode,
		"overwrite": trueVal,
	}
	storage["files"] = append(files, file)
}

func addIgnitionUnit(systemd map[string]interface{}, name string, enabled bool, contents string) {
	units := systemd["units"].([]interface{})
	unit := map[string]interface{}{
		"name":    name,
		"enabled": enabled,
	}
	if contents != "" {
		unit["contents"] = contents
	}
	systemd["units"] = append(units, unit)
}

func addKubeletDropin(systemd map[string]interface{}, extraArgs string) {
	units := systemd["units"].([]interface{})
	// Find existing kubelet.service entry and add dropin.
	for i, u := range units {
		unit, ok := u.(map[string]interface{})
		if !ok {
			continue
		}
		if unit["name"] == "kubelet.service" {
			dropins, _ := unit["dropins"].([]interface{})
			dropins = append(dropins, map[string]interface{}{
				"name": "10-caphr-args.conf",
				"contents": fmt.Sprintf(`[Service]
Environment="KUBELET_EXTRA_ARGS=%s"
`, extraArgs),
			})
			unit["dropins"] = dropins
			units[i] = unit
			systemd["units"] = units
			return
		}
	}
}

// addHetznerPrimaryNetwork writes the primary NIC config as a single consolidated
// network file. Hetzner requires static /32 + Peer=gateway (anti-spoofing).
// This file also includes VLAN attachment and IPv6 if configured.
// Using ONE file per interface avoids systemd-networkd priority conflicts.
func addHetznerPrimaryNetwork(storage map[string]interface{}, primaryMAC, serverIP, gatewayIP, ipv6Net string, vlanConfig *infrav1.VLANConfig) {
	var network strings.Builder

	// Match by MAC — stable across reboots regardless of interface name.
	fmt.Fprintf(&network, "[Match]\nMACAddress=%s\n", primaryMAC)

	// Network section: DNS + optional VLAN.
	fmt.Fprintf(&network, "\n[Network]\nDNS=185.12.64.1\nDNS=185.12.64.2\n")
	if vlanConfig != nil {
		fmt.Fprintf(&network, "VLAN=vlan%d\n", vlanConfig.ID)
	}

	// IPv4: Hetzner /32 + Peer=gateway (required by Hetzner anti-spoofing).
	fmt.Fprintf(&network, "\n[Address]\nAddress=%s/32\nPeer=%s/32\n", serverIP, gatewayIP)

	// Default route via gateway.
	fmt.Fprintf(&network, "\n[Route]\nDestination=0.0.0.0/0\nGateway=%s\n", gatewayIP)

	// IPv6 if configured.
	if ipv6Net != "" {
		ipv6Prefix := strings.Split(ipv6Net, "/")[0]
		ipv6Addr := strings.TrimSuffix(ipv6Prefix, "::") + "::1/64"
		fmt.Fprintf(&network, "\n[Address]\nAddress=%s\n", ipv6Addr)
		fmt.Fprintf(&network, "\n[Route]\nDestination=::/0\nGateway=fe80::1\n")
	}

	addIgnitionFile(storage, "/etc/systemd/network/10-public.network", network.String(), 0o644)
}

// addVLANNetdevAndNetwork creates the VLAN virtual device and assigns the
// internal IP. The VLAN is attached to the primary NIC via addHetznerPrimaryNetwork.
func addVLANNetdevAndNetwork(storage map[string]interface{}, vlanID int, internalIP string, prefixLen int) {
	// VLAN netdev
	vlanNetdev := fmt.Sprintf("[NetDev]\nName=vlan%d\nKind=vlan\n\n[VLAN]\nId=%d\n", vlanID, vlanID)
	addIgnitionFile(storage, fmt.Sprintf("/etc/systemd/network/10-vlan%d.netdev", vlanID), vlanNetdev, 0o644)

	// VLAN network — assigns internal IP
	vlanNetwork := fmt.Sprintf("[Match]\nName=vlan%d\n\n[Network]\nAddress=%s/%d\n", vlanID, internalIP, prefixLen)
	addIgnitionFile(storage, fmt.Sprintf("/etc/systemd/network/10-vlan%d.network", vlanID), vlanNetwork, 0o644)
}

// addNetworkdIPv6File writes standalone IPv6 config (only used when serverIP is empty).
func addNetworkdIPv6File(storage map[string]interface{}, ipv6Addr, primaryMAC string) {
	ipv6Network := fmt.Sprintf("[Match]\nMACAddress=%s\n\n[Network]\nAddress=%s\n\n[Route]\nDestination=::/0\nGateway=fe80::1\n", primaryMAC, ipv6Addr)
	addIgnitionFile(storage, "/etc/systemd/network/10-ipv6.network", ipv6Network, 0o644)
}

// addSSHAuthorizedKey injects an SSH public key into the Ignition passwd section
// for the specified user. Creates the passwd.users array if it doesn't exist.
// If the user already exists, appends the key to their sshAuthorizedKeys.
func addSSHAuthorizedKey(ign map[string]interface{}, username, pubKey string) {
	if _, ok := ign["passwd"]; !ok {
		ign["passwd"] = map[string]interface{}{}
	}
	passwd := ign["passwd"].(map[string]interface{})
	if _, ok := passwd["users"]; !ok {
		passwd["users"] = []interface{}{}
	}
	users := passwd["users"].([]interface{})

	// Find existing user entry.
	for i, u := range users {
		user, ok := u.(map[string]interface{})
		if !ok {
			continue
		}
		if user["name"] == username {
			// Append to existing sshAuthorizedKeys.
			keys, _ := user["sshAuthorizedKeys"].([]interface{})
			// Avoid duplicates.
			for _, k := range keys {
				if k == pubKey {
					return
				}
			}
			keys = append(keys, pubKey)
			user["sshAuthorizedKeys"] = keys
			users[i] = user
			passwd["users"] = users
			return
		}
	}

	// User doesn't exist — create entry.
	users = append(users, map[string]interface{}{
		"name":              username,
		"sshAuthorizedKeys": []interface{}{pubKey},
	})
	passwd["users"] = users
}

// writePreBootNetwork mounts the Flatcar ROOT partition (p9) from the rescue
// environment and writes static systemd-networkd config files so that the
// machine has a working network before Ignition runs on first boot.
//
// Why this is necessary: Hetzner dedicated servers have no DHCP — they use
// static routing. Flatcar boots with DHCP by default. network-online.target
// therefore never completes, Ignition waits forever (or times out), and sysext
// downloads from GitHub fail. Writing a pre-boot networkd file gives the
// machine the correct address so network-online.target succeeds immediately
// and Ignition can fetch all remote resources.
//
// The pre-boot file uses a low priority name (10-static-preboot.network) and
// matches by MAC. After Ignition runs it writes its own networkd files
// (10-public.network etc.) which also match by MAC. Both files coexist
// harmlessly — systemd-networkd applies all matching files in lexicographic
// order and the values are identical.
func writePreBootNetwork(
	sshClient *sshrescue.Client,
	installDisk string,
	primaryMAC string,
	serverIP string,
	gatewayIP string,
	ipv6Net string,
	internalIP string,
	vlanConfig *infrav1.VLANConfig,
) error {
	if serverIP == "" || gatewayIP == "" {
		return fmt.Errorf("cannot write pre-boot network: serverIP or gatewayIP empty")
	}

	// Derive p9 (ROOT partition) from the install disk.
	// Flatcar's disk layout: p1=EFI, p2=BIOS-BOOT, p3=USR-A, p4=USR-B,
	// p6=OEM, p9=ROOT (active rootfs), p10=r-dm-data (added by CAPHR).
	// ROOT is always p9 for all NVMe and SATA devices.
	rootPart := installDisk + "p9"

	mountPoint := "/mnt/flatcar-root"

	// Mount ROOT partition.
	mountCmd := fmt.Sprintf(
		"mkdir -p %s && mount %s %s",
		mountPoint, rootPart, mountPoint,
	)
	if out, err := sshClient.Run(mountCmd); err != nil {
		return fmt.Errorf("mount ROOT partition %s: %w\nOutput: %s", rootPart, err, out)
	}

	// Ensure unmount happens even on error.
	unmountCmd := fmt.Sprintf("umount %s 2>/dev/null || true", mountPoint)
	defer func() { sshClient.Run(unmountCmd) }() //nolint:errcheck // best-effort cleanup

	// Build the primary network file content.
	var primary strings.Builder
	fmt.Fprintf(&primary, "[Match]\nMACAddress=%s\n", primaryMAC)
	fmt.Fprintf(&primary, "\n[Network]\nDNS=185.12.64.1\nDNS=185.12.64.2\n")
	if vlanConfig != nil {
		fmt.Fprintf(&primary, "VLAN=vlan%d\n", vlanConfig.ID)
	}
	fmt.Fprintf(&primary, "\n[Address]\nAddress=%s/32\nPeer=%s/32\n", serverIP, gatewayIP)
	fmt.Fprintf(&primary, "\n[Route]\nDestination=0.0.0.0/0\nGateway=%s\n", gatewayIP)
	if ipv6Net != "" {
		ipv6Prefix := strings.Split(ipv6Net, "/")[0]
		ipv6Addr := strings.TrimSuffix(ipv6Prefix, "::") + "::1/64"
		fmt.Fprintf(&primary, "\n[Address]\nAddress=%s\n", ipv6Addr)
		fmt.Fprintf(&primary, "\n[Route]\nDestination=::/0\nGateway=fe80::1\n")
	}

	networkDir := mountPoint + "/etc/systemd/network"
	mkdirCmd := fmt.Sprintf("mkdir -p %s", networkDir)
	if out, err := sshClient.Run(mkdirCmd); err != nil {
		return fmt.Errorf("create networkd dir on ROOT: %w\nOutput: %s", err, out)
	}

	// Write primary network file via heredoc to avoid shell quoting issues.
	writeCmd := fmt.Sprintf("cat > %s/10-static-preboot.network << 'NETEOF'\n%sNETEOF",
		networkDir, primary.String())
	if out, err := sshClient.Run(writeCmd); err != nil {
		return fmt.Errorf("write 10-static-preboot.network: %w\nOutput: %s", err, out)
	}

	// Write VLAN netdev and network files if VLAN is configured.
	if vlanConfig != nil && internalIP != "" {
		prefixLen := vlanConfig.PrefixLength
		if prefixLen == 0 {
			prefixLen = 24
		}

		vlanNetdev := fmt.Sprintf("[NetDev]\nName=vlan%d\nKind=vlan\n\n[VLAN]\nId=%d\n",
			vlanConfig.ID, vlanConfig.ID)
		writeVlanNetdev := fmt.Sprintf("cat > %s/10-vlan%d.netdev << 'NETEOF'\n%sNETEOF",
			networkDir, vlanConfig.ID, vlanNetdev)
		if out, err := sshClient.Run(writeVlanNetdev); err != nil {
			return fmt.Errorf("write vlan netdev: %w\nOutput: %s", err, out)
		}

		vlanNetwork := fmt.Sprintf("[Match]\nName=vlan%d\n\n[Network]\nAddress=%s/%d\n",
			vlanConfig.ID, internalIP, prefixLen)
		writeVlanNetwork := fmt.Sprintf("cat > %s/10-vlan%d.network << 'NETEOF'\n%sNETEOF",
			networkDir, vlanConfig.ID, vlanNetwork)
		if out, err := sshClient.Run(writeVlanNetwork); err != nil {
			return fmt.Errorf("write vlan network: %w\nOutput: %s", err, out)
		}
	}

	return nil
}

func buildKubeletExtraArgs(providerID, internalIP, ipv6Net string) string {
	var args []string
	if providerID != "" {
		args = append(args, fmt.Sprintf("--provider-id=%s", providerID))
	}

	// Build node-ip (same logic as Talos injectKubeletNodeIP).
	var nodeIPv6 string
	if ipv6Net != "" {
		ipv6Prefix := strings.Split(ipv6Net, "/")[0]
		nodeIPv6 = strings.TrimSuffix(ipv6Prefix, "::") + "::1"
	}
	var nodeIP string
	switch {
	case internalIP != "" && nodeIPv6 != "":
		nodeIP = internalIP + "," + nodeIPv6 // dual-stack
	case internalIP != "":
		nodeIP = internalIP
	case nodeIPv6 != "":
		nodeIP = nodeIPv6
	}
	if nodeIP != "" {
		args = append(args, fmt.Sprintf("--node-ip=%s", nodeIP))
	}

	return strings.Join(args, " ")
}

// urlEncode performs minimal URL encoding for Ignition data: URIs.
// Only encodes characters that must be escaped in data: URIs.
func urlEncode(s string) string {
	var b strings.Builder
	for _, c := range s {
		switch {
		case c == ' ':
			b.WriteString("%20")
		case c == '\n':
			b.WriteString("%0A")
		case c == '\r':
			b.WriteString("%0D")
		case c == '%':
			b.WriteString("%25")
		case c == '#':
			b.WriteString("%23")
		case c == '&':
			b.WriteString("%26")
		case c == '=':
			b.WriteString("%3D")
		default:
			b.WriteRune(c)
		}
	}
	return b.String()
}

// ─── Config Templates ────────────────────────────────────────────────────────

func devmapperPoolServiceUnit() string {
	return `[Unit]
Description=Create devmapper thin-pool for containerd
DefaultDependencies=no
Before=containerd.service
After=dev-disk-by\x2dpartlabel-r\x2ddm\x2ddata.device
Wants=dev-disk-by\x2dpartlabel-r\x2ddm\x2ddata.device

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/opt/bin/setup-devmapper-pool.sh

[Install]
WantedBy=multi-user.target
`
}

func devmapperPoolScript() string {
	return `#!/bin/bash
set -euo pipefail

POOL_NAME="containerd-pool"
DM_PART="/dev/disk/by-partlabel/r-dm-data"
META_DIR="/var/lib/devmapper"
META_FILE="${META_DIR}/meta"

log() { echo "devmapper-pool: $*"; }

# Wait for partition device to appear.
WAIT=0
while [ ! -b "$DM_PART" ] && [ "$WAIT" -lt 30 ]; do
    sleep 1
    WAIT=$((WAIT + 1))
done
if [ ! -b "$DM_PART" ]; then
    log "ERROR: $DM_PART not found after ${WAIT}s"
    exit 1
fi
log "partition ready after ${WAIT}s"

# Check if pool already exists (persistent across reboots if metadata on disk).
if dmsetup info "$POOL_NAME" >/dev/null 2>&1; then
    log "pool already exists, nothing to do"
    exit 0
fi

DATA_DEV=$(readlink -f "$DM_PART")
log "data device: $DATA_DEV"

# Create metadata sparse file (4GB) on /var.
# On Flatcar /var is writable (stateful partition), unlike Talos extensions.
mkdir -p "$META_DIR"
if [ ! -f "$META_FILE" ]; then
    dd if=/dev/zero of="$META_FILE" bs=1 count=0 seek=4294967296 2>/dev/null
    log "metadata file created"
fi

META_LOOP=$(losetup -f --show "$META_FILE")
SECTORS=$(blockdev --getsz "$DATA_DEV")
log "creating pool: data=$DATA_DEV sectors=$SECTORS meta=$META_LOOP"

dmsetup create "$POOL_NAME" \
    --table "0 ${SECTORS} thin-pool ${META_LOOP} ${DATA_DEV} 512 32768 1 skip_block_zeroing"
log "pool created successfully"
`
}

func containerdDevmapperConfig() string {
	return `# Devmapper snapshotter for Kata containers.
# Pool created by devmapper-pool.service (Before=containerd.service).
[plugins."io.containerd.snapshotter.v1.devmapper"]
  pool_name = "containerd-pool"
  root_path = "/var/lib/containerd/devmapper"
  base_image_size = "10GB"
  discard_blocks = true
  fs_type = "ext4"
  async_remove = true
`
}

// isFlatcar returns true if the HRM spec indicates a Flatcar OS installation.
func isFlatcar(hrm *infrav1.HetznerRobotMachine) bool {
	return hrm.Spec.OSType == infrav1.OSTypeFlatcar
}
