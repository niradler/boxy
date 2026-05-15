package nsjail

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

// Adapter is the sandbox lifecycle interface implemented by NsjailAdapter.
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

// AdapterConfig holds paths injected at startup (mirrors the Rust Config fields).
type AdapterConfig struct {
	NsjailPath    string
	DefaultRootfs string
	SandboxRoot   string
	BinariesDir   string
}

type sandbox struct {
	req       api.SandboxCreateBody
	workspace string // absolute path to the per-sandbox R/W workspace
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

// Create validates the request, creates the per-sandbox workspace directory, and
// registers the sandbox in memory.
func (a *NsjailAdapter) Create(ctx context.Context, req *api.SandboxCreateBody) error {
	if err := a.validateCreateRequest(req); err != nil {
		return err
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if _, exists := a.sandboxes[req.SandboxID]; exists {
		return errConflict(req.SandboxID)
	}

	workspace := filepath.Join(a.cfg.SandboxRoot, req.SandboxID, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		return errInternal(fmt.Sprintf("mkdir workspace %s: %v", workspace, err))
	}

	a.sandboxes[req.SandboxID] = &sandbox{req: *req, workspace: workspace}
	return nil
}

// Exec runs a command inside the named sandbox via nsjail.
// Each call writes a fresh nsjail config file, invokes nsjail, then removes the file.
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
	var stdout, stderr bytes.Buffer
	nsjailCmd.Stdout = &stdout
	nsjailCmd.Stderr = &stderr

	runErr := nsjailCmd.Run()
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return &api.ExecResponseBody{
				Stdout:   stdout.String(),
				Stderr:   stderr.String(),
				ExitCode: exitErr.ExitCode(),
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

// Delete removes the sandbox workspace and deregisters the sandbox.
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

// ListIDs returns the IDs of all live sandboxes.
func (a *NsjailAdapter) ListIDs() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	ids := make([]string, 0, len(a.sandboxes))
	for id := range a.sandboxes {
		ids = append(ids, id)
	}
	return ids
}

// Count returns the number of live sandboxes.
func (a *NsjailAdapter) Count() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.sandboxes)
}

// buildNsjailConfig constructs a NsjailConfig for a single exec invocation.
// It merges sandbox-level settings with per-exec env and timeout.
func (a *NsjailAdapter) buildNsjailConfig(sb *sandbox, rootfs string, execEnv map[string]string, timeoutSecs int) *NsjailConfig {
	cfg := &NsjailConfig{
		Mode:                ModeOnce,
		Log:                 "/dev/null",
		Chroot:              rootfs,
		DisableCloneNewUser: true,
		DisableCloneNewNet:  true,
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

	// Network: allow internet access by not isolating the net namespace.
	if sb.req.Network != nil {
		net := sb.req.Network
		if net.Enabled != nil && !*net.Enabled {
			// network explicitly disabled: keep DisableCloneNewNet = true
		} else if net.AllowInternetAccess {
			cfg.DisableCloneNewNet = false
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

	// Standard mounts.
	cfg.Mounts = append(cfg.Mounts,
		MountPt{Src: sb.workspace, Dst: "/workspace", Rw: true, IsBind: true},
		MountPt{Dst: "/tmp", Fstype: "tmpfs", Rw: true},
	)

	// Sandbox volumes.
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

	// Allowed binaries: bind-mounted read-only from the host binaries dir.
	for _, bin := range sb.req.AllowedBinaries {
		cfg.Mounts = append(cfg.Mounts, MountPt{
			Src:    filepath.Join(a.cfg.BinariesDir, bin),
			Dst:    "/usr/local/bin/" + bin,
			IsBind: true,
		})
	}

	// Env vars: default PATH, then sandbox-level, then per-exec overrides.
	cfg.Envar = append(cfg.Envar, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	for k, v := range sb.req.Env {
		cfg.Envar = append(cfg.Envar, k+"="+v)
	}
	for k, v := range execEnv {
		cfg.Envar = append(cfg.Envar, k+"="+v)
	}

	return cfg
}

// validateCreateRequest enforces the same input constraints as the Rust implementation.
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
		if vol.HostPath != "" {
			if strings.Contains(vol.HostPath, "..") || !isUnderPath(vol.HostPath, a.cfg.SandboxRoot) {
				return errBadRequest(fmt.Sprintf("volume host_path must be under %s", a.cfg.SandboxRoot))
			}
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

// isUnderPath reports whether child is the same as or a subdirectory of parent.
func isUnderPath(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && !strings.HasPrefix(rel, "..")
}
