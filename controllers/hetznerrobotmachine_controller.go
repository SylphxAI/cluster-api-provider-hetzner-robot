package controllers

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"gopkg.in/yaml.v3"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/patch"

	infrav1 "github.com/SylphxAI/cluster-api-provider-hetzner-robot/api/v1alpha1"
	"github.com/SylphxAI/cluster-api-provider-hetzner-robot/pkg/robot"
	"github.com/SylphxAI/cluster-api-provider-hetzner-robot/pkg/sshrescue"
	"github.com/SylphxAI/cluster-api-provider-hetzner-robot/pkg/talos"
)

const (
	requeueAfterShort = 15 * time.Second
	requeueAfterLong  = 60 * time.Second

	talosFactoryDefaultBaseURL = "https://factory.talos.dev"
)

// HetznerRobotMachineReconciler reconciles a HetznerRobotMachine object.
type HetznerRobotMachineReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hetznerrobotmachines,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hetznerrobotmachines/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hetznerrobotmachines/finalizers,verbs=update
//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters;clusters/status;machines;machines/status,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *HetznerRobotMachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch HetznerRobotMachine
	hrm := &infrav1.HetznerRobotMachine{}
	if err := r.Get(ctx, req.NamespacedName, hrm); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Fetch the CAPI Machine
	machine, err := util.GetOwnerMachine(ctx, r.Client, hrm.ObjectMeta)
	if err != nil {
		return ctrl.Result{}, err
	}
	if machine == nil {
		logger.Info("Machine controller has not yet set OwnerRef on HetznerRobotMachine")
		return ctrl.Result{RequeueAfter: requeueAfterShort}, nil
	}

	// Fetch the Cluster
	cluster, err := util.GetClusterFromMetadata(ctx, r.Client, machine.ObjectMeta)
	if err != nil {
		return ctrl.Result{}, err
	}
	if cluster == nil {
		logger.Info("Cluster not found")
		return ctrl.Result{RequeueAfter: requeueAfterShort}, nil
	}

	if cluster.Spec.Paused {
		logger.Info("HetznerRobotMachine or Cluster is paused")
		return ctrl.Result{}, nil
	}

	// Fetch the HetznerRobotCluster
	if cluster.Spec.InfrastructureRef == nil {
		logger.Info("Cluster.Spec.InfrastructureRef not set yet")
		return ctrl.Result{RequeueAfter: requeueAfterShort}, nil
	}
	hrc := &infrav1.HetznerRobotCluster{}
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: cluster.Spec.InfrastructureRef.Namespace,
		Name:      cluster.Spec.InfrastructureRef.Name,
	}, hrc); err != nil {
		logger.Info("HetznerRobotCluster not found yet")
		return ctrl.Result{RequeueAfter: requeueAfterShort}, nil
	}

	// Set up patch helper
	patchHelper, err := patch.NewHelper(hrm, r.Client)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to init patch helper: %w", err)
	}
	defer func() {
		if pErr := patchHelper.Patch(ctx, hrm); pErr != nil {
			logger.Error(pErr, "Failed to patch HetznerRobotMachine")
		}
	}()

	// Handle deletion
	if !hrm.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, hrm, hrc, machine)
	}

	// Add finalizer
	if !controllerutil.ContainsFinalizer(hrm, infrav1.MachineFinalizer) {
		controllerutil.AddFinalizer(hrm, infrav1.MachineFinalizer)
		return ctrl.Result{}, nil
	}

	// Enforce backoff: status patches trigger watch events that bypass RequeueAfter.
	// If we're in a retry state, check that enough time has elapsed before retrying.
	if hrm.Status.RetryCount > 0 && hrm.Status.LastRetryTimestamp != nil {
		expectedBackoff := computeBackoff(hrm.Status.RetryCount)
		elapsed := time.Since(hrm.Status.LastRetryTimestamp.Time)
		if elapsed < expectedBackoff {
			remaining := expectedBackoff - elapsed
			logger.V(1).Info("Backoff not yet elapsed, skipping reconcile",
				"retryCount", hrm.Status.RetryCount, "remaining", remaining)
			return ctrl.Result{RequeueAfter: remaining}, nil
		}
	}

	// Wrap reconcileNormal with error classification:
	// - Permanent errors → terminal StateError immediately (config issues, missing resources)
	// - Transient errors → exponential backoff with no max limit (SSH, API, network failures)
	result, err := r.reconcileNormal(ctx, hrm, hrc, machine, cluster)
	if err != nil {
		if isPermanentError(err) {
			// Terminal failure: unrecoverable configuration or resource issue.
			// Set FailureMessage + FailureReason → CAPI marks Machine as Failed.
			msg := err.Error()
			hrm.Status.FailureMessage = &msg
			reason := "PermanentError"
			hrm.Status.FailureReason = &reason
			hrm.Status.ProvisioningState = infrav1.StateError
			hrm.Status.Ready = false
			logger.Error(err, "Permanent error, entering terminal StateError",
				"state", hrm.Status.ProvisioningState)
			return ctrl.Result{}, nil
		}

		// Transient error: retry with exponential backoff, no max limit.
		// Do NOT set FailureMessage — CAPI interprets it as terminal failure
		// and marks the Machine as Failed. Transient errors are logged only.
		hrm.Status.RetryCount++
		now := metav1.Now()
		hrm.Status.LastRetryTimestamp = &now
		backoff := computeBackoff(hrm.Status.RetryCount)
		logger.Info("Transient error, will retry",
			"retryCount", hrm.Status.RetryCount, "backoff", backoff, "error", err.Error())
		return ctrl.Result{RequeueAfter: backoff}, nil
	}
	// Success: reset retry counter
	hrm.Status.RetryCount = 0
	hrm.Status.FailureMessage = nil
	hrm.Status.FailureReason = nil
	hrm.Status.LastRetryTimestamp = nil
	return result, nil
}

// isPermanentError returns true for errors that indicate an unrecoverable configuration
// or resource issue. These errors will never resolve through retrying — they require
// human intervention (fix config, add hosts to pool, create missing secrets).
// All other errors (SSH failures, API timeouts, connection refused, crane/installer
// failures) are treated as transient and will be retried indefinitely with backoff.
func isPermanentError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())

	// No hosts available — transient during rolling updates (hosts free up as
	// old Machines are deleted). NOT permanent: retry until a host becomes Available.
	// Removed from permanent list: was causing Machines to fail during scale-up
	// when all hosts are temporarily in use.
	// if strings.Contains(msg, "no available hetznerrobothost found") — TRANSIENT

	// Config parsing/validation errors — bad YAML, invalid structure
	if strings.Contains(msg, "unmarshal") || strings.Contains(msg, "invalid") {
		return true
	}

	// Missing bootstrap data secret — CAPI hasn't created it, or it was deleted
	if strings.Contains(msg, "secret") && strings.Contains(msg, "not found") {
		return true
	}

	// Missing required spec fields
	if strings.Contains(msg, "must specify either") {
		return true
	}

	return false
}

// computeBackoff calculates exponential backoff: 30s * 2^(retryCount-1), capped at 5 minutes.
func computeBackoff(retryCount int) time.Duration {
	exp := retryCount - 1
	if exp < 0 {
		exp = 0
	}
	// Cap the exponent to avoid overflow — 2^10 * 30s = 30720s >> 5min cap anyway
	if exp > 10 {
		exp = 10
	}
	backoff := 30 * time.Second * time.Duration(math.Pow(2, float64(exp)))
	if backoff > 5*time.Minute {
		backoff = 5 * time.Minute
	}
	return backoff
}

func (r *HetznerRobotMachineReconciler) reconcileNormal(
	ctx context.Context,
	hrm *infrav1.HetznerRobotMachine,
	hrc *infrav1.HetznerRobotCluster,
	machine *clusterv1.Machine,
	cluster *clusterv1.Cluster,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// If already provisioned, just ensure status is correct
	if hrm.Status.ProvisioningState == infrav1.StateProvisioned {
		hrm.Status.Ready = true
		// Ensure HRH state is also marked Provisioned (idempotent)
		if hrm.Status.HostRef != "" {
			if err := r.updateHostState(ctx, hrm.Namespace, hrm.Status.HostRef, infrav1.HostStateProvisioned); err != nil {
				logger.Error(err, "Failed to update HRH state to Provisioned")
			}
		}
		return ctrl.Result{}, nil
	}

	// Build Robot API client
	// Resolve HetznerRobotHost (claim if needed) to get serverID + serverIP.
	// Server info comes from the HRH, not from hrm.Spec directly.
	hrh, err := r.resolveHost(ctx, hrm)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolve host: %w", err)
	}

	robotClient, err := r.buildRobotClient(ctx, hrc)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("build robot client: %w", err)
	}

	serverID := hrh.Spec.ServerID
	serverIP := hrh.Spec.ServerIP
	logger.Info("Reconciling machine", "serverID", serverID, "ip", serverIP, "state", hrm.Status.ProvisioningState)

	// Set addresses
	hrm.Status.Addresses = []clusterv1.MachineAddress{
		{Type: clusterv1.MachineExternalIP, Address: serverIP},
	}

	// Run state machine
	switch hrm.Status.ProvisioningState {
	case infrav1.StateNone:
		// Optimization: if Talos is already in maintenance mode (e.g. after talosctl reset),
		// skip rescue/install and apply config directly.
		if talos.IsInMaintenanceMode(ctx, serverIP) {
			logger.Info("Node already in Talos maintenance mode, skipping rescue/install", "ip", serverIP)
			hrm.Status.ProvisioningState = infrav1.StateBootingTalos
			return ctrl.Result{RequeueAfter: requeueAfterShort}, nil
		}
		// Optimization: if rescue SSH is already open (server already in rescue mode),
		// skip rescue activation and go straight to install.
		if sshrescue.IsReachable(serverIP) {
			logger.Info("Node already in rescue mode (SSH reachable), skipping rescue activation", "ip", serverIP)
			hrm.Status.ProvisioningState = infrav1.StateInRescue
			return ctrl.Result{RequeueAfter: requeueAfterShort}, nil
		}
		return r.stateActivateRescue(ctx, hrm, hrc, robotClient, serverID, serverIP)
	case infrav1.StateActivatingRescue:
		return r.stateCheckRescueActive(ctx, hrm, hrc, robotClient, serverID, serverIP)
	case infrav1.StateInRescue:
		return r.stateInstallTalos(ctx, hrm, hrc, robotClient, serverID, serverIP)
	case infrav1.StateInstalling:
		return r.stateWaitInstall(ctx, hrm, hrc, robotClient, serverID, serverIP)
	case infrav1.StateBootingTalos:
		return r.stateWaitTalosMaintenanceMode(ctx, hrm, machine, cluster, hrc, serverIP)
	case infrav1.StateApplyingConfig:
		return r.stateApplyConfig(ctx, hrm, machine, cluster, hrc, hrh, serverID, serverIP)
	case infrav1.StateWaitingForBoot:
		return r.stateWaitForBoot(ctx, hrm, machine, serverIP)
	case infrav1.StateBootstrapping:
		return r.stateBootstrap(ctx, hrm, machine, cluster, serverIP)
	case infrav1.StateError:
		// Terminal state. No auto-recovery. No polling.
		// Recovery via MachineHealthCheck remediation or manual Machine deletion.
		logger.Info("Machine in terminal error state",
			"failureReason", hrm.Status.FailureReason,
			"failureMessage", hrm.Status.FailureMessage)
		return ctrl.Result{}, nil
	default:
		return ctrl.Result{}, nil
	}
}

// maxResetRetries is the maximum number of hw reset attempts before marking the machine
// as failed. If rescue boot keeps failing (EFI boot order issue), the post-install
// efibootmgr fix will prevent recurrence. For the initial case, an operator must
// manually request a reset via Hetzner Robot panel.
const maxResetRetries = 8

// stateActivateRescue activates rescue mode via Robot API and triggers a hw reset.
// After maxResetRetries failed attempts, marks the machine as failed (StateError).
// An operator must then manually fix the server (e.g., Hetzner Robot panel manual reset)
// and reset the Machine status to retry.
func (r *HetznerRobotMachineReconciler) stateActivateRescue(
	ctx context.Context,
	hrm *infrav1.HetznerRobotMachine,
	hrc *infrav1.HetznerRobotCluster,
	robotClient *robot.Client,
	serverID int,
	serverIP string,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if hrm.Status.RetryCount > maxResetRetries {
		logger.Error(nil, "FATAL: rescue boot failed after max retries — server needs manual intervention via Hetzner Robot panel",
			"serverID", serverID, "retryCount", hrm.Status.RetryCount)
		hrm.Status.ProvisioningState = infrav1.StateError
		return ctrl.Result{}, nil
	}

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
	return ctrl.Result{RequeueAfter: 90 * time.Second}, nil
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
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Priority 1: Check Talos maintenance mode (UEFI NVMe-first boot order may skip rescue PXE).
	if talos.IsInMaintenanceMode(ctx, serverIP) {
		logger.Info("Talos maintenance mode detected, skipping rescue/install", "ip", serverIP)
		hrm.Status.ProvisioningState = infrav1.StateBootingTalos
		return ctrl.Result{RequeueAfter: requeueAfterShort}, nil
	}

	// Priority 2: Check if rescue SSH is already up.
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
				_, _ = client.Run(`
					if command -v efibootmgr > /dev/null 2>&1; then
						mount -o remount,rw /sys/firmware/efi/efivars 2>/dev/null || \
						mount -t efivarfs efivarfs /sys/firmware/efi/efivars 2>/dev/null || true
						for entry in $(efibootmgr 2>/dev/null | grep '^Boot[0-9A-Fa-f]' | grep -iv 'pxe\|network\|ipv4\|ipv6' | grep -o '^Boot[0-9A-Fa-f]*' | sed 's/Boot//'); do
							efibootmgr -b "$entry" -B 2>/dev/null
						done
					fi
					nohup bash -c 'sleep 1 && reboot' &>/dev/null &
				`)
				logger.Info("Rebooted existing OS with PXE-only EFI, should enter rescue on next boot", "ip", serverIP)
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
		// Rescue inactive AND SSH closed. Could mean:
		//   (a) Server rebooted back to normal OS (old Talos running, NOT our cluster) → re-activate rescue
		//   (b) Talos was successfully installed and is in maintenance mode → proceed
		//   (c) Talos was successfully installed, config applied, running in full mode → advance
		//
		// IMPORTANT: We can ONLY safely skip rescue/install if Talos is in maintenance mode.
		// If Talos is in full running mode, we CANNOT distinguish between:
		//   - Our freshly installed Talos (config applied, node joined) → safe to advance
		//   - An OLD Talos from a previous cluster (different CA) → MUST rescue+wipe
		//
		// Since the HRM is not yet in StateProvisioned (we're in StateCheckRescueActive),
		// the only safe assumption for full-mode Talos is: it's stale. Re-activate rescue.
		if talos.IsInMaintenanceMode(ctx, serverIP) {
			// Maintenance mode: Talos installed, waiting for machineconfig.
			logger.Info("Talos in maintenance mode (rescue consumed after install), proceeding", "ip", serverIP)
			hrm.Status.ProvisioningState = infrav1.StateBootingTalos
			return ctrl.Result{RequeueAfter: requeueAfterShort}, nil
		}
		// Either Talos is in full mode (stale OS) or not reachable at all.
		// In both cases, re-activate rescue and wipe.
		hrm.Status.RetryCount++
		now := metav1.Now()
		hrm.Status.LastRetryTimestamp = &now
		if hrm.Status.RetryCount > maxResetRetries {
			logger.Error(nil, "FATAL: rescue boot failed after max retries — server needs manual BIOS intervention (set PXE as first boot option)",
				"serverID", serverID, "retryCount", hrm.Status.RetryCount)
			hrm.Status.ProvisioningState = infrav1.StateError
			return ctrl.Result{}, nil
		}
		if talos.IsUp(ctx, serverIP) {
			logger.Info("Talos running in full mode during early provisioning — treating as stale OS, re-activating rescue",
				"serverID", serverID, "ip", serverIP, "retryCount", hrm.Status.RetryCount)
		} else {
			logger.Info("Rescue no longer active and nothing reachable, re-activating rescue",
				"serverID", serverID, "ip", serverIP, "retryCount", hrm.Status.RetryCount)
		}
		return r.stateActivateRescue(ctx, hrm, hrc, robotClient, serverID, serverIP)
	}

	// Rescue is armed but SSH not up yet.
	// Edge case: UEFI/BIOS boot order may skip PXE and boot Talos from NVMe instead
	// of the rescue system. Detect this by checking if Talos is already up.
	if talos.IsUp(ctx, serverIP) {
		if talos.IsInMaintenanceMode(ctx, serverIP) {
			// Talos maintenance mode — skip rescue, proceed directly to config apply.
			logger.Info("Rescue active but Talos booted from disk in maintenance mode — skipping rescue/install", "ip", serverIP)
			hrm.Status.ProvisioningState = infrav1.StateBootingTalos
			return ctrl.Result{RequeueAfter: requeueAfterShort}, nil
		}
		// Talos is running in full mode (old/stale config) — rescue didn't take effect.
		hrm.Status.RetryCount++
		now2 := metav1.Now()
		hrm.Status.LastRetryTimestamp = &now2
		if hrm.Status.RetryCount > maxResetRetries {
			logger.Error(nil, "FATAL: server keeps booting Talos instead of PXE rescue after max retries — needs manual BIOS intervention",
				"serverID", serverID, "retryCount", hrm.Status.RetryCount)
			hrm.Status.ProvisioningState = infrav1.StateError
			return ctrl.Result{}, nil
		}
		logger.Info("Rescue active but Talos booted from disk instead of PXE rescue — re-activating rescue",
			"serverID", serverID, "ip", serverIP, "retryCount", hrm.Status.RetryCount)
		return r.stateActivateRescue(ctx, hrm, hrc, robotClient, serverID, serverIP)
	}

	// Neither SSH (rescue) nor Talos (disk) is up — server is still booting.
	logger.Info("Rescue active, SSH not yet reachable, waiting", "ip", serverIP)
	return ctrl.Result{RequeueAfter: requeueAfterLong}, nil
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

	// Recovery: if Talos maintenance mode is already up (install succeeded but state wasn't saved),
	// skip to BootingTalos state.
	if talos.IsInMaintenanceMode(ctx, serverIP) {
		logger.Info("Talos already in maintenance mode (install completed), skipping re-install", "ip", serverIP)
		hrm.Status.ProvisioningState = infrav1.StateBootingTalos
		return ctrl.Result{RequeueAfter: requeueAfterShort}, nil
	}

	// Also recovery: if port 22 is not accessible, the node may have already rebooted
	// from a previous install attempt. Activate rescue again and retry.
	if !sshrescue.IsReachable(serverIP) {
		logger.Info("Rescue SSH not reachable in InRescue state, activating rescue again", "ip", serverIP)
		hrm.Status.ProvisioningState = infrav1.StateNone
		return ctrl.Result{RequeueAfter: requeueAfterShort}, nil
	}

	logger.Info("Installing Talos via rescue SSH", "ip", serverIP)

	// Get private key
	privateKey, err := r.getSSHPrivateKey(ctx, hrc)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get SSH private key: %w", err)
	}

	sshClient := sshrescue.New(serverIP, privateKey)
	if err := sshClient.Connect(); err != nil {
		return ctrl.Result{}, fmt.Errorf("SSH connect to rescue %s: %w", serverIP, err)
	}
	defer sshClient.Close()

	// Auto-detect primary NIC MAC address from rescue.
	// Used for Talos deviceSelector (hardware-based, not name-based).
	// NIC names differ between rescue (eth0) and Talos (enp193s0f0np0) — MAC is stable.
	primaryMAC, err := sshClient.Run(`ip link show $(ip route show default | awk '{print $5}') | grep ether | awk '{print $2}'`)
	if err != nil || strings.TrimSpace(primaryMAC) == "" {
		return ctrl.Result{}, fmt.Errorf("detect primary NIC MAC on %s: %w (output: %s)", serverIP, err, primaryMAC)
	}
	primaryMAC = strings.TrimSpace(primaryMAC)
	hrm.Status.PrimaryMAC = primaryMAC
	logger.Info("Detected primary NIC MAC", "mac", primaryMAC, "ip", serverIP)

	configuredDisk := hrm.Spec.InstallDisk
	if configuredDisk == "" {
		configuredDisk = "/dev/nvme0n1"
	}

	// Resolve the actual install disk by checking which NVMe device is safe.
	// NVMe device names can swap between rescue and Talos boot due to different
	// PCI probe order. ResolveInstallDisk picks the disk WITHOUT Ceph BlueStore.
	installDisk, err := sshClient.ResolveInstallDisk(configuredDisk)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolve install disk on %s: %w", serverIP, err)
	}
	if installDisk != configuredDisk {
		logger.Info("Install disk resolved to different device (NVMe name swap detected)",
			"configured", configuredDisk, "resolved", installDisk, "ip", serverIP)
	}

	// Resolve the install disk to a stable /dev/disk/by-id/ path.
	// NVMe device names (/dev/nvme0n1) swap between rescue and Talos boot due to
	// different PCI probe order. The by-id path references the physical disk by
	// serial number, so both the installer in rescue AND Talos at boot will
	// reference the same physical disk regardless of enumeration order.
	stableDisk, err := sshClient.ResolveStableDiskPath(installDisk)
	if err != nil {
		// Non-fatal: fall back to unstable path
		logger.Info("Could not resolve stable disk path, using bare device name",
			"disk", installDisk, "error", err)
		stableDisk = installDisk
	}
	if stableDisk != installDisk {
		logger.Info("Resolved install disk to stable by-id path",
			"bare", installDisk, "stable", stableDisk, "ip", serverIP)
	}

	// Persist the stable disk path in status so stateApplyConfig can inject it
	// into the Talos machineconfig. This ensures Talos uses the by-id path for
	// upgrades, surviving NVMe enumeration changes across reboots.
	hrm.Status.ResolvedInstallDisk = stableDisk

	// Storage nodes (ephemeralSize set): wipe ALL NVMe disks for fresh provision.
	// Old Talos installs on OTHER disks cause boot loops — the server boots from
	// the old install instead of the new one. This is safe because fresh storage
	// nodes have no existing Ceph data.
	//
	// Compute nodes (no ephemeralSize): wipe ONLY the OS install disk.
	// Ceph OSD data on other disks must survive reprovision.
	isStorageNode := hrm.Spec.EphemeralSize != ""
	if isStorageNode {
		logger.Info("Storage node: wiping ALL NVMe disks for fresh provision",
			"ip", serverIP, "installDisk", stableDisk, "ephemeralSize", hrm.Spec.EphemeralSize)
		if out, err := sshClient.WipeAllDisks(stableDisk); err != nil {
			return ctrl.Result{}, fmt.Errorf("wipe all disks on %s: %w\nOutput: %s", serverIP, err, out)
		} else {
			logger.Info("All NVMe disks wiped", "ip", serverIP, "output", out)
		}
	} else {
		logger.Info("Compute node: wiping OS disk only", "ip", serverIP, "disk", stableDisk)
		if out, err := sshClient.WipeOSDisk(installDisk); err != nil {
			return ctrl.Result{}, fmt.Errorf("wipe OS disk %s on %s: %w\nOutput: %s", installDisk, serverIP, err, out)
		} else {
			logger.Info("OS disk wiped", "ip", serverIP, "disk", installDisk, "output", out)
		}
	}

	factoryURL := hrc.Spec.TalosFactoryBaseURL
	if factoryURL == "" {
		factoryURL = talosFactoryDefaultBaseURL
	}

	// Use the BARE device path for the Talos installer in rescue chroot.
	// The by-id symlinks (/dev/disk/by-id/) may not exist inside the unshare chroot
	// because udev doesn't run there. The stable by-id path is only used for the
	// machineconfig (injected in stateApplyConfig), where Talos has full udev.
	if err := sshClient.InstallTalos(
		factoryURL,
		hrm.Spec.TalosSchematic,
		hrm.Spec.TalosVersion,
		installDisk, // bare path like /dev/nvme0n1 — works in rescue chroot
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
	return ctrl.Result{RequeueAfter: 3 * time.Minute}, nil
}

// stateWaitInstall transitions to BootingTalos after giving install time to complete.
func (r *HetznerRobotMachineReconciler) stateWaitInstall(
	ctx context.Context,
	hrm *infrav1.HetznerRobotMachine,
	hrc *infrav1.HetznerRobotCluster,
	robotClient *robot.Client,
	serverID int,
	serverIP string,
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
	// booted in full mode. Re-activate rescue to wipe and reinstall cleanly.
	if talos.IsUp(ctx, serverIP) {
		logger.Info("Talos booted in full mode after install (old config persisted) — re-activating rescue to reinstall",
			"serverID", serverID, "ip", serverIP)
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
	return ctrl.Result{RequeueAfter: requeueAfterLong}, nil
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
		return ctrl.Result{RequeueAfter: requeueAfterLong}, nil
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

// injectInstallDisk ensures machine.install.disk is set in the Talos machineconfig YAML.
// CAPT generates configs without install disk — CAPHR must inject it from the HRM spec
// before applying, otherwise Talos rejects the config with "install disk or diskSelector should be defined".
func injectInstallDisk(configData []byte, installDisk string) ([]byte, error) {
	if installDisk == "" {
		installDisk = "/dev/nvme0n1"
	}

	var config map[string]interface{}
	if err := yaml.Unmarshal(configData, &config); err != nil {
		return nil, fmt.Errorf("unmarshal machineconfig: %w", err)
	}

	// Ensure machine.install.disk exists
	machine, ok := config["machine"].(map[string]interface{})
	if !ok {
		machine = make(map[string]interface{})
		config["machine"] = machine
	}

	install, ok := machine["install"].(map[string]interface{})
	if !ok {
		install = make(map[string]interface{})
		machine["install"] = install
	}

	// Only set if not already defined (don't override explicit config)
	if _, exists := install["disk"]; !exists {
		install["disk"] = installDisk
	}

	return yaml.Marshal(config)
}

// injectCiliumStartupTaint adds node.cilium.io/agent-not-ready taint to kubelet config.
// Cilium removes this taint automatically once initialized. Prevents pod scheduling
// failures during the 1-2 min Cilium warmup on new nodes.
func injectCiliumStartupTaint(configData []byte) ([]byte, error) {
	var config map[string]interface{}
	if err := yaml.Unmarshal(configData, &config); err != nil {
		return nil, err
	}

	machine, ok := config["machine"].(map[string]interface{})
	if !ok {
		machine = make(map[string]interface{})
		config["machine"] = machine
	}

	kubelet, ok := machine["kubelet"].(map[string]interface{})
	if !ok {
		kubelet = make(map[string]interface{})
		machine["kubelet"] = kubelet
	}

	extraArgs, ok := kubelet["extraArgs"].(map[string]interface{})
	if !ok {
		extraArgs = make(map[string]interface{})
		kubelet["extraArgs"] = extraArgs
	}

	extraArgs["register-with-taints"] = "node.cilium.io/agent-not-ready=true:NoSchedule"
	return yaml.Marshal(config)
}

// injectIPv6Config adds a global IPv6 address and default route to the primary NIC.
// Uses deviceSelector by MAC address — works regardless of OS NIC naming
// (rescue uses eth0, Talos uses enp193s0f0np0, etc.).
// Also sets kubelet dual-stack nodeIP and IPv6 forwarding sysctl.
func injectIPv6Config(configData []byte, ipv6Net string, primaryMAC string, internalIP string) ([]byte, error) {
	if ipv6Net == "" || primaryMAC == "" {
		return configData, nil
	}

	var config map[string]interface{}
	if err := yaml.Unmarshal(configData, &config); err != nil {
		return nil, fmt.Errorf("unmarshal machineconfig for IPv6 injection: %w", err)
	}

	machine, ok := config["machine"].(map[string]interface{})
	if !ok {
		machine = make(map[string]interface{})
		config["machine"] = machine
	}

	network, ok := machine["network"].(map[string]interface{})
	if !ok {
		network = make(map[string]interface{})
		machine["network"] = network
	}

	// ipv6Net from Hetzner API may include prefix length (e.g. "2a01:4f8:271:3b49::/64").
	// Strip it before constructing the host address.
	ipv6Prefix := strings.Split(ipv6Net, "/")[0] // "2a01:4f8:271:3b49::"
	ipv6Addr := ipv6Prefix + "1/64"              // "2a01:4f8:271:3b49::1/64"

	// Find existing interface by deviceSelector MAC or create new
	interfaces, _ := network["interfaces"].([]interface{})
	found := false
	for _, iface := range interfaces {
		ifMap, ok := iface.(map[string]interface{})
		if !ok {
			continue
		}
		// Match by deviceSelector.hardwareAddr or by legacy interface name
		selector, _ := ifMap["deviceSelector"].(map[string]interface{})
		if (selector != nil && selector["hardwareAddr"] == primaryMAC) || ifMap["interface"] == primaryMAC {
			addrs, _ := ifMap["addresses"].([]interface{})
			addrs = append(addrs, ipv6Addr)
			ifMap["addresses"] = addrs
			routes, _ := ifMap["routes"].([]interface{})
			routes = append(routes, map[string]interface{}{
				"network": "::/0",
				"gateway": "fe80::1",
			})
			ifMap["routes"] = routes
			found = true
			break
		}
	}

	if !found {
		newIface := map[string]interface{}{
			"deviceSelector": map[string]interface{}{
				"hardwareAddr": primaryMAC, "physical": true,
			},
			"addresses": []interface{}{ipv6Addr},
			"routes": []interface{}{
				map[string]interface{}{
					"network": "::/0",
					"gateway": "fe80::1",
				},
			},
		}
		interfaces = append(interfaces, newIface)
		network["interfaces"] = interfaces
	}

	// Set IPv6 forwarding sysctl (required for pod routing)
	sysctls, ok := machine["sysctls"].(map[string]interface{})
	if !ok {
		sysctls = make(map[string]interface{})
		machine["sysctls"] = sysctls
	}
	sysctls["net.ipv6.conf.all.forwarding"] = "1"

	// Set kubelet nodeIP for dual-stack: IPv4 (VLAN) + IPv6.
	// Without this, kubelet only advertises the IPv4 address and K8s
	// doesn't know the node has IPv6 connectivity.
	kubelet, ok := machine["kubelet"].(map[string]interface{})
	if !ok {
		kubelet = make(map[string]interface{})
		machine["kubelet"] = kubelet
	}
	extraArgs, ok := kubelet["extraArgs"].(map[string]interface{})
	if !ok {
		extraArgs = make(map[string]interface{})
		kubelet["extraArgs"] = extraArgs
	}
	// Kubelet dual-stack nodeIP: VLAN IPv4 + public IPv6.
	// Both are needed for K8s to advertise the node as dual-stack.
	nodeIPv6 := strings.TrimSuffix(ipv6Prefix, "::") + "::1" // e.g. 2a01:4f8:2210:1a2e::1
	if internalIP != "" {
		extraArgs["node-ip"] = internalIP + "," + nodeIPv6
	} else {
		extraArgs["node-ip"] = nodeIPv6
	}

	return yaml.Marshal(config)
}

// injectHostname sets machine.network.hostname in the Talos machineconfig.
//
// Format: <role>-<dc>-<serverID>
//   - role: from HetznerRobotHost label "role" (e.g. "compute", "storage")
//     Defaults to "compute" for control-plane and worker roles.
//   - dc: Hetzner datacenter (e.g. "fsn1") — from HetznerRobotCluster.Spec.DC
//   - serverID: Hetzner Robot server ID (immutable hardware identifier)
//
// Examples: "compute-fsn1-2938104", "storage-fsn1-2965124"
func injectHostname(configData []byte, dc string, serverID int, hostRole string) ([]byte, error) {
	if serverID == 0 {
		return configData, nil
	}

	if dc == "" {
		dc = "fsn1"
	}
	// Map host role label to hostname prefix
	prefix := "compute"
	if hostRole == "storage" {
		prefix = "storage"
	}
	hostname := fmt.Sprintf("%s-%s-%d", prefix, dc, serverID)

	var config map[string]interface{}
	if err := yaml.Unmarshal(configData, &config); err != nil {
		return nil, fmt.Errorf("unmarshal machineconfig for hostname injection: %w", err)
	}

	machine, ok := config["machine"].(map[string]interface{})
	if !ok {
		machine = make(map[string]interface{})
		config["machine"] = machine
	}

	network, ok := machine["network"].(map[string]interface{})
	if !ok {
		network = make(map[string]interface{})
		machine["network"] = network
	}

	network["hostname"] = hostname
	return yaml.Marshal(config)
}

// injectVLANConfig adds a VLAN interface to the Talos machineconfig.
// Uses deviceSelector by MAC address for parent NIC identification.
// Ensures dhcp: true on parent (preserves public IP) and adds VLAN with internal IP.
func injectVLANConfig(configData []byte, vlanCfg *infrav1.VLANConfig, internalIP string, primaryMAC string) ([]byte, error) {
	if vlanCfg == nil || internalIP == "" {
		return configData, nil
	}

	prefixLen := vlanCfg.PrefixLength
	if prefixLen == 0 {
		prefixLen = 24
	}

	var config map[string]interface{}
	if err := yaml.Unmarshal(configData, &config); err != nil {
		return nil, fmt.Errorf("unmarshal machineconfig for VLAN injection: %w", err)
	}

	machine, ok := config["machine"].(map[string]interface{})
	if !ok {
		machine = make(map[string]interface{})
		config["machine"] = machine
	}

	network, ok := machine["network"].(map[string]interface{})
	if !ok {
		network = make(map[string]interface{})
		machine["network"] = network
	}

	// Build the VLAN entry
	vlanEntry := map[string]interface{}{
		"vlanId": vlanCfg.ID,
		"addresses": []interface{}{
			fmt.Sprintf("%s/%d", internalIP, prefixLen),
		},
	}

	// Build the interface entry with deviceSelector + dhcp: true + VLAN
	ifaceEntry := map[string]interface{}{
		"deviceSelector": map[string]interface{}{
			"hardwareAddr": primaryMAC, "physical": true,
		},
		"dhcp":  true, // CRITICAL: preserve public IP via DHCP
		"vlans": []interface{}{vlanEntry},
	}

	// Find or create the interfaces list
	interfaces, ok := network["interfaces"].([]interface{})
	if !ok {
		interfaces = []interface{}{}
	}

	// Check if an entry for this NIC already exists (by deviceSelector MAC) — merge VLAN into it
	found := false
	for i, iface := range interfaces {
		ifMap, ok := iface.(map[string]interface{})
		if !ok {
			continue
		}
		selector, _ := ifMap["deviceSelector"].(map[string]interface{})
		if (selector != nil && selector["hardwareAddr"] == primaryMAC) || ifMap["interface"] == primaryMAC {
			// Ensure dhcp: true is set
			ifMap["dhcp"] = true
			// Add VLAN to existing vlans list (or create new)
			existingVlans, _ := ifMap["vlans"].([]interface{})
			ifMap["vlans"] = append(existingVlans, vlanEntry)
			interfaces[i] = ifMap
			found = true
			break
		}
	}

	if !found {
		interfaces = append(interfaces, ifaceEntry)
	}

	network["interfaces"] = interfaces
	return yaml.Marshal(config)
}

// injectSecretboxEncryptionSecret replaces cluster.secretboxEncryptionSecret in the
// machineconfig YAML. CAPT may generate a different encryption key per Machine, but all
// CP nodes must use the same key to decrypt secrets in shared etcd. This function ensures
// the correct cluster-wide key is used.
func injectSecretboxEncryptionSecret(configData []byte, secret string) ([]byte, error) {
	if secret == "" {
		return configData, nil
	}

	var config map[string]interface{}
	if err := yaml.Unmarshal(configData, &config); err != nil {
		return nil, fmt.Errorf("unmarshal config for secretbox injection: %w", err)
	}

	cluster, _ := config["cluster"].(map[string]interface{})
	if cluster == nil {
		cluster = make(map[string]interface{})
		config["cluster"] = cluster
	}

	cluster["secretboxEncryptionSecret"] = secret
	return yaml.Marshal(config)
}

// injectServiceAccountKey overrides cluster.serviceAccount.key in the Talos machineconfig.
// CABPT generates a unique SA key per Machine, but all CP nodes sharing etcd must use the
// same key — otherwise API servers can't validate tokens signed by other CP nodes.
// Workers are unaffected (they don't run kube-apiserver), but injecting consistently
// ensures correctness if a worker is later promoted.
func injectServiceAccountKey(configData []byte, saKey string) ([]byte, error) {
	if saKey == "" {
		return configData, nil
	}

	var config map[string]interface{}
	if err := yaml.Unmarshal(configData, &config); err != nil {
		return nil, fmt.Errorf("unmarshal config for SA key injection: %w", err)
	}

	cluster, _ := config["cluster"].(map[string]interface{})
	if cluster == nil {
		cluster = make(map[string]interface{})
		config["cluster"] = cluster
	}

	sa, _ := cluster["serviceAccount"].(map[string]interface{})
	if sa == nil {
		sa = make(map[string]interface{})
		cluster["serviceAccount"] = sa
	}

	sa["key"] = saKey
	return yaml.Marshal(config)
}

// injectProviderID sets machine.kubelet.extraArgs["provider-id"] in the Talos
// machineconfig. This causes kubelet to register the Node with the correct providerID,
// allowing CAPI to match Machine → Node. Without this, CAPI can't find the Node
// and the Machine stays in Failed phase.
func injectProviderID(configData []byte, providerID string) ([]byte, error) {
	if providerID == "" {
		return configData, nil
	}

	var config map[string]interface{}
	if err := yaml.Unmarshal(configData, &config); err != nil {
		return nil, fmt.Errorf("unmarshal config for providerID injection: %w", err)
	}

	machine, ok := config["machine"].(map[string]interface{})
	if !ok {
		machine = make(map[string]interface{})
		config["machine"] = machine
	}

	kubelet, ok := machine["kubelet"].(map[string]interface{})
	if !ok {
		kubelet = make(map[string]interface{})
		machine["kubelet"] = kubelet
	}

	extraArgs, ok := kubelet["extraArgs"].(map[string]interface{})
	if !ok {
		extraArgs = make(map[string]interface{})
		kubelet["extraArgs"] = extraArgs
	}

	extraArgs["provider-id"] = providerID
	return yaml.Marshal(config)
}

// injectStorageVolumes appends VolumeConfig + RawVolumeConfig YAML documents to
// the machineconfig when EphemeralSize is set. Uses Talos v1.12+ native volume
// management instead of post-install sgdisk manipulation.
//
// VolumeConfig limits the EPHEMERAL partition to the specified maxSize.
// RawVolumeConfig creates an "osd-data" raw partition with remaining space,
// which appears at /dev/disk/by-partlabel/r-osd-data for Ceph OSD use.
//
// The volume name MUST NOT contain "ceph" — Ceph inventory rejects partitions
// with "ceph" in PARTLABEL.
func injectStorageVolumes(configData []byte, ephemeralSize string) ([]byte, error) {
	if ephemeralSize == "" {
		return configData, nil
	}

	volumeConfig := fmt.Sprintf(`---
apiVersion: v1alpha1
kind: VolumeConfig
name: EPHEMERAL
provisioning:
  maxSize: %s
`, ephemeralSize)

	rawVolumeConfig := `---
apiVersion: v1alpha1
kind: RawVolumeConfig
name: osd-data
provisioning:
  diskSelector:
    match: system_disk
  minSize: 50GB
`

	// Append the volume documents to the machineconfig as a multi-document YAML stream.
	// Talos config apply accepts multi-doc YAML — each document is processed independently.
	result := append(configData, []byte("\n"+volumeConfig+rawVolumeConfig)...)
	return result, nil
}

// stateApplyConfig applies the Talos machineconfig from the bootstrap secret.
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

	// Inject Cilium startup taint: prevents workload scheduling until Cilium is ready.
	// Cilium agent automatically removes this taint once it's initialized on the node.
	// Without this, pods scheduled to a new node fail with "unable to create endpoint".
	bootstrapData, err = injectCiliumStartupTaint(bootstrapData)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("inject Cilium startup taint: %w", err)
	}

	// Primary NIC MAC — detected during rescue install, stored in status
	primaryMAC := hrm.Status.PrimaryMAC
	if primaryMAC == "" {
		return ctrl.Result{}, fmt.Errorf("primary NIC MAC not detected — machine must go through rescue install first")
	}

	// Inject VLAN config if configured on the cluster
	if hrc.Spec.VLANConfig != nil {
		internalIP := hrh.Spec.InternalIP
		if internalIP == "" {
			return ctrl.Result{}, fmt.Errorf("VLANConfig is set on cluster but host %s has no internalIP", hrh.Name)
		}
		bootstrapData, err = injectVLANConfig(bootstrapData, hrc.Spec.VLANConfig, internalIP, primaryMAC)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("inject VLAN config: %w", err)
		}
		logger.Info("Injected VLAN config into machineconfig",
			"vlanID", hrc.Spec.VLANConfig.ID,
			"interface", hrc.Spec.VLANConfig.Interface,
			"internalIP", internalIP)
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

	// Inject storage volume config if ephemeralSize is set.
	// Appends VolumeConfig (limits EPHEMERAL) + RawVolumeConfig (creates raw OSD partition)
	// as additional YAML documents. Talos v1.12+ processes these natively during boot.
	if hrm.Spec.EphemeralSize != "" {
		bootstrapData, err = injectStorageVolumes(bootstrapData, hrm.Spec.EphemeralSize)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("inject storage volumes config: %w", err)
		}
		logger.Info("Injected storage volume config into machineconfig",
			"ephemeralSize", hrm.Spec.EphemeralSize)
	}

	logger.Info("Applying Talos machineconfig", "ip", serverIP)

	applyCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	if err := talos.ApplyConfig(applyCtx, serverIP, bootstrapData); err != nil {
		return ctrl.Result{}, fmt.Errorf("apply Talos config to %s: %w", serverIP, err)
	}

	// Set the providerID on the HRM spec (propagates to Machine via CAPI)
	hrm.Spec.ProviderID = &providerID

	// After apply-config, Talos reboots. We must wait for it to come back before bootstrapping.
	// Move to WaitingForBoot for both CP and workers (CP will go → Bootstrapping, worker → Provisioned).
	if util.IsControlPlaneMachine(machine) {
		logger.Info("Config applied, waiting for Talos reboot before bootstrapping", "serverID", serverID, "ip", serverIP)
		hrm.Status.ProvisioningState = infrav1.StateWaitingForBoot
		return ctrl.Result{RequeueAfter: requeueAfterLong}, nil
	}

	// Worker: also wait for reboot, then mark provisioned
	logger.Info("Worker config applied, waiting for Talos reboot", "serverID", serverID, "ip", serverIP)
	hrm.Status.ProvisioningState = infrav1.StateWaitingForBoot
	return ctrl.Result{RequeueAfter: requeueAfterLong}, nil
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
		return ctrl.Result{RequeueAfter: requeueAfterLong}, nil
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

// stateBootstrap calls `talosctl bootstrap` on the init control plane to initialize etcd.
// For joining control planes (not init), bootstrap is a no-op / returns AlreadyExists.
func (r *HetznerRobotMachineReconciler) stateBootstrap(
	ctx context.Context,
	hrm *infrav1.HetznerRobotMachine,
	machine *clusterv1.Machine,
	cluster *clusterv1.Cluster,
	serverIP string,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// If K8s API is already up, bootstrap already happened (e.g. additional CP joining)
	if talos.IsK8sAPIUp(ctx, serverIP) {
		logger.Info("K8s API already up, no bootstrap needed", "ip", serverIP)
		hrm.Status.ProvisioningState = infrav1.StateProvisioned
		hrm.Status.Ready = true
		return ctrl.Result{}, nil
	}

	// Guard: if Talos API is not reachable, the node is still rebooting — wait, don't error.
	if !talos.IsUp(ctx, serverIP) {
		logger.Info("Talos API not yet reachable, waiting for node to finish booting", "ip", serverIP)
		return ctrl.Result{RequeueAfter: requeueAfterLong}, nil
	}

	// Guard: if still in maintenance mode, config hasn't taken effect yet — wait.
	if talos.IsInMaintenanceMode(ctx, serverIP) {
		logger.Info("Talos still in maintenance mode, config apply not yet effective, waiting", "ip", serverIP)
		return ctrl.Result{RequeueAfter: requeueAfterLong}, nil
	}

	// Build authenticated TLS config from the actual machineconfig applied to this node.
	machineConfigData, err := r.getBootstrapData(ctx, machine)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get machineconfig for TLS: %w", err)
	}

	tlsCfg, err := talos.AdminTLSConfig(machineConfigData)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("generate admin TLS config from machineconfig: %w", err)
	}

	logger.Info("Bootstrapping etcd on init control plane via native gRPC", "serverIP", serverIP)
	bootstrapCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	if err := talos.Bootstrap(bootstrapCtx, serverIP, tlsCfg); err != nil {
		// Transient errors (connection refused, timeout, AlreadyExists) → requeue, don't error.
		// These happen when: node is mid-reboot, etcd is already bootstrapped, or API is briefly unavailable.
		if talos.IsTransientBootstrapError(err) {
			logger.Info("Bootstrap transient error, will retry", "ip", serverIP, "error", err.Error())
			return ctrl.Result{RequeueAfter: requeueAfterLong}, nil
		}
		return ctrl.Result{}, fmt.Errorf("Bootstrap on %s: %w", serverIP, err)
	}

	// Bootstrap triggered. etcd will self-start, K8s API will come up.
	logger.Info("Bootstrap triggered successfully, marking as Provisioned", "ip", serverIP)
	hrm.Status.ProvisioningState = infrav1.StateProvisioned
	hrm.Status.Ready = true
	return ctrl.Result{}, nil
}

func (r *HetznerRobotMachineReconciler) reconcileDelete(
	ctx context.Context,
	hrm *infrav1.HetznerRobotMachine,
	hrc *infrav1.HetznerRobotCluster,
	machine *clusterv1.Machine,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	hrm.Status.ProvisioningState = infrav1.StateDeleting

	// Resolve the claimed host to get serverID for hardware reset.
	// Best-effort: if host can't be resolved, log and proceed to remove finalizer.
	hrh, resolveErr := r.resolveHost(ctx, hrm)
	serverID := 0
	if resolveErr != nil {
		logger.Error(resolveErr, "Failed to resolve host during delete, will skip hardware reset")
	} else {
		serverID = hrh.Spec.ServerID
	}
	logger.Info("Deleting HetznerRobotMachine", "serverID", serverID, "nodeName", machine.Status.NodeRef)

	// CAPI Machine controller performs node drain BEFORE deleting the InfraMachine,
	// but only if the Machine has a NodeRef and the workload cluster API is reachable.
	// We verify drain has completed before proceeding to hardware reset.
	if machine.Status.NodeRef != nil {
		drainDone := false
		for _, cond := range machine.Status.Conditions {
			if cond.Type == clusterv1.DrainingSucceededCondition && cond.Status == "True" {
				drainDone = true
				break
			}
		}
		if !drainDone {
			// Check if drain timed out (CAPI sets reason=DrainError after timeout)
			for _, cond := range machine.Status.Conditions {
				if cond.Type == clusterv1.DrainingSucceededCondition && cond.Reason == "DrainError" {
					logger.Info("Node drain timed out, proceeding with hardware reset anyway", "nodeName", machine.Status.NodeRef.Name)
					drainDone = true
					break
				}
			}
		}
		if !drainDone {
			logger.Info("Waiting for CAPI to drain node before hardware reset", "nodeName", machine.Status.NodeRef.Name)
			return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
		}
		logger.Info("Node drain confirmed, proceeding with hardware reset", "nodeName", machine.Status.NodeRef.Name)
	}

	// Build Robot client
	robotClient, err := r.buildRobotClient(ctx, hrc)
	if err != nil {
		logger.Error(err, "Failed to build robot client during delete, removing finalizer anyway")
		controllerutil.RemoveFinalizer(hrm, infrav1.MachineFinalizer)
		return ctrl.Result{}, nil
	}

	// Activate rescue + hardware reset to wipe the node.
	// Skip if serverID could not be resolved (best-effort deletion).
	if serverID != 0 {
		sshFingerprint, _ := r.getSSHKeyFingerprint(ctx, hrc)
		if _, err := robotClient.ActivateRescue(ctx, serverID, sshFingerprint); err != nil {
			logger.Error(err, "Failed to activate rescue on delete, resetting anyway")
		}
		if err := robotClient.ResetServer(ctx, serverID, robot.ResetTypeHardware); err != nil {
			logger.Error(err, "Failed to reset server on delete, removing finalizer anyway")
		}
		logger.Info("Server reset triggered", "serverID", serverID)
	} else {
		logger.Info("Skipping hardware reset (serverID unknown)")
	}

	// Release the claimed host back to Available so it can be reused.
	if hrm.Status.HostRef != "" {
		if err := r.releaseHost(ctx, hrm.Namespace, hrm.Status.HostRef); err != nil {
			logger.Error(err, "Failed to release host, removing finalizer anyway", "host", hrm.Status.HostRef)
		} else {
			logger.Info("Released host back to Available", "host", hrm.Status.HostRef)
		}
	}

	controllerutil.RemoveFinalizer(hrm, infrav1.MachineFinalizer)
	return ctrl.Result{}, nil
}

// getBootstrapData retrieves the bootstrap data from the machine's bootstrap secret.
func (r *HetznerRobotMachineReconciler) getBootstrapData(ctx context.Context, machine *clusterv1.Machine) ([]byte, error) {
	if machine.Spec.Bootstrap.DataSecretName == nil {
		return nil, fmt.Errorf("bootstrap data secret not yet available on machine %s", machine.Name)
	}

	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: machine.Namespace,
		Name:      *machine.Spec.Bootstrap.DataSecretName,
	}, secret); err != nil {
		return nil, fmt.Errorf("get bootstrap secret %s: %w", *machine.Spec.Bootstrap.DataSecretName, err)
	}

	data, ok := secret.Data["value"]
	if !ok {
		return nil, fmt.Errorf("bootstrap secret %s has no 'value' key", *machine.Spec.Bootstrap.DataSecretName)
	}
	return data, nil
}

// getTalosSecretBundle reads and parses the Talos secret bundle from the cluster-level secret.
// Returns nil if TalosSecretRef is not configured.
func (r *HetznerRobotMachineReconciler) getTalosSecretBundle(ctx context.Context, hrc *infrav1.HetznerRobotCluster) (*talosSecretBundle, error) {
	if hrc.Spec.TalosSecretRef == nil {
		return nil, nil
	}

	secret := &corev1.Secret{}
	ns := hrc.Spec.TalosSecretRef.Namespace
	if ns == "" {
		ns = hrc.Namespace
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: hrc.Spec.TalosSecretRef.Name}, secret); err != nil {
		return nil, fmt.Errorf("get talos secret %s/%s: %w", ns, hrc.Spec.TalosSecretRef.Name, err)
	}

	bundle, ok := secret.Data["bundle"]
	if !ok {
		return nil, fmt.Errorf("talos secret %s has no 'bundle' key", hrc.Spec.TalosSecretRef.Name)
	}

	var bundleData talosSecretBundle
	if err := yaml.Unmarshal(bundle, &bundleData); err != nil {
		return nil, fmt.Errorf("parse talos secret bundle: %w", err)
	}

	return &bundleData, nil
}

// talosSecretBundle represents the relevant fields from the Talos secret bundle.
type talosSecretBundle struct {
	Secrets struct {
		SecretboxEncryptionSecret string `yaml:"secretboxencryptionsecret"`
		K8sServiceAccount        struct {
			Key string `yaml:"key"`
		} `yaml:"k8sserviceaccount"`
	} `yaml:"secrets"`
}

// buildRobotClient creates a Robot API client from the HRC's secret.
func (r *HetznerRobotMachineReconciler) buildRobotClient(ctx context.Context, hrc *infrav1.HetznerRobotCluster) (*robot.Client, error) {
	return robot.NewFromCluster(ctx, r.Client, hrc)
}

// getSSHPrivateKey retrieves the SSH private key from the HRC's SSH secret.
func (r *HetznerRobotMachineReconciler) getSSHPrivateKey(ctx context.Context, hrc *infrav1.HetznerRobotCluster) ([]byte, error) {
	secret := &corev1.Secret{}
	ns := hrc.Spec.SSHSecretRef.Namespace
	if ns == "" {
		ns = hrc.Namespace
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: hrc.Spec.SSHSecretRef.Name}, secret); err != nil {
		return nil, fmt.Errorf("get SSH secret %s/%s: %w", ns, hrc.Spec.SSHSecretRef.Name, err)
	}
	key, ok := secret.Data["ssh-privatekey"]
	if !ok {
		return nil, fmt.Errorf("SSH secret %s has no 'ssh-privatekey' key", hrc.Spec.SSHSecretRef.Name)
	}
	return key, nil
}

// getSSHKeyFingerprint retrieves the SSH public key fingerprint from the HRC's SSH secret.
// Returns empty string if not available (auth falls back to password from rescue activation).
func (r *HetznerRobotMachineReconciler) getSSHKeyFingerprint(ctx context.Context, hrc *infrav1.HetznerRobotCluster) (string, error) {
	secret := &corev1.Secret{}
	ns := hrc.Spec.SSHSecretRef.Namespace
	if ns == "" {
		ns = hrc.Namespace
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: hrc.Spec.SSHSecretRef.Name}, secret); err != nil {
		return "", fmt.Errorf("get SSH secret: %w", err)
	}
	return string(secret.Data["ssh-fingerprint"]), nil
}

// resolveHost finds (and claims if needed) the HetznerRobotHost for this machine.
// Uses hrm.Status.HostRef if already claimed; otherwise claims via Spec.HostRef or Spec.HostSelector.
// Sets hrm.Status.HostRef and HRH.Status.MachineRef + State=Claimed on first call.
func (r *HetznerRobotMachineReconciler) resolveHost(
	ctx context.Context,
	hrm *infrav1.HetznerRobotMachine,
) (*infrav1.HetznerRobotHost, error) {
	logger := log.FromContext(ctx)

	// Already claimed — just fetch it.
	if hrm.Status.HostRef != "" {
		hrh := &infrav1.HetznerRobotHost{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: hrm.Namespace, Name: hrm.Status.HostRef}, hrh); err != nil {
			return nil, fmt.Errorf("get claimed host %s: %w", hrm.Status.HostRef, err)
		}
		return hrh, nil
	}

	// Find the HRH to claim.
	var candidateName string
	if hrm.Spec.HostRef != nil && hrm.Spec.HostRef.Name != "" {
		// Direct reference — claim by name.
		candidateName = hrm.Spec.HostRef.Name
	} else if hrm.Spec.HostSelector != nil {
		// Label selector — find an Available HRH.
		selector, err := metav1.LabelSelectorAsSelector(hrm.Spec.HostSelector)
		if err != nil {
			return nil, fmt.Errorf("invalid hostSelector: %w", err)
		}
		list := &infrav1.HetznerRobotHostList{}
		if err := r.List(ctx, list, client.InNamespace(hrm.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
			return nil, fmt.Errorf("list hosts by selector: %w", err)
		}
		for _, h := range list.Items {
			if h.Status.State == infrav1.HostStateAvailable {
				candidateName = h.Name
				break
			}
		}
		if candidateName == "" {
			return nil, fmt.Errorf("no Available HetznerRobotHost found matching selector")
		}
	} else {
		return nil, fmt.Errorf("HetznerRobotMachine must specify either spec.hostRef or spec.hostSelector")
	}

	// Fetch + claim the candidate host.
	// Uses patch helper for optimistic concurrency — if another HRM claims the same host
	// between our Get and Patch, the patch will fail with a conflict error (409),
	// preventing double-claiming. The failed HRM retries on next reconcile.
	hrh := &infrav1.HetznerRobotHost{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: hrm.Namespace, Name: candidateName}, hrh); err != nil {
		return nil, fmt.Errorf("get candidate host %s: %w", candidateName, err)
	}
	if hrh.Status.State != infrav1.HostStateAvailable {
		return nil, fmt.Errorf("host %s is not Available (state=%s)", candidateName, hrh.Status.State)
	}

	// Claim: use patch helper for safe concurrent updates.
	hrhPatchHelper, err := patch.NewHelper(hrh, r.Client)
	if err != nil {
		return nil, fmt.Errorf("init HRH patch helper for claim: %w", err)
	}
	hrh.Status.State = infrav1.HostStateClaimed
	hrh.Status.MachineRef = &infrav1.MachineReference{
		Name:      hrm.Name,
		Namespace: hrm.Namespace,
	}
	if err := hrhPatchHelper.Patch(ctx, hrh); err != nil {
		return nil, fmt.Errorf("claim host %s (patch HRH): %w", candidateName, err)
	}

	// Record in HRM status.
	hrm.Status.HostRef = candidateName
	logger.Info("Claimed HetznerRobotHost", "host", candidateName, "serverID", hrh.Spec.ServerID)
	return hrh, nil
}

// releaseHost sets a HetznerRobotHost back to Available and clears its MachineRef.
// Used when a HetznerRobotMachine is deleted to return the host to the pool.
func (r *HetznerRobotMachineReconciler) releaseHost(ctx context.Context, namespace, hostName string) error {
	hrh := &infrav1.HetznerRobotHost{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: hostName}, hrh); err != nil {
		return fmt.Errorf("get host %s: %w", hostName, err)
	}
	hrhPatchHelper, err := patch.NewHelper(hrh, r.Client)
	if err != nil {
		return fmt.Errorf("init HRH patch helper: %w", err)
	}
	hrh.Status.State = infrav1.HostStateAvailable
	hrh.Status.MachineRef = nil
	if err := hrhPatchHelper.Patch(ctx, hrh); err != nil {
		return fmt.Errorf("patch host %s: %w", hostName, err)
	}
	return nil
}

// updateHostState sets a HetznerRobotHost to the given state using patch helper
// for safe concurrent updates. No-op if already in the target state.
func (r *HetznerRobotMachineReconciler) updateHostState(ctx context.Context, namespace, hostName string, state infrav1.HostState) error {
	hrh := &infrav1.HetznerRobotHost{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: hostName}, hrh); err != nil {
		return fmt.Errorf("get host %s: %w", hostName, err)
	}
	if hrh.Status.State == state {
		return nil // already in target state
	}
	hrhPatchHelper, err := patch.NewHelper(hrh, r.Client)
	if err != nil {
		return fmt.Errorf("init HRH patch helper: %w", err)
	}
	hrh.Status.State = state
	if err := hrhPatchHelper.Patch(ctx, hrh); err != nil {
		return fmt.Errorf("patch host %s state to %s: %w", hostName, state, err)
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *HetznerRobotMachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.HetznerRobotMachine{}).
		Complete(r)
}
