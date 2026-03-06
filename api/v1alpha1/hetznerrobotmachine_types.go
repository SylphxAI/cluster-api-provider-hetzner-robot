package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
)

const (
	MachineFinalizer = "hetznerrobotmachine.infrastructure.cluster.x-k8s.io"

	// MaxProvisioningRetries is the maximum number of consecutive errors before entering StateError.
	MaxProvisioningRetries = 10
)

// ProvisioningState represents the current state of the machine provisioning.
type ProvisioningState string

const (
	StateNone             ProvisioningState = ""
	StateActivatingRescue ProvisioningState = "ActivatingRescue"
	StateInRescue         ProvisioningState = "InRescue"
	StateInstalling       ProvisioningState = "Installing"
	StateBootingTalos     ProvisioningState = "BootingTalos"
	StateApplyingConfig   ProvisioningState = "ApplyingConfig"
	StateWaitingForBoot   ProvisioningState = "WaitingForBoot" // waiting for Talos reboot after config apply
	StateBootstrapping    ProvisioningState = "Bootstrapping"
	StateProvisioned      ProvisioningState = "Provisioned"
	StateDeleting         ProvisioningState = "Deleting"
	StateError            ProvisioningState = "Error"
)

// HetznerRobotMachineSpec defines the desired state of HetznerRobotMachine.
type HetznerRobotMachineSpec struct {
	// ProviderID is the unique identifier for this machine.
	// Set automatically by the controller.
	// +optional
	ProviderID *string `json:"providerID,omitempty"`

	// HostRef is a direct reference to a specific HetznerRobotHost by name.
	// Mutually exclusive with HostSelector. Use this for static per-server assignments.
	// +optional
	HostRef *corev1.LocalObjectReference `json:"hostRef,omitempty"`

	// HostSelector selects an Available HetznerRobotHost by label.
	// Mutually exclusive with HostRef. Use this for dynamic pool claiming.
	// +optional
	HostSelector *metav1.LabelSelector `json:"hostSelector,omitempty"`

	// TalosSchematic is the Talos factory schematic ID (with extensions).
	// Example: 3da7f440f279f4814fa73bdf83c84710a8e93c40a4a3cbba4d969f14afb96298
	TalosSchematic string `json:"talosSchematic"`

	// TalosVersion is the Talos version to install.
	// Example: v1.12.4
	TalosVersion string `json:"talosVersion"`

	// InstallDisk is the disk to install Talos on.
	// Defaults to /dev/nvme0n1
	// +optional
	// +kubebuilder:default="/dev/nvme0n1"
	InstallDisk string `json:"installDisk,omitempty"`
}

// HetznerRobotMachineStatus defines the observed state of HetznerRobotMachine.
type HetznerRobotMachineStatus struct {
	// Ready indicates the machine is provisioned and healthy.
	// +optional
	Ready bool `json:"ready,omitempty"`

	// Addresses is the list of addresses for this machine.
	// +optional
	Addresses []clusterv1.MachineAddress `json:"addresses,omitempty"`

	// ProvisioningState is the current state of provisioning.
	// +optional
	ProvisioningState ProvisioningState `json:"provisioningState,omitempty"`

	// HostRef tracks which HetznerRobotHost was claimed by this machine.
	// Set by the controller during ClaimHost state.
	// +optional
	HostRef string `json:"hostRef,omitempty"`

	// FailureReason is a brief string indicating why this machine failed.
	// +optional
	FailureReason *string `json:"failureReason,omitempty"`

	// FailureMessage is a verbose string indicating why this machine failed.
	// +optional
	FailureMessage *string `json:"failureMessage,omitempty"`

	// RetryCount tracks consecutive reconciliation errors for the current state.
	// Reset to 0 on successful state transition. Transitions to StateError at MaxProvisioningRetries.
	// +optional
	RetryCount int `json:"retryCount,omitempty"`

	// Conditions provides observations of the operational state.
	// +optional
	Conditions clusterv1.Conditions `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=hetznerrobotmachines,scope=Namespaced,categories=cluster-api
// +kubebuilder:printcolumn:name="Cluster",type="string",JSONPath=".metadata.labels.cluster\\.x-k8s\\.io/cluster-name"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.ready"
// +kubebuilder:printcolumn:name="State",type="string",JSONPath=".status.provisioningState"
// +kubebuilder:printcolumn:name="Host",type="string",JSONPath=".status.hostRef"

// HetznerRobotMachine is the Schema for the hetznerrobotmachines API.
type HetznerRobotMachine struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HetznerRobotMachineSpec   `json:"spec,omitempty"`
	Status HetznerRobotMachineStatus `json:"status,omitempty"`
}

func (m *HetznerRobotMachine) GetConditions() clusterv1.Conditions {
	return m.Status.Conditions
}

func (m *HetznerRobotMachine) SetConditions(conditions clusterv1.Conditions) {
	m.Status.Conditions = conditions
}

// +kubebuilder:object:root=true

// HetznerRobotMachineList contains a list of HetznerRobotMachine.
type HetznerRobotMachineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HetznerRobotMachine `json:"items"`
}

func init() {
	SchemeBuilder.Register(&HetznerRobotMachine{}, &HetznerRobotMachineList{})
}
