package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
)

const (
	MachineFinalizer = "hetznerrobotmachine.infrastructure.cluster.x-k8s.io"
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

	// CustomImageURL, if set, overrides the Talos Factory image URL.
	// Used for custom Talos images with baked-in extensions (e.g. devmapper-pool).
	// The URL must point to a raw metal image (zstd or xz compressed).
	// Example: "https://github.com/SylphxAI/talos-images/releases/download/v1.12.6/metal-amd64.raw.zst"
	// +optional
	CustomImageURL string `json:"customImageURL,omitempty"`

	// EphemeralSize, if set, limits the Talos EPHEMERAL partition (/var) to this size
	// and creates a raw data partition ("osd-data") with the remaining disk space.
	// Uses Talos v1.12+ native VolumeConfig + RawVolumeConfig documents.
	// The data partition appears at /dev/disk/by-partlabel/r-osd-data and is
	// intended for Ceph OSD use. Only applicable to storage nodes.
	// Example: "100GiB"
	// +optional
	EphemeralSize string `json:"ephemeralSize,omitempty"`
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

	// PrimaryMAC is the MAC address of the primary NIC, auto-detected during rescue.
	// Used for Talos deviceSelector — hardware-based NIC identification that works
	// regardless of OS naming (rescue eth0 vs Talos enp193s0f0np0).
	// +optional
	PrimaryMAC string `json:"primaryMAC,omitempty"`

	// GatewayIP is the default gateway IP, auto-detected from rescue mode.
	// Used to configure static routing with /32 addresses on the primary NIC.
	// Hetzner DHCP assigns /25 or /26 prefixes which create on-link routes for the
	// entire subnet — but Hetzner blocks direct L2 between servers. Static /32 + explicit
	// gateway avoids this by forcing all traffic through the gateway.
	// +optional
	GatewayIP string `json:"gatewayIP,omitempty"`

	// ResolvedInstallDisk is the stable /dev/disk/by-id/ path resolved during rescue.
	// NVMe device names (/dev/nvme0n1) swap between rescue and Talos boot due to
	// different PCI probe order. This stable path ensures both the installer and
	// Talos machineconfig reference the same physical disk.
	// +optional
	ResolvedInstallDisk string `json:"resolvedInstallDisk,omitempty"`

	// FailureReason is a brief string indicating why this machine failed.
	// +optional
	FailureReason *string `json:"failureReason,omitempty"`

	// FailureMessage is a verbose string indicating why this machine failed.
	// +optional
	FailureMessage *string `json:"failureMessage,omitempty"`

	// ProvisionStarted is set when provisioning begins (first reconcile in StateNone).
	// Used for provision timeout: if provisioning doesn't complete within the timeout,
	// the machine enters StateError → CAPI marks it Failed → MHC remediates.
	// This ensures rolling updates are never blocked by a single stuck machine.
	// +optional
	ProvisionStarted *metav1.Time `json:"provisionStarted,omitempty"`

	// RetryCount tracks consecutive transient reconciliation errors.
	// Used for exponential backoff spacing between retries.
	// +optional
	RetryCount int `json:"retryCount,omitempty"`

	// LastRetryTimestamp is set each time RetryCount is incremented.
	// Used to enforce backoff: status patches trigger watch events that bypass
	// RequeueAfter, so the controller checks this timestamp to skip premature reconciles.
	// +optional
	LastRetryTimestamp *metav1.Time `json:"lastRetryTimestamp,omitempty"`

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
