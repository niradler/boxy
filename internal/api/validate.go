package api

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/api/resource"
)

const (
	LabelSandboxID     = "boxy.dev/sandbox-id"
	LabelSessionID     = "boxy.dev/session-id"
	LabelOwner         = "boxy.dev/owner"
	LabelManagedBy     = "boxy.dev/managed-by"
	LabelWorkerRuntime = "boxy.dev/worker-runtime"

	SandboxRuntimeInstrumented = "instrumented"
	WorkerExecAPIPath          = "/v1/exec"

	AnnotationCreatedAt        = "boxy.dev/created-at"
	AnnotationMaxLifetimeSec   = "boxy.dev/max-lifetime-seconds"
	AnnotationTTLSeconds       = "boxy.dev/ttl-seconds"
	AnnotationExpiresAtRFC3339 = "boxy.dev/expires-at"
	AnnotationProvisionedImage = "boxy.dev/provisioned-image"
)

func PodRefEmpty(p *PodRef) bool {
	if p == nil {
		return true
	}
	return strings.TrimSpace(p.Namespace) == "" &&
		strings.TrimSpace(p.Name) == "" &&
		strings.TrimSpace(p.UID) == ""
}

func podRefPartial(p *PodRef) bool {
	if p == nil || PodRefEmpty(p) {
		return false
	}
	n := 0
	if strings.TrimSpace(p.Namespace) != "" {
		n++
	}
	if strings.TrimSpace(p.Name) != "" {
		n++
	}
	if strings.TrimSpace(p.UID) != "" {
		n++
	}
	return n != 0 && n != 3
}

func ValidateExecRequest(
	r *ExecRequestBody,
	maxTimeoutSec int,
	maxArgs int,
	maxEnvKeys int,
) error {
	if r == nil {
		return fmt.Errorf("request is nil")
	}
	if strings.TrimSpace(r.SessionID) == "" {
		return fmt.Errorf("sessionId is required")
	}
	if strings.TrimSpace(r.SandboxID) == "" {
		return fmt.Errorf("sandboxId is required")
	}
	if podRefPartial(&r.PodRef) {
		return fmt.Errorf("podRef must include namespace, name, and uid together, or omit all three for lookup")
	}
	if !PodRefEmpty(&r.PodRef) {
		if err := ValidatePodRef(&r.PodRef); err != nil {
			return err
		}
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
	}
	if r.TimeoutSeconds <= 0 {
		return fmt.Errorf("timeoutSeconds must be positive")
	}
	if maxTimeoutSec > 0 && r.TimeoutSeconds > maxTimeoutSec {
		return fmt.Errorf("timeoutSeconds exceeds max %d", maxTimeoutSec)
	}
	switch r.Mode {
	case ExecModeAPI, ExecModePod:
	default:
		return fmt.Errorf("mode must be api_exec or pod_exec")
	}
	return nil
}

func ValidatePodRef(p *PodRef) error {
	if p == nil {
		return fmt.Errorf("podRef is required")
	}
	if strings.TrimSpace(p.Namespace) == "" {
		return fmt.Errorf("podRef.namespace is required")
	}
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("podRef.name is required")
	}
	if strings.TrimSpace(p.UID) == "" {
		return fmt.Errorf("podRef.uid is required")
	}
	return nil
}

func ValidateSandboxCreate(b *SandboxCreateBody, maxTTL, maxLife int) error {
	if b == nil {
		return fmt.Errorf("request is nil")
	}
	if strings.TrimSpace(b.SessionID) == "" {
		return fmt.Errorf("sessionId is required")
	}
	if strings.TrimSpace(b.SandboxID) == "" {
		return fmt.Errorf("sandboxId is required")
	}
	if strings.TrimSpace(b.Owner) == "" {
		return fmt.Errorf("owner is required")
	}
	if b.TTLSeconds < 0 || b.MaxLifetimeSec < 0 {
		return fmt.Errorf("ttl and max lifetime must be non-negative")
	}
	if maxTTL > 0 && b.TTLSeconds > maxTTL {
		return fmt.Errorf("ttlSeconds exceeds max %d", maxTTL)
	}
	if maxLife > 0 && b.MaxLifetimeSec > maxLife {
		return fmt.Errorf("maxLifetimeSeconds exceeds max %d", maxLife)
	}
	return nil
}

func ReservedMetadataKey(k string) bool {
	lk := strings.ToLower(strings.TrimSpace(k))
	return strings.HasPrefix(lk, "kubernetes.io/") || strings.HasPrefix(lk, "k8s.io/") || strings.HasPrefix(lk, "boxy.dev/")
}

func ValidateSandboxProvisioning(b *SandboxCreateBody, maxTTL, maxLife int, lim SandboxProvisionLimits) error {
	if err := ValidateSandboxCreate(b, maxTTL, maxLife); err != nil {
		return err
	}
	if strings.TrimSpace(b.Image) != "" {
		if err := validateImageRef(b.Image, lim.MaxImageRefLen); err != nil {
			return err
		}
	}
	if b.WorkerPort != nil {
		p := *b.WorkerPort
		if lim.MinWorkerPort > 0 && p < lim.MinWorkerPort {
			return fmt.Errorf("workerPort below minimum %d", lim.MinWorkerPort)
		}
		if lim.MaxWorkerPort > 0 && p > lim.MaxWorkerPort {
			return fmt.Errorf("workerPort above maximum %d", lim.MaxWorkerPort)
		}
	}
	if pol := strings.TrimSpace(b.ImagePullPolicy); pol != "" {
		switch pol {
		case "Always", "Never", "IfNotPresent":
		default:
			return fmt.Errorf("invalid imagePullPolicy")
		}
	}
	if sa := strings.TrimSpace(b.ServiceAccountName); sa != "" {
		if _, ok := lim.AllowedServiceAcct[sa]; !ok {
			return fmt.Errorf("serviceAccountName not allowed")
		}
	}
	if ps := strings.TrimSpace(b.ImagePullSecretName); ps != "" {
		if len(lim.AllowedPullSecrets) == 0 {
			if lim.GlobalPullSecret == "" || ps != lim.GlobalPullSecret {
				return fmt.Errorf("imagePullSecretName not allowed")
			}
		} else if _, ok := lim.AllowedPullSecrets[ps]; !ok {
			return fmt.Errorf("imagePullSecretName not allowed")
		}
	}
	if b.Env != nil {
		if lim.MaxEnvKeys > 0 && len(b.Env) > lim.MaxEnvKeys {
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
	if b.Labels != nil {
		if lim.MaxLabels > 0 && len(b.Labels) > lim.MaxLabels {
			return fmt.Errorf("too many sandbox labels")
		}
		for k, v := range b.Labels {
			if ReservedMetadataKey(k) {
				return fmt.Errorf("reserved label key")
			}
			if !wellFormedMetadataKey(k) {
				return fmt.Errorf("invalid label key")
			}
			if utf8.RuneCountInString(v) > 63 {
				return fmt.Errorf("label value too long")
			}
			if strings.Contains(v, "\n") {
				return fmt.Errorf("invalid label value")
			}
		}
	}
	if b.Annotations != nil {
		if lim.MaxAnnotations > 0 && len(b.Annotations) > lim.MaxAnnotations {
			return fmt.Errorf("too many sandbox annotations")
		}
		for k, v := range b.Annotations {
			if ReservedMetadataKey(k) {
				return fmt.Errorf("reserved annotation key")
			}
			if !wellFormedMetadataKey(k) {
				return fmt.Errorf("invalid annotation key")
			}
			if len(v) > 4096 {
				return fmt.Errorf("annotation value too long")
			}
		}
	}
	if b.Resources != nil {
		if err := checkResourceCeiling(b.Resources, lim); err != nil {
			return err
		}
	}
	return nil
}

func validateImageRef(s string, maxLen int) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return fmt.Errorf("image empty")
	}
	if maxLen > 0 && len(s) > maxLen {
		return fmt.Errorf("image ref too long")
	}
	if strings.ContainsAny(s, "\n\r\t ") {
		return fmt.Errorf("invalid image ref")
	}
	for _, r := range s {
		if r < 32 || r > 127 {
			return fmt.Errorf("invalid image ref")
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

func wellFormedMetadataKey(k string) bool {
	k = strings.TrimSpace(k)
	if k == "" || len(k) > 253 {
		return false
	}
	for _, r := range k {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			continue
		}
		switch r {
		case '-', '_', '.', '/':
			continue
		default:
			return false
		}
	}
	return true
}

func checkResourceCeiling(r *SandboxResources, lim SandboxProvisionLimits) error {
	var maxCPU resource.Quantity
	var maxMem resource.Quantity
	var err error
	if strings.TrimSpace(lim.MaxCPU) != "" {
		maxCPU, err = resource.ParseQuantity(lim.MaxCPU)
		if err != nil {
			return fmt.Errorf("invalid max cpu ceiling config")
		}
	}
	if strings.TrimSpace(lim.MaxMemory) != "" {
		maxMem, err = resource.ParseQuantity(lim.MaxMemory)
		if err != nil {
			return fmt.Errorf("invalid max memory ceiling config")
		}
	}
	check := func(s string, max resource.Quantity, kind string) error {
		if strings.TrimSpace(s) == "" || max.Sign() == 0 {
			return nil
		}
		q, err := resource.ParseQuantity(s)
		if err != nil {
			return fmt.Errorf("invalid resource quantity")
		}
		if q.Cmp(max) > 0 {
			if kind == "cpu" {
				return fmt.Errorf("cpu resource above maximum allowed")
			}
			return fmt.Errorf("memory resource above maximum allowed")
		}
		return nil
	}
	if err := check(r.CPURequest, maxCPU, "cpu"); err != nil {
		return err
	}
	if err := check(r.CPULimit, maxCPU, "cpu"); err != nil {
		return err
	}
	if err := check(r.MemoryRequest, maxMem, "mem"); err != nil {
		return err
	}
	if err := check(r.MemoryLimit, maxMem, "mem"); err != nil {
		return err
	}
	return nil
}
