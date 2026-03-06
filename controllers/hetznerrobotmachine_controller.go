package controllers

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

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

	// Wrap reconcileNormal with retry counting → StateError on persistent failures
	result, err := r.reconcileNormal(ctx, hrm, hrc, machine, cluster)
	if err != nil {
		hrm.Status.RetryCount++
		msg := err.Error()
		hrm.Status.FailureMessage = &msg
		if hrm.Status.RetryCount >= infrav1.MaxProvisioningRetries {
			logger.Error(err, "Max retries exceeded, entering StateError",
				"retries", hrm.Status.RetryCount, "state", hrm.Status.ProvisioningState)
			reason := "MaxRetriesExceeded"
			hrm.Status.FailureReason = &reason
			hrm.Status.ProvisioningState = infrav1.StateError
			hrm.Status.Ready = false
			return ctrl.Result{}, nil // Stop retrying; human intervention needed
		}
		return ctrl.Result{RequeueAfter: requeueAfterShort}, nil
	}
	// Success: reset retry counter
	hrm.Status.RetryCount = 0
	hrm.Status.FailureMessage = nil
	return result, nil
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
		return r.stateInstallTalos(ctx, hrm, hrc, serverIP)
	case infrav1.StateInstalling:
		return r.stateWaitInstall(ctx, hrm, serverIP)
	case infrav1.StateBootingTalos:
		return r.stateWaitTalosMaintenanceMode(ctx, hrm, machine, cluster, hrc, serverIP)
	case infrav1.StateApplyingConfig:
		return r.stateApplyConfig(ctx, hrm, machine, cluster, hrc, serverID, serverIP)
	case infrav1.StateBootstrapping:
		return r.stateBootstrap(ctx, hrm, machine, cluster, serverIP)
	case infrav1.StateError:
		// Terminal state — human intervention required.
		// To retry, patch status.provisioningState back to "" (StateNone).
		logger.Info("Machine in StateError, halting reconciliation",
			"failureReason", hrm.Status.FailureReason,
			"failureMessage", hrm.Status.FailureMessage)
		return ctrl.Result{}, nil
	default:
		return ctrl.Result{}, nil
	}
}

// stateActivateRescue activates rescue mode via Robot API and triggers a hardware reset.
func (r *HetznerRobotMachineReconciler) stateActivateRescue(
	ctx context.Context,
	hrm *infrav1.HetznerRobotMachine,
	hrc *infrav1.HetznerRobotCluster,
	robotClient *robot.Client,
	serverID int,
	serverIP string,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Activating rescue mode", "serverID", serverID, "ip", serverIP)

	// Get SSH key fingerprint for rescue auth
	sshFingerprint, err := r.getSSHKeyFingerprint(ctx, hrc)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get SSH key fingerprint: %w", err)
	}

	// Activate rescue mode
	_, err = robotClient.ActivateRescue(ctx, serverID, sshFingerprint)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("activate rescue on server %d: %w", serverID, err)
	}

	// Hardware reset to boot into rescue
	if err := robotClient.ResetServer(ctx, serverID, robot.ResetTypeHardware); err != nil {
		return ctrl.Result{}, fmt.Errorf("reset server %d: %w", serverID, err)
	}

	hrm.Status.ProvisioningState = infrav1.StateActivatingRescue
	logger.Info("Rescue activated, server resetting", "serverID", serverID)
	return ctrl.Result{RequeueAfter: 90 * time.Second}, nil // Give it time to reset + boot rescue
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

	// Check Robot API: is rescue still armed?
	rescueStatus, err := robotClient.GetRescueStatus(ctx, serverID)
	if err != nil {
		logger.Error(err, "Failed to get rescue status, will retry", "serverID", serverID)
		return ctrl.Result{RequeueAfter: requeueAfterLong}, nil
	}

	if !rescueStatus.Active {
		// Rescue is no longer active — server must have rebooted back to its normal OS
		// (one-time boot consumed). Re-activate and reset again.
		logger.Info("Rescue no longer active (server rebooted out of rescue), re-activating", "serverID", serverID, "ip", serverIP)
		hrm.Status.RetryCount++
		return r.stateActivateRescue(ctx, hrm, hrc, robotClient, serverID, serverIP)
	}

	// Rescue is armed. Check if SSH is up yet.
	if !sshrescue.IsReachable(serverIP) {
		logger.Info("Rescue active, SSH not yet reachable, waiting", "ip", serverIP)
		return ctrl.Result{RequeueAfter: requeueAfterLong}, nil
	}

	logger.Info("Rescue SSH reachable", "ip", serverIP)
	hrm.Status.ProvisioningState = infrav1.StateInRescue
	return ctrl.Result{RequeueAfter: requeueAfterShort}, nil
}

// stateInstallTalos SSHes into rescue and installs Talos.
func (r *HetznerRobotMachineReconciler) stateInstallTalos(
	ctx context.Context,
	hrm *infrav1.HetznerRobotMachine,
	hrc *infrav1.HetznerRobotCluster,
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

	factoryURL := hrc.Spec.TalosFactoryBaseURL
	if factoryURL == "" {
		factoryURL = talosFactoryDefaultBaseURL
	}

	installDisk := hrm.Spec.InstallDisk
	if installDisk == "" {
		installDisk = "/dev/nvme0n1"
	}

	if err := sshClient.InstallTalos(
		factoryURL,
		hrm.Spec.TalosSchematic,
		hrm.Spec.TalosVersion,
		installDisk,
	); err != nil {
		return ctrl.Result{}, fmt.Errorf("install Talos on %s: %w", serverIP, err)
	}

	logger.Info("Talos image written, triggering reboot", "ip", serverIP)
	// Reboot into Talos
	sshClient.Run("reboot") //nolint:errcheck // reboot disconnects SSH, error expected

	hrm.Status.ProvisioningState = infrav1.StateInstalling
	return ctrl.Result{RequeueAfter: 3 * time.Minute}, nil
}

// stateWaitInstall transitions to BootingTalos after giving install time to complete.
func (r *HetznerRobotMachineReconciler) stateWaitInstall(
	ctx context.Context,
	hrm *infrav1.HetznerRobotMachine,
	serverIP string,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Check if Talos maintenance mode is already up
	if talos.IsInMaintenanceMode(ctx, serverIP) {
		logger.Info("Talos maintenance mode detected after install", "ip", serverIP)
		hrm.Status.ProvisioningState = infrav1.StateBootingTalos
		return ctrl.Result{RequeueAfter: requeueAfterShort}, nil
	}

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

// stateApplyConfig applies the Talos machineconfig from the bootstrap secret.
func (r *HetznerRobotMachineReconciler) stateApplyConfig(
	ctx context.Context,
	hrm *infrav1.HetznerRobotMachine,
	machine *clusterv1.Machine,
	cluster *clusterv1.Cluster,
	hrc *infrav1.HetznerRobotCluster,
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

	logger.Info("Applying Talos machineconfig", "ip", serverIP)

	applyCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	if err := talos.ApplyConfig(applyCtx, serverIP, bootstrapData); err != nil {
		return ctrl.Result{}, fmt.Errorf("apply Talos config to %s: %w", serverIP, err)
	}

	// Set the providerID
	providerID := fmt.Sprintf("hetzner-robot://%d", serverID)
	hrm.Spec.ProviderID = &providerID

	// For control plane nodes, we need to bootstrap etcd (talosctl bootstrap).
	// Workers join automatically via bootstrap token.
	// NOTE: CAPI sets the control-plane label with empty value "", not "true".
	// Always use util.IsControlPlaneMachine() which checks label presence, not value.
	if util.IsControlPlaneMachine(machine) {
		logger.Info("Control plane node provisioned, moving to Bootstrapping state", "serverID", serverID, "ip", serverIP)
		hrm.Status.ProvisioningState = infrav1.StateBootstrapping
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Worker: ready immediately after apply-config
	hrm.Status.ProvisioningState = infrav1.StateProvisioned
	hrm.Status.Ready = true
	logger.Info("Worker machine provisioned successfully", "serverID", serverID, "ip", serverIP)
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
		logger.Info("Control plane provisioned successfully", "ip", serverIP)
		return ctrl.Result{}, nil
	}

	// Build authenticated TLS config from the actual machineconfig applied to this node.
	// machine.ca in the machineconfig is the CA the Talos API server uses to sign its TLS cert.
	// This is NOT the same as the CABT bundle's certs.os — CABT generates machine.ca independently.
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
		return ctrl.Result{}, fmt.Errorf("Bootstrap on %s: %w", serverIP, err)
	}

	// Bootstrap triggered successfully. Mark as Provisioned — etcd will self-start
	// and K8s API will come up. The CAPI Machine controller checks InfraReady + BootstrapReady
	// independently; we don't need to wait for K8s API here.
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

// buildRobotClient creates a Robot API client from the HRC's secret.
func (r *HetznerRobotMachineReconciler) buildRobotClient(ctx context.Context, hrc *infrav1.HetznerRobotCluster) (*robot.Client, error) {
	secret := &corev1.Secret{}
	ns := hrc.Spec.RobotSecretRef.Namespace
	if ns == "" {
		ns = hrc.Namespace
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: hrc.Spec.RobotSecretRef.Name}, secret); err != nil {
		return nil, fmt.Errorf("get robot secret %s/%s: %w", ns, hrc.Spec.RobotSecretRef.Name, err)
	}
	return robot.New(string(secret.Data["robot-user"]), string(secret.Data["robot-password"])), nil
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
