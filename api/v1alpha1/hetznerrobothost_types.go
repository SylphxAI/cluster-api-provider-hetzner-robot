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
)

// HostLifecycleClass describes the operational role of a physical host.
type HostLifecycleClass string

const (
	// HostLifecycleClassCompute is disposable worker capacity.
	HostLifecycleClassCompute HostLifecycleClass = "compute"
	// HostLifecycleClassControlPlane is an adopted or dedicated Kubernetes control-plane host.
	HostLifecycleClassControlPlane HostLifecycleClass = "control-plane"
	// HostLifecycleClassStorage is a Rook/Ceph storage host.
	HostLifecycleClassStorage HostLifecycleClass = "storage"
)

// DestructiveProvisioningPolicy controls whether CAPHR may wipe or reset a host.
type DestructiveProvisioningPolicy string

const (
	// DestructiveProvisioningPolicyAlwaysCleanSlate permits full disk wipe before provisioning.
	DestructiveProvisioningPolicyAlwaysCleanSlate DestructiveProvisioningPolicy = "AlwaysCleanSlate"
	// DestructiveProvisioningPolicyNeverDestructiveByDefault denies generic destructive provisioning.
	DestructiveProvisioningPolicyNeverDestructiveByDefault DestructiveProvisioningPolicy = "NeverDestructiveByDefault"
	// DestructiveProvisioningPolicyRequiresExternalRelease requires a separate storage lifecycle release.
	DestructiveProvisioningPolicyRequiresExternalRelease DestructiveProvisioningPolicy = "RequiresExternalRelease"
)

// HetznerRobotHostSpec defines the desired state of a physical server.
// +kubebuilder:validation:XValidation:rule="self.serverID == oldSelf.serverID",message="serverID is immutable; create a new HetznerRobotHost for a different physical server"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.serverIP) || oldSelf.serverIP == '' || (has(self.serverIP) && self.serverIP == oldSelf.serverIP)",message="serverIP is immutable once populated"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.serverIPv6Net) || oldSelf.serverIPv6Net == '' || (has(self.serverIPv6Net) && self.serverIPv6Net == oldSelf.serverIPv6Net)",message="serverIPv6Net is immutable once populated"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.internalIP) || oldSelf.internalIP == '' || (has(self.internalIP) && self.internalIP == oldSelf.internalIP)",message="internalIP is immutable once populated"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.installDisk) || oldSelf.installDisk == '' || (has(self.installDisk) && self.installDisk == oldSelf.installDisk)",message="installDisk is immutable once populated"
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

	// PrimaryInterface removed — CAPHR uses primaryMAC (auto-detected from
	// rescue via DetectHardware) for deviceSelector. NIC names differ between
	// rescue (eth0) and Talos (enp193s0f0np0); MAC is stable across boots.

	// InstallDisk is the disk on which Talos will be installed.
	// Defaults to /dev/nvme0n1
	// +optional
	// +kubebuilder:default="/dev/nvme0n1"
	InstallDisk string `json:"installDisk,omitempty"`

	// LifecycleClass declares the physical role of this host. Missing or unknown
	// values fail closed for destructive provisioning.
	// +optional
	// +kubebuilder:validation:Enum=compute;control-plane;storage
	LifecycleClass HostLifecycleClass `json:"lifecycleClass,omitempty"`

	// MaintenanceMode prevents new claims and destructive provisioning for this host.
	// Existing status ownership is left intact so an operator can inspect and repair.
	// +optional
	MaintenanceMode bool `json:"maintenanceMode,omitempty"`

	// DestructiveProvisioningPolicy declares whether CAPHR may wipe or reset this host.
	// Missing or unknown values fail closed.
	// +optional
	// +kubebuilder:validation:Enum=AlwaysCleanSlate;NeverDestructiveByDefault;RequiresExternalRelease
	DestructiveProvisioningPolicy DestructiveProvisioningPolicy `json:"destructiveProvisioningPolicy,omitempty"`
}

// HetznerRobotHostStatus defines the observed state of a physical server.
type HetznerRobotHostStatus struct {
	// State is the current lifecycle state of the host.
	// +optional
	State HostState `json:"state,omitempty"`

	// ConsumerRef is a reference to the HetznerRobotMachine that has claimed this host.
	// Nil when the host is Available. This is the canonical ownership field.
	// +optional
	ConsumerRef *MachineReference `json:"consumerRef,omitempty"`

	// MachineRef is the legacy alias for ConsumerRef, kept for compatibility with
	// existing manifests, print columns, and external readers.
	// +optional
	MachineRef *MachineReference `json:"machineRef,omitempty"`

	// HardwareDetails contains controller-discovered physical facts from rescue mode.
	// +optional
	HardwareDetails *HostHardwareDetails `json:"hardwareDetails,omitempty"`

	// LastConsumerRef records the last HetznerRobotMachine that owned this host.
	// +optional
	LastConsumerRef *MachineReference `json:"lastConsumerRef,omitempty"`

	// DirtyReason explains why an Available host may still need clean-slate provisioning.
	// +optional
	DirtyReason string `json:"dirtyReason,omitempty"`

	// ErrorMessage contains a human-readable description of a terminal error.
	// +optional
	ErrorMessage string `json:"errorMessage,omitempty"`
}

// HostHardwareDetails captures hardware facts discovered in rescue mode.
type HostHardwareDetails struct {
	// PrimaryMAC is the MAC address of the primary NIC.
	// +optional
	PrimaryMAC string `json:"primaryMAC,omitempty"`

	// GatewayIP is the detected default gateway for the public interface.
	// +optional
	GatewayIP string `json:"gatewayIP,omitempty"`

	// NVMeDisks is the list of whole NVMe disk devices detected in rescue.
	// +optional
	NVMeDisks []string `json:"nvmeDisks,omitempty"`

	// CephDisks is the subset of NVMeDisks with Ceph BlueStore signatures.
	// +optional
	CephDisks []string `json:"cephDisks,omitempty"`

	// ByIDPaths maps detected disk devices to stable /dev/disk/by-id paths.
	// +optional
	ByIDPaths map[string]string `json:"byIDPaths,omitempty"`
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
// +kubebuilder:printcolumn:name="Consumer",type="string",JSONPath=".status.consumerRef.name"
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
