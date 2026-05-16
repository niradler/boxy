package api

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

var K8sNameRegex = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)

const (
	LabelSessionID = "boxy.dev/session-id"
	LabelOwner     = "boxy.dev/owner"
)

func ValidateExecRequest(
	r *ExecRequestBody,
	maxTimeoutSec int,
	maxArgs int,
	maxEnvKeys int,
) error {
	if r == nil {
		return fmt.Errorf("request is nil")
	}
	if strings.TrimSpace(r.SandboxID) == "" {
		return fmt.Errorf("sandboxId is required")
	}
	if strings.TrimSpace(r.Command) == "" {
		return fmt.Errorf("command is required")
	}
	if maxArgs > 0 && len(r.Args) > maxArgs {
		return fmt.Errorf("too many args: max %d", maxArgs)
	}
	if maxEnvKeys > 0 && r.Env != nil && len(r.Env) > maxEnvKeys {
		return fmt.Errorf("too many env keys: max %d", maxEnvKeys)
	}
	for k := range r.Env {
		if strings.Contains(k, "=") || strings.TrimSpace(k) == "" {
			return fmt.Errorf("invalid env key: %q", k)
		}
		if blockedSandboxEnvKey(k) {
			return fmt.Errorf("exec env key not allowed: %q", k)
		}
	}
	if r.TimeoutSeconds <= 0 {
		return fmt.Errorf("timeoutSeconds must be positive")
	}
	if maxTimeoutSec > 0 && r.TimeoutSeconds > maxTimeoutSec {
		return fmt.Errorf("timeoutSeconds exceeds max %d", maxTimeoutSec)
	}
	return nil
}

func ValidateSandboxCreate(b *SandboxCreateBody, maxTTL int) error {
	if b == nil {
		return fmt.Errorf("request is nil")
	}
	if strings.TrimSpace(b.SandboxID) == "" {
		return fmt.Errorf("sandboxId is required")
	}
	if len(b.SandboxID) > 253 {
		return fmt.Errorf("sandboxId too long")
	}
	if !K8sNameRegex.MatchString(b.SandboxID) {
		return fmt.Errorf("sandboxId must be a valid K8s name (lowercase alphanumeric, '-', '.')")
	}
	if b.TTLSeconds < 0 {
		return fmt.Errorf("ttlSeconds must be non-negative")
	}
	if maxTTL > 0 && b.TTLSeconds > maxTTL {
		return fmt.Errorf("ttlSeconds exceeds max %d", maxTTL)
	}
	if b.Env != nil {
		if len(b.Env) > 64 {
			return fmt.Errorf("too many sandbox env keys")
		}
		for k, v := range b.Env {
			if strings.TrimSpace(k) == "" || strings.Contains(k, "=") {
				return fmt.Errorf("invalid sandbox env key")
			}
			if blockedSandboxEnvKey(k) {
				return fmt.Errorf("sandbox env key not allowed")
			}
			if utf8.RuneCountInString(v) > 16384 {
				return fmt.Errorf("sandbox env value too long")
			}
		}
	}
	return nil
}

func ValidateSessionCreate(b *SessionCreateBody) error {
	if b == nil {
		return fmt.Errorf("request is nil")
	}
	if strings.TrimSpace(b.SandboxID) == "" {
		return fmt.Errorf("sandboxId is required")
	}
	if b.SessionID != "" {
		if len(b.SessionID) > 253 {
			return fmt.Errorf("sessionId too long")
		}
		if !K8sNameRegex.MatchString(b.SessionID) {
			return fmt.Errorf("sessionId must be a valid K8s name (lowercase alphanumeric, '-', '.')")
		}
	}
	return nil
}

func blockedSandboxEnvKey(k string) bool {
	lk := strings.ToUpper(strings.TrimSpace(k))
	if strings.HasPrefix(lk, "KUBERNETES_") {
		return true
	}
	if strings.HasPrefix(lk, "BOXY_") {
		return true
	}
	return false
}
