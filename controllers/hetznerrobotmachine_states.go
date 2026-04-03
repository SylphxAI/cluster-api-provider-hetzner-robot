package controllers

import (
	"context"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/patch"

	infrav1 "github.com/SylphxAI/cluster-api-provider-hetzner-robot/api/v1alpha1"
	"github.com/SylphxAI/cluster-api-provider-hetzner-robot/pkg/robot"
	"github.com/SylphxAI/cluster-api-provider-hetzner-robot/pkg/sshrescue"
	"github.com/SylphxAI/cluster-api-provider-hetzner-robot/pkg/talos"
)

// Provision timeout is handled by the controller (provisionTimeout constant).
// No per-state retry limits — the global timeout covers all failure modes.

// stateActivateRescue activates rescue mode via Robot API and triggers a hw reset.
// Provision timeout (in controller) handles the case where rescue never succeeds.
func (r *HetznerRobotMachineReconciler) stateActivateRescue(
	ctx context.Context,
	hrm *infrav1.HetznerRobotMachine,
	hrc *infrav1.HetznerRobotCluster,
	robotClient *robot.Client,
	serverID int,
	serverIP string,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	sshFingerprint, err := r.getSSHKeyFingerprint(ctx, hrc)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get SSH key fingerprint: %w", err)
	}

	_, err = robotClient.ActivateRescue(ctx, serverID, sshFingerprint)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("activate rescue on server %d: %w", serverID, err)
	}

	logger.Info("Activating rescue mode", "serverID", serverID, "ip", serverIP,
		"retryCount", hrm.Status.RetryCount)

	if err := robotClient.ResetServer(ctx, serverID, robot.ResetTypeHardware); err != nil {
		return ctrl.Result{}, fmt.Errorf("reset server %d: %w", serverID, err)
	}

	hrm.Status.ProvisioningState = infrav1.StateActivatingRescue
	logger.Info("Rescue activated, server resetting", "serverID", serverID)
	// Hetzner rescue is one-shot: the rescue API flag is consumed on first PXE boot.
	// After hw reset, BIOS POST + PXE load + rescue boot takes 3-4 minutes.
	// If we poll before rescue SSH is up, the API reports rescue as inactive
	// (consumed) and nothing is reachable → we'd incorrectly re-activate rescue,
	// interrupting the boot. Wait 4 minutes to give rescue time to fully load.
	return ctrl.Result{RequeueAfter: 4 * time.Minute}, nil
}

// stateCheckRescueActive waits for SSH to be available in rescue mode.
// If rescue is no longer active (server rebooted back to normal OS), it re-activates
// rescue and triggers another hardware reset automatically.
func (r *HetznerRobotMachineReconciler) stateCheckRescueActive(
	ctx context.Context,
	hrm *infrav1.HetznerRobotMachine,
	hrc *infrav1.HetznerRobotCluster,
	robotClient *robot.Client,
	serverID int,
	serverIP string,
	machine *clusterv1.Machine,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Check if rescue SSH is already up.
	// IMPORTANT: Must verify it's actually rescue, not a pre-existing OS (Debian/Talos).
	// A pre-existing OS has SSH on port 22 too — treating it as rescue causes installer
	// failures (installer expects rescue environment, not a running OS).
	if sshrescue.IsReachable(serverIP) {
		privateKey, keyErr := r.getSSHPrivateKey(ctx, hrc)
		if keyErr == nil {
			client := sshrescue.New(serverIP, privateKey)
			if connErr := client.Connect(); connErr == nil {
				defer client.Close()
				// Hetzner rescue has hostname "rescue" and /etc/hetzner-build
				out, _ := client.Run("([ \"$(hostname)\" = \"rescue\" ] || test -f /etc/hetzner-build) && echo RESCUE || echo NOT_RESCUE")
				if strings.TrimSpace(out) == "RESCUE" {
					logger.Info("Rescue SSH reachable and verified", "ip", serverIP)
					hrm.Status.ProvisioningState = infrav1.StateInRescue
					return ctrl.Result{RequeueAfter: requeueAfterShort}, nil
				}
				// SSH reachable but NOT rescue — server booted existing OS.
				// Fix EFI boot order to PXE first, then reboot into rescue.
				logger.Info("SSH reachable but not rescue (existing OS detected), fixing EFI boot order",
					"ip", serverIP)
				efiOut, efiErr := client.Run(`
					if command -v efibootmgr > /dev/null 2>&1; then
						mount -o remount,rw /sys/firmware/efi/efivars 2>/dev/null || \
						mount -t efivarfs efivarfs /sys/firmware/efi/efivars 2>/dev/null || true
						for entry in $(efibootmgr 2>/dev/null | grep '^Boot[0-9A-Fa-f]' | grep -iv 'pxe\|network\|ipv4\|ipv6' | grep -o '^Boot[0-9A-Fa-f]*' | sed 's/Boot//'); do
							efibootmgr -b "$entry" -B 2>/dev/null
						done
					fi
					nohup bash -c 'sleep 1 && reboot' &>/dev/null &
				`)
				if efiErr != nil {
					logger.Info("EFI boot order fix via SSH failed (non-fatal, will retry via rescue)",
						"error", efiErr, "output", efiOut, "ip", serverIP)
				} else {
					logger.Info("Rebooted existing OS with PXE-only EFI, should enter rescue on next boot",
						"output", efiOut, "ip", serverIP)
				}
				return ctrl.Result{RequeueAfter: 90 * time.Second}, nil
			}
		}
		// SSH reachable but can't authenticate — might be rescue with wrong key or existing OS
		logger.Info("SSH port open but cannot authenticate, waiting for rescue", "ip", serverIP)
	}

	// Priority 3: Check Robot API rescue status.
	// Only re-activate if BOTH rescue is inactive AND SSH is closed (server rebooted to normal OS).
	rescueStatus, err := robotClient.GetRescueStatus(ctx, serverID)
	if err != nil {
		logger.Error(err, "Failed to get rescue status, will retry", "serverID", serverID)
		return ctrl.Result{RequeueAfter: requeueAfterLong}, nil
	}

	if !rescueStatus.Active {
		// Rescue inactive. Server may have booted something else (Talos, old OS) or
		// is still mid-boot after a recent hw reset. No shortcuts — we always need
		// rescue for a clean provision (wipe all + fresh install).
		// In both cases, re-activate rescue and wipe.
		if talos.IsUp(ctx, serverIP) {
			logger.Info("Talos running in full mode during early provisioning — treating as stale OS, wiping EFI + re-activating rescue",
				"serverID", serverID, "ip", serverIP, "retryCount", hrm.Status.RetryCount)
			// Wipe EFI so PXE can boot on next reset. Try insecure first (maintenance),
			// then authenticated (full mode with mTLS from bootstrap data).
			r.attemptEFIWipe(ctx, serverIP, machine)
			return r.stateActivateRescue(ctx, hrm, hrc, robotClient, serverID, serverIP)
		}
		// Nothing reachable and rescue inactive. Most likely: server is still booting
		// after a recent hw reset — Hetzner rescue is one-shot (consumed on first PXE
		// boot) and SSH takes 3-4 minutes to come up. Wait rather than re-activate,
		// which would interrupt an in-progress rescue boot.
		logger.Info("Rescue inactive but nothing reachable — server likely still booting, waiting",
			"serverID", serverID, "ip", serverIP)
		return ctrl.Result{RequeueAfter: 2 * time.Minute}, nil
	}

	// Rescue is armed but SSH not up yet.
	// Edge case: UEFI/BIOS boot order may skip PXE and boot Talos from NVMe instead
	// of the rescue system. Detect this by checking if Talos is already up.
	if talos.IsUp(ctx, serverIP) {
		// Talos booted instead of PXE rescue (maintenance or full mode).
		// No shortcuts — always wipe EFI and retry rescue for a clean provision.
		// EFI boot order has Talos UKI before PXE — need to wipe EFI and retry.
		// Wipe EFI boot entries via Talos API so PXE can boot next time.
		// Try insecure (maintenance) first, then authenticated (full mode with mTLS).
		logger.Info("Attempting to wipe EFI partition via Talos API to allow PXE boot",
			"serverID", serverID, "ip", serverIP, "retryCount", hrm.Status.RetryCount)
		r.attemptEFIWipe(ctx, serverIP, machine)
		logger.Info("Rescue active but Talos booted from disk instead of PXE rescue — re-activating rescue",
			"serverID", serverID, "ip", serverIP, "retryCount", hrm.Status.RetryCount)
		return r.stateActivateRescue(ctx, hrm, hrc, robotClient, serverID, serverIP)
	}

	// Neither SSH (rescue) nor Talos (disk) is up — server is still booting.
	logger.Info("Rescue active, SSH not yet reachable, waiting", "ip", serverIP)
	return ctrl.Result{RequeueAfter: requeueAfterLong}, nil
}

// attemptEFIWipe tries to wipe EFI + STATE partitions via the Talos API.
// First tries the insecure maintenance API (no client cert). If that fails
// (node is in full mode, not maintenance), falls back to an authenticated
// connection using the admin TLS credentials from the Machine's bootstrap data.
// Best-effort: logs failures but never returns an error — the caller retries
// on next reconcile cycle via rescue re-activation + hardware reset.
func (r *HetznerRobotMachineReconciler) attemptEFIWipe(
	ctx context.Context,
	serverIP string,
	machine *clusterv1.Machine,
) {
	logger := log.FromContext(ctx)

	// Try insecure wipe first (works if node is in maintenance mode).
	if err := talos.WipeEFIPartition(ctx, serverIP); err == nil {
		logger.Info("Successfully wiped EFI via Talos maintenance API — PXE should boot on next reset",
			"ip", serverIP)
		return
	}

	// Insecure wipe failed — node is likely in full mode (requires mTLS).
	// Fall back to authenticated wipe using bootstrap data.
	if machine == nil {
		logger.Info("EFI wipe failed: maintenance API rejected and no Machine available for authenticated fallback",
			"ip", serverIP, "outcome", "will_retry")
		return
	}
	machineConfigData, err := r.getBootstrapData(ctx, machine)
	if err != nil {
		logger.Info("EFI wipe failed: cannot get bootstrap data for authenticated fallback",
			"error", err, "ip", serverIP, "outcome", "will_retry")
		return
	}
	if err := talos.WipeEFIPartitionAuthenticated(ctx, serverIP, machineConfigData); err != nil {
		logger.Info("EFI wipe failed: both maintenance and authenticated attempts unsuccessful",
			"error", err, "ip", serverIP, "outcome", "will_retry")
		return
	}
	logger.Info("Successfully wiped EFI via authenticated Talos API — PXE should boot on next reset",
		"ip", serverIP)
}

// stateInstallTalos SSHes into rescue and installs Talos.
func (r *HetznerRobotMachineReconciler) stateInstallTalos(
	ctx context.Context,
	hrm *infrav1.HetznerRobotMachine,
	hrc *infrav1.HetznerRobotCluster,
	robotClient *robot.Client,
	serverID int,
	serverIP string,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// If port 22 is not accessible, the node may have already rebooted
	// from a previous install attempt. Activate rescue again and retry.
	if !sshrescue.IsReachable(serverIP) {
		logger.Info("Rescue SSH not reachable in InRescue state, activating rescue again", "ip", serverIP)
		hrm.Status.ProvisioningState = infrav1.StateNone
		return ctrl.Result{RequeueAfter: requeueAfterShort}, nil
	}

	// CRITICAL: SSH reachable does NOT mean rescue mode. A server running a normal OS
	// (Debian with cephadm, old Talos) also has SSH on port 22. Installing Talos on a
	// running production server would destroy it. Verify rescue before proceeding.
	privateKey, err := r.getSSHPrivateKey(ctx, hrc)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get SSH private key for rescue check: %w", err)
	}
	isRescue, rescueErr := sshrescue.IsRescueMode(serverIP, privateKey)
	if rescueErr != nil {
		// SSH auth failure or command failure — cannot determine state. Retry.
		logger.Info("Could not verify rescue mode, will retry", "ip", serverIP, "error", rescueErr)
		return ctrl.Result{RequeueAfter: requeueAfterLong}, nil
	}
	if !isRescue {
		// Server is running a normal OS, not rescue. Re-activate rescue + hw reset.
		logger.Info("Server running normal OS instead of rescue, re-activating rescue and triggering hw reset",
			"serverID", serverID, "ip", serverIP, "retryCount", hrm.Status.RetryCount)
		return r.stateActivateRescue(ctx, hrm, hrc, robotClient, serverID, serverIP)
	}

	logger.Info("Rescue mode verified, installing Talos via rescue SSH", "ip", serverIP)

	// privateKey already retrieved above for the rescue mode check — reuse it.
	sshClient := sshrescue.New(serverIP, privateKey)
	if err := sshClient.Connect(); err != nil {
		return ctrl.Result{}, fmt.Errorf("SSH connect to rescue %s: %w", serverIP, err)
	}
	defer sshClient.Close()

	// Detect all hardware in a single SSH call: MAC, gateway, NVMe disks,
	// Ceph BlueStore signatures, and stable /dev/disk/by-id/ paths.
	// Replaces 5 separate SSH roundtrips with 1.
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

	// Resolve install disk from detected hardware (pure Go, no SSH).
	configuredDisk := hrm.Spec.InstallDisk
	if configuredDisk == "" {
		configuredDisk = "/dev/nvme0n1"
	}
	installDisk, err := sshrescue.ResolveInstallDiskFromInfo(hw, configuredDisk)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolve install disk on %s: %w", serverIP, err)
	}
	if installDisk != configuredDisk {
		logger.Info("Install disk resolved to different device (NVMe name swap detected)",
			"configured", configuredDisk, "resolved", installDisk, "ip", serverIP)
	}

	// Resolve stable /dev/disk/by-id/ path from detected hardware (pure Go, no SSH).
	stableDisk := installDisk
	if byID, ok := hw.ByIDPaths[installDisk]; ok {
		logger.Info("Resolved install disk to stable by-id path",
			"bare", installDisk, "stable", byID, "ip", serverIP)
		stableDisk = byID
	}
	hrm.Status.ResolvedInstallDisk = stableDisk

	// Always wipe ALL NVMe disks for a clean slate — same contract as cloud providers.
	// Prevents boot loops from old Talos installs on other disks.
	// Ceph data recovery is Rook's responsibility (3x replica), not the infra provider's.
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

	factoryURL := hrc.Spec.TalosFactoryBaseURL
	if factoryURL == "" {
		factoryURL = talosFactoryDefaultBaseURL
	}

	// Use the BARE device path for the raw image dd in rescue.
	// The stable by-id path is only used for the machineconfig
	// (injected in stateApplyConfig), where Talos has full udev.
	if err := sshClient.InstallTalos(
		factoryURL,
		hrm.Spec.TalosSchematic,
		hrm.Spec.TalosVersion,
		installDisk,
	); err != nil {
		return ctrl.Result{}, fmt.Errorf("install Talos on %s: %w", serverIP, err)
	}

	logger.Info("Talos image written, fixing EFI boot order post-install", "ip", serverIP)

	// Fix EFI boot order after Talos install: delete ALL non-Talos, non-PXE
	// entries (e.g. old Debian "UEFI OS"), then set Talos FIRST, PXE LAST.
	// Some Hetzner BIOS firmwares ignore BootOrder and use their own NVMe
	// boot priority — deleting competing entries is the only reliable fix.
	if out, err := sshClient.Run(`
		if command -v efibootmgr > /dev/null 2>&1; then
			# Mount efivars read-write (rescue mounts it read-only by default)
			mount -o remount,rw /sys/firmware/efi/efivars 2>/dev/null || \
			mount -t efivarfs efivarfs /sys/firmware/efi/efivars 2>/dev/null || true

			echo "Before cleanup:"
			efibootmgr

			# Delete ALL boot entries except PXE/Network and Talos
			for entry in $(efibootmgr 2>/dev/null | grep '^Boot[0-9A-Fa-f]' | grep -iv 'pxe\|network\|ipv4\|ipv6\|talos' | grep -o '^Boot[0-9A-Fa-f]*' | sed 's/Boot//'); do
				echo "Deleting non-Talos/non-PXE entry: Boot${entry}"
				efibootmgr -b "$entry" -B 2>/dev/null || true
			done

			# Set boot order: Talos first, PXE last
			TALOS_NUM=$(efibootmgr | grep -i 'Talos' | grep -oP 'Boot\K[0-9A-Fa-f]+' | head -1)
			PXE_NUMS=$(efibootmgr | grep -i 'PXE\|Network\|IPv4' | grep -oP 'Boot\K[0-9A-Fa-f]+' | paste -sd,)
			if [ -n "$TALOS_NUM" ] && [ -n "$PXE_NUMS" ]; then
				efibootmgr -o "${TALOS_NUM},${PXE_NUMS}" 2>&1
			elif [ -n "$TALOS_NUM" ]; then
				efibootmgr -o "${TALOS_NUM}" 2>&1
			fi

			echo "After cleanup:"
			efibootmgr
		fi
	`); err != nil {
		logger.Info("Post-install EFI fix failed (non-fatal)", "error", err, "output", out)
	} else {
		logger.Info("Post-install EFI boot order fix applied", "output", out)
	}

	// Deactivate rescue BEFORE rebooting. PXE is first in boot order but rescue is
	// deactivated, so PXE silently fails → falls through to Talos.
	if err := robotClient.DeactivateRescue(ctx, serverID); err != nil {
		// Non-fatal: worst case the server boots back into rescue, and the next
		// reconcile (stateWaitInstall) will detect it and retry.
		logger.Error(err, "Failed to deactivate rescue, server may boot back to rescue",
			"serverID", serverID)
	}

	// Reboot into Talos
	sshClient.Run("reboot") //nolint:errcheck // reboot disconnects SSH, error expected

	hrm.Status.ProvisioningState = infrav1.StateInstalling
	// Raw dd install is fast (~30s). Talos reboots in 60-90s. First check at 90s.
	return ctrl.Result{RequeueAfter: 90 * time.Second}, nil
}

// stateWaitInstall transitions to BootingTalos after giving install time to complete.
func (r *HetznerRobotMachineReconciler) stateWaitInstall(
	ctx context.Context,
	hrm *infrav1.HetznerRobotMachine,
	hrc *infrav1.HetznerRobotCluster,
	robotClient *robot.Client,
	serverID int,
	serverIP string,
	machine *clusterv1.Machine,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Check if Talos maintenance mode is already up
	if talos.IsInMaintenanceMode(ctx, serverIP) {
		logger.Info("Talos maintenance mode detected after install", "ip", serverIP)
		hrm.Status.ProvisioningState = infrav1.StateBootingTalos
		return ctrl.Result{RequeueAfter: requeueAfterShort}, nil
	}

	// Edge case: if Talos is up but NOT in maintenance mode, the dd install
	// didn't fully wipe the STATE partition — old config persisted and Talos
	// booted in full mode. Wipe EFI + re-activate rescue to wipe and reinstall.
	if talos.IsUp(ctx, serverIP) {
		logger.Info("Talos booted in full mode after install (old config persisted) — wiping EFI + re-activating rescue to reinstall",
			"serverID", serverID, "ip", serverIP)
		r.attemptEFIWipe(ctx, serverIP, machine)
		hrm.Status.RetryCount++
		now := metav1.Now()
		hrm.Status.LastRetryTimestamp = &now
		return r.stateActivateRescue(ctx, hrm, hrc, robotClient, serverID, serverIP)
	}

	// DO NOT check sshrescue.IsReachable() here. After stateInstallTalos sends the
	// `reboot` command, the status patch triggers a watch event → immediate reconcile.
	// SSH port 22 stays open for several seconds after `reboot` is issued (the SSH
	// daemon hasn't shut down yet). Checking rescue SSH here would wrongly set
	// state=InRescue → trigger reinstall on a half-rebooted server → SSH drops
	// mid-wipe → state corruption. Only look for positive Talos signals.

	// Still waiting for reboot — stay in StateInstalling until maintenance mode is detected
	logger.Info("Waiting for Talos to boot (not yet in maintenance mode)", "ip", serverIP)
	return ctrl.Result{RequeueAfter: requeueAfterMedium}, nil
}

// stateWaitTalosMaintenanceMode waits until port 50000 is reachable.
func (r *HetznerRobotMachineReconciler) stateWaitTalosMaintenanceMode(
	ctx context.Context,
	hrm *infrav1.HetznerRobotMachine,
	machine *clusterv1.Machine,
	cluster *clusterv1.Cluster,
	hrc *infrav1.HetznerRobotCluster,
	serverIP string,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !talos.IsInMaintenanceMode(ctx, serverIP) {
		logger.Info("Talos not yet in maintenance mode", "ip", serverIP)
		return ctrl.Result{RequeueAfter: requeueAfterMedium}, nil
	}

	logger.Info("Talos in maintenance mode, proceeding to apply config", "ip", serverIP)

	// Set control plane endpoint on cluster if this is a control plane machine
	// and endpoint is not yet set. Use patch helper to avoid version conflicts
	// (r.Update would fail if the object was modified between read and write).
	if util.IsControlPlaneMachine(machine) && hrc.Spec.ControlPlaneEndpoint.Host == "" {
		hrcPatchHelper, patchErr := patch.NewHelper(hrc, r.Client)
		if patchErr != nil {
			return ctrl.Result{}, fmt.Errorf("init HRC patch helper: %w", patchErr)
		}
		hrc.Spec.ControlPlaneEndpoint = clusterv1.APIEndpoint{
			Host: serverIP,
			Port: 6443,
		}
		if patchErr = hrcPatchHelper.Patch(ctx, hrc); patchErr != nil {
			return ctrl.Result{}, fmt.Errorf("patch control plane endpoint: %w", patchErr)
		}
		logger.Info("Set control plane endpoint", "host", serverIP)
	}

	hrm.Status.ProvisioningState = infrav1.StateApplyingConfig
	return ctrl.Result{RequeueAfter: requeueAfterShort}, nil
}

func (r *HetznerRobotMachineReconciler) stateApplyConfig(
	ctx context.Context,
	hrm *infrav1.HetznerRobotMachine,
	machine *clusterv1.Machine,
	cluster *clusterv1.Cluster,
	hrc *infrav1.HetznerRobotCluster,
	hrh *infrav1.HetznerRobotHost,
	serverID int,
	serverIP string,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Get bootstrap data from CAPT bootstrap secret
	bootstrapData, err := r.getBootstrapData(ctx, machine)
	if err != nil {
		logger.Error(err, "Bootstrap data not ready yet")
		return ctrl.Result{RequeueAfter: requeueAfterShort}, nil
	}

	// Inject install disk into machineconfig — CAPT doesn't include it,
	// but Talos requires machine.install.disk to be set.
	// Prefer the stable /dev/disk/by-id/ path resolved during rescue install.
	// This ensures Talos references the correct physical disk regardless of
	// NVMe enumeration order (which can differ between rescue and Talos boot).
	installDisk := hrm.Status.ResolvedInstallDisk
	if installDisk == "" {
		// Fallback for machines provisioned before this fix was deployed
		installDisk = hrm.Spec.InstallDisk
		if installDisk == "" {
			installDisk = "/dev/nvme0n1"
		}
	}
	bootstrapData, err = injectInstallDisk(bootstrapData, installDisk)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("inject install disk into config: %w", err)
	}
	logger.Info("Injected install disk into machineconfig", "disk", installDisk)

	// Primary NIC MAC — detected during rescue install, stored in status
	primaryMAC := hrm.Status.PrimaryMAC
	if primaryMAC == "" {
		return ctrl.Result{}, fmt.Errorf("primary NIC MAC not detected — machine must go through rescue install first")
	}

	// Inject VLAN config if configured on the cluster.
	// Uses static /32 IP + explicit default route instead of DHCP to avoid Hetzner's
	// L2 isolation issue. DHCP assigns /25 which creates on-link routes — servers in
	// the same /25 try direct ARP instead of routing through the gateway. Hetzner
	// blocks this, breaking inter-node connectivity.
	if hrc.Spec.VLANConfig != nil {
		internalIP := hrh.Spec.InternalIP
		if internalIP == "" {
			return ctrl.Result{}, fmt.Errorf("VLANConfig is set on cluster but host %s has no internalIP", hrh.Name)
		}
		gatewayIP := hrm.Status.GatewayIP
		if gatewayIP == "" {
			return ctrl.Result{}, fmt.Errorf("gateway IP not detected — machine must go through rescue install first")
		}
		bootstrapData, err = injectVLANConfig(bootstrapData, hrc.Spec.VLANConfig, internalIP, primaryMAC, serverIP, gatewayIP)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("inject VLAN config: %w", err)
		}
		logger.Info("Injected VLAN config into machineconfig",
			"vlanID", hrc.Spec.VLANConfig.ID,
			"internalIP", internalIP,
			"serverIP", serverIP+"/32",
			"gatewayIP", gatewayIP)
	}

	// Inject deterministic hostname: <role>-<dc>-<serverID>.
	// Role from HetznerRobotHost label (storage → "storage-", else "compute-").
	// Server ID is immutable (Hetzner hardware ID) — zero collision risk.
	{
		dc := hrc.Spec.DC
		hostRole := hrh.Labels["role"]
		bootstrapData, err = injectHostname(bootstrapData, dc, serverID, hostRole)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("inject hostname into config: %w", err)
		}
		if dc == "" {
			dc = "fsn1"
		}
		prefix := "compute"
		if hostRole == "storage" {
			prefix = "storage"
		}
		hostname := fmt.Sprintf("%s-%s-%d", prefix, dc, serverID)
		logger.Info("Injected hostname into machineconfig", "hostname", hostname)
	}

	// Inject IPv6 config if the host has an IPv6 subnet from Hetzner.
	// Each Hetzner server gets a /64 — we assign ::1 and route via fe80::1.
	if hrh.Spec.ServerIPv6Net != "" {
		bootstrapData, err = injectIPv6Config(bootstrapData, hrh.Spec.ServerIPv6Net, primaryMAC, hrh.Spec.InternalIP)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("inject IPv6 config: %w", err)
		}
		logger.Info("Injected IPv6 config into machineconfig",
			"ipv6Net", hrh.Spec.ServerIPv6Net,
			"mac", primaryMAC)
	}

	// Inject cluster-level secrets from the Talos secret bundle.
	// CABPT generates unique keys per Machine, but all CP nodes sharing etcd must
	// use the same keys. Without this:
	// - Different secretboxEncryptionSecret → new CP nodes can't decrypt existing K8s secrets
	// - Different serviceAccount.key → API servers can't validate tokens signed by other CP nodes
	if hrc.Spec.TalosSecretRef != nil {
		bundle, err := r.getTalosSecretBundle(ctx, hrc)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("get talos secret bundle: %w", err)
		}
		if bundle != nil {
			if s := bundle.Secrets.SecretboxEncryptionSecret; s != "" {
				bootstrapData, err = injectSecretboxEncryptionSecret(bootstrapData, s)
				if err != nil {
					return ctrl.Result{}, fmt.Errorf("inject secretbox encryption secret: %w", err)
				}
				logger.Info("Injected secretboxEncryptionSecret from cluster talos secret")
			}

			if s := bundle.Secrets.K8sServiceAccount.Key; s != "" {
				bootstrapData, err = injectServiceAccountKey(bootstrapData, s)
				if err != nil {
					return ctrl.Result{}, fmt.Errorf("inject service account key: %w", err)
				}
				logger.Info("Injected serviceAccount.key from cluster talos secret")
			}
		}
	}

	// Inject providerID into kubelet extraArgs so the Node registers with the
	// correct providerID. Without this, CAPI can't match Machine → Node and the
	// Machine stays in Failed phase ("Waiting for a node with matching ProviderID").
	providerID := fmt.Sprintf("hetzner-robot://%d", serverID)
	bootstrapData, err = injectProviderID(bootstrapData, providerID)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("inject providerID into config: %w", err)
	}
	logger.Info("Injected providerID into machineconfig", "providerID", providerID)

	logger.Info("Applying Talos machineconfig", "ip", serverIP)

	applyCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	if err := talos.ApplyConfig(applyCtx, serverIP, bootstrapData); err != nil {
		return ctrl.Result{}, fmt.Errorf("apply Talos config to %s: %w", serverIP, err)
	}

	// Set the providerID on the HRM spec (propagates to Machine via CAPI)
	hrm.Spec.ProviderID = &providerID

	// After apply-config, Talos reboots. We must wait for it to come back before bootstrapping.
	// Move to WaitingForBoot for both CP and workers (CP will go → Bootstrapping, worker → Provisioned).
	if util.IsControlPlaneMachine(machine) {
		logger.Info("Config applied, waiting for Talos reboot", "serverID", serverID, "ip", serverIP)
		hrm.Status.ProvisioningState = infrav1.StateWaitingForBoot
		return ctrl.Result{RequeueAfter: requeueAfterMedium}, nil
	}

	// Worker: also wait for reboot, then mark provisioned
	logger.Info("Worker config applied, waiting for Talos reboot", "serverID", serverID, "ip", serverIP)
	hrm.Status.ProvisioningState = infrav1.StateWaitingForBoot
	return ctrl.Result{RequeueAfter: requeueAfterMedium}, nil
}

// stateWaitForBoot waits for Talos to come back up in running stage after a config-apply reboot.
func (r *HetznerRobotMachineReconciler) stateWaitForBoot(
	ctx context.Context,
	hrm *infrav1.HetznerRobotMachine,
	machine *clusterv1.Machine,
	serverIP string,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !talos.IsUp(ctx, serverIP) {
		logger.Info("Waiting for Talos to come up after config-apply reboot", "ip", serverIP)
		return ctrl.Result{RequeueAfter: requeueAfterMedium}, nil
	}

	logger.Info("Talos running after reboot", "ip", serverIP)

	// Both CP and worker: mark provisioned once Talos is running.
	// etcd bootstrap/join is CACPPT's responsibility, not CAPHR's.
	// Workers join via bootstrap token automatically.
	// CPs join etcd automatically via the endpoints in their machineconfig.
	hrm.Status.ProvisioningState = infrav1.StateProvisioned
	hrm.Status.Ready = true
	if util.IsControlPlaneMachine(machine) {
		logger.Info("Control plane machine provisioned — CACPPT handles etcd join", "ip", serverIP)
	} else {
		logger.Info("Worker machine provisioned successfully after boot", "ip", serverIP)
	}
	return ctrl.Result{}, nil
}

