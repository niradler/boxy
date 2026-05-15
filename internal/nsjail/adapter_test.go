package nsjail

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"boxy.dev/boxy/internal/api"
)

// helpers

func newTestAdapter(t *testing.T) (*NsjailAdapter, string) {
	t.Helper()
	root := t.TempDir()
	binsDir := t.TempDir()
	a := NewNsjailAdapter(AdapterConfig{
		NsjailPath:    "/usr/sbin/nsjail",
		DefaultRootfs: "/rootfs/ubuntu-24.04",
		SandboxRoot:   root,
		BinariesDir:   binsDir,
	})
	return a, binsDir
}

func writeFakeBinary(t *testing.T, dir, name string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho fake"), 0o755); err != nil {
		t.Fatalf("write fake binary %s: %v", name, err)
	}
}

func findMount(mounts []MountPt, dst string) (MountPt, bool) {
	for _, m := range mounts {
		if m.Dst == dst {
			return m, true
		}
	}
	return MountPt{}, false
}

// -----------------------------------------------------------------------
// buildNsjailConfig — mount correctness
// These tests encode invariants that were only caught by manual testing:
//   - resolv.conf must be injected when allowInternetAccess=true (DNS fix)
//   - /usr/local/bin must be a single per-sandbox bind-mount, not a
//     per-exec tmpfs (binary visibility + persistence fix)
// -----------------------------------------------------------------------

func makeSandbox(req api.SandboxCreateBody, workspace, binDir string) *sandbox {
	return &sandbox{req: req, workspace: workspace, binDir: binDir}
}

func TestBuildNsjailConfig_BinDirBindMount(t *testing.T) {
	a, _ := newTestAdapter(t)
	sb := makeSandbox(api.SandboxCreateBody{}, "/tmp/ws", "/tmp/bin")
	cfg := a.buildNsjailConfig(sb, a.cfg.DefaultRootfs, nil, 0)

	m, ok := findMount(cfg.Mounts, "/usr/local/bin")
	if !ok {
		t.Fatal("/usr/local/bin mount missing from nsjail config")
	}
	if !m.IsBind {
		t.Error("/usr/local/bin must be a bind-mount (per-sandbox dir), not a tmpfs")
	}
	if m.Src != "/tmp/bin" {
		t.Errorf("/usr/local/bin src = %q, want /tmp/bin", m.Src)
	}
	if !m.Rw {
		t.Error("/usr/local/bin bind-mount must be rw so writes persist across execs")
	}
}

func TestBuildNsjailConfig_NoBinDirTmpfs(t *testing.T) {
	// Regression: previous implementation mounted a per-exec tmpfs at
	// /usr/local/bin, then bind-mounted individual allowed binaries into it.
	// That left 0-byte mount-point artifacts in the shared rootfs, making
	// filenames visible to sandboxes that weren't granted those binaries.
	a, _ := newTestAdapter(t)
	sb := makeSandbox(api.SandboxCreateBody{
		AllowedBinaries: []string{"jq"},
	}, "/tmp/ws", "/tmp/bin")
	cfg := a.buildNsjailConfig(sb, a.cfg.DefaultRootfs, nil, 0)

	for _, m := range cfg.Mounts {
		if m.Dst == "/usr/local/bin" && m.Fstype == "tmpfs" {
			t.Error("found per-exec tmpfs at /usr/local/bin — use per-sandbox bind-mount instead")
		}
		if m.Dst == "/usr/local/bin/jq" {
			t.Error("found per-file bind-mount at /usr/local/bin/jq — binaries should be pre-copied into binDir")
		}
	}
}

func TestBuildNsjailConfig_ResolvConfInjected(t *testing.T) {
	// Regression: when allowInternetAccess=true the sandbox inherits the pod's
	// network namespace but the bare Ubuntu rootfs has an empty /etc/resolv.conf.
	// The bind-mount must be added so DNS resolution works inside the sandbox.
	a, _ := newTestAdapter(t)
	enabled := true
	sb := makeSandbox(api.SandboxCreateBody{
		Network: &api.SandboxNetworkConfig{
			Enabled:             &enabled,
			AllowInternetAccess: true,
		},
	}, "/tmp/ws", "/tmp/bin")
	cfg := a.buildNsjailConfig(sb, a.cfg.DefaultRootfs, nil, 0)

	m, ok := findMount(cfg.Mounts, "/etc/resolv.conf")
	if !ok {
		t.Fatal("/etc/resolv.conf not bind-mounted when allowInternetAccess=true — DNS will fail inside sandbox")
	}
	if m.Src != "/etc/resolv.conf" {
		t.Errorf("resolv.conf src = %q, want /etc/resolv.conf", m.Src)
	}
}

func TestBuildNsjailConfig_NoResolvConfWhenNetworkDisabled(t *testing.T) {
	a, _ := newTestAdapter(t)
	sb := makeSandbox(api.SandboxCreateBody{}, "/tmp/ws", "/tmp/bin")
	cfg := a.buildNsjailConfig(sb, a.cfg.DefaultRootfs, nil, 0)

	if _, ok := findMount(cfg.Mounts, "/etc/resolv.conf"); ok {
		t.Error("/etc/resolv.conf should not be mounted when allowInternetAccess is not set")
	}
}

func TestBuildNsjailConfig_WorkspaceMount(t *testing.T) {
	a, _ := newTestAdapter(t)
	sb := makeSandbox(api.SandboxCreateBody{}, "/var/lib/boxy/sandboxes/abc/workspace", "/var/lib/boxy/sandboxes/abc/bin")
	cfg := a.buildNsjailConfig(sb, a.cfg.DefaultRootfs, nil, 0)

	m, ok := findMount(cfg.Mounts, "/workspace")
	if !ok {
		t.Fatal("/workspace mount missing")
	}
	if m.Src != "/var/lib/boxy/sandboxes/abc/workspace" {
		t.Errorf("workspace src = %q", m.Src)
	}
	if !m.Rw || !m.IsBind {
		t.Error("/workspace must be rw bind-mount")
	}
}

func TestBuildNsjailConfig_NetworkIsolatedByDefault(t *testing.T) {
	a, _ := newTestAdapter(t)
	sb := makeSandbox(api.SandboxCreateBody{}, "/tmp/ws", "/tmp/bin")
	cfg := a.buildNsjailConfig(sb, a.cfg.DefaultRootfs, nil, 0)

	if !cfg.DisableCloneNewNet {
		t.Error("DisableCloneNewNet must be true by default (sandbox must not inherit pod netns)")
	}
}

func TestBuildNsjailConfig_InternetEnablesNetNS(t *testing.T) {
	a, _ := newTestAdapter(t)
	sb := makeSandbox(api.SandboxCreateBody{
		Network: &api.SandboxNetworkConfig{AllowInternetAccess: true},
	}, "/tmp/ws", "/tmp/bin")
	cfg := a.buildNsjailConfig(sb, a.cfg.DefaultRootfs, nil, 0)

	if cfg.DisableCloneNewNet {
		t.Error("DisableCloneNewNet must be false when allowInternetAccess=true (sandbox needs pod netns)")
	}
}

// -----------------------------------------------------------------------
// Create — binDir setup
// -----------------------------------------------------------------------

func TestCreate_BinDirCreated(t *testing.T) {
	a, binsDir := newTestAdapter(t)
	writeFakeBinary(t, binsDir, "jq")
	writeFakeBinary(t, binsDir, "yq")

	req := &api.SandboxCreateBody{
		SandboxID:       "test-sb-1",
		AllowedBinaries: []string{"jq"},
	}
	if err := a.Create(context.TODO(), req); err != nil {
		t.Fatalf("Create: %v", err)
	}

	sb := a.sandboxes["test-sb-1"]
	if sb == nil {
		t.Fatal("sandbox not registered")
	}

	// binDir must exist and contain jq but not yq.
	if _, err := os.Stat(sb.binDir); err != nil {
		t.Fatalf("binDir does not exist: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sb.binDir, "jq")); err != nil {
		t.Error("jq not copied to binDir")
	}
	if _, err := os.Stat(filepath.Join(sb.binDir, "yq")); err == nil {
		t.Error("yq should not be in binDir (not in AllowedBinaries)")
	}
}

func TestCreate_MissingBinaryFails(t *testing.T) {
	a, _ := newTestAdapter(t)
	// "nonexistent" is not in binsDir — Create should fail, not defer the error to Exec.
	req := &api.SandboxCreateBody{
		SandboxID:       "test-sb-missing",
		AllowedBinaries: []string{"nonexistent"},
	}
	if err := a.Create(context.TODO(), req); err == nil {
		t.Fatal("Create with nonexistent binary should return an error")
	}
}

func TestCreate_EmptyAllowedBinaries(t *testing.T) {
	a, _ := newTestAdapter(t)
	req := &api.SandboxCreateBody{
		SandboxID:       "test-sb-empty",
		AllowedBinaries: []string{},
	}
	if err := a.Create(context.TODO(), req); err != nil {
		t.Fatalf("Create with empty allowedBinaries should succeed: %v", err)
	}

	sb := a.sandboxes["test-sb-empty"]
	entries, _ := os.ReadDir(sb.binDir)
	if len(entries) != 0 {
		t.Errorf("binDir should be empty, got %d entries", len(entries))
	}
}

func TestCreate_Conflict(t *testing.T) {
	a, _ := newTestAdapter(t)
	req := &api.SandboxCreateBody{SandboxID: "dup"}

	if err := a.Create(context.TODO(), req); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	err := a.Create(context.TODO(), req)
	if err == nil {
		t.Fatal("second Create with same ID should fail")
	}
	ae, ok := err.(*AdapterError)
	if !ok || ae.Code != 409 {
		t.Errorf("expected 409 AdapterError, got %v", err)
	}
}

func TestDelete_CleansUpBinDir(t *testing.T) {
	a, binsDir := newTestAdapter(t)
	writeFakeBinary(t, binsDir, "jq")

	req := &api.SandboxCreateBody{SandboxID: "del-test", AllowedBinaries: []string{"jq"}}
	if err := a.Create(context.TODO(), req); err != nil {
		t.Fatalf("Create: %v", err)
	}
	sandboxBase := filepath.Dir(a.sandboxes["del-test"].workspace)

	if err := a.Delete(context.TODO(), "del-test"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(sandboxBase); err == nil {
		t.Error("sandbox directory should be removed after Delete")
	}
}

func TestDelete_NotFound(t *testing.T) {
	a, _ := newTestAdapter(t)
	err := a.Delete(context.TODO(), "no-such-sandbox")
	ae, ok := err.(*AdapterError)
	if !ok || ae.Code != 404 {
		t.Errorf("expected 404 AdapterError, got %v", err)
	}
}

// -----------------------------------------------------------------------
// validateCreateRequest
// -----------------------------------------------------------------------

func TestValidateCreateRequest_RootUserBlocked(t *testing.T) {
	a, _ := newTestAdapter(t)
	for _, user := range []string{"root", "0"} {
		req := &api.SandboxCreateBody{VM: &api.VMConfig{User: user}}
		if err := a.validateCreateRequest(req); err == nil {
			t.Errorf("user %q should be rejected", user)
		}
	}
}

func TestValidateCreateRequest_AllowedBinaryTraversal(t *testing.T) {
	a, _ := newTestAdapter(t)
	for _, bin := range []string{"../etc/passwd", "sub/dir", "", "bin/sh"} {
		req := &api.SandboxCreateBody{AllowedBinaries: []string{bin}}
		if err := a.validateCreateRequest(req); err == nil {
			t.Errorf("binary %q should be rejected", bin)
		}
	}
}

func TestValidateCreateRequest_ValidBinaryName(t *testing.T) {
	a, _ := newTestAdapter(t)
	req := &api.SandboxCreateBody{AllowedBinaries: []string{"jq", "yq", "python3"}}
	if err := a.validateCreateRequest(req); err != nil {
		t.Errorf("valid binary names should pass: %v", err)
	}
}

// -----------------------------------------------------------------------
// resolveCommandInChroot
// -----------------------------------------------------------------------

func TestResolveCommandInChroot_AbsolutePassthrough(t *testing.T) {
	for _, cmd := range []string{"/usr/bin/python3", "/bin/sh", "/usr/local/bin/jq"} {
		if got := resolveCommandInChroot(cmd, "/rootfs"); got != cmd {
			t.Errorf("resolveCommandInChroot(%q) = %q, want passthrough", cmd, got)
		}
	}
}

func TestResolveCommandInChroot_RelativeWithSlash(t *testing.T) {
	cmd := "sub/command"
	if got := resolveCommandInChroot(cmd, "/rootfs"); got != cmd {
		t.Errorf("path-containing command should pass through: got %q", got)
	}
}

func TestResolveCommandInChroot_FoundInChroot(t *testing.T) {
	chroot := t.TempDir()
	binDir := filepath.Join(chroot, "usr", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "mybin"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := resolveCommandInChroot("mybin", chroot)
	if got != "/usr/bin/mybin" {
		t.Errorf("resolveCommandInChroot = %q, want /usr/bin/mybin", got)
	}
}

func TestResolveCommandInChroot_NotFoundReturnsAsIs(t *testing.T) {
	chroot := t.TempDir()
	got := resolveCommandInChroot("notexist", chroot)
	if got != "notexist" {
		t.Errorf("missing command should be returned as-is, got %q", got)
	}
}
