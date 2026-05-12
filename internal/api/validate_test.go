package api

import (
	"strings"
	"testing"
)

func TestValidateExecRequest(t *testing.T) {
	r := &ExecRequestBody{
		SessionID:      "s1",
		SandboxID:      "b1",
		PodRef:         PodRef{Namespace: "ns", Name: "p", UID: "uid"},
		Command:        "echo",
		Args:           []string{"hi"},
		Env:            map[string]string{"A": "1"},
		TimeoutSeconds: 10,
		Mode:           ExecModeAPI,
	}
	if err := ValidateExecRequest(r, 60, 256, 64); err != nil {
		t.Fatal(err)
	}
	r.PodRef = PodRef{}
	if err := ValidateExecRequest(r, 60, 256, 64); err != nil {
		t.Fatal(err)
	}
	r.PodRef = PodRef{Namespace: "ns", Name: "p", UID: ""}
	if err := ValidateExecRequest(r, 60, 256, 64); err == nil {
		t.Fatal("expected error for partial podRef")
	}
	r.PodRef = PodRef{Namespace: "ns", Name: "p", UID: "uid"}
	r.TimeoutSeconds = 0
	if err := ValidateExecRequest(r, 60, 256, 64); err == nil {
		t.Fatal("expected error")
	}
	r.TimeoutSeconds = 10
	r.Mode = "bad"
	if err := ValidateExecRequest(r, 60, 256, 64); err == nil {
		t.Fatal("expected error")
	}
}

func TestValidatePodRef(t *testing.T) {
	if err := ValidatePodRef(&PodRef{Namespace: "n", Name: "p", UID: "u"}); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePodRef(&PodRef{Namespace: "", Name: "p", UID: "u"}); err == nil {
		t.Fatal("expected error")
	}
}

func TestValidateSandboxCreate(t *testing.T) {
	b := &SandboxCreateBody{SessionID: "s", SandboxID: "x", Owner: "o", TTLSeconds: 10}
	if err := ValidateSandboxCreate(b, 100, 1000); err != nil {
		t.Fatal(err)
	}
	b.TTLSeconds = 200
	if err := ValidateSandboxCreate(b, 100, 1000); err == nil {
		t.Fatal("expected error")
	}
}

func TestValidateExecEnvKey(t *testing.T) {
	r := &ExecRequestBody{
		SessionID: "s", SandboxID: "b",
		PodRef:  PodRef{Namespace: "ns", Name: "p", UID: "u"},
		Command: "x", TimeoutSeconds: 1, Mode: ExecModePod,
		Env: map[string]string{"bad=key": "v"},
	}
	if err := ValidateExecRequest(r, 10, 10, 10); err == nil || !strings.Contains(err.Error(), "env") {
		t.Fatalf("got %v", err)
	}
}

func provisionLimitsDefaults() SandboxProvisionLimits {
	return SandboxProvisionLimits{
		MaxEnvKeys:         32,
		MaxLabels:          16,
		MaxAnnotations:     32,
		MaxImageRefLen:     256,
		MinWorkerPort:      1,
		MaxWorkerPort:      65535,
		AllowedServiceAcct: map[string]struct{}{"boxy-worker": {}},
		AllowedPullSecrets: map[string]struct{}{"reg": {}},
		DefaultServiceAcct: "boxy-worker",
		GlobalPullSecret:   "reg",
	}
}

func TestValidateSandboxProvisioningBlockedEnv(t *testing.T) {
	b := &SandboxCreateBody{
		SessionID: "s", SandboxID: "x", Owner: "o",
		Env: map[string]string{"BOXY_X": "1"},
	}
	if err := ValidateSandboxProvisioning(b, 100, 1000, provisionLimitsDefaults()); err == nil {
		t.Fatal("expected error")
	}
}

func TestValidateSandboxProvisioningReservedLabel(t *testing.T) {
	b := &SandboxCreateBody{
		SessionID: "s", SandboxID: "x", Owner: "o",
		Labels: map[string]string{"boxy.dev/x": "y"},
	}
	if err := ValidateSandboxProvisioning(b, 100, 1000, provisionLimitsDefaults()); err == nil {
		t.Fatal("expected error")
	}
}

func TestValidateSandboxProvisioningPullSecret(t *testing.T) {
	lim := provisionLimitsDefaults()
	lim.GlobalPullSecret = "reg"
	lim.AllowedPullSecrets = map[string]struct{}{"reg": {}}
	b := &SandboxCreateBody{
		SessionID: "s", SandboxID: "x", Owner: "o",
		ImagePullSecretName: "other",
	}
	if err := ValidateSandboxProvisioning(b, 100, 1000, lim); err == nil {
		t.Fatal("expected error")
	}
}

func TestValidateSandboxProvisioningCPU(t *testing.T) {
	lim := provisionLimitsDefaults()
	lim.MaxCPU = "1"
	b := &SandboxCreateBody{
		SessionID: "s", SandboxID: "x", Owner: "o",
		Resources: &SandboxResources{CPULimit: "4"},
	}
	if err := ValidateSandboxProvisioning(b, 100, 1000, lim); err == nil {
		t.Fatal("expected error")
	}
}
