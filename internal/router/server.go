package router

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"

	boxyv1 "boxy.dev/boxy/api/v1alpha1"
	"boxy.dev/boxy/internal/api"
	ctrlclient "boxy.dev/boxy/internal/controller"
)

type Config struct {
	ListenAddr       string
	DevToken         string // optional static bypass for local dev / e2e; empty = K8s SA tokens only
	AuthCacheTTL     time.Duration
	SandboxNamespace string
	MaxBodyBytes     int
	MaxOutputBytes   int
	MaxTimeoutSec    int
	MaxArgs          int
	MaxEnvKeys       int
	MaxConcurrency   int
	MaxSandboxTTLSec int

	MTLSDisabled      bool
	TLSCAPath         string
	TLSClientCertPath string
	TLSClientKeyPath  string

	ControllerPort int32

	DefaultSandboxEnabled bool
	DefaultSandboxConfig  *api.SandboxCreateBody

	CreateTimeout time.Duration
}

func envInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func envBool(key string, def bool) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	if v == "" {
		return def
	}
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

func envStr(key, def string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	return v
}

func ConfigFromEnv() (*Config, error) {
	ns := strings.TrimSpace(os.Getenv("BOXY_SANDBOX_NAMESPACE"))
	if ns == "" {
		ns = metav1.NamespaceDefault
	}
	cacheTTL := time.Duration(envInt("BOXY_AUTH_CACHE_TTL_SECONDS", 30)) * time.Second
	cfg := &Config{
		ListenAddr:        strings.TrimSpace(os.Getenv("BOXY_LISTEN_ADDR")),
		DevToken:          strings.TrimSpace(os.Getenv("BOXY_ROUTER_TOKEN")), // optional static bypass
		AuthCacheTTL:      cacheTTL,
		SandboxNamespace:  ns,
		MaxBodyBytes:      envInt("BOXY_MAX_BODY_BYTES", 1<<20),
		MaxOutputBytes:    envInt("BOXY_MAX_OUTPUT_BYTES", 2<<20),
		MaxTimeoutSec:     envInt("BOXY_MAX_TIMEOUT_SECONDS", 3600),
		MaxArgs:           envInt("BOXY_MAX_ARGS", 256),
		MaxEnvKeys:        envInt("BOXY_MAX_ENV_KEYS", 64),
		MaxConcurrency:    envInt("BOXY_MAX_CONCURRENCY", 100),
		MaxSandboxTTLSec:  envInt("BOXY_MAX_SANDBOX_TTL_SECONDS", 86400),
		MTLSDisabled:      envBool("BOXY_MTLS_DISABLED", false),
		TLSCAPath:         envStr("BOXY_TLS_CA_PATH", "/tls/ca.crt"),
		TLSClientCertPath: envStr("BOXY_TLS_CLIENT_CERT_PATH", "/tls/tls.crt"),
		TLSClientKeyPath:  envStr("BOXY_TLS_CLIENT_KEY_PATH", "/tls/tls.key"),
		ControllerPort:    int32(envInt("BOXY_CONTROLLER_PORT", 8080)),
		CreateTimeout:     time.Duration(envInt("BOXY_CREATE_TIMEOUT_SECONDS", 30)) * time.Second,
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":8080"
	}
	cfg.DefaultSandboxEnabled = envBool("BOXY_DEFAULT_SANDBOX_ENABLED", false)
	if cfg.DefaultSandboxEnabled {
		raw := strings.TrimSpace(os.Getenv("BOXY_DEFAULT_SANDBOX_CONFIG"))
		if raw == "" {
			return nil, fmt.Errorf("BOXY_DEFAULT_SANDBOX_CONFIG is required when default sandbox is enabled")
		}
		var body api.SandboxCreateBody
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			return nil, fmt.Errorf("parse BOXY_DEFAULT_SANDBOX_CONFIG: %w", err)
		}
		if body.SandboxID == "" {
			body.SandboxID = "default"
		}
		if body.Owner == "" {
			body.Owner = "system"
		}
		if body.SessionID == "" {
			body.SessionID = "default-box"
		}
		cfg.DefaultSandboxConfig = &body
	}
	return cfg, nil
}

type Server struct {
	cfg        Config
	log        *slog.Logger
	sem        chan struct{}
	k8sClient  client.Client
	k8sReader  client.Reader
	ctrlClient *ctrlclient.Client
	auth       *tokenReviewer
}

func NewServer(cfg Config, k8sClient client.Client, k8sReader client.Reader, cs kubernetes.Interface) *Server {
	if cfg.MaxConcurrency <= 0 {
		cfg.MaxConcurrency = 1
	}
	ttl := cfg.AuthCacheTTL
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	return &Server{
		cfg:       cfg,
		log:       slog.Default(),
		sem:       make(chan struct{}, cfg.MaxConcurrency),
		k8sClient: k8sClient,
		k8sReader: k8sReader,
		auth:      newTokenReviewer(cs, ttl, cfg.DevToken),
		ctrlClient: ctrlclient.NewClient(ctrlclient.ClientConfig{
			MTLSDisabled: cfg.MTLSDisabled,
			CACertPath:   cfg.TLSCAPath,
			ClientCert:   cfg.TLSClientCertPath,
			ClientKey:    cfg.TLSClientKeyPath,
		}),
	}
}

func NewScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(boxyv1.AddToScheme(s))
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("POST /v1/exec", s.withAuth(s.withBodyLimit(s.handleExec)))
	mux.HandleFunc("POST /v1/sandboxes", s.withAuth(s.withBodyLimit(s.handleSandboxCreate)))
	mux.HandleFunc("GET /v1/sandboxes/{sandboxId}", s.withAuth(s.handleSandboxGet))
	mux.HandleFunc("DELETE /v1/sandboxes/{sandboxId}", s.withAuth(s.handleSandboxDelete))
	mux.Handle("/mcp", s.withAuthHandler(s.newMCPHandler()))
	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) extractBearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return "", false
	}
	tok := strings.TrimSpace(h[len(prefix):])
	return tok, tok != ""
}

func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := s.extractBearer(r)
		if !ok {
			s.jsonErr(w, http.StatusUnauthorized, "unauthorized", "")
			return
		}
		user, err := s.auth.authenticate(r.Context(), tok)
		if err != nil {
			s.log.Debug("auth failed", "err", err)
			s.jsonErr(w, http.StatusUnauthorized, "unauthorized", "")
			return
		}
		ctx := context.WithValue(r.Context(), authUserKey, user)
		next(w, r.WithContext(ctx))
	}
}

func (s *Server) withAuthHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := s.extractBearer(r)
		if !ok {
			s.jsonErr(w, http.StatusUnauthorized, "unauthorized", "")
			return
		}
		user, err := s.auth.authenticate(r.Context(), tok)
		if err != nil {
			s.log.Debug("auth failed", "err", err)
			s.jsonErr(w, http.StatusUnauthorized, "unauthorized", "")
			return
		}
		ctx := context.WithValue(r.Context(), authUserKey, user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) withBodyLimit(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.MaxBodyBytes > 0 {
			r.Body = http.MaxBytesReader(w, r.Body, int64(s.cfg.MaxBodyBytes))
		}
		next(w, r)
	}
}

func (s *Server) jsonErr(w http.ResponseWriter, code int, msg, cerr string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(api.ErrorBody{Error: msg, Code: cerr})
}

func (s *Server) writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	default:
		s.jsonErr(w, http.StatusTooManyRequests, "concurrency limit", "too_many_in_flight")
		return
	}
	var body api.ExecRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.jsonErr(w, http.StatusBadRequest, "invalid json", "")
		return
	}
	if err := api.ValidateExecRequest(&body, s.cfg.MaxTimeoutSec, s.cfg.MaxArgs, s.cfg.MaxEnvKeys); err != nil {
		s.jsonErr(w, http.StatusBadRequest, err.Error(), "validation")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(body.TimeoutSeconds)*time.Second+5*time.Second)
	defer cancel()

	sandbox, err := s.lookupSandbox(ctx, body.SandboxID)
	if err != nil {
		s.jsonErr(w, http.StatusInternalServerError, err.Error(), "store")
		return
	}
	if sandbox == nil || sandbox.Status.Phase != boxyv1.SandboxPhaseRunning {
		s.jsonErr(w, http.StatusNotFound, "sandbox not found", "sandbox_lookup")
		return
	}

	baseURL := s.controllerURL(sandbox)
	result, err := s.ctrlClient.Exec(ctx, baseURL, ctrlclient.ExecReq{
		SandboxID:      body.SandboxID,
		Command:        body.Command,
		Args:           body.Args,
		Env:            body.Env,
		TimeoutSeconds: body.TimeoutSeconds,
	})
	if err != nil {
		s.jsonErr(w, http.StatusBadGateway, err.Error(), "exec")
		return
	}

	go s.touchLastExec(sandbox)

	s.writeJSON(w, http.StatusOK, &api.ExecResponseBody{
		Stdout:   result.Stdout,
		Stderr:   result.Stderr,
		ExitCode: result.ExitCode,
		TimedOut: result.TimedOut,
	})
}

func (s *Server) handleSandboxCreate(w http.ResponseWriter, r *http.Request) {
	var body api.SandboxCreateBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.jsonErr(w, http.StatusBadRequest, "invalid json", "")
		return
	}
	if err := api.ValidateSandboxCreate(&body, s.cfg.MaxSandboxTTLSec); err != nil {
		s.jsonErr(w, http.StatusBadRequest, err.Error(), "provisioning")
		return
	}

	resp, err := s.createSandboxFromBody(r.Context(), &body)
	if err != nil {
		s.jsonErr(w, http.StatusBadGateway, err.Error(), "create_sandbox")
		return
	}
	s.writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) handleSandboxGet(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("sandboxId"))
	if id == "" {
		s.jsonErr(w, http.StatusBadRequest, "sandboxId required", "")
		return
	}
	sandbox, err := s.lookupSandbox(r.Context(), id)
	if err != nil {
		s.jsonErr(w, http.StatusInternalServerError, err.Error(), "")
		return
	}
	if sandbox == nil {
		s.jsonErr(w, http.StatusNotFound, "not found", "")
		return
	}
	s.writeJSON(w, http.StatusOK, s.sandboxToResponse(sandbox))
}

func (s *Server) handleSandboxDelete(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("sandboxId"))
	if id == "" {
		s.jsonErr(w, http.StatusBadRequest, "sandboxId required", "")
		return
	}
	ctx := r.Context()

	sandbox, err := s.lookupSandbox(ctx, id)
	if err != nil {
		s.jsonErr(w, http.StatusInternalServerError, err.Error(), "store")
		return
	}
	if sandbox == nil {
		s.jsonErr(w, http.StatusNotFound, "sandbox not found", "")
		return
	}

	if err := s.k8sClient.Delete(ctx, sandbox); err != nil {
		s.jsonErr(w, http.StatusInternalServerError, err.Error(), "delete")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) lookupSandbox(ctx context.Context, sandboxID string) (*boxyv1.Sandbox, error) {
	var list boxyv1.SandboxList
	if err := s.k8sReader.List(ctx, &list,
		client.InNamespace(s.cfg.SandboxNamespace),
		client.MatchingLabels{boxyv1.LabelSandboxID: sandboxID},
	); err != nil {
		return nil, err
	}
	for i := range list.Items {
		sb := &list.Items[i]
		if sb.Spec.SandboxID == sandboxID && sb.DeletionTimestamp.IsZero() {
			return sb, nil
		}
	}
	return nil, nil
}

func (s *Server) createSandboxFromBody(ctx context.Context, body *api.SandboxCreateBody) (*api.SandboxResponseBody, error) {
	sandbox := &boxyv1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      body.SandboxID,
			Namespace: s.cfg.SandboxNamespace,
			Labels: map[string]string{
				boxyv1.LabelSandboxID: body.SandboxID,
				api.LabelSessionID:    body.SessionID,
				api.LabelOwner:        body.Owner,
			},
		},
		Spec: boxyv1.SandboxSpec{
			SandboxID:       body.SandboxID,
			SessionID:       body.SessionID,
			Owner:           body.Owner,
			TTLSeconds:      body.TTLSeconds,
			Env:             body.Env,
			AllowedBinaries: body.AllowedBinaries,
			VM:              body.VM,
			Network:         body.Network,
			Volumes:         body.Volumes,
			Patches:         body.Patches,
		},
	}

	if err := s.k8sClient.Create(ctx, sandbox); err != nil {
		return nil, fmt.Errorf("create sandbox CR: %w", err)
	}

	timeout := s.cfg.CreateTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	deadline := time.Now().Add(timeout)

	for {
		if err := s.k8sReader.Get(ctx, client.ObjectKeyFromObject(sandbox), sandbox); err == nil {
			if sandbox.Status.Phase == boxyv1.SandboxPhaseRunning {
				return s.sandboxToResponse(sandbox), nil
			}
			if sandbox.Status.Phase == boxyv1.SandboxPhaseTerminated {
				return nil, fmt.Errorf("sandbox terminated: %s", sandbox.Status.Message)
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timeout waiting for sandbox to become running")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (s *Server) sandboxToResponse(sb *boxyv1.Sandbox) *api.SandboxResponseBody {
	phase := string(sb.Status.Phase)
	if phase == "" {
		phase = "Pending"
	}
	return &api.SandboxResponseBody{
		SandboxID: sb.Spec.SandboxID,
		SessionID: sb.Spec.SessionID,
		Owner:     sb.Spec.Owner,
		Runtime:   "nsjail",
		PodRef:    api.PodRef{Namespace: s.cfg.SandboxNamespace, Name: sb.Status.ControllerPod},
		Phase:     phase,
		Ready:     sb.Status.Phase == boxyv1.SandboxPhaseRunning,
	}
}

func (s *Server) controllerURL(sandbox *boxyv1.Sandbox) string {
	scheme := "https"
	if s.cfg.MTLSDisabled {
		scheme = "http"
	}
	return fmt.Sprintf("%s://%s:%d", scheme, sandbox.Status.ControllerAddress, sandbox.Status.Port)
}

func (s *Server) touchLastExec(sandbox *boxyv1.Sandbox) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	patch := client.MergeFrom(sandbox.DeepCopy())
	now := metav1.Now()
	sandbox.Status.LastExecAt = &now
	if err := s.k8sClient.Status().Patch(ctx, sandbox, patch); err != nil {
		s.log.Warn("failed to patch lastExecAt", "sandbox", sandbox.Name, "err", err)
	}
}

// EnsureDefaultSandbox creates the default sandbox CR if it doesn't exist.
func (s *Server) EnsureDefaultSandbox(ctx context.Context) error {
	if !s.cfg.DefaultSandboxEnabled || s.cfg.DefaultSandboxConfig == nil {
		return nil
	}
	id := s.cfg.DefaultSandboxConfig.SandboxID
	existing, _ := s.lookupSandbox(ctx, id)
	if existing != nil {
		return nil
	}
	_, err := s.createSandboxFromBody(ctx, s.cfg.DefaultSandboxConfig)
	if err != nil {
		return fmt.Errorf("create default sandbox: %w", err)
	}
	s.log.Info("default sandbox created", "sandboxId", id)
	return nil
}

func (s *Server) resolveDefaultSandboxID(ctx context.Context) (string, error) {
	if !s.cfg.DefaultSandboxEnabled || s.cfg.DefaultSandboxConfig == nil {
		return "", fmt.Errorf("default sandbox is disabled")
	}
	id := s.cfg.DefaultSandboxConfig.SandboxID
	existing, _ := s.lookupSandbox(ctx, id)
	if existing != nil && existing.Status.Phase == boxyv1.SandboxPhaseRunning {
		return id, nil
	}
	if err := s.EnsureDefaultSandbox(ctx); err != nil {
		return "", err
	}
	return id, nil
}
