package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"boxy.dev/boxy/internal/api"
)

// SandboxPhase represents the lifecycle state of a sandbox.
type SandboxPhase string

const (
	SandboxPhasePending    SandboxPhase = "Pending"
	SandboxPhaseCreating   SandboxPhase = "Creating"
	SandboxPhaseRunning    SandboxPhase = "Running"
	SandboxPhaseDeleting   SandboxPhase = "Deleting"
	SandboxPhaseTerminated SandboxPhase = "Terminated"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=sbx
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="SandboxID",type=string,JSONPath=".spec.sandboxId"
// +kubebuilder:printcolumn:name="Controller",type=string,JSONPath=".status.controllerPod"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// Sandbox is a single sandbox VM instance managed by the boxy operator.
type Sandbox struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SandboxSpec   `json:"spec,omitempty"`
	Status SandboxStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SandboxList contains a list of Sandbox resources.
type SandboxList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Sandbox `json:"items"`
}

type SandboxSpec struct {
	SandboxID       string                    `json:"sandboxId"`
	SessionID       string                    `json:"sessionId"`
	Owner           string                    `json:"owner"`
	TTLSeconds      int                       `json:"ttlSeconds,omitempty"`
	Env             map[string]string         `json:"env,omitempty"`
	AllowedBinaries []string                  `json:"allowedBinaries,omitempty"`
	VM              *api.VMConfig             `json:"vm,omitempty"`
	Network         *api.SandboxNetworkConfig `json:"network,omitempty"`
	Volumes         []api.VolumeMount         `json:"volumes,omitempty"`
	Patches         []api.SandboxPatch        `json:"patches,omitempty"`
	RetentionPeriod *metav1.Duration          `json:"retentionPeriod,omitempty"`
}

type SandboxStatus struct {
	Phase             SandboxPhase `json:"phase,omitempty"`
	ControllerPod     string       `json:"controllerPod,omitempty"`
	ControllerAddress string       `json:"controllerAddress,omitempty"`
	Port              int32        `json:"port,omitempty"`
	CreatedAt         *metav1.Time `json:"createdAt,omitempty"`
	ExpiresAt         *metav1.Time `json:"expiresAt,omitempty"`
	TerminatedAt      *metav1.Time `json:"terminatedAt,omitempty"`
	LastExecAt        *metav1.Time `json:"lastExecAt,omitempty"`
	Message           string       `json:"message,omitempty"`
}

const (
	FinalizerSandboxCleanup = "boxy.dev/sandbox-cleanup"
	LabelSandboxID          = "boxy.dev/sandbox-id"
)
