package controllers

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/cluster-api/util/patch"

	infrav1 "github.com/SylphxAI/cluster-api-provider-hetzner-robot/api/v1alpha1"
)

// HetznerRobotHostReconciler reconciles HetznerRobotHost objects.
// The HRH represents a physical server in the Robot pool. It is permanent —
// it is never deleted automatically, only by an operator when decommissioning a server.
//
// The reconciler's job is minimal:
//   - Add a finalizer to prevent accidental deletion while claimed
//   - On delete: block until no Machine has claimed this host (MachineRef == nil)
//   - Initialise Status.State = Available for newly created hosts
type HetznerRobotHostReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hetznerrobothosts,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hetznerrobothosts/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hetznerrobothosts/finalizers,verbs=update

func (r *HetznerRobotHostReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	host := &infrav1.HetznerRobotHost{}
	if err := r.Get(ctx, req.NamespacedName, host); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Use patch helper for safe concurrent updates (consistent with other controllers).
	patchHelper, err := patch.NewHelper(host, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}
	defer func() {
		if pErr := patchHelper.Patch(ctx, host); pErr != nil {
			logger.Error(pErr, "Failed to patch HetznerRobotHost")
		}
	}()

	// Add finalizer to prevent deletion while claimed
	if !controllerutil.ContainsFinalizer(host, infrav1.HostFinalizer) {
		controllerutil.AddFinalizer(host, infrav1.HostFinalizer)
		return ctrl.Result{}, nil
	}

	// Initialise State to Available if not set
	if host.Status.State == "" {
		host.Status.State = infrav1.HostStateAvailable
		return ctrl.Result{}, nil
	}

	// Handle deletion
	if !host.DeletionTimestamp.IsZero() {
		// Block deletion if a Machine currently claims this host
		if host.Status.MachineRef != nil {
			logger.Info("Host is still claimed, blocking deletion",
				"machineRef", host.Status.MachineRef.Name)
			// Requeue — the HRM controller will release the host when the Machine is deleted
			return ctrl.Result{RequeueAfter: requeueAfterShort}, nil
		}
		// Safe to delete
		controllerutil.RemoveFinalizer(host, infrav1.HostFinalizer)
		logger.Info("HetznerRobotHost finalizer removed, deletion proceeding",
			"serverID", host.Spec.ServerID)
		return ctrl.Result{}, nil
	}

	return ctrl.Result{}, nil
}

func (r *HetznerRobotHostReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.HetznerRobotHost{}).
		Complete(r)
}
