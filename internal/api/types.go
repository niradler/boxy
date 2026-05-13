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
	TimedOut bool   `json:"timedOut,omitempty"`
}

// SandboxResources configures Kubernetes pod resource requests/limits for the
// controller pod. These are pod-level, not per-VM.
type SandboxResources struct {
	CPURequest    string `json:"cpuRequest,omitempty"`
	CPULimit      string `json:"cpuLimit,omitempty"`
	MemoryRequest string `json:"memoryRequest,omitempty"`
	MemoryLimit   string `json:"memoryLimit,omitempty"`
}

// VMConfig configures the MicroVM runtime (passed to microsandbox).

type VMConfig struct {
	// MemoryMB is guest RAM in mebibytes. Default: 512.
	MemoryMB int `json:"memoryMb,omitempty"`
	// VCPUs is the number of virtual CPUs. Default: 1.
	VCPUs int `json:"vcpus,omitempty"`
	// Workdir overrides the working directory inside the VM.
	Workdir string `json:"workdir,omitempty"`
	// Shell overrides the default shell (e.g. "/bin/bash").
	Shell string `json:"shell,omitempty"`
	// Hostname sets the guest hostname. Default: sandbox ID.
	Hostname string `json:"hostname,omitempty"`
	// User sets the guest user identity (e.g. "nobody", "1000").
	User string `json:"user,omitempty"`
	// MaxDurationSec is a hard cap on VM lifetime in seconds. 0 = no limit.
	MaxDurationSec int `json:"maxDurationSec,omitempty"`
	// IdleTimeoutSec kills the VM after N seconds of no exec activity. 0 = no timeout.
	IdleTimeoutSec int `json:"idleTimeoutSec,omitempty"`
	// Rlimits sets per-process resource limits inside the VM.
	Rlimits []VMRlimit `json:"rlimits,omitempty"`
	// Scripts are named shell scripts placed at /.msb/scripts/<name> inside the VM.
	Scripts []VMScript `json:"scripts,omitempty"`
}

// VMRlimit sets a POSIX resource limit for processes inside the VM.
// Resource names: "nofile", "nproc", "memlock", "msgqueue", "sigpending",
// "nice", "rtprio", "rttime".
type VMRlimit struct {
	Resource string `json:"resource"`
	Soft     uint64 `json:"soft"`
	Hard     uint64 `json:"hard"`
}

// VMScript is a named script placed at /.msb/scripts/<name> inside the VM.
type VMScript struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

// SandboxNetworkConfig configures all VM-level networking via microsandbox.
// Domain/IP filtering is enforced inside the VM; Kubernetes NetworkPolicy
// governs pod-level traffic separately.
type SandboxNetworkConfig struct {
	// Enabled toggles networking on/off. Default: true.
	Enabled *bool `json:"enabled,omitempty"`

	// --- Common shortcuts (cover 90% of use cases) ---

	// AllowInternetAccess permits unrestricted outbound traffic. Overrides Rules.
	AllowInternetAccess bool `json:"allowInternetAccess,omitempty"`
	// AllowedEgressDomains is a deny-all policy that allows only these hostnames.
	// Ignored when AllowInternetAccess is true or Rules is set.
	AllowedEgressDomains []string `json:"allowedEgressDomains,omitempty"`

	// --- Advanced ---

	// Rules is a full ordered rule set. When set, shortcuts above are ignored.
	// Each rule is evaluated in order; first match wins per direction.
	Rules []NetworkRule `json:"rules,omitempty"`
	// Ports publishes TCP/UDP ports from the VM to the host.
	Ports []PortMapping `json:"ports,omitempty"`
	// DNS configures upstream resolvers and DNS options for the VM.
	DNS *DNSConfig `json:"dns,omitempty"`
	// Secrets injects credential values into outbound HTTP/HTTPS requests
	// only when the destination host matches. Values are never logged.
	Secrets []NetworkSecret `json:"secrets,omitempty"`
	// MaxConnections caps concurrent outbound connections. Default: 256.
	MaxConnections int `json:"maxConnections,omitempty"`
	// TrustHostCAs copies the host's root CA bundle into the VM, useful for
	// corporate proxies with custom CAs.
	TrustHostCAs bool `json:"trustHostCAs,omitempty"`
}

// NetworkRule is a single firewall rule evaluated inside the VM.
type NetworkRule struct {
	// Direction: "egress", "ingress", or "any". Required.
	Direction string `json:"direction"`
	// Action: "allow" or "deny". Required.
	Action string `json:"action"`
	// Protocols filters by protocol. Empty = any.
	// Values: "tcp", "udp", "icmpv4", "icmpv6".
	Protocols []string `json:"protocols,omitempty"`
	// Ports matches specific destination ports.
	Ports []uint16 `json:"ports,omitempty"`
	// PortRanges matches inclusive port ranges.
	PortRanges []PortRange `json:"portRanges,omitempty"`
	// Domains matches exact hostnames.
	Domains []string `json:"domains,omitempty"`
	// DomainSuffixes matches hostname suffixes (e.g. ".amazonaws.com").
	DomainSuffixes []string `json:"domainSuffixes,omitempty"`
	// CIDRs matches IP ranges (e.g. "10.0.0.0/8", "2001:db8::/32").
	CIDRs []string `json:"cidrs,omitempty"`
	// Groups matches predefined destination groups.
	// Values: "public", "private", "loopback", "link_local", "metadata",
	// "multicast", "host".
	Groups []string `json:"groups,omitempty"`
}

// PortRange is an inclusive range of ports.
type PortRange struct {
	Start uint16 `json:"start"`
	End   uint16 `json:"end"`
}

// PortMapping publishes a VM port to the host.
type PortMapping struct {
	HostPort  uint16 `json:"hostPort"`
	GuestPort uint16 `json:"guestPort"`
	// Protocol: "tcp" (default) or "udp".
	Protocol string `json:"protocol,omitempty"`
}

// DNSConfig overrides DNS resolver settings inside the VM.
type DNSConfig struct {
	// Nameservers is the list of upstream resolvers (e.g. "1.1.1.1:53").
	Nameservers []string `json:"nameservers,omitempty"`
	// RebindProtection blocks DNS rebinding attacks. Default: true.
	RebindProtection *bool `json:"rebindProtection,omitempty"`
	// QueryTimeoutMs is the per-query timeout in milliseconds. Default: 5000.
	QueryTimeoutMs int `json:"queryTimeoutMs,omitempty"`
}

// NetworkSecret injects a credential value into outbound HTTP/HTTPS requests
// destined for the specified hosts. The value is never logged or returned in
// exec output.
type NetworkSecret struct {
	// EnvVar is the environment variable name used as a placeholder in requests.
	EnvVar string `json:"envVar"`
	// Value is the secret value to inject.
	Value string `json:"value"`
	// AllowedHosts lists exact hostnames where injection is permitted.
	AllowedHosts []string `json:"allowedHosts,omitempty"`
	// AllowedHostPatterns lists wildcard patterns (e.g. "*.amazonaws.com").
	AllowedHostPatterns []string `json:"allowedHostPatterns,omitempty"`
	// AllowAnyHostDangerous injects into all hosts regardless of destination.
	// Use only in fully isolated sandboxes.
	AllowAnyHostDangerous bool `json:"allowAnyHostDangerous,omitempty"`
}

// VolumeMount attaches storage to a path inside the VM.
type VolumeMount struct {
	// GuestPath is the mount destination inside the VM. Required.
	GuestPath string `json:"guestPath"`
	// Type: "bind", "named", or "tmpfs". Required.
	Type string `json:"type"`
	// HostPath is the source directory on the host node. Required for "bind".
	HostPath string `json:"hostPath,omitempty"`
	// Name identifies a named (persistent) volume. Required for "named".
	Name string `json:"name,omitempty"`
	// SizeMB is the size limit in mebibytes. Used for "tmpfs" only.
	SizeMB int `json:"sizeMb,omitempty"`
	// Readonly mounts the volume as read-only.
	Readonly bool `json:"readonly,omitempty"`
}

// SandboxPatch modifies the VM filesystem before the sandbox starts.
// Patches are applied in order. For OCI-rooted VMs they bake into the
// overlay upper layer; they do not modify the base image.
type SandboxPatch struct {
	// Type determines the operation. Required.
	// Values: "text", "bytes", "copy_file", "copy_dir", "symlink",
	// "mkdir", "remove", "append".
	Type string `json:"type"`
	// Path is the destination path inside the VM. Required for all types
	// except "copy_dir" (where it is the destination directory).
	Path string `json:"path"`
	// Content is the text content. Used for "text" and "append".
	Content string `json:"content,omitempty"`
	// Bytes is base64-encoded binary content. Used for "bytes".
	Bytes string `json:"bytes,omitempty"`
	// HostPath is the source on the controller host. Used for "copy_file"
	// and "copy_dir". Must be a path pre-installed in the controller image.
	HostPath string `json:"hostPath,omitempty"`
	// Target is the symlink target path. Used for "symlink".
	Target string `json:"target,omitempty"`
	// Mode is the Unix file permission bits in octal (e.g. 493 for 0755).
	Mode int `json:"mode,omitempty"`
	// Replace overwrites the destination if it already exists.
	Replace bool `json:"replace,omitempty"`
}

type SandboxCreateBody struct {
	SessionID           string                `json:"sessionId"`
	SandboxID           string                `json:"sandboxId"`
	Owner               string                `json:"owner"`
	TTLSeconds          int                   `json:"ttlSeconds,omitempty"`
	MaxLifetimeSec      int                   `json:"maxLifetimeSeconds,omitempty"`
	Image               string                `json:"image,omitempty"`
	WorkerPort          *int                  `json:"workerPort,omitempty"`
	ImagePullPolicy     string                `json:"imagePullPolicy,omitempty"`
	ImagePullSecretName string                `json:"imagePullSecretName,omitempty"`
	ServiceAccountName  string                `json:"serviceAccountName,omitempty"`
	Resources           *SandboxResources     `json:"resources,omitempty"`
	VM                  *VMConfig             `json:"vm,omitempty"`
	Network             *SandboxNetworkConfig `json:"network,omitempty"`
	// AllowedBinaries is a shorthand to inject pre-installed CLI binaries
	// (e.g. "aws", "gcloud", "curl") from the controller image into the VM.
	// Equivalent to Patches with type "copy_file" from /usr/local/bin.
	AllowedBinaries []string          `json:"allowedBinaries,omitempty"`
	Volumes         []VolumeMount     `json:"volumes,omitempty"`
	Patches         []SandboxPatch    `json:"patches,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	Annotations     map[string]string `json:"annotations,omitempty"`
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
