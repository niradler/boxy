package nsjail

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"boxy.dev/boxy/internal/api"
)

type Adapter interface {
	Create(ctx context.Context, req *api.SandboxCreateBody) error
	Exec(ctx context.Context, sandboxID string, command string, args []string, env map[string]string, timeoutSecs int) (*api.ExecResponseBody, error)
	Delete(ctx context.Context, sandboxID string) error
	ListIDs() []string
	Count() int
}

// AdapterError carries an HTTP status code alongside the message.
type AdapterError struct {
	Code    int
	Message string
}

func (e *AdapterError) Error() string { return e.Message }

func errBadRequest(msg string) *AdapterError { return &AdapterError{400, msg} }
func errConflict(id string) *AdapterError    { return &AdapterError{409, "sandbox already exists: " + id} }
func errNotFound(id string) *AdapterError    { return &AdapterError{404, "sandbox not found: " + id} }
func errInternal(msg string) *AdapterError   { return &AdapterError{500, msg} }

type AdapterConfig struct {
	NsjailPath     string
	DefaultRootfs  string
	SandboxRoot    string
	BinariesDir    string
	MaxOutputBytes int // truncate stdout/stderr above this size; 0 = unlimited
}

type sandbox struct {
	req       api.SandboxCreateBody
	workspace string // absolute path to the per-sandbox R/W workspace
	binDir    string // absolute path to the per-sandbox /usr/local/bin mirror
}

// NsjailAdapter implements Adapter using nsjail with protobuf text-format config files.
type NsjailAdapter struct {
	cfg       AdapterConfig
	mu        sync.RWMutex
	sandboxes map[string]*sandbox
}

func NewNsjailAdapter(cfg AdapterConfig) *NsjailAdapter {
	return &NsjailAdapter{
		cfg:       cfg,
		sandboxes: make(map[string]*sandbox),
	}
}

func (a *NsjailAdapter) Create(ctx context.Context, req *api.SandboxCreateBody) error {
	if err := a.validateCreateRequest(req); err != nil {
		return err
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if _, exists := a.sandboxes[req.SandboxID]; exists {
		return errConflict(req.SandboxID)
	}

	sandboxBase := filepath.Join(a.cfg.SandboxRoot, req.SandboxID)
	workspace := filepath.Join(sandboxBase, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		return errInternal(fmt.Sprintf("mkdir workspace %s: %v", workspace, err))
	}

	binDir := filepath.Join(sandboxBase, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		_ = os.RemoveAll(sandboxBase)
		return errInternal(fmt.Sprintf("mkdir bin %s: %v", binDir, err))
	}
	for _, bin := range req.AllowedBinaries {
		src := filepath.Join(a.cfg.BinariesDir, bin)
		dst := filepath.Join(binDir, bin)
		if err := copyExec(src, dst); err != nil {
			_ = os.RemoveAll(sandboxBase)
			return errInternal(fmt.Sprintf("copy binary %q: %v", bin, err))
		}
	}

	a.sandboxes[req.SandboxID] = &sandbox{req: *req, workspace: workspace, binDir: binDir}
	return nil
}

func (a *NsjailAdapter) Exec(
	ctx context.Context,
	sandboxID string,
	command string,
	args []string,
	env map[string]string,
	timeoutSecs int,
) (*api.ExecResponseBody, error) {
	a.mu.RLock()
	sb, ok := a.sandboxes[sandboxID]
	a.mu.RUnlock()
	if !ok {
		return nil, errNotFound(sandboxID)
	}

	rootfs := a.cfg.DefaultRootfs
	if sb.req.VM != nil && sb.req.VM.Image != "" {
		rootfs = sb.req.VM.Image
	}

	resolvedCmd := resolveCommandInChroot(command, rootfs)

	cfg := a.buildNsjailConfig(sb, rootfs, env, timeoutSecs)

	cfgFile, err := os.CreateTemp("", "nsjail-*.pb.txt")
	if err != nil {
		return nil, errInternal(fmt.Sprintf("create temp config: %v", err))
	}
	defer os.Remove(cfgFile.Name())

	if _, err := cfgFile.WriteString(cfg.ToTextProto()); err != nil {
		cfgFile.Close()
		return nil, errInternal(fmt.Sprintf("write nsjail config: %v", err))
	}
	cfgFile.Close()

	cmdArgs := append([]string{"--config", cfgFile.Name(), "--"}, resolvedCmd)
	cmdArgs = append(cmdArgs, args...)

	var deadline context.Context
	var cancel context.CancelFunc
	if timeoutSecs > 0 {
		deadline, cancel = context.WithTimeout(ctx, time.Duration(timeoutSecs+5)*time.Second)
	} else {
		deadline, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	nsjailCmd := exec.CommandContext(deadline, a.cfg.NsjailPath, cmdArgs...)
	var stdout, stderr limitWriter
	if a.cfg.MaxOutputBytes > 0 {
		stdout.limit = int64(a.cfg.MaxOutputBytes)
		stderr.limit = int64(a.cfg.MaxOutputBytes)
	}
	nsjailCmd.Stdout = &stdout
	nsjailCmd.Stderr = &stderr

	runErr := nsjailCmd.Run()
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			// Exit code 137 = SIGKILL (128+9). nsjail sends SIGKILL to the child
			// when time_limit fires and propagates the exit status, so this is the
			// reliable signal that the sandbox's configured timeout was hit.
			timedOut := timeoutSecs > 0 && exitErr.ExitCode() == 137
			return &api.ExecResponseBody{
				Stdout:   stdout.String(),
				Stderr:   stderr.String(),
				ExitCode: exitErr.ExitCode(),
				TimedOut: timedOut,
			}, nil
		}
		if deadline.Err() == context.DeadlineExceeded {
			return &api.ExecResponseBody{
				Stderr:   "execution timed out",
				ExitCode: -1,
				TimedOut: true,
			}, nil
		}
		return nil, errInternal(fmt.Sprintf("nsjail error: %v", runErr))
	}

	return &api.ExecResponseBody{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: 0,
	}, nil
}

// limitWriter is an io.Writer that stops accepting data once limit bytes have
// been written, silently discarding additional bytes. Zero limit = unlimited.
type limitWriter struct {
	buf   bytes.Buffer
	limit int64
}

func (lw *limitWriter) Write(p []byte) (int, error) {
	if lw.limit > 0 {
		remaining := lw.limit - int64(lw.buf.Len())
		if remaining <= 0 {
			return len(p), nil // discard; report success so the process isn't killed
		}
		if int64(len(p)) > remaining {
			p = p[:remaining]
		}
	}
	n, err := lw.buf.Write(p)
	return n, err
}

func (lw *limitWriter) String() string { return lw.buf.String() }

func (a *NsjailAdapter) Delete(ctx context.Context, sandboxID string) error {
	a.mu.Lock()
	sb, ok := a.sandboxes[sandboxID]
	if ok {
		delete(a.sandboxes, sandboxID)
	}
	a.mu.Unlock()

	if !ok {
		return errNotFound(sandboxID)
	}

	base := filepath.Dir(sb.workspace)
	if err := os.RemoveAll(base); err != nil {
		slog.Warn("sandbox cleanup failed", "sandboxId", sandboxID, "path", base, "err", err)
	}
	return nil
}

func (a *NsjailAdapter) ListIDs() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	ids := make([]string, 0, len(a.sandboxes))
	for id := range a.sandboxes {
		ids = append(ids, id)
	}
	return ids
}

func (a *NsjailAdapter) Count() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.sandboxes)
}

func (a *NsjailAdapter) buildNsjailConfig(sb *sandbox, rootfs string, execEnv map[string]string, timeoutSecs int) *NsjailConfig {
	// Processes run as uid 0 inside nsjail. Running as a non-root uid requires
	// either user namespaces (blocked by Docker Desktop / most container runtimes)
	// or setuid-via-uidmap (nsjail calls setuid before mount setup, so the child
	// can't create dirs in the root-owned /run/user/nsjail.*.root temp tree).
	// Security boundary is enforced by mount, PID, and network namespace isolation.
	cfg := &NsjailConfig{
		Mode:                ModeOnce,
		Log:                 "/dev/null",
		Chroot:              rootfs,
		DisableCloneNewUser: true,
		DisableCloneNewNet:  false,
	}

	if sb.req.VM != nil {
		vm := sb.req.VM
		cfg.Hostname = vm.Hostname
		cfg.Cwd = vm.Workdir
		cfg.User = vm.User
		cfg.SeccompString = vm.SeccompString
		cfg.CloneNewTime = vm.CloneNewTime

		if vm.MemoryMB > 0 {
			cfg.CgroupMemMax = uint64(vm.MemoryMB) * 1024 * 1024
		}
		for _, rl := range vm.Rlimits {
			switch rl.Resource {
			case "as":
				cfg.RlimitAS = rl.Soft
			case "core":
				cfg.RlimitCore = rl.Soft
			case "cpu":
				cfg.RlimitCPU = rl.Soft
			case "fsize":
				cfg.RlimitFsize = rl.Soft
			case "nofile":
				cfg.RlimitNofile = rl.Soft
			case "nproc":
				cfg.RlimitNproc = rl.Soft
			case "stack":
				cfg.RlimitStack = rl.Soft
			}
		}
	}

	if timeoutSecs > 0 {
		cfg.TimeLimit = uint32(timeoutSecs)
	}

	// DisableCloneNewNet=true means "disable the clone_newnet flag" which paradoxically
	// gives internet access (sandbox inherits pod netns instead of getting an isolated one).
	if sb.req.Network != nil {
		net := sb.req.Network
		if net.Enabled != nil && !*net.Enabled {
			// leave DisableCloneNewNet = false (isolated)
		} else if net.AllowInternetAccess {
			cfg.DisableCloneNewNet = true
		}
		if net.UsePasta {
			cfg.UsePasta = true
		}
		if net.Macvlan != nil {
			cfg.Macvlan = &MacvlanConfig{
				Iface:   net.Macvlan.Interface,
				IP:      net.Macvlan.IP,
				Netmask: net.Macvlan.Netmask,
				Gateway: net.Macvlan.Gateway,
				MAC:     net.Macvlan.MAC,
			}
		}
	}

	cfg.Mounts = append(cfg.Mounts,
		MountPt{Src: sb.workspace, Dst: "/workspace", Rw: true, IsBind: true},
		MountPt{Dst: "/tmp", Fstype: "tmpfs", Rw: true},
		// Bind essential device files from the host. The ubuntu rootfs /dev/ is
		// empty when used as a bind-mount pivot root; nsjail creates the
		// destination file automatically when is_bind: true and dst is absent.
		MountPt{Src: "/dev/null", Dst: "/dev/null", Rw: true, IsBind: true},
		MountPt{Src: "/dev/zero", Dst: "/dev/zero", IsBind: true},
		MountPt{Src: "/dev/urandom", Dst: "/dev/urandom", IsBind: true},
		MountPt{Src: "/dev/random", Dst: "/dev/random", IsBind: true},
	)
	// When internet access is enabled the sandbox inherits the pod's network namespace,
	// but the bare Ubuntu rootfs has an empty /etc/resolv.conf. Bind-mount the pod's
	// resolv.conf so DNS resolution works inside the sandbox.
	if sb.req.Network != nil && sb.req.Network.AllowInternetAccess {
		cfg.Mounts = append(cfg.Mounts, MountPt{
			Src:    "/etc/resolv.conf",
			Dst:    "/etc/resolv.conf",
			IsBind: true,
		})
	}

	for _, vol := range sb.req.Volumes {
		if vol.Type == "tmpfs" {
			cfg.Mounts = append(cfg.Mounts, MountPt{Dst: vol.GuestPath, Fstype: "tmpfs", Rw: true})
		} else {
			host := vol.HostPath
			if host == "" {
				host = vol.GuestPath
			}
			cfg.Mounts = append(cfg.Mounts, MountPt{
				Src:    host,
				Dst:    vol.GuestPath,
				Rw:     !vol.Readonly,
				IsBind: true,
			})
		}
	}

	// Each sandbox gets its own /usr/local/bin with only its allowedBinaries pre-copied in.
	cfg.Mounts = append(cfg.Mounts, MountPt{
		Src:    sb.binDir,
		Dst:    "/usr/local/bin",
		Rw:     true,
		IsBind: true,
	})

	// Env vars: baseline (PATH, HOME), then sandbox-level, then per-exec overrides.
	// HOME=/workspace is the only non-PATH baseline: many tools (npm, pip, git)
	// fail without a writable HOME directory. Sandbox env can override it.
	cfg.Envar = append(cfg.Envar,
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/workspace",
	)
	for k, v := range sb.req.Env {
		cfg.Envar = append(cfg.Envar, k+"="+v)
	}
	for k, v := range execEnv {
		cfg.Envar = append(cfg.Envar, k+"="+v)
	}

	return cfg
}

func (a *NsjailAdapter) validateCreateRequest(req *api.SandboxCreateBody) error {
	if req.VM != nil {
		if req.VM.Image != "" {
			parent := filepath.Dir(a.cfg.DefaultRootfs)
			if strings.Contains(req.VM.Image, "..") || !isUnderPath(req.VM.Image, parent) {
				return errBadRequest(fmt.Sprintf("vm.image must be under %s", parent))
			}
		}
		if req.VM.User == "root" || req.VM.User == "0" {
			return errBadRequest("vm.user 'root'/'0' is not permitted")
		}
		if req.VM.User != "" {
			if uid, err := strconv.ParseUint(req.VM.User, 10, 64); err == nil && uid == 0 {
				return errBadRequest("vm.user uid 0 is not permitted")
			}
		}
		if req.VM.Workdir != "" && (!strings.HasPrefix(req.VM.Workdir, "/") || strings.Contains(req.VM.Workdir, "..")) {
			return errBadRequest("vm.workdir must be an absolute path without '..'")
		}
		if req.VM.Hostname != "" {
			for _, c := range req.VM.Hostname {
				if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-') {
					return errBadRequest("vm.hostname must contain only alphanumeric characters and hyphens")
				}
			}
		}
	}

	for _, bin := range req.AllowedBinaries {
		if bin == "" || strings.Contains(bin, "/") || strings.Contains(bin, "..") {
			return errBadRequest(fmt.Sprintf("allowed_binaries entry %q must be a plain filename", bin))
		}
	}

	for _, vol := range req.Volumes {
		if vol.Type == "tmpfs" {
			continue
		}
		if vol.HostPath == "" {
			return errBadRequest("volume host_path is required for non-tmpfs volumes")
		}
		if strings.Contains(vol.HostPath, "..") || !isUnderPath(vol.HostPath, a.cfg.SandboxRoot) {
			return errBadRequest(fmt.Sprintf("volume host_path must be under %s", a.cfg.SandboxRoot))
		}
	}

	return nil
}

// resolveCommandInChroot resolves a relative command name to an absolute path by
// walking common PATH directories inside the chroot. nsjail calls execve(2)
// directly, so there is no automatic PATH search.
func resolveCommandInChroot(command, chroot string) string {
	if strings.HasPrefix(command, "/") || strings.Contains(command, "/") {
		return command
	}
	searchDirs := []string{
		"/usr/local/sbin", "/usr/local/bin",
		"/usr/sbin", "/usr/bin",
		"/sbin", "/bin",
	}
	for _, dir := range searchDirs {
		if _, err := os.Stat(chroot + dir + "/" + command); err == nil {
			return dir + "/" + command
		}
	}
	return command
}

func isUnderPath(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && !strings.HasPrefix(rel, "..")
}

func copyExec(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
