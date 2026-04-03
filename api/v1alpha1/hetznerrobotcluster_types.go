package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
)

const ClusterFinalizer = "hetznerrobotcluster.infrastructure.cluster.x-k8s.io"

// VLANConfig defines an internal VLAN network to be injected into Talos machineconfigs.
// When set, CAPHR injects a VLAN interface entry during ApplyConfig, ensuring
// each node gets its internal IP from HetznerRobotHost.Spec.InternalIP.
type VLANConfig struct {
	// ID is the VLAN ID (e.g. 4000 for Hetzner vSwitch).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=4094
	ID int `json:"id"`

	// Interface removed — CAPHR uses primaryMAC (auto-detected from rescue)
	// for deviceSelector instead of interface name. NIC names differ between
	// rescue (eth0) and Talos (enp193s0f0np0); MAC is stable.

	// PrefixLength is the CIDR prefix length for the VLAN subnet (e.g. 24 for /24).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=128
	// +kubebuilder:default=24
	PrefixLength int `json:"prefixLength,omitempty"`
}

// HetznerRobotClusterSpec defines the desired state of HetznerRobotCluster.
type HetznerRobotClusterSpec struct {
	// ControlPlaneEndpoint is the endpoint for the control plane.
	// +optional
	ControlPlaneEndpoint clusterv1.APIEndpoint `json:"controlPlaneEndpoint,omitempty"`

	// RobotSecretRef references the secret containing Hetzner Robot API credentials.
	// The secret must have keys: robot-user, robot-password
	RobotSecretRef corev1.SecretReference `json:"robotSecretRef"`

	// SSHSecretRef references the secret containing the SSH key for rescue access.
	// Required key: ssh-privatekey (PEM-encoded private key for root@rescue).
	// Optional key: ssh-fingerprint (public key fingerprint for rescue activation).
	SSHSecretRef corev1.SecretReference `json:"sshSecretRef"`

	// TalosFactoryBaseURL is the base URL for the Talos factory image.
	// Defaults to https://factory.talos.dev
	// +optional
	TalosFactoryBaseURL string `json:"talosFactoryBaseURL,omitempty"`

	// VLANConfig configures an internal VLAN network on all machines in this cluster.
	// When set, CAPHR injects VLAN interface config into each node's Talos machineconfig
	// during provisioning. The per-node IP comes from HetznerRobotHost.Spec.InternalIP.
	// +optional
	VLANConfig *VLANConfig `json:"vlanConfig,omitempty"`

	// TalosSecretRef removed — CABPT/CACPPT already shares cluster-level
	// secrets (secretboxEncryptionSecret, serviceAccount.key) across all CP nodes.
	// CAPHR's previous injection was redundant (overwriting with identical values).

	// DC removed — hostname is managed by CABPT via HostnameConfig document.
	// CAPHR no longer injects hostname.
}

// HetznerRobotClusterStatus defines the observed state of HetznerRobotCluster.
type HetznerRobotClusterStatus struct {
	// Ready indicates the cluster infrastructure is ready.
	// +optional
	Ready bool `json:"ready,omitempty"`

	// Conditions provides observations of the operational state of a HetznerRobotCluster.
	// +optional
	Conditions clusterv1.Conditions `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=hetznerrobotclusters,scope=Namespaced,categories=cluster-api
// +kubebuilder:printcolumn:name="Cluster",type="string",JSONPath=".metadata.labels.cluster\\.x-k8s\\.io/cluster-name"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.ready"
// +kubebuilder:printcolumn:name="Endpoint",type="string",JSONPath=".spec.controlPlaneEndpoint.host"

// HetznerRobotCluster is the Schema for the hetznerrobotclusters API.
type HetznerRobotCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HetznerRobotClusterSpec   `json:"spec,omitempty"`
	Status HetznerRobotClusterStatus `json:"status,omitempty"`
}

func (c *HetznerRobotCluster) GetConditions() clusterv1.Conditions {
	return c.Status.Conditions
}

func (c *HetznerRobotCluster) SetConditions(conditions clusterv1.Conditions) {
	c.Status.Conditions = conditions
}

// +kubebuilder:object:root=true

// HetznerRobotClusterList contains a list of HetznerRobotCluster.
type HetznerRobotClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HetznerRobotCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&HetznerRobotCluster{}, &HetznerRobotClusterList{})
}
