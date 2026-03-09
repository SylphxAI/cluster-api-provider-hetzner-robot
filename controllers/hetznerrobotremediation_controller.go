package controllers

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
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

	switch remediation.Status.Phase {
	case "": // Initial — no phase set yet
		return r.reconcileInitial(ctx, remediation, serverID, hrc, logger)

	case infrav1.RemediationPhaseRunning:
		return r.reconcileRunning(ctx, remediation, logger)

	case infrav1.RemediationPhaseWaiting:
		return r.reconcileWaiting(ctx, remediation, serverID, hrc, logger)

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
	logger interface{ Info(string, ...any) },
) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	robotClient, err := r.buildRobotClient(ctx, hrc)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("build robot client: %w", err)
	}

	log.Info("Initiating hardware reset for unhealthy machine", "serverID", serverID)
	if err := robotClient.ResetServer(ctx, serverID, robot.ResetTypeHardware); err != nil {
		return ctrl.Result{}, fmt.Errorf("hardware reset server %d: %w", serverID, err)
	}

	now := metav1.Now()
	remediation.Status.Phase = infrav1.RemediationPhaseRunning
	remediation.Status.LastRemediated = &now
	remediation.Status.RetryCount = 1

	timeout := remediation.Spec.Strategy.Timeout.Duration
	logger.Info("Hardware reset issued, waiting for recovery",
		"serverID", serverID,
		"retryCount", remediation.Status.RetryCount,
		"timeout", timeout,
	)

	return ctrl.Result{RequeueAfter: timeout}, nil
}

// reconcileRunning transitions from Running to Waiting after the timeout period has elapsed.
// The requeue delay from the previous phase acts as the timer.
func (r *HetznerRobotRemediationReconciler) reconcileRunning(
	_ context.Context,
	remediation *infrav1.HetznerRobotRemediation,
	logger interface{ Info(string, ...any) },
) (ctrl.Result, error) {
	remediation.Status.Phase = infrav1.RemediationPhaseWaiting

	timeout := remediation.Spec.Strategy.Timeout.Duration
	logger.Info("Recovery timeout elapsed, transitioning to Waiting",
		"retryCount", remediation.Status.RetryCount,
		"retryLimit", remediation.Spec.Strategy.RetryLimit,
		"timeout", timeout,
	)

	return ctrl.Result{RequeueAfter: timeout}, nil
}

// reconcileWaiting checks whether the retry limit is exhausted.
// If retries remain, it triggers another hardware reset cycle (back to Running).
// If exhausted, it transitions to Deleting (terminal) so CAPI replaces the Machine.
func (r *HetznerRobotRemediationReconciler) reconcileWaiting(
	ctx context.Context,
	remediation *infrav1.HetznerRobotRemediation,
	serverID int,
	hrc *infrav1.HetznerRobotCluster,
	logger interface{ Info(string, ...any) },
) (ctrl.Result, error) {
	if remediation.Status.RetryCount >= remediation.Spec.Strategy.RetryLimit {
		// Retry limit exhausted — enter terminal phase.
		// CAPI will see that remediation is complete (Deleting) and delete the Machine,
		// triggering a replacement through the MachineDeployment/MachineSet.
		remediation.Status.Phase = infrav1.RemediationPhaseDeleting
		logger.Info("Retry limit exhausted, transitioning to Deleting phase — CAPI will replace the Machine",
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

	log := log.FromContext(ctx)
	log.Info("Issuing retry hardware reset", "serverID", serverID, "retryCount", remediation.Status.RetryCount+1)
	if err := robotClient.ResetServer(ctx, serverID, robot.ResetTypeHardware); err != nil {
		return ctrl.Result{}, fmt.Errorf("hardware reset server %d: %w", serverID, err)
	}

	now := metav1.Now()
	remediation.Status.Phase = infrav1.RemediationPhaseRunning
	remediation.Status.LastRemediated = &now
	remediation.Status.RetryCount++

	timeout := remediation.Spec.Strategy.Timeout.Duration
	logger.Info("Retry hardware reset issued",
		"serverID", serverID,
		"retryCount", remediation.Status.RetryCount,
		"retryLimit", remediation.Spec.Strategy.RetryLimit,
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

// SetupWithManager sets up the controller with the Manager.
func (r *HetznerRobotRemediationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.HetznerRobotRemediation{}).
		Complete(r)
}
