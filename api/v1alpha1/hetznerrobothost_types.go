package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const HostFinalizer = "hetznerrobothost.infrastructure.cluster.x-k8s.io"

// HostState represents the lifecycle state of a physical server.
type HostState string

const (
	// HostStateAvailable means the server is registered and ready to be claimed.
	HostStateAvailable HostState = "Available"
	// HostStateClaimed means a HetznerRobotMachine has claimed this host and is provisioning it.
	HostStateClaimed HostState = "Claimed"
	// HostStateProvisioned means Talos has been installed and the node is running.
	HostStateProvisioned HostState = "Provisioned"
	// HostStateDeprovisioning means the host is being released (wiping + power off).
	HostStateDeprovisioning HostState = "Deprovisioning"
	// HostStateError means an unrecoverable error occurred.
	HostStateError HostState = "Error"
)

// HetznerRobotHostSpec defines the desired state of a physical server.
type HetznerRobotHostSpec struct {
	// ServerID is the Hetzner Robot server ID.
	// Find it in the Robot web console or via the Robot API.
	// +kubebuilder:validation:Minimum=1
	ServerID int `json:"serverID"`

	// ServerIP is the public IPv4 address of the server.
	// Auto-detected from Hetzner Robot API if empty.
	// +optional
	ServerIP string `json:"serverIP,omitempty"`

	// ServerIPv6Net is the IPv6 /64 subnet assigned by Hetzner (e.g. "2a01:4f8:271:3b49::/64").
	// Auto-detected from Hetzner Robot API if empty.
	// CAPHR injects {net}1/64 as the node's IPv6 address + fe80::1 as gateway
	// into the Talos machineconfig during provisioning. Enables dual-stack pods.
	// +optional
	ServerIPv6Net string `json:"serverIPv6Net,omitempty"`

	// InternalIP is the VLAN IP address for this server (e.g. "10.10.0.1").
	// Used with HetznerRobotCluster.Spec.VLANConfig to inject VLAN interface
	// config into the Talos machineconfig during provisioning.
	// Required when VLANConfig is set on the cluster.
	// +optional
	InternalIP string `json:"internalIP,omitempty"`

	// PrimaryInterface is the network interface name for public + IPv6 traffic.
	// Defaults to "enp193s0f0np0" (Hetzner AX-series standard NIC).
	// Override for non-standard hardware (e.g. older EX-series use "enp0s31f6").
	// +optional
	// +kubebuilder:default="enp193s0f0np0"
	PrimaryInterface string `json:"primaryInterface,omitempty"`

	// InstallDisk is the disk on which Talos will be installed.
	// Defaults to /dev/nvme0n1
	// +optional
	// +kubebuilder:default="/dev/nvme0n1"
	InstallDisk string `json:"installDisk,omitempty"`
}

// HetznerRobotHostStatus defines the observed state of a physical server.
type HetznerRobotHostStatus struct {
	// State is the current lifecycle state of the host.
	// +optional
	State HostState `json:"state,omitempty"`

	// MachineRef is a reference to the HetznerRobotMachine that has claimed this host.
	// Nil when the host is Available.
	// +optional
	MachineRef *MachineReference `json:"machineRef,omitempty"`

	// ErrorMessage contains a human-readable description of a terminal error.
	// +optional
	ErrorMessage string `json:"errorMessage,omitempty"`
}

// MachineReference is a reference to a HetznerRobotMachine.
type MachineReference struct {
	// Name is the name of the HetznerRobotMachine.
	Name string `json:"name"`
	// Namespace is the namespace of the HetznerRobotMachine.
	Namespace string `json:"namespace"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=hetznerrobothosts,scope=Namespaced,shortName=hrh,categories=cluster-api
// +kubebuilder:printcolumn:name="ServerID",type="integer",JSONPath=".spec.serverID"
// +kubebuilder:printcolumn:name="ServerIP",type="string",JSONPath=".spec.serverIP"
// +kubebuilder:printcolumn:name="State",type="string",JSONPath=".status.state"
// +kubebuilder:printcolumn:name="Machine",type="string",JSONPath=".status.machineRef.name"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// HetznerRobotHost represents a single physical server in the Hetzner Robot pool.
// One HetznerRobotHost per physical server. Permanent — not deleted when a Machine is removed.
// When a HetznerRobotMachine needs a server, it claims an Available host via hostSelector.
type HetznerRobotHost struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HetznerRobotHostSpec   `json:"spec,omitempty"`
	Status HetznerRobotHostStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// HetznerRobotHostList contains a list of HetznerRobotHost.
type HetznerRobotHostList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HetznerRobotHost `json:"items"`
}

func init() {
	SchemeBuilder.Register(&HetznerRobotHost{}, &HetznerRobotHostList{})
}
