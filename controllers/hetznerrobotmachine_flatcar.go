package controllers

import (
	"context"
	"fmt"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/conditions"

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
	if err := r.recordHostHardwareDetails(ctx, hrm.Namespace, hrm.Status.HostRef, hw); err != nil {
		logger.Error(err, "Failed to record host hardware details", "host", hrm.Status.HostRef)
	}
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

	// Wipe all NVMe disks only after reconcileNormal has authorized this host's
	// destructive provisioning policy.
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
	hrm.Status.Initialization = &infrav1.InfrastructureMachineInitialization{Provisioned: true}
	conditions.MarkTrue(hrm, infrav1.ReadyCondition)

	if util.IsControlPlaneMachine(machine) {
		logger.Info("Control plane Flatcar machine provisioned", "ip", serverIP)
	} else {
		logger.Info("Worker Flatcar machine provisioned", "ip", serverIP)
	}
	return ctrl.Result{}, nil
}
