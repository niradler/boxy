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
// +kubebuilder:resource:shortName=sbx
// +kubebuilder:printcolumn:name="SandboxID",type=string,JSONPath=".spec.sandboxId"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

type Sandbox struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec SandboxSpec `json:"spec,omitempty"`
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
	TTLSeconds      int                       `json:"ttlSeconds,omitempty"`
	Env             map[string]string         `json:"env,omitempty"`
	AllowedBinaries []string                  `json:"allowedBinaries,omitempty"`
	VM              *api.VMConfig             `json:"vm,omitempty"`
	Network         *api.SandboxNetworkConfig `json:"network,omitempty"`
	Volumes         []api.VolumeMount         `json:"volumes,omitempty"`
	Patches         []api.SandboxPatch        `json:"patches,omitempty"`
	SetupScript     string                    `json:"setupScript,omitempty"`
	TeardownScript  string                    `json:"teardownScript,omitempty"`
	ScriptEnv       map[string]string         `json:"scriptEnv,omitempty"`
}

const (
	FinalizerSandboxCleanup       = "boxy.dev/sandbox-cleanup"
	FinalizerSessionCleanup       = "boxy.dev/session-cleanup"
	FinalizerSandboxConfigCleanup = "boxy.dev/sandbox-config-cleanup"
	LabelSandboxID                = "boxy.dev/sandbox-id"
)

// Session CR types

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=sess
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="SandboxID",type=string,JSONPath=".spec.sandboxId"
// +kubebuilder:printcolumn:name="Controller",type=string,JSONPath=".status.controllerPod"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

type Session struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SessionSpec   `json:"spec,omitempty"`
	Status            SessionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type SessionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Session `json:"items"`
}

type SessionSpec struct {
	SessionID string `json:"sessionId"`
	SandboxID string `json:"sandboxId"`
	Owner     string `json:"owner,omitempty"`
}

type SessionStatus struct {
	Phase             SandboxPhase `json:"phase,omitempty"`
	ControllerPool    string       `json:"controllerPool,omitempty"`
	ControllerPod     string       `json:"controllerPod,omitempty"`
	ControllerAddress string       `json:"controllerAddress,omitempty"`
	Port              int32        `json:"port,omitempty"`
	CreatedAt         *metav1.Time `json:"createdAt,omitempty"`
	ExpiresAt         *metav1.Time `json:"expiresAt,omitempty"`
	LastExecAt        *metav1.Time `json:"lastExecAt,omitempty"`
	TerminatedAt      *metav1.Time `json:"terminatedAt,omitempty"`
	Message           string       `json:"message,omitempty"`
}
