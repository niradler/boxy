package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=cp
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=".status.readyReplicas"
// +kubebuilder:printcolumn:name="Active",type=integer,JSONPath=".status.activeSandboxCount"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// ControllerPool represents the fleet of boxy-controller pods (a single StatefulSet).
// One ControllerPool exists per StatefulSet deployment. Sandbox CRs reference the pool
// via status.controllerPool.
type ControllerPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ControllerPoolSpec   `json:"spec,omitempty"`
	Status ControllerPoolStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ControllerPoolList contains a list of ControllerPool resources.
type ControllerPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ControllerPool `json:"items"`
}

type ControllerPoolSpec struct {
	MaxSandboxes         int      `json:"maxSandboxes"`
	MaxReplicas          int32    `json:"maxReplicas"`
	MinReplicas          int32    `json:"minReplicas"`
	Image                string   `json:"image,omitempty"`
	PreinstalledBinaries []string `json:"preinstalledBinaries,omitempty"`
}

type ControllerPoolStatus struct {
	ReadyReplicas      int32              `json:"readyReplicas"`
	ActiveSandboxCount int32              `json:"activeSandboxCount"`
	LastScaleTime      *metav1.Time       `json:"lastScaleTime,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}
