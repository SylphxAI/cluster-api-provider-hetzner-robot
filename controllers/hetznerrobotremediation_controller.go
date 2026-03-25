package controllers

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrav1 "github.com/SylphxAI/cluster-api-provider-hetzner-robot/api/v1alpha1"
	"github.com/SylphxAI/cluster-api-provider-hetzner-robot/pkg/robot"
)

// HetznerRobotRemediationReconciler reconciles HetznerRobotRemediation objects
// created by CAPI's MachineHealthCheck when a Machine is detected as unhealthy.
// It performs hardware resets via the Hetzner Robot API to recover unresponsive nodes.
type HetznerRobotRemediationReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hetznerrobotremediations,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hetznerrobotremediations/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hetznerrobotremediations/finalizers,verbs=update
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hetznerrobotmachines,verbs=get;list;watch
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hetznerrobothosts,verbs=get;list;watch
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hetznerrobotclusters,verbs=get;list;watch
//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines;machines/status,verbs=get;list;watch
//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *HetznerRobotRemediationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the HetznerRobotRemediation
	remediation := &infrav1.HetznerRobotRemediation{}
	if err := r.Get(ctx, req.NamespacedName, remediation); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	logger = logger.WithValues(
		"remediation", remediation.Name,
		"namespace", remediation.Namespace,
		"phase", remediation.Status.Phase,
	)

	// Fetch the owning CAPI Machine (MachineHealthCheck sets ownerRef → Machine)
	machine, err := util.GetOwnerMachine(ctx, r.Client, remediation.ObjectMeta)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get owner Machine: %w", err)
	}
	if machine == nil {
		logger.Info("Waiting for Machine controller to set OwnerRef on HetznerRobotRemediation")
		return ctrl.Result{RequeueAfter: requeueAfterShort}, nil
	}

	logger = logger.WithValues("machine", machine.Name)

	// Set up patch helper for atomic status updates
	patchHelper, err := patch.NewHelper(remediation, r.Client)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to init patch helper: %w", err)
	}
	defer func() {
		if pErr := patchHelper.Patch(ctx, remediation); pErr != nil {
			logger.Error(pErr, "Failed to patch HetznerRobotRemediation")
		}
	}()

	// Terminal phase — nothing to do
	if remediation.Status.Phase == infrav1.RemediationPhaseDeleting {
		logger.Info("Remediation in terminal Deleting phase, retry limit exhausted")
		return ctrl.Result{}, nil
	}

	// Resolve the full object chain to get serverID and robot credentials:
	// Machine → InfrastructureRef → HetznerRobotMachine → HostRef → HetznerRobotHost → ServerID
	// Machine → Cluster → InfrastructureRef → HetznerRobotCluster → RobotSecretRef
	serverID, hrc, err := r.resolveServerAndCluster(ctx, machine)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolve server and cluster: %w", err)
	}

	logger = logger.WithValues("serverID", serverID)
	// Inject enriched logger into context so phase methods get full structured context.
	ctx = log.IntoContext(ctx, logger)

	switch remediation.Status.Phase {
	case "": // Initial — no phase set yet
		return r.reconcileInitial(ctx, remediation, serverID, hrc)

	case infrav1.RemediationPhaseRunning:
		return r.reconcileRunning(ctx, remediation)

	case infrav1.RemediationPhaseWaiting:
		return r.reconcileWaiting(ctx, remediation, serverID, hrc)

	default:
		logger.Info("Unknown remediation phase, ignoring", "phase", remediation.Status.Phase)
		return ctrl.Result{}, nil
	}
}

// reconcileInitial handles the first reconcile: triggers a hardware reset and transitions to Running.
func (r *HetznerRobotRemediationReconciler) reconcileInitial(
	ctx context.Context,
	remediation *infrav1.HetznerRobotRemediation,
	serverID int,
	hrc *infrav1.HetznerRobotCluster,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	robotClient, err := r.buildRobotClient(ctx, hrc)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("build robot client: %w", err)
	}

	logger.Info("Initiating hardware reset for unhealthy machine", "serverID", serverID)
	if err := robotClient.ResetServer(ctx, serverID, robot.ResetTypeHardware); err != nil {
		return ctrl.Result{}, fmt.Errorf("hardware reset server %d: %w", serverID, err)
	}

	now := metav1.Now()
	remediation.Status.Phase = infrav1.RemediationPhaseRunning
	remediation.Status.LastRemediated = &now
	remediation.Status.RetryCount = 1

	timeout := r.getTimeout(remediation)
	logger.Info("Hardware reset issued, waiting for recovery",
		"retryCount", remediation.Status.RetryCount,
		"timeout", timeout,
	)

	return ctrl.Result{RequeueAfter: timeout}, nil
}

// reconcileRunning transitions from Running to Waiting after the timeout period has elapsed.
// The requeue delay from reconcileInitial/reconcileWaiting acts as the timer — by the time
// this fires, the node has already had the full timeout to recover.
func (r *HetznerRobotRemediationReconciler) reconcileRunning(
	ctx context.Context,
	remediation *infrav1.HetznerRobotRemediation,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	remediation.Status.Phase = infrav1.RemediationPhaseWaiting

	logger.Info("Recovery timeout elapsed, checking retry status",
		"retryCount", remediation.Status.RetryCount,
		"retryLimit", remediation.Spec.Strategy.RetryLimit,
	)

	// Requeue immediately — the timeout already elapsed during Running phase.
	// reconcileWaiting will decide: retry (another reset) or give up (Deleting).
	return ctrl.Result{Requeue: true}, nil
}

// reconcileWaiting checks whether the retry limit is exhausted.
// If retries remain, it triggers another hardware reset cycle (back to Running).
// If exhausted, it transitions to Deleting (terminal) so CAPI replaces the Machine.
func (r *HetznerRobotRemediationReconciler) reconcileWaiting(
	ctx context.Context,
	remediation *infrav1.HetznerRobotRemediation,
	serverID int,
	hrc *infrav1.HetznerRobotCluster,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if remediation.Status.RetryCount >= remediation.Spec.Strategy.RetryLimit {
		// Retry limit exhausted — enter terminal phase.
		// The node is still unhealthy after all retry attempts. MHC sees the
		// remediation CR still exists and node is unhealthy — it will handle
		// Machine deletion based on its own maxUnhealthy policy.
		remediation.Status.Phase = infrav1.RemediationPhaseDeleting
		logger.Info("Retry limit exhausted, entering terminal Deleting phase",
			"retryCount", remediation.Status.RetryCount,
			"retryLimit", remediation.Spec.Strategy.RetryLimit,
		)
		return ctrl.Result{}, nil
	}

	// Retries remain — issue another hardware reset
	robotClient, err := r.buildRobotClient(ctx, hrc)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("build robot client: %w", err)
	}

	logger.Info("Issuing retry hardware reset",
		"serverID", serverID,
		"retryCount", remediation.Status.RetryCount+1,
		"retryLimit", remediation.Spec.Strategy.RetryLimit,
	)
	if err := robotClient.ResetServer(ctx, serverID, robot.ResetTypeHardware); err != nil {
		return ctrl.Result{}, fmt.Errorf("hardware reset server %d: %w", serverID, err)
	}

	now := metav1.Now()
	remediation.Status.Phase = infrav1.RemediationPhaseRunning
	remediation.Status.LastRemediated = &now
	remediation.Status.RetryCount++

	timeout := r.getTimeout(remediation)
	logger.Info("Retry hardware reset issued, waiting for recovery",
		"retryCount", remediation.Status.RetryCount,
		"timeout", timeout,
	)

	return ctrl.Result{RequeueAfter: timeout}, nil
}

// resolveServerAndCluster traverses the 4-hop object chain from Machine to ServerID and HetznerRobotCluster:
//
//	Machine → Spec.InfrastructureRef → HetznerRobotMachine
//	HetznerRobotMachine → Status.HostRef → HetznerRobotHost → Spec.ServerID
//	Machine → Cluster → Spec.InfrastructureRef → HetznerRobotCluster
func (r *HetznerRobotRemediationReconciler) resolveServerAndCluster(
	ctx context.Context,
	machine *clusterv1.Machine,
) (int, *infrav1.HetznerRobotCluster, error) {
	// Step 1: Machine → HetznerRobotMachine via InfrastructureRef
	if machine.Spec.InfrastructureRef.Name == "" {
		return 0, nil, fmt.Errorf("Machine %s/%s has no InfrastructureRef", machine.Namespace, machine.Name)
	}
	hrm := &infrav1.HetznerRobotMachine{}
	hrmKey := types.NamespacedName{
		Namespace: machine.Namespace,
		Name:      machine.Spec.InfrastructureRef.Name,
	}
	if err := r.Get(ctx, hrmKey, hrm); err != nil {
		if apierrors.IsNotFound(err) {
			return 0, nil, fmt.Errorf("HetznerRobotMachine %s not found: %w", hrmKey, err)
		}
		return 0, nil, fmt.Errorf("get HetznerRobotMachine %s: %w", hrmKey, err)
	}

	// Step 2: HetznerRobotMachine → HetznerRobotHost via Status.HostRef
	if hrm.Status.HostRef == "" {
		return 0, nil, fmt.Errorf("HetznerRobotMachine %s has no HostRef — machine may not be provisioned yet", hrmKey)
	}
	host := &infrav1.HetznerRobotHost{}
	hostKey := types.NamespacedName{
		Namespace: machine.Namespace,
		Name:      hrm.Status.HostRef,
	}
	if err := r.Get(ctx, hostKey, host); err != nil {
		if apierrors.IsNotFound(err) {
			return 0, nil, fmt.Errorf("HetznerRobotHost %s not found: %w", hostKey, err)
		}
		return 0, nil, fmt.Errorf("get HetznerRobotHost %s: %w", hostKey, err)
	}

	// Step 3: Machine → Cluster → HetznerRobotCluster
	cluster, err := util.GetClusterFromMetadata(ctx, r.Client, machine.ObjectMeta)
	if err != nil {
		return 0, nil, fmt.Errorf("get Cluster from Machine metadata: %w", err)
	}
	if cluster == nil {
		return 0, nil, fmt.Errorf("Cluster not found for Machine %s/%s", machine.Namespace, machine.Name)
	}
	if cluster.Spec.InfrastructureRef == nil {
		return 0, nil, fmt.Errorf("Cluster %s/%s has no InfrastructureRef", cluster.Namespace, cluster.Name)
	}

	hrc := &infrav1.HetznerRobotCluster{}
	hrcKey := types.NamespacedName{
		Namespace: cluster.Spec.InfrastructureRef.Namespace,
		Name:      cluster.Spec.InfrastructureRef.Name,
	}
	if err := r.Get(ctx, hrcKey, hrc); err != nil {
		if apierrors.IsNotFound(err) {
			return 0, nil, fmt.Errorf("HetznerRobotCluster %s not found: %w", hrcKey, err)
		}
		return 0, nil, fmt.Errorf("get HetznerRobotCluster %s: %w", hrcKey, err)
	}

	return host.Spec.ServerID, hrc, nil
}

// buildRobotClient creates a Robot API client from the HetznerRobotCluster's secret reference.
func (r *HetznerRobotRemediationReconciler) buildRobotClient(ctx context.Context, hrc *infrav1.HetznerRobotCluster) (*robot.Client, error) {
	return robot.NewFromCluster(ctx, r.Client, hrc)
}

// getTimeout returns the configured strategy timeout, defaulting to 5 minutes if unset.
func (r *HetznerRobotRemediationReconciler) getTimeout(remediation *infrav1.HetznerRobotRemediation) time.Duration {
	if d := remediation.Spec.Strategy.Timeout.Duration; d > 0 {
		return d
	}
	return 5 * time.Minute
}

// SetupWithManager sets up the controller with the Manager.
func (r *HetznerRobotRemediationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.HetznerRobotRemediation{}).
		Complete(r)
}
