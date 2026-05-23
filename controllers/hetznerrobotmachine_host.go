package controllers

import (
	"context"
	"fmt"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/cluster-api/util/patch"

	infrav1 "github.com/SylphxAI/cluster-api-provider-hetzner-robot/api/v1alpha1"
	"github.com/SylphxAI/cluster-api-provider-hetzner-robot/pkg/sshrescue"
)

// resolveHost finds (and claims if needed) the HetznerRobotHost for this machine.
// Uses hrm.Status.HostRef if already claimed; otherwise claims via Spec.HostRef or Spec.HostSelector.
// Sets hrm.Status.HostRef plus HRH.status.consumerRef and the legacy machineRef
// when the host is claimed for the first time.
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
		if err := r.ensureHostConsumerRef(ctx, hrh, hrm); err != nil {
			return nil, err
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
		if hostIsClaimedBy(hrh, hrm) {
			hrm.Status.HostRef = hrh.Name
			if err := r.ensureHostConsumerRef(ctx, hrh, hrm); err != nil {
				return nil, err
			}
			logger.Info("Recovered host claim after controller restart",
				"host", hrh.Name, "serverID", hrh.Spec.ServerID)
			return hrh, nil
		}
		if hrh.Spec.MaintenanceMode {
			return nil, fmt.Errorf("host %s is in maintenanceMode and cannot be claimed", hrh.Name)
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
			if hostIsClaimedBy(h, hrm) {
				hrm.Status.HostRef = h.Name
				if err := r.ensureHostConsumerRef(ctx, h, hrm); err != nil {
					return nil, err
				}
				logger.Info("Recovered host claim after controller restart",
					"host", h.Name, "serverID", h.Spec.ServerID)
				return h, nil
			}
		}
		// Second pass: find an Available host to claim.
		for _, h := range list.Items {
			if h.Status.State == infrav1.HostStateAvailable && !h.Spec.MaintenanceMode {
				candidateName = h.Name
				break
			}
		}
		if candidateName == "" {
			return nil, fmt.Errorf("no non-maintenance Available HetznerRobotHost found matching selector")
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
	if hrh.Spec.MaintenanceMode {
		return nil, fmt.Errorf("host %s is in maintenanceMode and cannot be claimed", candidateName)
	}

	// Claim: use patch helper for safe concurrent updates.
	hrhPatchHelper, err := patch.NewHelper(hrh, r.Client)
	if err != nil {
		return nil, fmt.Errorf("init HRH patch helper for claim: %w", err)
	}
	hrh.Status.State = infrav1.HostStateClaimed
	setHostConsumerRef(hrh, machineReferenceFor(hrm))
	hrh.Status.DirtyReason = ""
	if err := hrhPatchHelper.Patch(ctx, hrh); err != nil {
		return nil, fmt.Errorf("claim host %s (patch HRH): %w", candidateName, err)
	}

	// Record in HRM status.
	hrm.Status.HostRef = candidateName
	logger.Info("Claimed HetznerRobotHost", "host", candidateName, "serverID", hrh.Spec.ServerID)
	return hrh, nil
}

// resolveHostForDelete finds the physical host for deletion without claiming
// any new host. Delete reconciliation can trigger Robot rescue + hardware reset,
// so it must never call resolveHost() when status.hostRef is empty: selector
// based claiming in a delete path can reset an unrelated available bare-metal
// server.
func (r *HetznerRobotMachineReconciler) resolveHostForDelete(
	ctx context.Context,
	hrm *infrav1.HetznerRobotMachine,
) (*infrav1.HetznerRobotHost, bool, error) {
	if hrm.Status.HostRef != "" {
		hrh := &infrav1.HetznerRobotHost{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: hrm.Namespace, Name: hrm.Status.HostRef}, hrh); err != nil {
			return nil, false, fmt.Errorf("get claimed host %s: %w", hrm.Status.HostRef, err)
		}
		return hrh, true, nil
	}

	if hrm.Spec.HostRef != nil && hrm.Spec.HostRef.Name != "" {
		hrh := &infrav1.HetznerRobotHost{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: hrm.Namespace, Name: hrm.Spec.HostRef.Name}, hrh); err != nil {
			return nil, false, fmt.Errorf("get directly referenced host %s: %w", hrm.Spec.HostRef.Name, err)
		}
		if hostIsClaimedBy(hrh, hrm) {
			return hrh, true, nil
		}
		return nil, false, nil
	}

	if hrm.Spec.HostSelector != nil {
		selector, err := metav1.LabelSelectorAsSelector(hrm.Spec.HostSelector)
		if err != nil {
			return nil, false, fmt.Errorf("invalid hostSelector: %w", err)
		}
		list := &infrav1.HetznerRobotHostList{}
		if err := r.List(ctx, list, client.InNamespace(hrm.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
			return nil, false, fmt.Errorf("list hosts by selector: %w", err)
		}
		for i := range list.Items {
			h := &list.Items[i]
			if hostIsClaimedBy(h, hrm) {
				return h, true, nil
			}
		}
	}

	return nil, false, nil
}

func hostIsClaimedBy(host *infrav1.HetznerRobotHost, hrm *infrav1.HetznerRobotMachine) bool {
	return machineReferenceMatches(host.Status.ConsumerRef, hrm) ||
		machineReferenceMatches(host.Status.MachineRef, hrm)
}

// releaseHost returns a HetznerRobotHost to the claimable pool while recording
// why it must go through policy-authorized clean-slate provisioning next time.
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
	hrh.Status.LastConsumerRef = currentHostConsumerRef(hrh)
	clearHostConsumerRef(hrh)
	hrh.Status.DirtyReason = "ReleasedAfterMachineDelete"
	hrh.Status.ErrorMessage = ""
	if err := hrhPatchHelper.Patch(ctx, hrh); err != nil {
		return fmt.Errorf("patch host %s: %w", hostName, err)
	}
	return nil
}

func machineReferenceFor(hrm *infrav1.HetznerRobotMachine) *infrav1.MachineReference {
	return &infrav1.MachineReference{
		Name:      hrm.Name,
		Namespace: hrm.Namespace,
	}
}

func machineReferenceMatches(ref *infrav1.MachineReference, hrm *infrav1.HetznerRobotMachine) bool {
	return ref != nil && ref.Name == hrm.Name && ref.Namespace == hrm.Namespace
}

func currentHostConsumerRef(host *infrav1.HetznerRobotHost) *infrav1.MachineReference {
	if host.Status.ConsumerRef != nil {
		return host.Status.ConsumerRef.DeepCopy()
	}
	if host.Status.MachineRef != nil {
		return host.Status.MachineRef.DeepCopy()
	}
	return nil
}

func setHostConsumerRef(host *infrav1.HetznerRobotHost, ref *infrav1.MachineReference) {
	host.Status.ConsumerRef = ref.DeepCopy()
	host.Status.MachineRef = ref.DeepCopy()
}

func clearHostConsumerRef(host *infrav1.HetznerRobotHost) {
	host.Status.ConsumerRef = nil
	host.Status.MachineRef = nil
}

func (r *HetznerRobotMachineReconciler) ensureHostConsumerRef(
	ctx context.Context,
	host *infrav1.HetznerRobotHost,
	hrm *infrav1.HetznerRobotMachine,
) error {
	ref := currentHostConsumerRef(host)
	if ref != nil && !machineReferenceMatches(ref, hrm) {
		return fmt.Errorf("host %s is claimed by %s/%s, not %s/%s", host.Name, ref.Namespace, ref.Name, hrm.Namespace, hrm.Name)
	}
	if machineReferenceMatches(host.Status.ConsumerRef, hrm) && machineReferenceMatches(host.Status.MachineRef, hrm) {
		return nil
	}
	hrhPatchHelper, err := patch.NewHelper(host, r.Client)
	if err != nil {
		return fmt.Errorf("init HRH patch helper for consumerRef backfill: %w", err)
	}
	setHostConsumerRef(host, machineReferenceFor(hrm))
	if err := hrhPatchHelper.Patch(ctx, host); err != nil {
		return fmt.Errorf("backfill host %s consumerRef: %w", host.Name, err)
	}
	return nil
}

func (r *HetznerRobotMachineReconciler) recordHostHardwareDetails(
	ctx context.Context,
	namespace string,
	hostName string,
	hw *sshrescue.HardwareInfo,
) error {
	if hostName == "" || hw == nil {
		return nil
	}
	hrh := &infrav1.HetznerRobotHost{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: hostName}, hrh); err != nil {
		return fmt.Errorf("get host %s: %w", hostName, err)
	}
	hrhPatchHelper, err := patch.NewHelper(hrh, r.Client)
	if err != nil {
		return fmt.Errorf("init HRH patch helper: %w", err)
	}
	cephDisks := make([]string, 0, len(hw.CephDisks))
	for disk, hasCeph := range hw.CephDisks {
		if hasCeph {
			cephDisks = append(cephDisks, disk)
		}
	}
	sort.Strings(cephDisks)
	nvmeDisks := append([]string(nil), hw.NVMeDisks...)
	sort.Strings(nvmeDisks)
	byIDPaths := make(map[string]string, len(hw.ByIDPaths))
	for disk, byID := range hw.ByIDPaths {
		byIDPaths[disk] = byID
	}
	hrh.Status.HardwareDetails = &infrav1.HostHardwareDetails{
		PrimaryMAC: hw.PrimaryMAC,
		GatewayIP:  hw.GatewayIP,
		NVMeDisks:  nvmeDisks,
		CephDisks:  cephDisks,
		ByIDPaths:  byIDPaths,
	}
	if err := hrhPatchHelper.Patch(ctx, hrh); err != nil {
		return fmt.Errorf("patch host %s hardware details: %w", hostName, err)
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
