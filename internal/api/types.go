package api

type ExecMode string

const (
	ExecModeAPI ExecMode = "api_exec"
	ExecModePod ExecMode = "pod_exec"
)

type PodRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}

type ExecRequestBody struct {
	SessionID       string            `json:"sessionId"`
	SandboxID       string            `json:"sandboxId"`
	PodRef          PodRef            `json:"podRef"`
	Command         string            `json:"command"`
	Args            []string          `json:"args"`
	Env             map[string]string `json:"env"`
	TimeoutSeconds  int               `json:"timeoutSeconds"`
	Mode            ExecMode          `json:"mode"`
	WorkerContainer string            `json:"workerContainer,omitempty"`
}

type ExecResponseBody struct {
	ExitCode int    `json:"exitCode"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

type SandboxResources struct {
	CPURequest    string `json:"cpuRequest,omitempty"`
	CPULimit      string `json:"cpuLimit,omitempty"`
	MemoryRequest string `json:"memoryRequest,omitempty"`
	MemoryLimit   string `json:"memoryLimit,omitempty"`
}

type SandboxCreateBody struct {
	SessionID           string            `json:"sessionId"`
	SandboxID           string            `json:"sandboxId"`
	Owner               string            `json:"owner"`
	TTLSeconds          int               `json:"ttlSeconds,omitempty"`
	MaxLifetimeSec      int               `json:"maxLifetimeSeconds,omitempty"`
	Image               string            `json:"image,omitempty"`
	WorkerPort          *int              `json:"workerPort,omitempty"`
	ImagePullPolicy     string            `json:"imagePullPolicy,omitempty"`
	ImagePullSecretName string            `json:"imagePullSecretName,omitempty"`
	ServiceAccountName  string            `json:"serviceAccountName,omitempty"`
	Resources           *SandboxResources `json:"resources,omitempty"`
	Env                 map[string]string `json:"env,omitempty"`
	Labels              map[string]string `json:"labels,omitempty"`
	Annotations         map[string]string `json:"annotations,omitempty"`
}

type SandboxResponseBody struct {
	SandboxID   string `json:"sandboxId"`
	SessionID   string `json:"sessionId"`
	Owner       string `json:"owner"`
	Runtime     string `json:"runtime"`
	ExecAPIPath string `json:"execApiPath"`
	Image       string `json:"image"`
	WorkerPort  int    `json:"workerPort"`
	PodRef      PodRef `json:"podRef"`
	Phase       string `json:"phase"`
	Ready       bool   `json:"ready"`
}

type ErrorBody struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

type SandboxProvisionLimits struct {
	MaxEnvKeys         int
	MaxLabels          int
	MaxAnnotations     int
	MaxImageRefLen     int
	MinWorkerPort      int
	MaxWorkerPort      int
	MaxCPU             string
	MaxMemory          string
	AllowedServiceAcct map[string]struct{}
	AllowedPullSecrets map[string]struct{}
	DefaultServiceAcct string
	GlobalPullSecret   string
}
