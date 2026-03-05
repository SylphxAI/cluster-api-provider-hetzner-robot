package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
)

// HetznerRobotMachineTemplateSpec defines the desired state of HetznerRobotMachineTemplate.
type HetznerRobotMachineTemplateSpec struct {
	Template HetznerRobotMachineTemplateResource `json:"template"`
}

// HetznerRobotMachineTemplateResource describes the data needed to create a HetznerRobotMachine from a template.
type HetznerRobotMachineTemplateResource struct {
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec HetznerRobotMachineSpec `json:"spec"`
}

// HetznerRobotMachineTemplateStatus defines the observed state of HetznerRobotMachineTemplate.
type HetznerRobotMachineTemplateStatus struct {
	// Capacity defines the resource capacity for this machine.
	// +optional
	Capacity clusterv1.ResourceTable `json:"capacity,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=hetznerrobotmachinetemplates,scope=Namespaced,categories=cluster-api

// HetznerRobotMachineTemplate is the Schema for the hetznerrobotmachinetemplates API.
type HetznerRobotMachineTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HetznerRobotMachineTemplateSpec   `json:"spec,omitempty"`
	Status HetznerRobotMachineTemplateStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// HetznerRobotMachineTemplateList contains a list of HetznerRobotMachineTemplate.
type HetznerRobotMachineTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HetznerRobotMachineTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&HetznerRobotMachineTemplate{}, &HetznerRobotMachineTemplateList{})
}
