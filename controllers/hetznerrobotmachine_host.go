package controllers

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/cluster-api/util/patch"

	infrav1 "github.com/SylphxAI/cluster-api-provider-hetzner-robot/api/v1alpha1"
)

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
		// But first check for claim recovery: if this host is already claimed by us
		// (pod restarted after HRH patch but before HRM status persist), re-adopt it.
		hrh := &infrav1.HetznerRobotHost{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: hrm.Namespace, Name: hrm.Spec.HostRef.Name}, hrh); err != nil {
			return nil, fmt.Errorf("get host %s: %w", hrm.Spec.HostRef.Name, err)
		}
		if hrh.Status.MachineRef != nil &&
			hrh.Status.MachineRef.Name == hrm.Name &&
			hrh.Status.MachineRef.Namespace == hrm.Namespace {
			hrm.Status.HostRef = hrh.Name
			logger.Info("Recovered host claim after controller restart",
				"host", hrh.Name, "serverID", hrh.Spec.ServerID)
			return hrh, nil
		}
		candidateName = hrm.Spec.HostRef.Name
	} else if hrm.Spec.HostSelector != nil {
		// Label selector — find an Available HRH.
		// Single List serves both claim recovery and new host selection.
		selector, err := metav1.LabelSelectorAsSelector(hrm.Spec.HostSelector)
		if err != nil {
			return nil, fmt.Errorf("invalid hostSelector: %w", err)
		}
		list := &infrav1.HetznerRobotHostList{}
		if err := r.List(ctx, list, client.InNamespace(hrm.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
			return nil, fmt.Errorf("list hosts by selector: %w", err)
		}
		// First pass: check for claim recovery (pod restarted after HRH patch
		// but before HRM status persist). Without this, the HRM finds no Available
		// host (original is stuck in Claimed) and loops forever.
		for i := range list.Items {
			h := &list.Items[i]
			if h.Status.MachineRef != nil &&
				h.Status.MachineRef.Name == hrm.Name &&
				h.Status.MachineRef.Namespace == hrm.Namespace {
				hrm.Status.HostRef = h.Name
				logger.Info("Recovered host claim after controller restart",
					"host", h.Name, "serverID", h.Spec.ServerID)
				return h, nil
			}
		}
		// Second pass: find an Available host to claim.
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

// releaseHost returns a HetznerRobotHost to the pool in a clean state.
// Clears all machine-specific state so the next claim gets a clean slate.
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
	hrh.Status.ErrorMessage = ""
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
