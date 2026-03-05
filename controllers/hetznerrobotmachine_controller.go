package controllers

import (
	"context"
	"fmt"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
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
		return ctrl.Result{}, nil
	}

	// Build Robot API client
	robotClient, err := r.buildRobotClient(ctx, hrc)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("build robot client: %w", err)
	}

	// Get server info
	serverInfo, err := robotClient.GetServer(hrm.Spec.ServerID)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get server %d: %w", hrm.Spec.ServerID, err)
	}

	serverIP := serverInfo.ServerIP
	logger.Info("Reconciling machine", "serverID", hrm.Spec.ServerID, "ip", serverIP, "state", hrm.Status.ProvisioningState)

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
		return r.stateActivateRescue(ctx, hrm, hrc, robotClient, serverIP)
	case infrav1.StateActivatingRescue:
		return r.stateCheckRescueActive(ctx, hrm, hrc, robotClient, serverIP)
	case infrav1.StateInRescue:
		return r.stateInstallTalos(ctx, hrm, hrc, serverIP)
	case infrav1.StateInstalling:
		return r.stateWaitInstall(ctx, hrm, serverIP)
	case infrav1.StateBootingTalos:
		return r.stateWaitTalosMaintenanceMode(ctx, hrm, machine, cluster, hrc, serverIP)
	case infrav1.StateApplyingConfig:
		return r.stateApplyConfig(ctx, hrm, machine, cluster, hrc, serverIP)
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
	serverIP string,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Activating rescue mode", "serverID", hrm.Spec.ServerID, "ip", serverIP)

	// Get SSH key fingerprint for rescue auth
	sshFingerprint, err := r.getSSHKeyFingerprint(ctx, hrc)
	if err != nil {
		logger.Error(err, "Failed to get SSH key fingerprint")
	}

	// Activate rescue mode
	_, err = robotClient.ActivateRescue(hrm.Spec.ServerID, sshFingerprint)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("activate rescue on server %d: %w", hrm.Spec.ServerID, err)
	}

	// Hardware reset to boot into rescue
	if err := robotClient.ResetServer(hrm.Spec.ServerID, robot.ResetTypeHardware); err != nil {
		return ctrl.Result{}, fmt.Errorf("reset server %d: %w", hrm.Spec.ServerID, err)
	}

	hrm.Status.ProvisioningState = infrav1.StateActivatingRescue
	logger.Info("Rescue activated, server resetting", "serverID", hrm.Spec.ServerID)
	return ctrl.Result{RequeueAfter: 90 * time.Second}, nil // Give it time to reset + boot rescue
}

// stateCheckRescueActive waits for SSH to be available in rescue mode.
func (r *HetznerRobotMachineReconciler) stateCheckRescueActive(
	ctx context.Context,
	hrm *infrav1.HetznerRobotMachine,
	_ *infrav1.HetznerRobotCluster,
	_ *robot.Client,
	serverIP string,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !sshrescue.IsReachable(serverIP) {
		logger.Info("SSH not yet reachable in rescue mode, waiting", "ip", serverIP)
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

	// Still waiting for reboot
	logger.Info("Waiting for Talos to boot (not yet in maintenance mode)", "ip", serverIP)
	hrm.Status.ProvisioningState = infrav1.StateBootingTalos
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
	// and endpoint is not yet set
	if util.IsControlPlaneMachine(machine) && hrc.Spec.ControlPlaneEndpoint.Host == "" {
		hrc.Spec.ControlPlaneEndpoint = clusterv1.APIEndpoint{
			Host: serverIP,
			Port: 6443,
		}
		if err := r.Update(ctx, hrc); err != nil {
			return ctrl.Result{}, fmt.Errorf("update control plane endpoint: %w", err)
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
	providerID := fmt.Sprintf("hetzner-robot://%d", hrm.Spec.ServerID)
	hrm.Spec.ProviderID = &providerID

	// For control plane nodes, we need to bootstrap etcd (talosctl bootstrap).
	// Workers join automatically via bootstrap token.
	isControlPlane := machine.Labels["cluster.x-k8s.io/control-plane"] == "true"
	if isControlPlane {
		logger.Info("Control plane node provisioned, moving to Bootstrapping state", "serverID", hrm.Spec.ServerID, "ip", serverIP)
		hrm.Status.ProvisioningState = infrav1.StateBootstrapping
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Worker: ready immediately after apply-config
	hrm.Status.ProvisioningState = infrav1.StateProvisioned
	hrm.Status.Ready = true
	logger.Info("Worker machine provisioned successfully", "serverID", hrm.Spec.ServerID, "ip", serverIP)
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

	// Wait for node to come back up after apply-config reboot
	if !talos.IsInMaintenanceMode(ctx, serverIP) && !talos.IsK8sAPIUp(ctx, serverIP) {
		logger.Info("Waiting for node to come back up after config apply", "ip", serverIP)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// If K8s API is already up, bootstrap already happened (joining CP)
	if talos.IsK8sAPIUp(ctx, serverIP) {
		logger.Info("K8s API already up, no bootstrap needed", "ip", serverIP)
		hrm.Status.ProvisioningState = infrav1.StateProvisioned
		hrm.Status.Ready = true
		logger.Info("Control plane provisioned successfully", "serverID", hrm.Spec.ServerID, "ip", serverIP)
		return ctrl.Result{}, nil
	}

	// Get cluster talosconfig secret (CAPT stores it as <cluster-name>-talosconfig)
	talosConfigSecret := &corev1.Secret{}
	talosConfigSecretName := fmt.Sprintf("%s-talosconfig", cluster.Name)
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: cluster.Namespace,
		Name:      talosConfigSecretName,
	}, talosConfigSecret); err != nil {
		return ctrl.Result{}, fmt.Errorf("get talosconfig secret %s: %w", talosConfigSecretName, err)
	}

	talosConfigData, ok := talosConfigSecret.Data["talosconfig"]
	if !ok {
		return ctrl.Result{}, fmt.Errorf("talosconfig secret %s has no 'talosconfig' key", talosConfigSecretName)
	}

	// Write talosconfig to temp file
	tmpFile, err := os.CreateTemp("", "talosconfig-*.yaml")
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("create temp talosconfig: %w", err)
	}
	defer os.Remove(tmpFile.Name()) //nolint:errcheck
	if _, err := tmpFile.Write(talosConfigData); err != nil {
		tmpFile.Close()
		return ctrl.Result{}, fmt.Errorf("write talosconfig: %w", err)
	}
	tmpFile.Close()

	logger.Info("Bootstrapping etcd on init control plane", "ip", serverIP)
	bootstrapCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	if err := talos.Bootstrap(bootstrapCtx, serverIP, tmpFile.Name()); err != nil {
		// If node is in maintenance mode, bootstrap might fail because it rebooted after apply-config
		// with the config and port 50000 is the maintenance port only before any config
		// In that case, the node IS configured, just waiting for bootstrap signal on port 50000
		return ctrl.Result{}, fmt.Errorf("bootstrap control plane %s: %w", serverIP, err)
	}

	logger.Info("Bootstrap triggered, waiting for K8s API to come up", "ip", serverIP)
	hrm.Status.ProvisioningState = infrav1.StateBootstrapping // stay here until K8s API up
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func (r *HetznerRobotMachineReconciler) reconcileDelete(
	ctx context.Context,
	hrm *infrav1.HetznerRobotMachine,
	hrc *infrav1.HetznerRobotCluster,
	machine *clusterv1.Machine,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Deleting HetznerRobotMachine", "serverID", hrm.Spec.ServerID, "nodeName", machine.Status.NodeRef)

	hrm.Status.ProvisioningState = infrav1.StateDeleting

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

	// Activate rescue + hardware reset to wipe the node
	sshFingerprint, _ := r.getSSHKeyFingerprint(ctx, hrc)
	_, err = robotClient.ActivateRescue(hrm.Spec.ServerID, sshFingerprint)
	if err != nil {
		logger.Error(err, "Failed to activate rescue on delete, resetting anyway")
	}

	if err := robotClient.ResetServer(hrm.Spec.ServerID, robot.ResetTypeHardware); err != nil {
		logger.Error(err, "Failed to reset server on delete, removing finalizer anyway")
	}

	logger.Info("Server reset triggered, removing finalizer", "serverID", hrm.Spec.ServerID)
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

// SetupWithManager sets up the controller with the Manager.
func (r *HetznerRobotMachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.HetznerRobotMachine{}).
		Complete(r)
}
