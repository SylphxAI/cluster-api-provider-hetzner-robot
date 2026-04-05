package controllers

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

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
)

const (
	requeueAfterShort  = 15 * time.Second
	requeueAfterMedium = 20 * time.Second
	requeueAfterLong   = 60 * time.Second

	// provisionTimeout is the maximum time allowed for a machine to go from
	// StateNone to StateProvisioned. If exceeded, the machine enters StateError
	// and CAPI marks it as Failed → MachineHealthCheck remediates.
	provisionTimeout = 30 * time.Minute

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

	// Provision timeout: if provisioning doesn't complete within 30 minutes,
	// enter terminal StateError. CAPI marks Machine as Failed → MHC remediates
	// (deletes + recreates) → rolling update is never blocked by a stuck machine.
	// Same pattern as cloud providers (CAPA, CAPZ, CAPG).
	if hrm.Status.ProvisioningState != infrav1.StateProvisioned &&
		hrm.Status.ProvisioningState != infrav1.StateError &&
		hrm.Status.ProvisioningState != infrav1.StateDeleting {
		if hrm.Status.ProvisionStarted == nil {
			now := metav1.Now()
			hrm.Status.ProvisionStarted = &now
		} else if time.Since(hrm.Status.ProvisionStarted.Time) > provisionTimeout {
			msg := fmt.Sprintf("Provision did not complete within %s (started %s, current state: %s)",
				provisionTimeout, hrm.Status.ProvisionStarted.Time.Format(time.RFC3339), hrm.Status.ProvisioningState)
			hrm.Status.FailureMessage = &msg
			reason := "ProvisionTimeout"
			hrm.Status.FailureReason = &reason
			hrm.Status.ProvisioningState = infrav1.StateError
			hrm.Status.Ready = false
			logger.Info("Provision timeout exceeded, entering terminal StateError",
				"timeout", provisionTimeout, "started", hrm.Status.ProvisionStarted.Time,
				"state", hrm.Status.ProvisioningState, "action", "manual_intervention_required")
			return ctrl.Result{}, nil
		}
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
// All other errors (SSH failures, API timeouts, connection refused, install
// failures) are treated as transient and will be retried with backoff
// until the global provisionTimeout is reached.
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

	// Set addresses — report all known IPs to CAPI consumers
	// (MachineHealthCheck, kubectl get machines, monitoring, etc.).
	hrm.Status.Addresses = []clusterv1.MachineAddress{
		{Type: clusterv1.MachineExternalIP, Address: serverIP},
	}
	if hrh.Spec.InternalIP != "" && hrc.Spec.VLANConfig != nil {
		// Only report InternalIP when VLAN is configured — the IP is only
		// reachable when the VLAN interface is actually injected into machineconfig.
		hrm.Status.Addresses = append(hrm.Status.Addresses,
			clusterv1.MachineAddress{Type: clusterv1.MachineInternalIP, Address: hrh.Spec.InternalIP})
	}
	if hrh.Spec.ServerIPv6Net != "" {
		// Derive the node's IPv6 address from the /64 subnet (same logic as injectIPv6Config).
		ipv6Prefix := strings.Split(hrh.Spec.ServerIPv6Net, "/")[0]
		ipv6Addr := strings.TrimSuffix(ipv6Prefix, "::") + "::1"
		hrm.Status.Addresses = append(hrm.Status.Addresses,
			clusterv1.MachineAddress{Type: clusterv1.MachineExternalIP, Address: ipv6Addr})
	}

	// Run state machine — OS-aware routing.
	// Common states (rescue) are shared between Talos and Flatcar.
	// Install/boot/config states branch based on hrm.Spec.OSType.
	switch hrm.Status.ProvisioningState {
	case infrav1.StateNone:
		// Clean slate: always go through the full rescue → wipe → install cycle.
		// No shortcuts — same contract as cloud VMs. Every provision starts fresh,
		// regardless of what's currently on disk (stale Talos, old OS, maintenance mode).
		// This eliminates stale state issues (hostname conflicts, partial configs, etc.).
		return r.stateActivateRescue(ctx, hrm, hrc, robotClient, serverID, serverIP)
	case infrav1.StateActivatingRescue:
		return r.stateCheckRescueActive(ctx, hrm, hrc, robotClient, serverID, serverIP)
	case infrav1.StateInRescue:
		// Branch: Talos vs Flatcar install in rescue.
		if isFlatcar(hrm) {
			return r.stateInstallFlatcar(ctx, hrm, machine, hrc, hrh, robotClient, serverID, serverIP)
		}
		return r.stateInstallTalos(ctx, hrm, hrc, robotClient, serverID, serverIP)
	case infrav1.StateInstalling:
		// Branch: wait for different OS boot indicators.
		if isFlatcar(hrm) {
			return r.stateWaitFlatcarInstall(ctx, hrm, hrc, robotClient, serverID, serverIP)
		}
		return r.stateWaitInstall(ctx, hrm, hrc, robotClient, serverID, serverIP)

	// Talos-only states:
	case infrav1.StateBootingTalos:
		return r.stateWaitTalosMaintenanceMode(ctx, hrm, machine, cluster, hrc, serverIP)
	case infrav1.StateApplyingConfig:
		return r.stateApplyConfig(ctx, hrm, machine, cluster, hrc, hrh, serverID, serverIP)
	case infrav1.StateWaitingForBoot:
		return r.stateWaitForBoot(ctx, hrm, machine, serverIP)

	// Flatcar-only state:
	case infrav1.StateBootingFlatcar:
		return r.stateWaitFlatcarBoot(ctx, hrm, machine, hrc, serverIP)

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

// SetupWithManager sets up the controller with the Manager.
func (r *HetznerRobotMachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.HetznerRobotMachine{}).
		Complete(r)
}
