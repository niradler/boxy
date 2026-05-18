package api

type ExecRequestBody struct {
	SandboxID      string            `json:"sandboxId"`
	SessionID      string            `json:"sessionId,omitempty"`
	Owner          string            `json:"owner,omitempty"`
	Command        string            `json:"command"`
	Args           []string          `json:"args"`
	Env            map[string]string `json:"env"`
	TimeoutSeconds int               `json:"timeoutSeconds"`
	PTY            bool              `json:"pty,omitempty"`
}

type ExecResponseBody struct {
	ExitCode int    `json:"exitCode"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	TimedOut bool   `json:"timedOut,omitempty"`
}

type VMConfig struct {
	Image          string     `json:"image,omitempty"`
	MemoryMB       int        `json:"memoryMb,omitempty"`
	VCPUs          int        `json:"vcpus,omitempty"`
	Workdir        string     `json:"workdir,omitempty"`
	Shell          string     `json:"shell,omitempty"`
	Hostname       string     `json:"hostname,omitempty"`
	User           string     `json:"user,omitempty"`
	MaxDurationSec int        `json:"maxDurationSec,omitempty"`
	IdleTimeoutSec int        `json:"idleTimeoutSec,omitempty"`
	Rlimits        []VMRlimit `json:"rlimits,omitempty"`
	Scripts        []VMScript `json:"scripts,omitempty"`
	SeccompString  string     `json:"seccompString,omitempty"`
	CloneNewTime   bool       `json:"cloneNewTime,omitempty"`
}

type VMRlimit struct {
	Resource string `json:"resource"`
	Soft     uint64 `json:"soft"`
	Hard     uint64 `json:"hard"`
}

type VMScript struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

type MacvlanConfig struct {
	Interface string `json:"interface"`
	IP        string `json:"ip,omitempty"`
	Netmask   string `json:"netmask,omitempty"`
	Gateway   string `json:"gateway,omitempty"`
	MAC       string `json:"mac,omitempty"`
}

type SandboxNetworkConfig struct {
	Enabled             *bool          `json:"enabled,omitempty"`
	AllowInternetAccess bool           `json:"allowInternetAccess,omitempty"`
	Macvlan             *MacvlanConfig `json:"macvlan,omitempty"`
	UsePasta            bool           `json:"usePasta,omitempty"`
}

type VolumeMount struct {
	GuestPath string `json:"guestPath"`
	Type      string `json:"type"`
	HostPath  string `json:"hostPath,omitempty"`
	Name      string `json:"name,omitempty"`
	SizeMB    int    `json:"sizeMb,omitempty"`
	Readonly  bool   `json:"readonly,omitempty"`
}

type SandboxPatch struct {
	Type     string `json:"type"`
	Path     string `json:"path"`
	Content  string `json:"content,omitempty"`
	Bytes    string `json:"bytes,omitempty"`
	HostPath string `json:"hostPath,omitempty"`
	Target   string `json:"target,omitempty"`
	Mode     int    `json:"mode,omitempty"`
	Replace  bool   `json:"replace,omitempty"`
}

type SandboxCreateBody struct {
	SandboxID       string                `json:"sandboxId"`
	TTLSeconds      int                   `json:"ttlSeconds,omitempty"`
	Env             map[string]string     `json:"env,omitempty"`
	AllowedBinaries []string              `json:"allowedBinaries,omitempty"`
	VM              *VMConfig             `json:"vm,omitempty"`
	Network         *SandboxNetworkConfig `json:"network,omitempty"`
	Volumes         []VolumeMount         `json:"volumes,omitempty"`
	Patches         []SandboxPatch        `json:"patches,omitempty"`
	SetupScript     string                `json:"setupScript,omitempty"`
	TeardownScript  string                `json:"teardownScript,omitempty"`
	ScriptEnv       map[string]string     `json:"scriptEnv,omitempty"`
}

type ErrorBody struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

type SessionCreateBody struct {
	SandboxID string `json:"sandboxId"`
	SessionID string `json:"sessionId,omitempty"`
	Owner     string `json:"owner,omitempty"`
}

type SessionResponseBody struct {
	SessionID         string `json:"sessionId"`
	SandboxID         string `json:"sandboxId"`
	Owner             string `json:"owner,omitempty"`
	Phase             string `json:"phase"`
	Ready             bool   `json:"ready"`
	ControllerPod     string `json:"controllerPod,omitempty"`
	ControllerAddress string `json:"controllerAddress,omitempty"`
	Port              int32  `json:"port,omitempty"`
	CreatedAt         string `json:"createdAt,omitempty"`
	ExpiresAt         string `json:"expiresAt,omitempty"`
	LastExecAt        string `json:"lastExecAt,omitempty"`
}

type SessionListResponse struct {
	Sessions []SessionResponseBody `json:"sessions"`
}

type SandboxConfigResponse struct {
	SandboxID      string `json:"sandboxId"`
	TTLSeconds     int    `json:"ttlSeconds,omitempty"`
	ActiveSessions int    `json:"activeSessions"`
}

type SandboxConfigListResponse struct {
	Sandboxes []SandboxConfigResponse `json:"sandboxes"`
}

type SandboxEvictResponse struct {
	EvictedSessions int    `json:"evictedSessions"`
	Message         string `json:"message"`
}
