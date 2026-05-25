package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// HostReleaseAction declares the destructive host action authorized by a release.
type HostReleaseAction string

const (
	// HostReleaseActionWipeAndReinstall authorizes CAPHR to wipe and reinstall the released host.
	HostReleaseActionWipeAndReinstall HostReleaseAction = "WipeAndReinstall"
)

// ReleasedMachineReference binds a Host release to one CAPI Machine identity.
type ReleasedMachineReference struct {
	// Name is the CAPI Machine name.
	Name string `json:"name"`
	// Namespace is the CAPI Machine namespace.
	Namespace string `json:"namespace"`
	// UID is the CAPI Machine UID. This prevents a stale release from matching a
	// new Machine that reuses the same name.
	UID string `json:"uid"`
}

// HetznerRobotHostReleaseSpec defines a narrow, expiring release for a physical host.
type HetznerRobotHostReleaseSpec struct {
	// HostRef names the physical Host this release authorizes.
	HostRef corev1.LocalObjectReference `json:"hostRef"`

	// MachineRef binds this release to one CAPI Machine identity.
	MachineRef ReleasedMachineReference `json:"machineRef"`

	// ApprovedAction is the destructive host action CAPHR may perform.
	// +kubebuilder:validation:Enum=WipeAndReinstall
	ApprovedAction HostReleaseAction `json:"approvedAction"`

	// ExpiresAt is the deadline after which CAPHR must reject this release.
	ExpiresAt metav1.Time `json:"expiresAt"`

	// Reason is the human-readable maintenance reason recorded by the lifecycle gate.
	// +optional
	Reason string `json:"reason,omitempty"`
}

// HetznerRobotHostReleaseStatus is reserved for lifecycle-gate observations.
type HetznerRobotHostReleaseStatus struct {
	// Authorized reports whether the lifecycle gate last observed this release as usable.
	// +optional
	Authorized bool `json:"authorized,omitempty"`

	// Reason explains the latest lifecycle-gate observation.
	// +optional
	Reason string `json:"reason,omitempty"`

	// LastObservedTime records when the lifecycle gate last evaluated this release.
	// +optional
	LastObservedTime *metav1.Time `json:"lastObservedTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=hetznerrobothostreleases,scope=Namespaced,shortName=hrhr,categories=cluster-api
// +kubebuilder:printcolumn:name="Host",type="string",JSONPath=".spec.hostRef.name"
// +kubebuilder:printcolumn:name="Machine",type="string",JSONPath=".spec.machineRef.name"
// +kubebuilder:printcolumn:name="Action",type="string",JSONPath=".spec.approvedAction"
// +kubebuilder:printcolumn:name="Expires",type="date",JSONPath=".spec.expiresAt"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// HetznerRobotHostRelease authorizes CAPHR to perform one destructive action on
// one physical host for one CAPI Machine UID. CAPHR does not decide Ceph or etcd
// safety; an external infrastructure lifecycle gate must create this object only
// after backup, quorum, and storage-health checks pass.
type HetznerRobotHostRelease struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HetznerRobotHostReleaseSpec   `json:"spec,omitempty"`
	Status HetznerRobotHostReleaseStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// HetznerRobotHostReleaseList contains a list of HetznerRobotHostRelease.
type HetznerRobotHostReleaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HetznerRobotHostRelease `json:"items"`
}

func init() {
	SchemeBuilder.Register(&HetznerRobotHostRelease{}, &HetznerRobotHostReleaseList{})
}
