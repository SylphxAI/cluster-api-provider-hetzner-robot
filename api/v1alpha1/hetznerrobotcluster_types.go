package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
)

const ClusterFinalizer = "hetznerrobotcluster.infrastructure.cluster.x-k8s.io"

// HetznerRobotClusterSpec defines the desired state of HetznerRobotCluster.
type HetznerRobotClusterSpec struct {
	// ControlPlaneEndpoint is the endpoint for the control plane.
	// +optional
	ControlPlaneEndpoint clusterv1.APIEndpoint `json:"controlPlaneEndpoint,omitempty"`

	// RobotSecretRef references the secret containing Hetzner Robot API credentials.
	// The secret must have keys: robot-user, robot-password
	RobotSecretRef corev1.SecretReference `json:"robotSecretRef"`

	// SSHSecretRef references the secret containing the SSH key for rescue access.
	// The secret must have keys: ssh-privatekey, ssh-publickey
	SSHSecretRef corev1.SecretReference `json:"sshSecretRef"`

	// TalosFactoryBaseURL is the base URL for the Talos factory image.
	// Defaults to https://factory.talos.dev
	// +optional
	TalosFactoryBaseURL string `json:"talosFactoryBaseURL,omitempty"`
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
