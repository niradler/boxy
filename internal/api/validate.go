package api

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

var K8sNameRegex = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)
var K8sLabelValueRegex = regexp.MustCompile(`^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$`)

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
	if err := ValidateSandboxID(r.SandboxID); err != nil {
		return err
	}
	if strings.TrimSpace(r.SessionID) != "" {
		if err := ValidateSessionID(r.SessionID); err != nil {
			return err
		}
	}
	if err := validateOptionalLabelValue("owner", r.Owner); err != nil {
		return err
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
	if err := ValidateSandboxID(b.SandboxID); err != nil {
		return err
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
	if err := ValidateSandboxID(b.SandboxID); err != nil {
		return err
	}
	if b.SessionID != "" {
		if err := ValidateSessionID(b.SessionID); err != nil {
			return err
		}
	}
	if err := validateOptionalLabelValue("owner", b.Owner); err != nil {
		return err
	}
	return nil
}

const maxFilePathLen = 4096

func ValidateFilePath(path string) error {
	p := strings.TrimSpace(path)
	if p == "" {
		return fmt.Errorf("path is required")
	}
	if len(p) > maxFilePathLen {
		return fmt.Errorf("path too long: max %d characters", maxFilePathLen)
	}
	if strings.ContainsRune(p, 0) {
		return fmt.Errorf("path must not contain NUL")
	}
	return nil
}

func ValidateSandboxID(id string) error {
	return validateLabelBackedName("sandboxId", id)
}

func ValidateSessionID(id string) error {
	return validateLabelBackedName("sessionId", id)
}

func ValidateOwner(owner string) error {
	return validateOptionalLabelValue("owner", owner)
}

func validateLabelBackedName(field, value string) error {
	if len(value) > 63 {
		return fmt.Errorf("%s too long: max 63 characters", field)
	}
	if !K8sNameRegex.MatchString(value) {
		return fmt.Errorf("%s must be a valid K8s label-backed name (lowercase alphanumeric, '-', '.', max 63 chars)", field)
	}
	return nil
}

func validateOptionalLabelValue(field, value string) error {
	if value == "" {
		return nil
	}
	if len(value) > 63 {
		return fmt.Errorf("%s too long: max 63 characters", field)
	}
	if !K8sLabelValueRegex.MatchString(value) {
		return fmt.Errorf("%s must be a valid K8s label value", field)
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
