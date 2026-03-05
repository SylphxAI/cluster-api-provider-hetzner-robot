package controllers

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/patch"

	infrav1 "github.com/SylphxAI/cluster-api-provider-hetzner-robot/api/v1alpha1"
)

// HetznerRobotClusterReconciler reconciles a HetznerRobotCluster object.
type HetznerRobotClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hetznerrobotclusters,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hetznerrobotclusters/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hetznerrobotclusters/finalizers,verbs=update
//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters;clusters/status,verbs=get;list;watch

func (r *HetznerRobotClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the HetznerRobotCluster
	hrc := &infrav1.HetznerRobotCluster{}
	if err := r.Get(ctx, req.NamespacedName, hrc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Fetch the owning Cluster
	cluster, err := util.GetOwnerCluster(ctx, r.Client, hrc.ObjectMeta)
	if err != nil {
		return ctrl.Result{}, err
	}
	if cluster == nil {
		logger.Info("Cluster controller has not yet set OwnerRef on HetznerRobotCluster")
		return ctrl.Result{RequeueAfter: requeueAfterShort}, nil
	}

	if cluster.Spec.Paused {
		logger.Info("HetznerRobotCluster or Cluster is paused")
		return ctrl.Result{}, nil
	}

	// Set up patch helper
	patchHelper, err := patch.NewHelper(hrc, r.Client)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to init patch helper: %w", err)
	}

	// Always patch the object on exit
	defer func() {
		if err := patchHelper.Patch(ctx, hrc); err != nil {
			logger.Error(err, "Failed to patch HetznerRobotCluster")
		}
	}()

	// Handle deletion
	if !hrc.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, hrc, cluster)
	}

	// Add finalizer
	if !controllerutil.ContainsFinalizer(hrc, infrav1.ClusterFinalizer) {
		controllerutil.AddFinalizer(hrc, infrav1.ClusterFinalizer)
		return ctrl.Result{}, nil
	}

	return r.reconcileNormal(ctx, hrc, cluster)
}

func (r *HetznerRobotClusterReconciler) reconcileNormal(
	ctx context.Context,
	hrc *infrav1.HetznerRobotCluster,
	cluster *clusterv1.Cluster,
) (ctrl.Result, error) {
	// The cluster is ready when the control plane endpoint is set.
	// The control plane endpoint is set either:
	// 1. By the user (spec.controlPlaneEndpoint), or
	// 2. Automatically from the first control plane machine's IP
	if hrc.Spec.ControlPlaneEndpoint.Host == "" {
		// Wait for first control plane machine to set the endpoint
		log.FromContext(ctx).Info("Control plane endpoint not yet set, waiting for control plane machines")
		return ctrl.Result{RequeueAfter: requeueAfterShort}, nil
	}

	hrc.Status.Ready = true
	return ctrl.Result{}, nil
}

func (r *HetznerRobotClusterReconciler) reconcileDelete(
	ctx context.Context,
	hrc *infrav1.HetznerRobotCluster,
	_ *clusterv1.Cluster,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Check if there are still machines
	machines := &infrav1.HetznerRobotMachineList{}
	if err := r.List(ctx, machines, client.InNamespace(hrc.Namespace)); err != nil {
		return ctrl.Result{}, err
	}

	clusterMachines := filterMachinesByCluster(machines.Items, hrc.Name)
	if len(clusterMachines) > 0 {
		logger.Info("Waiting for HetznerRobotMachines to be deleted", "count", len(clusterMachines))
		return ctrl.Result{RequeueAfter: requeueAfterShort}, nil
	}

	controllerutil.RemoveFinalizer(hrc, infrav1.ClusterFinalizer)
	return ctrl.Result{}, nil
}

func filterMachinesByCluster(machines []infrav1.HetznerRobotMachine, clusterName string) []infrav1.HetznerRobotMachine {
	var result []infrav1.HetznerRobotMachine
	for _, m := range machines {
		if m.Labels[clusterv1.ClusterNameLabel] == clusterName {
			result = append(result, m)
		}
	}
	return result
}

// SetupWithManager sets up the controller with the Manager.
func (r *HetznerRobotClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.HetznerRobotCluster{}).
		Complete(r)
}
