package api

import (
	"strings"
	"testing"
)

func TestValidateExecRequest(t *testing.T) {
	r := &ExecRequestBody{
		SessionID:      "s1",
		SandboxID:      "b1",
		Command:        "echo",
		Args:           []string{"hi"},
		Env:            map[string]string{"A": "1"},
		TimeoutSeconds: 10,
	}
	if err := ValidateExecRequest(r, 60, 256, 64); err != nil {
		t.Fatal(err)
	}
	r.TimeoutSeconds = 0
	if err := ValidateExecRequest(r, 60, 256, 64); err == nil {
		t.Fatal("expected error")
	}
}

func TestValidateSandboxCreate(t *testing.T) {
	b := &SandboxCreateBody{SandboxID: "x", TTLSeconds: 10}
	if err := ValidateSandboxCreate(b, 100); err != nil {
		t.Fatal(err)
	}
	b.TTLSeconds = 200
	if err := ValidateSandboxCreate(b, 100); err == nil {
		t.Fatal("expected error for ttl over max")
	}

	b2 := &SandboxCreateBody{SandboxID: strings.Repeat("x", 64), TTLSeconds: 10}
	if err := ValidateSandboxCreate(b2, 100); err == nil {
		t.Fatal("expected error for sandboxId > 63 chars")
	}

	b3 := &SandboxCreateBody{SandboxID: "UPPERCASE", TTLSeconds: 10}
	if err := ValidateSandboxCreate(b3, 100); err == nil {
		t.Fatal("expected error for invalid k8s name")
	}
}

func TestValidateExecEnvKey(t *testing.T) {
	r := &ExecRequestBody{
		SessionID: "s", SandboxID: "b",
		Command: "x", TimeoutSeconds: 1,
		Env: map[string]string{"bad=key": "v"},
	}
	if err := ValidateExecRequest(r, 10, 10, 10); err == nil || !strings.Contains(err.Error(), "env") {
		t.Fatalf("got %v", err)
	}
}

func TestValidateSandboxCreateBlockedEnv(t *testing.T) {
	b := &SandboxCreateBody{
		SandboxID: "x",
		Env:       map[string]string{"BOXY_X": "1"},
	}
	if err := ValidateSandboxCreate(b, 100); err == nil {
		t.Fatal("expected error")
	}
}

func TestValidateExecBlockedEnv(t *testing.T) {
	for _, key := range []string{"BOXY_ROUTER_TOKEN", "KUBERNETES_SERVICE_HOST"} {
		r := &ExecRequestBody{
			SessionID: "s", SandboxID: "b",
			Command: "x", TimeoutSeconds: 1,
			Env: map[string]string{key: "v"},
		}
		if err := ValidateExecRequest(r, 10, 10, 10); err == nil {
			t.Errorf("expected blocked env key %q to be rejected", key)
		}
	}
}

func TestValidateFilePath(t *testing.T) {
	valid := []string{"a.txt", "sub/dir/file", "/workspace/x", "/workspace", ".hidden", "/etc/hosts"}
	for _, p := range valid {
		if err := ValidateFilePath(p); err != nil {
			t.Errorf("ValidateFilePath(%q) = %v, want nil", p, err)
		}
	}

	invalid := []string{"", "   ", "a\x00b"}
	for _, p := range invalid {
		if err := ValidateFilePath(p); err == nil {
			t.Errorf("ValidateFilePath(%q) = nil, want error", p)
		}
	}

	if err := ValidateFilePath(strings.Repeat("a", maxFilePathLen+1)); err == nil {
		t.Error("expected error for over-long path")
	}
}

func TestValidateFileEncoding(t *testing.T) {
	for _, e := range []string{"", "utf-8", "utf8", "UTF-8", "base64", "BASE64"} {
		if err := ValidateFileEncoding(e); err != nil {
			t.Errorf("ValidateFileEncoding(%q) = %v, want nil", e, err)
		}
	}
	for _, e := range []string{"ascii", "hex", "binary"} {
		if err := ValidateFileEncoding(e); err == nil {
			t.Errorf("ValidateFileEncoding(%q) = nil, want error", e)
		}
	}
}
