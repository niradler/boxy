package nsjail

import (
	"bytes"
	"context"
	"encoding/json"
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
	Exec(ctx context.Context, sandboxID string, command string, args []string, env map[string]string, timeoutSecs int, pty bool) (*api.ExecResponseBody, error)
	ExecStream(ctx context.Context, sandboxID string, command string, args []string, env map[string]string, timeoutSecs int, onEvent func(string, string)) (*api.ExecResponseBody, error)
	ReadFile(ctx context.Context, sandboxID, path string) (data []byte, truncated bool, err error)
	WriteFile(ctx context.Context, sandboxID, path, content string) (int, error)
	EditFile(ctx context.Context, sandboxID, path, oldStr, newStr string, replaceAll bool) (int, error)
	Delete(ctx context.Context, sandboxID string) error
	ListIDs() []string
	Count() int
}

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
	MaxOutputBytes int
}

type sandbox struct {
	req       api.SandboxCreateBody
	workspace string
	binDir    string
}

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

	sb := &sandbox{req: *req, workspace: workspace, binDir: binDir}

	if req.SetupScript != "" {
		a.mu.Unlock()
		err := a.runHookScript(ctx, req.SetupScript, sb)
		a.mu.Lock()
		if err != nil {
			_ = os.RemoveAll(sandboxBase)
			return errInternal(fmt.Sprintf("setup script failed: %v", err))
		}
	}

	a.sandboxes[req.SandboxID] = sb
	return nil
}

func (a *NsjailAdapter) Exec(
	ctx context.Context,
	sandboxID string,
	command string,
	args []string,
	env map[string]string,
	timeoutSecs int,
	pty bool,
) (*api.ExecResponseBody, error) {
	return a.execWithStdin(ctx, sandboxID, command, args, env, timeoutSecs, pty, nil)
}

func (a *NsjailAdapter) execWithStdin(
	ctx context.Context,
	sandboxID string,
	command string,
	args []string,
	env map[string]string,
	timeoutSecs int,
	pty bool,
	stdin []byte,
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

	deadline, cancel := a.execContext(ctx, timeoutSecs)
	defer cancel()

	nsjailCmd := exec.CommandContext(deadline, a.cfg.NsjailPath, cmdArgs...)
	if stdin != nil {
		nsjailCmd.Stdin = bytes.NewReader(stdin)
	}

	if pty {
		return a.execPTY(deadline, nsjailCmd, timeoutSecs)
	}

	var stdout, stderr limitWriter
	if a.cfg.MaxOutputBytes > 0 {
		stdout.limit = int64(a.cfg.MaxOutputBytes)
		stderr.limit = int64(a.cfg.MaxOutputBytes)
	}
	nsjailCmd.Stdout = &stdout
	nsjailCmd.Stderr = &stderr

	runErr := nsjailCmd.Run()
	return a.buildExecResponse(runErr, deadline, timeoutSecs, stdout.String(), stderr.String())
}

func (a *NsjailAdapter) ExecStream(
	ctx context.Context,
	sandboxID string,
	command string,
	args []string,
	env map[string]string,
	timeoutSecs int,
	onEvent func(string, string),
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

	deadline, cancel := a.execContext(ctx, timeoutSecs)
	defer cancel()

	nsjailCmd := exec.CommandContext(deadline, a.cfg.NsjailPath, cmdArgs...)

	stdoutPipe, err := nsjailCmd.StdoutPipe()
	if err != nil {
		return nil, errInternal(fmt.Sprintf("stdout pipe: %v", err))
	}
	stderrPipe, err := nsjailCmd.StderrPipe()
	if err != nil {
		return nil, errInternal(fmt.Sprintf("stderr pipe: %v", err))
	}

	if err := nsjailCmd.Start(); err != nil {
		return nil, errInternal(fmt.Sprintf("nsjail start: %v", err))
	}

	maxBytes := int64(a.cfg.MaxOutputBytes)
	var (
		totalMu   sync.Mutex
		totalRead int64
		truncated bool
	)
	var stdoutBuf, stderrBuf limitWriter
	if maxBytes > 0 {
		stdoutBuf.limit = maxBytes
		stderrBuf.limit = maxBytes
	}

	drain := func(pipe io.ReadCloser, stream string, buf *limitWriter) {
		chunk := make([]byte, 4096)
		for {
			n, err := pipe.Read(chunk)
			if n > 0 {
				data := chunk[:n]

				totalMu.Lock()
				var toSend []byte
				var emitTruncated bool
				if maxBytes <= 0 {
					toSend = data
					totalRead += int64(len(data))
				} else if totalRead < maxBytes {
					remaining := maxBytes - totalRead
					if int64(len(data)) > remaining {
						toSend = data[:remaining]
					} else {
						toSend = data
					}
					totalRead += int64(len(toSend))
					if totalRead >= maxBytes && !truncated {
						truncated = true
						emitTruncated = true
					}
				}
				totalMu.Unlock()

				if len(toSend) > 0 {
					buf.Write(toSend) //nolint:errcheck
					onEvent(stream, string(toSend))
				}
				if emitTruncated {
					onEvent("truncated", "")
				}
			}
			if err != nil {
				break
			}
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); drain(stdoutPipe, "stdout", &stdoutBuf) }()
	go func() { defer wg.Done(); drain(stderrPipe, "stderr", &stderrBuf) }()

	waitErr := nsjailCmd.Wait()
	wg.Wait()

	return a.buildExecResponse(waitErr, deadline, timeoutSecs, stdoutBuf.String(), stderrBuf.String())
}

func (a *NsjailAdapter) execContext(ctx context.Context, timeoutSecs int) (context.Context, context.CancelFunc) {
	if timeoutSecs > 0 {
		return context.WithTimeout(ctx, time.Duration(timeoutSecs+5)*time.Second)
	}
	return context.WithCancel(ctx)
}

func (a *NsjailAdapter) buildExecResponse(
	runErr error,
	deadline context.Context,
	timeoutSecs int,
	stdout, stderr string,
) (*api.ExecResponseBody, error) {
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			// Exit code 137 (128+9, SIGKILL) is how nsjail propagates a time_limit kill.
			timedOut := timeoutSecs > 0 && exitErr.ExitCode() == 137
			return &api.ExecResponseBody{
				Stdout:   stdout,
				Stderr:   stderr,
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
		Stdout:   stdout,
		Stderr:   stderr,
		ExitCode: 0,
	}, nil
}

type limitWriter struct {
	buf   bytes.Buffer
	limit int64
}

func (lw *limitWriter) Write(p []byte) (int, error) {
	if lw.limit > 0 {
		remaining := lw.limit - int64(lw.buf.Len())
		if remaining <= 0 {
			return len(p), nil
		}
		if int64(len(p)) > remaining {
			p = p[:remaining]
		}
	}
	n, err := lw.buf.Write(p)
	return n, err
}

func (lw *limitWriter) String() string { return lw.buf.String() }

const fileOpTimeoutSecs = 30

func sandboxFilePath(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		p = strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/")
	}
	if strings.HasPrefix(p, "/") {
		return p
	}
	return "/workspace/" + p
}

func (a *NsjailAdapter) ReadFile(ctx context.Context, sandboxID, path string) (data []byte, truncated bool, err error) {
	res, err := a.Exec(ctx, sandboxID, "sh", []string{"-c", `cat -- "$1"`, "sh", sandboxFilePath(path)}, nil, fileOpTimeoutSecs, false)
	if err != nil {
		return nil, false, err
	}
	if res.ExitCode != 0 {
		return nil, false, errBadRequest(fileOpError("read", res.Stderr))
	}
	out := []byte(res.Stdout)
	truncated = a.cfg.MaxOutputBytes > 0 && len(out) >= a.cfg.MaxOutputBytes
	return out, truncated, nil
}

func (a *NsjailAdapter) WriteFile(ctx context.Context, sandboxID, path string, content string) (int, error) {
	res, err := a.execWithStdin(ctx, sandboxID, "sh",
		[]string{"-c", `mkdir -p -- "$(dirname -- "$1")" && cat > "$1"`, "sh", sandboxFilePath(path)},
		nil, fileOpTimeoutSecs, false, []byte(content))
	if err != nil {
		return 0, err
	}
	if res.ExitCode != 0 {
		return 0, errBadRequest(fileOpError("write", res.Stderr))
	}
	return len(content), nil
}

func (a *NsjailAdapter) EditFile(ctx context.Context, sandboxID, path, oldStr, newStr string, replaceAll bool) (int, error) {
	if oldStr == "" {
		return 0, errBadRequest("oldString must not be empty")
	}
	data, truncated, err := a.ReadFile(ctx, sandboxID, path)
	if err != nil {
		return 0, err
	}
	if truncated {
		return 0, errBadRequest(fmt.Sprintf("file exceeds the %d-byte read limit; cannot edit safely, use the bash tool", a.cfg.MaxOutputBytes))
	}
	content := string(data)
	count := strings.Count(content, oldStr)
	if count == 0 {
		return 0, errBadRequest("oldString not found in file")
	}
	if count > 1 && !replaceAll {
		return 0, errBadRequest(fmt.Sprintf("oldString is not unique (%d matches); add context or set replaceAll", count))
	}

	if replaceAll {
		content = strings.ReplaceAll(content, oldStr, newStr)
	} else {
		content = strings.Replace(content, oldStr, newStr, 1)
		count = 1
	}

	if _, err := a.WriteFile(ctx, sandboxID, path, content); err != nil {
		return 0, err
	}
	return count, nil
}

func fileOpError(op, stderr string) string {
	if s := strings.TrimSpace(stderr); s != "" {
		return s
	}
	return op + " failed"
}

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

	if sb.req.TeardownScript != "" {
		if err := a.runHookScript(ctx, sb.req.TeardownScript, sb); err != nil {
			slog.Warn("teardown script failed", "sandboxId", sandboxID, "err", err)
		}
	}

	base := filepath.Dir(sb.workspace)
	if err := os.RemoveAll(base); err != nil {
		slog.Warn("sandbox cleanup failed", "sandboxId", sandboxID, "path", base, "err", err)
	}
	return nil
}

func (a *NsjailAdapter) runHookScript(ctx context.Context, script string, sb *sandbox) error {
	configJSON, err := json.Marshal(sb.req)
	if err != nil {
		return fmt.Errorf("marshal sandbox config: %w", err)
	}

	cmd := exec.CommandContext(ctx, script)
	cmd.Stdin = bytes.NewReader(configJSON)
	cmd.Env = append(os.Environ(),
		"BOXY_SANDBOX_ID="+sb.req.SandboxID,
		"BOXY_SANDBOX_ROOT="+filepath.Dir(sb.workspace),
		"BOXY_WORKSPACE="+sb.workspace,
	)
	for k, v := range sb.req.ScriptEnv {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, stderr.String())
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

	// DisableCloneNewNet=true disables the clone_newnet flag, which grants internet access (sandbox inherits pod netns).
	if sb.req.Network != nil {
		net := sb.req.Network
		if net.Enabled != nil && !*net.Enabled {
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
		MountPt{Src: "/dev/null", Dst: "/dev/null", Rw: true, IsBind: true},
		MountPt{Src: "/dev/zero", Dst: "/dev/zero", IsBind: true},
		MountPt{Src: "/dev/urandom", Dst: "/dev/urandom", IsBind: true},
		MountPt{Src: "/dev/random", Dst: "/dev/random", IsBind: true},
	)
	if sb.req.Network != nil && sb.req.Network.AllowInternetAccess {
		cfg.Mounts = append(cfg.Mounts,
			MountPt{Src: "/etc/resolv.conf", Dst: "/etc/resolv.conf", IsBind: true},
			MountPt{Src: "/etc/ssl/certs/ca-certificates.crt", Dst: "/etc/ssl/certs/ca-certificates.crt", IsBind: true},
		)
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

	cfg.Mounts = append(cfg.Mounts, MountPt{
		Src:    sb.binDir,
		Dst:    "/usr/local/bin",
		Rw:     true,
		IsBind: true,
	})

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

	for k := range req.ScriptEnv {
		if strings.Contains(k, "=") {
			return errBadRequest(fmt.Sprintf("scriptEnv key %q must not contain '='", k))
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

// nsjail calls execve(2) directly with no PATH search, so relative commands must be resolved here.
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
