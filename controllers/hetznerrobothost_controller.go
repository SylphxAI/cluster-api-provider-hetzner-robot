package controllers

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/cluster-api/util/patch"

	infrav1 "github.com/SylphxAI/cluster-api-provider-hetzner-robot/api/v1alpha1"
	"github.com/SylphxAI/cluster-api-provider-hetzner-robot/pkg/robot"
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
	}

	// Auto-detect serverIP and serverIPv6Net from Hetzner Robot API if not set.
	// Only requires serverID in spec — everything else is resolved automatically.
	if (host.Spec.ServerIP == "" || host.Spec.ServerIPv6Net == "") && host.Spec.ServerID > 0 {
		robotClient, err := r.getRobotClient(ctx, host)
		if err != nil {
			logger.Error(err, "Failed to create Robot client for auto-detect")
		} else {
			serverInfo, err := robotClient.GetServer(ctx, host.Spec.ServerID)
			if err != nil {
				logger.Error(err, "Failed to auto-detect server info from Hetzner API",
					"serverID", host.Spec.ServerID)
			} else {
				changed := false
				if host.Spec.ServerIP == "" && serverInfo.ServerIP != "" {
					host.Spec.ServerIP = serverInfo.ServerIP
					changed = true
				}
				if host.Spec.ServerIPv6Net == "" && serverInfo.ServerIPv6Net != "" {
					// Hetzner returns "2a01:4f8:2b04:201::" — normalize to CIDR /64
					ipv6 := strings.TrimSuffix(serverInfo.ServerIPv6Net, "::")
					host.Spec.ServerIPv6Net = fmt.Sprintf("%s::/64", ipv6)
					changed = true
				}
				if changed {
					logger.Info("Auto-detected server info from Hetzner API",
						"serverID", host.Spec.ServerID,
						"serverIP", host.Spec.ServerIP,
						"serverIPv6Net", host.Spec.ServerIPv6Net)
				}
			}
		}
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

// getRobotClient creates a Robot API client by finding the HetznerRobotCluster
// that owns this host (via cluster-name label) and reading its credentials secret.
func (r *HetznerRobotHostReconciler) getRobotClient(ctx context.Context, host *infrav1.HetznerRobotHost) (*robot.Client, error) {
	clusterName := host.Labels["cluster.x-k8s.io/cluster-name"]
	if clusterName == "" {
		return nil, fmt.Errorf("host %s has no cluster-name label", host.Name)
	}

	// Find the HetznerRobotCluster in the same namespace
	hrcList := &infrav1.HetznerRobotClusterList{}
	if err := r.List(ctx, hrcList, client.InNamespace(host.Namespace)); err != nil {
		return nil, fmt.Errorf("list HetznerRobotClusters: %w", err)
	}

	var hrc *infrav1.HetznerRobotCluster
	for i := range hrcList.Items {
		if hrcList.Items[i].Labels["cluster.x-k8s.io/cluster-name"] == clusterName {
			hrc = &hrcList.Items[i]
			break
		}
	}
	if hrc == nil {
		return nil, fmt.Errorf("no HetznerRobotCluster found for cluster %s", clusterName)
	}

	// Read Robot API credentials from the referenced secret
	secret := &corev1.Secret{}
	ns := hrc.Spec.RobotSecretRef.Namespace
	if ns == "" {
		ns = hrc.Namespace
	}
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: hrc.Spec.RobotSecretRef.Name}, secret); err != nil {
		return nil, fmt.Errorf("get robot credentials secret: %w", err)
	}

	return robot.New(
		string(secret.Data["robot-user"]),
		string(secret.Data["robot-password"]),
	), nil
}

func (r *HetznerRobotHostReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.HetznerRobotHost{}).
		Complete(r)
}
