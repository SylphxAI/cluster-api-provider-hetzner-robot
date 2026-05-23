package controllers

import (
	"fmt"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/conditions"

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
			"destructive provisioning denied for storage host %s: external storage release is not implemented/enforced yet",
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
