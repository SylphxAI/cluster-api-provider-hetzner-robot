package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RemediationPhase represents the current phase of remediation.
type RemediationPhase string

const (
	RemediationPhaseRunning  RemediationPhase = "Running"
	RemediationPhaseWaiting  RemediationPhase = "Waiting"
	RemediationPhaseDeleting RemediationPhase = "Deleting"
)

// RemediationStrategyType defines the type of remediation strategy.
type RemediationStrategyType string

const (
	RemediationStrategyReboot RemediationStrategyType = "Reboot"
)

// RemediationStrategy defines how remediation should be performed.
type RemediationStrategy struct {
	// Type of remediation. Currently only "Reboot" (hardware reset via Robot API).
	// +kubebuilder:default=Reboot
	// +kubebuilder:validation:Enum=Reboot
	Type RemediationStrategyType `json:"type,omitempty"`

	// RetryLimit is the maximum number of hardware resets before giving up.
	// When exhausted, the remediation enters Deleting phase and CAPI handles Machine replacement.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	RetryLimit int `json:"retryLimit,omitempty"`

	// Timeout is the duration to wait after a hardware reset for the node to recover
	// before considering the attempt failed.
	// +kubebuilder:default="300s"
	Timeout metav1.Duration `json:"timeout,omitempty"`
}

// HetznerRobotRemediationSpec defines the desired state of HetznerRobotRemediation.
type HetznerRobotRemediationSpec struct {
	// Strategy defines the remediation strategy to use.
	Strategy RemediationStrategy `json:"strategy,omitempty"`
}

// HetznerRobotRemediationStatus defines the observed state of HetznerRobotRemediation.
type HetznerRobotRemediationStatus struct {
	// Phase represents the current phase of remediation.
	// +optional
	Phase RemediationPhase `json:"phase,omitempty"`

	// RetryCount is the number of hardware resets attempted so far.
	// +optional
	RetryCount int `json:"retryCount,omitempty"`

	// LastRemediated is the timestamp of the last hardware reset.
	// +optional
	LastRemediated *metav1.Time `json:"lastRemediated,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=hetznerrobotremediations,scope=Namespaced,categories=cluster-api
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="RetryCount",type="integer",JSONPath=".status.retryCount"
// +kubebuilder:printcolumn:name="LastRemediated",type="date",JSONPath=".status.lastRemediated"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// HetznerRobotRemediation is the Schema for the hetznerrobotremediations API.
// Created by MachineHealthCheck when a Machine is detected as unhealthy.
type HetznerRobotRemediation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HetznerRobotRemediationSpec   `json:"spec,omitempty"`
	Status HetznerRobotRemediationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// HetznerRobotRemediationList contains a list of HetznerRobotRemediation.
type HetznerRobotRemediationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HetznerRobotRemediation `json:"items"`
}

// HetznerRobotRemediationTemplateSpec defines the desired state of HetznerRobotRemediationTemplate.
type HetznerRobotRemediationTemplateSpec struct {
	Template HetznerRobotRemediationTemplateResource `json:"template"`
}

// HetznerRobotRemediationTemplateResource describes the data needed to create a HetznerRobotRemediation from a template.
type HetznerRobotRemediationTemplateResource struct {
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec HetznerRobotRemediationSpec `json:"spec"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:path=hetznerrobotremediationtemplates,scope=Namespaced,categories=cluster-api

// HetznerRobotRemediationTemplate is the Schema for the hetznerrobotremediationtemplates API.
// Referenced by MachineHealthCheck to instantiate HetznerRobotRemediation objects.
type HetznerRobotRemediationTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec HetznerRobotRemediationTemplateSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// HetznerRobotRemediationTemplateList contains a list of HetznerRobotRemediationTemplate.
type HetznerRobotRemediationTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HetznerRobotRemediationTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(
		&HetznerRobotRemediation{},
		&HetznerRobotRemediationList{},
		&HetznerRobotRemediationTemplate{},
		&HetznerRobotRemediationTemplateList{},
	)
}
