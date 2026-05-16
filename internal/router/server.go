package router

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	ControllerToken   string

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
		MaxBodyBytes:      envInt("BOXY_MAX_BODY_BYTES", 6<<20),
		MaxOutputBytes:    envInt("BOXY_MAX_OUTPUT_BYTES", 6<<20),
		MaxTimeoutSec:     envInt("BOXY_MAX_TIMEOUT_SECONDS", 3600),
		MaxArgs:           envInt("BOXY_MAX_ARGS", 256),
		MaxEnvKeys:        envInt("BOXY_MAX_ENV_KEYS", 64),
		MaxConcurrency:    envInt("BOXY_MAX_CONCURRENCY", 100),
		MaxSandboxTTLSec:  envInt("BOXY_MAX_SANDBOX_TTL_SECONDS", 86400),
		MTLSDisabled:      envBool("BOXY_MTLS_DISABLED", false),
		TLSCAPath:         envStr("BOXY_TLS_CA_PATH", "/tls/ca.crt"),
		TLSClientCertPath: envStr("BOXY_TLS_CLIENT_CERT_PATH", "/tls/tls.crt"),
		TLSClientKeyPath:  envStr("BOXY_TLS_CLIENT_KEY_PATH", "/tls/tls.key"),
		ControllerToken:   envStr("BOXY_CONTROLLER_TOKEN", ""),
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

func NewServer(ctx context.Context, cfg Config, k8sClient client.Client, k8sReader client.Reader, cs kubernetes.Interface) *Server {
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
		auth:      newTokenReviewer(ctx, cs, ttl, cfg.DevToken),
		ctrlClient: ctrlclient.NewClient(ctrlclient.ClientConfig{
			MTLSDisabled:    cfg.MTLSDisabled,
			CACertPath:      cfg.TLSCAPath,
			ClientCert:      cfg.TLSClientCertPath,
			ClientKey:       cfg.TLSClientKeyPath,
			ControllerToken: cfg.ControllerToken,
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

	mux.HandleFunc("POST /v1/sessions/exec", s.withAuth(s.withBodyLimit(s.handleSessionExec)))
	mux.HandleFunc("POST /v1/sessions", s.withAuth(s.withBodyLimit(s.handleSessionCreate)))
	mux.HandleFunc("GET /v1/sessions", s.withAuth(s.handleSessionList))
	mux.HandleFunc("GET /v1/sessions/{sessionId}", s.withAuth(s.handleSessionGet))
	mux.HandleFunc("DELETE /v1/sessions/{sessionId}", s.withAuth(s.handleSessionDelete))

	mux.HandleFunc("POST /v1/sandboxes", s.withAuth(s.withBodyLimit(s.handleSandboxCreate)))
	mux.HandleFunc("GET /v1/sandboxes", s.withAuth(s.handleSandboxList))
	mux.HandleFunc("GET /v1/sandboxes/{sandboxId}", s.withAuth(s.handleSandboxGet))
	mux.HandleFunc("PUT /v1/sandboxes/{sandboxId}", s.withAuth(s.withBodyLimit(s.handleSandboxUpdate)))
	mux.HandleFunc("DELETE /v1/sandboxes/{sandboxId}", s.withAuth(s.handleSandboxDelete))
	mux.HandleFunc("POST /v1/sandboxes/{sandboxId}/evict", s.withAuth(s.handleSandboxEvict))
	mux.HandleFunc("GET /v1/sandboxes/{sandboxId}/sessions", s.withAuth(s.handleSandboxSessions))

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

func (s *Server) decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		if errors.As(err, new(*http.MaxBytesError)) {
			s.jsonErr(w, http.StatusRequestEntityTooLarge, "request body too large", "")
		} else {
			s.jsonErr(w, http.StatusBadRequest, "invalid json", "")
		}
		return false
	}
	return true
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

func (s *Server) handleSandboxCreate(w http.ResponseWriter, r *http.Request) {
	var body api.SandboxCreateBody
	if !s.decodeBody(w, r, &body) {
		return
	}
	if err := api.ValidateSandboxCreate(&body, s.cfg.MaxSandboxTTLSec); err != nil {
		s.jsonErr(w, http.StatusBadRequest, err.Error(), "provisioning")
		return
	}
	if !s.requireResourceAccess(w, r, "create", "sandboxes", body.SandboxID) {
		return
	}
	resp, err := s.createSandboxFromBody(r.Context(), &body)
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			s.jsonErr(w, http.StatusConflict, "sandbox already exists", "create_sandbox")
			return
		}
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
	if err := api.ValidateSandboxID(id); err != nil {
		s.jsonErr(w, http.StatusBadRequest, err.Error(), "validation")
		return
	}
	if !s.requireResourceAccess(w, r, "get", "sandboxes", id) {
		return
	}
	ctx := r.Context()
	sb, err := s.lookupSandbox(ctx, id)
	if err != nil {
		s.jsonErr(w, http.StatusInternalServerError, err.Error(), "store")
		return
	}
	if sb == nil {
		s.jsonErr(w, http.StatusNotFound, "not found", "")
		return
	}
	active, _ := s.countSessionsForSandbox(ctx, id)
	s.writeJSON(w, http.StatusOK, api.SandboxConfigResponse{
		SandboxID:      sb.Spec.SandboxID,
		TTLSeconds:     sb.Spec.TTLSeconds,
		ActiveSessions: active,
	})
}

func (s *Server) handleSandboxDelete(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("sandboxId"))
	if id == "" {
		s.jsonErr(w, http.StatusBadRequest, "sandboxId required", "")
		return
	}
	if err := api.ValidateSandboxID(id); err != nil {
		s.jsonErr(w, http.StatusBadRequest, err.Error(), "validation")
		return
	}
	if !s.requireResourceAccess(w, r, "delete", "sandboxes", id) {
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

func (s *Server) createSandboxFromBody(ctx context.Context, body *api.SandboxCreateBody) (*api.SandboxConfigResponse, error) {
	sandbox := &boxyv1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      body.SandboxID,
			Namespace: s.cfg.SandboxNamespace,
			Labels: map[string]string{
				boxyv1.LabelSandboxID: body.SandboxID,
			},
		},
		Spec: boxyv1.SandboxSpec{
			SandboxID:       body.SandboxID,
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
	return &api.SandboxConfigResponse{SandboxID: body.SandboxID, TTLSeconds: body.TTLSeconds}, nil
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
	if err := s.EnsureDefaultSandbox(ctx); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Server) lookupSession(ctx context.Context, sessionID string) (*boxyv1.Session, error) {
	var list boxyv1.SessionList
	if err := s.k8sReader.List(ctx, &list,
		client.InNamespace(s.cfg.SandboxNamespace),
		client.MatchingLabels{api.LabelSessionID: sessionID},
	); err != nil {
		return nil, err
	}
	for i := range list.Items {
		sess := &list.Items[i]
		if sess.Spec.SessionID == sessionID && sess.DeletionTimestamp.IsZero() {
			return sess, nil
		}
	}
	return nil, nil
}

func (s *Server) controllerURLFromSession(session *boxyv1.Session) string {
	scheme := "https"
	if s.cfg.MTLSDisabled {
		scheme = "http"
	}
	return fmt.Sprintf("%s://%s:%d", scheme, session.Status.ControllerAddress, session.Status.Port)
}

func (s *Server) sessionToResponse(sess *boxyv1.Session) api.SessionResponseBody {
	phase := string(sess.Status.Phase)
	if phase == "" {
		phase = "Pending"
	}
	resp := api.SessionResponseBody{
		SessionID:         sess.Spec.SessionID,
		SandboxID:         sess.Spec.SandboxID,
		Owner:             sess.Spec.Owner,
		Phase:             phase,
		Ready:             sess.Status.Phase == boxyv1.SandboxPhaseRunning,
		ControllerPod:     sess.Status.ControllerPod,
		ControllerAddress: sess.Status.ControllerAddress,
		Port:              sess.Status.Port,
	}
	if sess.Status.CreatedAt != nil {
		resp.CreatedAt = sess.Status.CreatedAt.UTC().Format(time.RFC3339)
	}
	if sess.Status.ExpiresAt != nil {
		resp.ExpiresAt = sess.Status.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if sess.Status.LastExecAt != nil {
		resp.LastExecAt = sess.Status.LastExecAt.UTC().Format(time.RFC3339)
	}
	return resp
}

func (s *Server) touchLastExecSession(session *boxyv1.Session) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	patch := client.MergeFrom(session.DeepCopy())
	now := metav1.Now()
	session.Status.LastExecAt = &now
	if err := s.k8sClient.Status().Patch(ctx, session, patch); err != nil {
		s.log.Warn("failed to patch session lastExecAt", "session", session.Name, "err", err)
	}
}

func (s *Server) createAndWaitForSession(ctx context.Context, sessionID, sandboxID, owner string) (*boxyv1.Session, error) {
	sess := &boxyv1.Session{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sessionID,
			Namespace: s.cfg.SandboxNamespace,
			Labels: map[string]string{
				api.LabelSessionID:    sessionID,
				boxyv1.LabelSandboxID: sandboxID,
				api.LabelOwner:        owner,
			},
		},
		Spec: boxyv1.SessionSpec{
			SessionID: sessionID,
			SandboxID: sandboxID,
			Owner:     owner,
		},
	}

	if err := s.k8sClient.Create(ctx, sess); err != nil {
		return nil, fmt.Errorf("create session CR: %w", err)
	}

	timeout := s.cfg.CreateTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	deadline := time.Now().Add(timeout)

	for {
		if err := s.k8sReader.Get(ctx, client.ObjectKeyFromObject(sess), sess); err == nil {
			if sess.Status.Phase == boxyv1.SandboxPhaseRunning {
				return sess, nil
			}
			if sess.Status.Phase == boxyv1.SandboxPhaseTerminated {
				return nil, fmt.Errorf("session terminated: %s", sess.Status.Message)
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timeout waiting for session to become running")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func generateSessionID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("sess-%x", b)
}

func (s *Server) handleSessionExec(w http.ResponseWriter, r *http.Request) {
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	default:
		s.jsonErr(w, http.StatusTooManyRequests, "concurrency limit", "too_many_in_flight")
		return
	}

	var body api.ExecRequestBody
	if !s.decodeBody(w, r, &body) {
		return
	}
	if err := api.ValidateExecRequest(&body, s.cfg.MaxTimeoutSec, s.cfg.MaxArgs, s.cfg.MaxEnvKeys); err != nil {
		s.jsonErr(w, http.StatusBadRequest, err.Error(), "validation")
		return
	}

	sessionID := strings.TrimSpace(body.SessionID)
	if sessionID == "" {
		sessionID = generateSessionID()
	} else if err := api.ValidateSessionID(sessionID); err != nil {
		s.jsonErr(w, http.StatusBadRequest, "invalid sessionId format", "validation")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(body.TimeoutSeconds)*time.Second+30*time.Second)
	defer cancel()

	session, err := s.lookupSession(ctx, sessionID)
	if err != nil {
		s.jsonErr(w, http.StatusInternalServerError, err.Error(), "store")
		return
	}

	sessionCreated := false
	if session == nil || session.Status.Phase == boxyv1.SandboxPhaseTerminated {
		if !s.requireResourceAccess(w, r, "get", "sandboxes", body.SandboxID) {
			return
		}
		if !s.requireResourceAccess(w, r, "create", "sessions", sessionID) {
			return
		}
		if session != nil && session.Status.Phase == boxyv1.SandboxPhaseTerminated {
			if err := s.k8sClient.Delete(ctx, session); err != nil {
				s.jsonErr(w, http.StatusInternalServerError, err.Error(), "delete_terminated")
				return
			}
			sessionID = generateSessionID()
		}

		sb, err := s.lookupSandbox(ctx, body.SandboxID)
		if err != nil {
			s.jsonErr(w, http.StatusInternalServerError, err.Error(), "store")
			return
		}
		if sb == nil {
			s.jsonErr(w, http.StatusNotFound, "sandbox config not found", "sandbox_lookup")
			return
		}

		session, err = s.createAndWaitForSession(ctx, sessionID, body.SandboxID, body.Owner)
		if err != nil {
			s.jsonErr(w, http.StatusBadGateway, err.Error(), "create_session")
			return
		}
		sessionCreated = true
	} else {
		if session.Spec.SandboxID != body.SandboxID {
			s.jsonErr(w, http.StatusBadRequest, "sessionId does not belong to sandboxId", "validation")
			return
		}
		if !s.requireResourceAccess(w, r, "update", "sessions", session.Name) {
			return
		}
	}

	if session.Status.Phase != boxyv1.SandboxPhaseRunning {
		s.jsonErr(w, http.StatusServiceUnavailable, "session not ready", "session_not_ready")
		return
	}

	baseURL := s.controllerURLFromSession(session)
	result, err := s.ctrlClient.Exec(ctx, baseURL, ctrlclient.ExecReq{
		SandboxID:      session.Spec.SessionID,
		Command:        body.Command,
		Args:           body.Args,
		Env:            body.Env,
		TimeoutSeconds: body.TimeoutSeconds,
	})
	if err != nil {
		s.jsonErr(w, http.StatusBadGateway, err.Error(), "exec")
		return
	}

	go s.touchLastExecSession(session.DeepCopy())

	w.Header().Set("X-Boxy-Session-Id", sessionID)
	if sessionCreated {
		w.Header().Set("X-Boxy-Session-Created", "true")
	}
	s.writeJSON(w, http.StatusOK, &api.ExecResponseBody{
		Stdout:   result.Stdout,
		Stderr:   result.Stderr,
		ExitCode: result.ExitCode,
		TimedOut: result.TimedOut,
	})
}

func (s *Server) handleSessionCreate(w http.ResponseWriter, r *http.Request) {
	var body api.SessionCreateBody
	if !s.decodeBody(w, r, &body) {
		return
	}
	if err := api.ValidateSessionCreate(&body); err != nil {
		s.jsonErr(w, http.StatusBadRequest, err.Error(), "validation")
		return
	}

	sessionID := strings.TrimSpace(body.SessionID)
	if sessionID == "" {
		sessionID = generateSessionID()
	}
	if !s.requireResourceAccess(w, r, "get", "sandboxes", body.SandboxID) {
		return
	}
	if !s.requireResourceAccess(w, r, "create", "sessions", sessionID) {
		return
	}

	ctx := r.Context()
	sb, err := s.lookupSandbox(ctx, body.SandboxID)
	if err != nil {
		s.jsonErr(w, http.StatusInternalServerError, err.Error(), "store")
		return
	}
	if sb == nil {
		s.jsonErr(w, http.StatusNotFound, "sandbox config not found", "sandbox_lookup")
		return
	}

	createCtx, cancel := context.WithTimeout(ctx, s.cfg.CreateTimeout+5*time.Second)
	defer cancel()

	sess, err := s.createAndWaitForSession(createCtx, sessionID, body.SandboxID, body.Owner)
	if err != nil {
		s.jsonErr(w, http.StatusBadGateway, err.Error(), "create_session")
		return
	}

	resp := s.sessionToResponse(sess)
	s.writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) handleSessionList(w http.ResponseWriter, r *http.Request) {
	if !s.requireResourceAccess(w, r, "list", "sessions", "") {
		return
	}
	labels := client.MatchingLabels{}
	if sandboxID := r.URL.Query().Get("sandboxId"); sandboxID != "" {
		if err := api.ValidateSandboxID(sandboxID); err != nil {
			s.jsonErr(w, http.StatusBadRequest, err.Error(), "validation")
			return
		}
		labels[boxyv1.LabelSandboxID] = sandboxID
	}
	if owner := r.URL.Query().Get("owner"); owner != "" {
		if err := api.ValidateOwner(owner); err != nil {
			s.jsonErr(w, http.StatusBadRequest, err.Error(), "validation")
			return
		}
		labels[api.LabelOwner] = owner
	}

	var list boxyv1.SessionList
	if err := s.k8sReader.List(r.Context(), &list,
		client.InNamespace(s.cfg.SandboxNamespace),
		labels,
	); err != nil {
		s.jsonErr(w, http.StatusInternalServerError, err.Error(), "store")
		return
	}

	phaseFilter := r.URL.Query().Get("phase")
	sessions := make([]api.SessionResponseBody, 0, len(list.Items))
	for i := range list.Items {
		sess := &list.Items[i]
		if sess.DeletionTimestamp != nil {
			continue
		}
		phase := string(sess.Status.Phase)
		if phase == "" {
			phase = "Pending"
		}
		if phaseFilter != "" && phase != phaseFilter {
			continue
		}
		sessions = append(sessions, s.sessionToResponse(sess))
	}

	s.writeJSON(w, http.StatusOK, api.SessionListResponse{Sessions: sessions})
}

func (s *Server) handleSessionGet(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("sessionId"))
	if id == "" {
		s.jsonErr(w, http.StatusBadRequest, "sessionId required", "")
		return
	}
	if err := api.ValidateSessionID(id); err != nil {
		s.jsonErr(w, http.StatusBadRequest, err.Error(), "validation")
		return
	}
	if !s.requireResourceAccess(w, r, "get", "sessions", id) {
		return
	}
	sess, err := s.lookupSession(r.Context(), id)
	if err != nil {
		s.jsonErr(w, http.StatusInternalServerError, err.Error(), "store")
		return
	}
	if sess == nil {
		s.jsonErr(w, http.StatusNotFound, "not found", "")
		return
	}
	s.writeJSON(w, http.StatusOK, s.sessionToResponse(sess))
}

func (s *Server) handleSessionDelete(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("sessionId"))
	if id == "" {
		s.jsonErr(w, http.StatusBadRequest, "sessionId required", "")
		return
	}
	if err := api.ValidateSessionID(id); err != nil {
		s.jsonErr(w, http.StatusBadRequest, err.Error(), "validation")
		return
	}
	if !s.requireResourceAccess(w, r, "delete", "sessions", id) {
		return
	}
	ctx := r.Context()
	sess, err := s.lookupSession(ctx, id)
	if err != nil {
		s.jsonErr(w, http.StatusInternalServerError, err.Error(), "store")
		return
	}
	if sess == nil {
		s.jsonErr(w, http.StatusNotFound, "not found", "")
		return
	}
	if err := s.k8sClient.Delete(ctx, sess); err != nil {
		s.jsonErr(w, http.StatusInternalServerError, err.Error(), "delete")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSandboxList(w http.ResponseWriter, r *http.Request) {
	if !s.requireResourceAccess(w, r, "list", "sandboxes", "") {
		return
	}
	var list boxyv1.SandboxList
	if err := s.k8sReader.List(r.Context(), &list, client.InNamespace(s.cfg.SandboxNamespace)); err != nil {
		s.jsonErr(w, http.StatusInternalServerError, err.Error(), "store")
		return
	}

	configs := make([]api.SandboxConfigResponse, 0, len(list.Items))
	for i := range list.Items {
		sb := &list.Items[i]
		if sb.DeletionTimestamp != nil {
			continue
		}
		active, _ := s.countSessionsForSandbox(r.Context(), sb.Spec.SandboxID)
		configs = append(configs, api.SandboxConfigResponse{
			SandboxID:      sb.Spec.SandboxID,
			TTLSeconds:     sb.Spec.TTLSeconds,
			ActiveSessions: active,
		})
	}

	s.writeJSON(w, http.StatusOK, api.SandboxConfigListResponse{Sandboxes: configs})
}

func (s *Server) handleSandboxUpdate(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("sandboxId"))
	if id == "" {
		s.jsonErr(w, http.StatusBadRequest, "sandboxId required", "")
		return
	}
	if err := api.ValidateSandboxID(id); err != nil {
		s.jsonErr(w, http.StatusBadRequest, err.Error(), "validation")
		return
	}
	var body api.SandboxCreateBody
	if !s.decodeBody(w, r, &body) {
		return
	}
	body.SandboxID = id
	if err := api.ValidateSandboxCreate(&body, s.cfg.MaxSandboxTTLSec); err != nil {
		s.jsonErr(w, http.StatusBadRequest, err.Error(), "validation")
		return
	}
	if !s.requireResourceAccess(w, r, "update", "sandboxes", id) {
		return
	}

	ctx := r.Context()
	sb, err := s.lookupSandbox(ctx, id)
	if err != nil {
		s.jsonErr(w, http.StatusInternalServerError, err.Error(), "store")
		return
	}
	if sb == nil {
		s.jsonErr(w, http.StatusNotFound, "sandbox config not found", "")
		return
	}

	patch := client.MergeFrom(sb.DeepCopy())
	sb.Spec.TTLSeconds = body.TTLSeconds
	sb.Spec.Env = body.Env
	sb.Spec.AllowedBinaries = body.AllowedBinaries
	sb.Spec.VM = body.VM
	sb.Spec.Network = body.Network
	sb.Spec.Volumes = body.Volumes
	sb.Spec.Patches = body.Patches

	if err := s.k8sClient.Patch(ctx, sb, patch); err != nil {
		s.jsonErr(w, http.StatusInternalServerError, err.Error(), "update")
		return
	}

	active, _ := s.countSessionsForSandbox(ctx, id)
	s.writeJSON(w, http.StatusOK, api.SandboxConfigResponse{
		SandboxID:      id,
		TTLSeconds:     sb.Spec.TTLSeconds,
		ActiveSessions: active,
	})
}

func (s *Server) handleSandboxEvict(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("sandboxId"))
	if id == "" {
		s.jsonErr(w, http.StatusBadRequest, "sandboxId required", "")
		return
	}
	if err := api.ValidateSandboxID(id); err != nil {
		s.jsonErr(w, http.StatusBadRequest, err.Error(), "validation")
		return
	}
	if !s.requireResourceAccess(w, r, "update", "sandboxes", id) {
		return
	}
	ctx := r.Context()

	var list boxyv1.SessionList
	if err := s.k8sReader.List(ctx, &list,
		client.InNamespace(s.cfg.SandboxNamespace),
		client.MatchingLabels{boxyv1.LabelSandboxID: id},
	); err != nil {
		s.jsonErr(w, http.StatusInternalServerError, err.Error(), "store")
		return
	}

	evicted := 0
	for i := range list.Items {
		sess := &list.Items[i]
		if sess.Status.Phase == boxyv1.SandboxPhaseTerminated || !sess.DeletionTimestamp.IsZero() {
			continue
		}
		patch := client.MergeFrom(sess.DeepCopy())
		sess.Status.Phase = boxyv1.SandboxPhaseDeleting
		if err := s.k8sClient.Status().Patch(ctx, sess, patch); err != nil {
			s.log.Warn("evict: failed to patch session", "session", sess.Name, "err", err)
			continue
		}
		evicted++
	}

	s.writeJSON(w, http.StatusOK, api.SandboxEvictResponse{
		EvictedSessions: evicted,
		Message:         "eviction initiated",
	})
}

func (s *Server) handleSandboxSessions(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("sandboxId"))
	if id == "" {
		s.jsonErr(w, http.StatusBadRequest, "sandboxId required", "")
		return
	}
	if err := api.ValidateSandboxID(id); err != nil {
		s.jsonErr(w, http.StatusBadRequest, err.Error(), "validation")
		return
	}
	if !s.requireResourceAccess(w, r, "list", "sessions", "") {
		return
	}

	var list boxyv1.SessionList
	if err := s.k8sReader.List(r.Context(), &list,
		client.InNamespace(s.cfg.SandboxNamespace),
		client.MatchingLabels{boxyv1.LabelSandboxID: id},
	); err != nil {
		s.jsonErr(w, http.StatusInternalServerError, err.Error(), "store")
		return
	}

	sessions := make([]api.SessionResponseBody, 0, len(list.Items))
	for i := range list.Items {
		if list.Items[i].DeletionTimestamp != nil {
			continue
		}
		sessions = append(sessions, s.sessionToResponse(&list.Items[i]))
	}

	s.writeJSON(w, http.StatusOK, api.SessionListResponse{Sessions: sessions})
}

func (s *Server) countSessionsForSandbox(ctx context.Context, sandboxID string) (int, error) {
	var list boxyv1.SessionList
	if err := s.k8sReader.List(ctx, &list,
		client.InNamespace(s.cfg.SandboxNamespace),
		client.MatchingLabels{boxyv1.LabelSandboxID: sandboxID},
	); err != nil {
		return 0, err
	}
	n := 0
	for i := range list.Items {
		switch list.Items[i].Status.Phase {
		case boxyv1.SandboxPhasePending, boxyv1.SandboxPhaseCreating, boxyv1.SandboxPhaseRunning:
			n++
		}
	}
	return n, nil
}
