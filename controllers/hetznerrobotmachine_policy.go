package controllers

import (
	"context"
	"fmt"
	"time"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/SylphxAI/cluster-api-provider-hetzner-robot/api/v1alpha1"
)

const destructiveProvisioningDeniedReason = "DestructiveProvisioningDenied"

func authorizeDestructiveProvisioning(host *infrav1.HetznerRobotHost) error {
	if host == nil {
		return fmt.Errorf("destructive provisioning denied: host is nil")
	}
	if host.Spec.MaintenanceMode {
		return fmt.Errorf("destructive provisioning denied for host %s: maintenanceMode=true", host.Name)
	}

	switch host.Spec.LifecycleClass {
	case infrav1.HostLifecycleClassCompute:
		if host.Spec.DestructiveProvisioningPolicy == infrav1.DestructiveProvisioningPolicyAlwaysCleanSlate {
			return nil
		}
		return fmt.Errorf(
			"destructive provisioning denied for compute host %s: policy=%q, want %q",
			host.Name,
			host.Spec.DestructiveProvisioningPolicy,
			infrav1.DestructiveProvisioningPolicyAlwaysCleanSlate,
		)
	case infrav1.HostLifecycleClassControlPlane:
		return fmt.Errorf(
			"destructive provisioning denied for control-plane host %s: policy=%q",
			host.Name,
			host.Spec.DestructiveProvisioningPolicy,
		)
	case infrav1.HostLifecycleClassStorage:
		return fmt.Errorf(
			"destructive provisioning denied for storage host %s: active storage host release is required",
			host.Name,
		)
	case "":
		return fmt.Errorf("destructive provisioning denied for host %s: lifecycleClass is required", host.Name)
	default:
		return fmt.Errorf(
			"destructive provisioning denied for host %s: unsupported lifecycleClass=%q",
			host.Name,
			host.Spec.LifecycleClass,
		)
	}
}

func (r *HetznerRobotMachineReconciler) authorizeDestructiveProvisioning(
	ctx context.Context,
	hrm *infrav1.HetznerRobotMachine,
	machine *clusterv1.Machine,
	host *infrav1.HetznerRobotHost,
) error {
	if host == nil {
		return fmt.Errorf("destructive provisioning denied: host is nil")
	}
	if hrm == nil {
		return fmt.Errorf("destructive provisioning denied for host %s: HetznerRobotMachine is nil", host.Name)
	}
	if host.Spec.LifecycleClass != infrav1.HostLifecycleClassStorage {
		return authorizeDestructiveProvisioning(host)
	}
	if host.Spec.MaintenanceMode {
		return fmt.Errorf("destructive provisioning denied for host %s: maintenanceMode=true", host.Name)
	}
	if host.Spec.DestructiveProvisioningPolicy != infrav1.DestructiveProvisioningPolicyRequiresExternalRelease {
		return fmt.Errorf(
			"destructive provisioning denied for storage host %s: policy=%q, want %q",
			host.Name,
			host.Spec.DestructiveProvisioningPolicy,
			infrav1.DestructiveProvisioningPolicyRequiresExternalRelease,
		)
	}
	release, err := r.findActiveHostRelease(ctx, host, machine)
	if err != nil {
		return err
	}
	if release == nil {
		return fmt.Errorf(
			"destructive provisioning denied for storage host %s: active HetznerRobotHostRelease for Machine %s/%s uid=%s is required",
			host.Name,
			machine.Namespace,
			machine.Name,
			machine.UID,
		)
	}
	return nil
}

func (r *HetznerRobotMachineReconciler) findActiveHostRelease(
	ctx context.Context,
	host *infrav1.HetznerRobotHost,
	machine *clusterv1.Machine,
) (*infrav1.HetznerRobotHostRelease, error) {
	if machine == nil {
		return nil, fmt.Errorf("destructive provisioning denied for host %s: CAPI Machine is nil", host.Name)
	}
	releases := &infrav1.HetznerRobotHostReleaseList{}
	if err := r.List(ctx, releases, client.InNamespace(host.Namespace)); err != nil {
		return nil, fmt.Errorf("list HetznerRobotHostReleases for host %s: %w", host.Name, err)
	}
	now := time.Now()
	for i := range releases.Items {
		release := &releases.Items[i]
		if release.Spec.HostRef.Name != host.Name {
			continue
		}
		if release.Spec.ApprovedAction != infrav1.HostReleaseActionWipeAndReinstall {
			continue
		}
		if !release.Spec.ExpiresAt.After(now) {
			continue
		}
		if release.Spec.MachineRef.Name != machine.Name ||
			release.Spec.MachineRef.Namespace != machine.Namespace ||
			release.Spec.MachineRef.UID != string(machine.UID) {
			continue
		}
		return release, nil
	}
	return nil, nil
}

func authorizeAutomatedHardwareReset(host *infrav1.HetznerRobotHost) error {
	if host == nil {
		return fmt.Errorf("automated hardware reset denied: host is nil")
	}
	if host.Spec.MaintenanceMode {
		return fmt.Errorf("automated hardware reset denied for host %s: maintenanceMode=true", host.Name)
	}

	switch host.Spec.LifecycleClass {
	case infrav1.HostLifecycleClassCompute:
		if host.Spec.DestructiveProvisioningPolicy == infrav1.DestructiveProvisioningPolicyAlwaysCleanSlate {
			return nil
		}
		return fmt.Errorf(
			"automated hardware reset denied for compute host %s: policy=%q, want %q",
			host.Name,
			host.Spec.DestructiveProvisioningPolicy,
			infrav1.DestructiveProvisioningPolicyAlwaysCleanSlate,
		)
	case infrav1.HostLifecycleClassControlPlane:
		return fmt.Errorf(
			"automated hardware reset denied for control-plane host %s: platform quorum gate required",
			host.Name,
		)
	case infrav1.HostLifecycleClassStorage:
		return fmt.Errorf(
			"automated hardware reset denied for storage host %s: external storage health/release gate required",
			host.Name,
		)
	case "":
		return fmt.Errorf("automated hardware reset denied for host %s: lifecycleClass is required", host.Name)
	default:
		return fmt.Errorf(
			"automated hardware reset denied for host %s: unsupported lifecycleClass=%q",
			host.Name,
			host.Spec.LifecycleClass,
		)
	}
}

func markDestructiveProvisioningDenied(hrm *infrav1.HetznerRobotMachine, err error) {
	conditions.MarkFalse(
		hrm,
		infrav1.ReadyCondition,
		destructiveProvisioningDeniedReason,
		clusterv1.ConditionSeverityWarning,
		"%s",
		err.Error(),
	)
}

func provisioningMayBecomeDestructive(state infrav1.ProvisioningState) bool {
	switch state {
	case infrav1.StateNone,
		infrav1.StateActivatingRescue,
		infrav1.StateInRescue,
		infrav1.StateInstalling:
		return true
	default:
		return false
	}
}
