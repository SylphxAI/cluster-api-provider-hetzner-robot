package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
)

const MachineFinalizer = "hetznerrobotmachine.infrastructure.cluster.x-k8s.io"

// ProvisioningState represents the current state of the machine provisioning.
type ProvisioningState string

const (
	StateNone             ProvisioningState = ""
	StateActivatingRescue ProvisioningState = "ActivatingRescue"
	StateInRescue         ProvisioningState = "InRescue"
	StateInstalling       ProvisioningState = "Installing"
	StateBootingTalos     ProvisioningState = "BootingTalos"
	StateApplyingConfig   ProvisioningState = "ApplyingConfig"
	StateBootstrapping    ProvisioningState = "Bootstrapping"
	StateProvisioned      ProvisioningState = "Provisioned"
	StateDeleting         ProvisioningState = "Deleting"
	StateError           ProvisioningState = "Error"
)

// HetznerRobotMachineSpec defines the desired state of HetznerRobotMachine.
type HetznerRobotMachineSpec struct {
	// ProviderID is the unique identifier for this machine.
	// Set automatically by the controller.
	// +optional
	ProviderID *string `json:"providerID,omitempty"`

	// ServerID is the Hetzner Robot server ID.
	// Find it in the Robot dashboard or via the Robot API.
	ServerID int `json:"serverID"`

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

	// FailureReason is a brief string indicating why this machine failed.
	// +optional
	FailureReason *string `json:"failureReason,omitempty"`

	// FailureMessage is a verbose string indicating why this machine failed.
	// +optional
	FailureMessage *string `json:"failureMessage,omitempty"`

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
// +kubebuilder:printcolumn:name="ServerID",type="integer",JSONPath=".spec.serverID"

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
