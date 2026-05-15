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
	b := &SandboxCreateBody{SessionID: "s", SandboxID: "x", Owner: "o", TTLSeconds: 10}
	if err := ValidateSandboxCreate(b, 100); err != nil {
		t.Fatal(err)
	}
	b.TTLSeconds = 200
	if err := ValidateSandboxCreate(b, 100); err == nil {
		t.Fatal("expected error")
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
		SessionID: "s", SandboxID: "x", Owner: "o",
		Env: map[string]string{"BOXY_X": "1"},
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
