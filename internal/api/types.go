package api

type PodRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}

type ExecRequestBody struct {
	SessionID      string            `json:"sessionId"`
	SandboxID      string            `json:"sandboxId"`
	Command        string            `json:"command"`
	Args           []string          `json:"args"`
	Env            map[string]string `json:"env"`
	TimeoutSeconds int               `json:"timeoutSeconds"`
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
	Rules               []NetworkRule  `json:"rules,omitempty"`
	Ports               []PortMapping  `json:"ports,omitempty"`
	DNS                 *DNSConfig     `json:"dns,omitempty"`
	Secrets             []NetworkSecret `json:"secrets,omitempty"`
	MaxConnections      int            `json:"maxConnections,omitempty"`
	TrustHostCAs        bool           `json:"trustHostCAs,omitempty"`
	Macvlan             *MacvlanConfig `json:"macvlan,omitempty"`
	UsePasta            bool           `json:"usePasta,omitempty"`
}

type NetworkRule struct {
	Direction      string      `json:"direction"`
	Action         string      `json:"action"`
	Protocols      []string    `json:"protocols,omitempty"`
	Ports          []uint16    `json:"ports,omitempty"`
	PortRanges     []PortRange `json:"portRanges,omitempty"`
	Domains        []string    `json:"domains,omitempty"`
	DomainSuffixes []string    `json:"domainSuffixes,omitempty"`
	CIDRs          []string    `json:"cidrs,omitempty"`
	Groups         []string    `json:"groups,omitempty"`
}

type PortRange struct {
	Start uint16 `json:"start"`
	End   uint16 `json:"end"`
}

type PortMapping struct {
	HostPort  uint16 `json:"hostPort"`
	GuestPort uint16 `json:"guestPort"`
	Protocol  string `json:"protocol,omitempty"`
}

type DNSConfig struct {
	Nameservers      []string `json:"nameservers,omitempty"`
	RebindProtection *bool    `json:"rebindProtection,omitempty"`
	QueryTimeoutMs   int      `json:"queryTimeoutMs,omitempty"`
}

type NetworkSecret struct {
	EnvVar                string   `json:"envVar"`
	Value                 string   `json:"value"`
	AllowedHosts          []string `json:"allowedHosts,omitempty"`
	AllowedHostPatterns   []string `json:"allowedHostPatterns,omitempty"`
	AllowAnyHostDangerous bool     `json:"allowAnyHostDangerous,omitempty"`
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
	SessionID       string                `json:"sessionId"`
	SandboxID       string                `json:"sandboxId"`
	Owner           string                `json:"owner"`
	TTLSeconds      int                   `json:"ttlSeconds,omitempty"`
	Env             map[string]string     `json:"env,omitempty"`
	AllowedBinaries []string              `json:"allowedBinaries,omitempty"`
	VM              *VMConfig             `json:"vm,omitempty"`
	Network         *SandboxNetworkConfig `json:"network,omitempty"`
	Volumes         []VolumeMount         `json:"volumes,omitempty"`
	Patches         []SandboxPatch        `json:"patches,omitempty"`
}

type SandboxResponseBody struct {
	SandboxID string `json:"sandboxId"`
	SessionID string `json:"sessionId"`
	Owner     string `json:"owner"`
	Runtime   string `json:"runtime"`
	PodRef    PodRef `json:"podRef"`
	Phase     string `json:"phase"`
	Ready     bool   `json:"ready"`
}

type ErrorBody struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}
