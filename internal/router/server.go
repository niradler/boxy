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
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"boxy.dev/boxy/internal/api"
	"boxy.dev/boxy/internal/kube"
	"boxy.dev/boxy/internal/session"
)

type Config struct {
	ListenAddr     string
	AuthToken      string
	SandboxNamespace string
	MaxBodyBytes   int
	MaxOutputBytes int
	MaxTimeoutSec  int
	MaxArgs        int
	MaxEnvKeys     int
	MaxConcurrency int
	MaxSandboxTTLSec int
	ReaperEvery    time.Duration
	Kube           kubernetes.Interface
	RESTConfig     *rest.Config

	ControllerImage           string
	ControllerPort            int32
	ControllerTTLSec          int
	MaxSandboxesPerController int
	ControllerServiceAcct     string
	PullSecret                string
	MTLSDisabled              bool
	MTLSControllerSecret      string
	TLSCAPath                 string
	TLSClientCertPath         string
	TLSClientKeyPath          string
	VMLogLevel                string
	VMMetricsIntMs            int
	VMPullPolicy              string
	LibKrunfwPath             string
	KVMMode                   string
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
	k, err := kube.NewClientset()
	if err != nil {
		return nil, err
	}
	rc, err := kube.BuildConfig()
	if err != nil {
		return nil, err
	}
	auth := strings.TrimSpace(os.Getenv("BOXY_ROUTER_TOKEN"))
	if auth == "" {
		return nil, fmt.Errorf("BOXY_ROUTER_TOKEN is required")
	}
	ns := strings.TrimSpace(os.Getenv("BOXY_SANDBOX_NAMESPACE"))
	if ns == "" {
		ns = metav1.NamespaceDefault
	}
	pull := strings.TrimSpace(os.Getenv("BOXY_IMAGE_PULL_SECRET"))
	cfg := &Config{
		ListenAddr:       strings.TrimSpace(os.Getenv("BOXY_LISTEN_ADDR")),
		AuthToken:        auth,
		SandboxNamespace: ns,
		PullSecret:       pull,
		MaxBodyBytes:     envInt("BOXY_MAX_BODY_BYTES", 1<<20),
		MaxOutputBytes:   envInt("BOXY_MAX_OUTPUT_BYTES", 2<<20),
		MaxTimeoutSec:    envInt("BOXY_MAX_TIMEOUT_SECONDS", 3600),
		MaxArgs:          envInt("BOXY_MAX_ARGS", 256),
		MaxEnvKeys:       envInt("BOXY_MAX_ENV_KEYS", 64),
		MaxConcurrency:   envInt("BOXY_MAX_CONCURRENCY", 100),
		MaxSandboxTTLSec: envInt("BOXY_MAX_SANDBOX_TTL_SECONDS", 86400),
		ReaperEvery:      time.Duration(envInt("BOXY_REAPER_INTERVAL_SECONDS", 30)) * time.Second,
		Kube:             k,
		RESTConfig:       rc,

		ControllerImage:           strings.TrimSpace(os.Getenv("BOXY_CONTROLLER_IMAGE")),
		ControllerPort:            int32(envInt("BOXY_CONTROLLER_PORT", 8080)),
		ControllerTTLSec:          envInt("BOXY_CONTROLLER_TTL_SECONDS", 3600),
		MaxSandboxesPerController: envInt("BOXY_MAX_SANDBOXES_PER_CONTROLLER", 20),
		ControllerServiceAcct:     strings.TrimSpace(os.Getenv("BOXY_CONTROLLER_SERVICE_ACCOUNT")),
		MTLSDisabled:              envBool("BOXY_MTLS_DISABLED", false),
		MTLSControllerSecret:      envStr("BOXY_MTLS_CONTROLLER_SECRET", "boxy-mtls-controller"),
		TLSCAPath:                 envStr("BOXY_TLS_CA_PATH", "/tls/ca.crt"),
		TLSClientCertPath:         envStr("BOXY_TLS_CLIENT_CERT_PATH", "/tls/tls.crt"),
		TLSClientKeyPath:          envStr("BOXY_TLS_CLIENT_KEY_PATH", "/tls/tls.key"),
		VMLogLevel:                envStr("BOXY_VM_LOG_LEVEL", ""),
		VMMetricsIntMs:            envInt("BOXY_VM_METRICS_INTERVAL_MS", 0),
		VMPullPolicy:              envStr("BOXY_VM_PULL_POLICY", ""),
		LibKrunfwPath:             envStr("BOXY_LIBKRUNFW_PATH", ""),
		KVMMode:                   envStr("BOXY_KVM_MODE", "device"),
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":8080"
	}
	return cfg, nil
}

type Server struct {
	cfg           Config
	log           *slog.Logger
	sem           chan struct{}
	httpTransport *http.Transport
	store         *kube.SandboxRouteStore
	ctrlClient    *ControllerClient
	ctrlSpec      kube.ControllerPodSpec
	sync          *SyncReconciler
}

func NewServer(cfg Config) *Server {
	if cfg.MaxConcurrency <= 0 {
		cfg.MaxConcurrency = 1
	}
	s := &Server{
		cfg: cfg,
		log: slog.Default(),
		sem: make(chan struct{}, cfg.MaxConcurrency),
		httpTransport: &http.Transport{
			MaxIdleConns:        128,
			MaxIdleConnsPerHost: 64,
			IdleConnTimeout:     90 * time.Second,
			ForceAttemptHTTP2:   true,
		},
	}
	s.store = kube.NewSandboxRouteStore(cfg.Kube, cfg.SandboxNamespace, "boxy-sandbox-routes")
	s.ctrlClient = NewControllerClient(ControllerClientConfig{
		MTLSDisabled: cfg.MTLSDisabled,
		CACertPath:   cfg.TLSCAPath,
		ClientCert:   cfg.TLSClientCertPath,
		ClientKey:    cfg.TLSClientKeyPath,
	})
	s.ctrlSpec = kube.ControllerPodSpec{
		Namespace:       cfg.SandboxNamespace,
		ControllerImage: cfg.ControllerImage,
		MaxSandboxes:    cfg.MaxSandboxesPerController,
		Port:            cfg.ControllerPort,
		TTLSeconds:      cfg.ControllerTTLSec,
		PullSecretName:  cfg.PullSecret,
		ServiceAccount:  cfg.ControllerServiceAcct,
		MTLSDisabled:    cfg.MTLSDisabled,
		MTLSSecretName:  cfg.MTLSControllerSecret,
		VMLogLevel:      cfg.VMLogLevel,
		VMMetricsIntMs:  cfg.VMMetricsIntMs,
		VMPullPolicy:    cfg.VMPullPolicy,
		LibKrunfwPath:   cfg.LibKrunfwPath,
		KVMMode:         cfg.KVMMode,
	}
	scheme := "https"
	if cfg.MTLSDisabled {
		scheme = "http"
	}
	s.sync = NewSyncReconciler(SyncReconcilerConfig{
		Kube:       cfg.Kube,
		Namespace:  cfg.SandboxNamespace,
		Store:      s.store,
		HTTPClient: s.ctrlClient.RawClient(),
		Scheme:     scheme,
		Port:       cfg.ControllerPort,
		Logger:     s.log,
	})
	s.store.SetSyncHooks(
		func(_ context.Context, reason string) { s.sync.TriggerAsync("store-conflict:" + reason) },
		func(_ context.Context, reason string) { s.sync.TriggerAsync("parse-error:" + reason) },
	)
	return s
}

func (s *Server) StartupSync(ctx context.Context) {
	if err := s.sync.Trigger(ctx, "startup"); err != nil {
		s.log.Warn("startup sync failed; serving with lazy recovery", "err", err)
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("POST /v1/exec", s.withAuth(s.withBodyLimit(s.handleExec)))
	mux.HandleFunc("POST /v1/sandboxes", s.withAuth(s.withBodyLimit(s.handleSandboxCreate)))
	mux.HandleFunc("GET /v1/sandboxes/{sandboxId}", s.withAuth(s.handleSandboxGet))
	mux.HandleFunc("DELETE /v1/sandboxes/{sandboxId}", s.withAuth(s.handleSandboxDelete))
	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		const p = "Bearer "
		if !strings.HasPrefix(h, p) || strings.TrimSpace(h[len(p):]) != s.cfg.AuthToken {
			s.jsonErr(w, http.StatusUnauthorized, "unauthorized", "")
			return
		}
		next(w, r)
	}
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

	route, ok, err := s.store.Get(ctx, body.SandboxID)
	if err != nil {
		s.jsonErr(w, http.StatusInternalServerError, err.Error(), "store")
		return
	}
	if !ok {
		s.jsonErr(w, http.StatusNotFound, "sandbox not found", "sandbox_lookup")
		return
	}

	scheme := "https"
	if s.cfg.MTLSDisabled {
		scheme = "http"
	}
	baseURL := fmt.Sprintf("%s://%s:%d", scheme, route.ControllerIP, route.Port)

	result, err := s.ctrlClient.Exec(ctx, baseURL, ExecReq{
		SandboxID:      body.SandboxID,
		Command:        body.Command,
		Args:           body.Args,
		Env:            body.Env,
		TimeoutSeconds: body.TimeoutSeconds,
	})
	if err != nil {
		if IsStaleRouteError(err) {
			s.store.InvalidateCache(body.SandboxID)
			s.sync.TriggerAsync("exec-stale-route")
		}
		s.jsonErr(w, http.StatusBadGateway, err.Error(), "exec")
		return
	}

	_ = kube.RefreshControllerTTL(ctx, s.cfg.Kube, s.cfg.SandboxNamespace, route.ControllerPodName, s.ctrlSpec.TTLSeconds)

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
	ctx := r.Context()

	pod, err := s.claimControllerSeat(ctx)
	if err != nil {
		s.jsonErr(w, http.StatusInternalServerError, fmt.Sprintf("controller pod: %v", err), "controller_pod")
		return
	}
	if pod.Status.PodIP == "" {
		var waitErr error
		pod, waitErr = kube.WaitForPodIP(ctx, s.cfg.Kube, s.cfg.SandboxNamespace, pod.Name, 30*time.Second)
		if waitErr != nil {
			_ = kube.IncrementSandboxCount(ctx, s.cfg.Kube, s.cfg.SandboxNamespace, pod.Name, -1)
			s.jsonErr(w, http.StatusServiceUnavailable, fmt.Sprintf("controller pod not ready: %v", waitErr), "controller_not_ready")
			return
		}
	}

	if err := kube.RefreshControllerTTL(ctx, s.cfg.Kube, s.cfg.SandboxNamespace, pod.Name, s.ctrlSpec.TTLSeconds); err != nil {
		s.log.Warn("failed to refresh controller TTL before create", "pod", pod.Name, "err", err)
	}

	scheme := "https"
	if s.cfg.MTLSDisabled {
		scheme = "http"
	}
	baseURL := fmt.Sprintf("%s://%s:%d", scheme, pod.Status.PodIP, s.ctrlSpec.Port)

	req := CreateSandboxReq{
		SandboxID:       body.SandboxID,
		Env:             body.Env,
		AllowedBinaries: body.AllowedBinaries,
		VM:              body.VM,
		Network:         body.Network,
		Volumes:         body.Volumes,
		Patches:         body.Patches,
		TTLSeconds:      body.TTLSeconds,
	}
	if err := s.ctrlClient.CreateSandbox(ctx, baseURL, req); err != nil {
		if derr := kube.IncrementSandboxCount(ctx, s.cfg.Kube, s.cfg.SandboxNamespace, pod.Name, -1); derr != nil {
			s.log.Warn("release controller seat after create failure",
				"pod", pod.Name, "err", derr)
		}
		s.jsonErr(w, http.StatusBadGateway, fmt.Sprintf("create sandbox: %v", err), "create_sandbox")
		return
	}

	route := kube.SandboxRoute{
		ControllerPodName: pod.Name,
		ControllerIP:      pod.Status.PodIP,
		Port:              s.ctrlSpec.Port,
	}
	if err := s.store.Set(ctx, body.SandboxID, route); err != nil {
		s.jsonErr(w, http.StatusInternalServerError, fmt.Sprintf("store route: %v", err), "store")
		return
	}

	_ = kube.RefreshControllerTTL(ctx, s.cfg.Kube, s.cfg.SandboxNamespace, pod.Name, s.ctrlSpec.TTLSeconds)

	resp := &api.SandboxResponseBody{
		SandboxID: body.SandboxID,
		SessionID: body.SessionID,
		Owner:     body.Owner,
		Runtime:   "microsandbox",
		PodRef:    api.PodRef{Namespace: pod.Namespace, Name: pod.Name, UID: string(pod.UID)},
		Phase:     string(pod.Status.Phase),
		Ready:     kube.PodRunningReady(pod),
	}
	s.writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) claimControllerSeat(ctx context.Context) (*corev1.Pod, error) {
	const maxAttempts = 4
	var lastErr error
	for range maxAttempts {
		pod, err := kube.SelectOrCreateControllerPod(ctx, s.cfg.Kube, s.ctrlSpec)
		if err != nil {
			return nil, err
		}
		err = kube.ClaimSandboxSlot(ctx, s.cfg.Kube, s.cfg.SandboxNamespace, pod.Name, s.ctrlSpec.MaxSandboxes)
		if err == nil {
			return pod, nil
		}
		lastErr = err
		s.log.Info("controller seat claim retry", "pod", pod.Name, "err", err)
	}
	return nil, fmt.Errorf("could not claim controller seat after %d attempts: %w", maxAttempts, lastErr)
}

func (s *Server) handleSandboxGet(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("sandboxId"))
	if id == "" {
		s.jsonErr(w, http.StatusBadRequest, "sandboxId required", "")
		return
	}
	route, ok, err := s.store.Get(r.Context(), id)
	if err != nil {
		s.jsonErr(w, http.StatusInternalServerError, err.Error(), "")
		return
	}
	if !ok {
		s.jsonErr(w, http.StatusNotFound, "not found", "")
		return
	}
	s.writeJSON(w, http.StatusOK, &api.SandboxResponseBody{
		SandboxID: id,
		Runtime:   "microsandbox",
		PodRef:    api.PodRef{Namespace: s.cfg.SandboxNamespace, Name: route.ControllerPodName},
	})
}

func (s *Server) handleSandboxDelete(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("sandboxId"))
	if id == "" {
		s.jsonErr(w, http.StatusBadRequest, "sandboxId required", "")
		return
	}
	ctx := r.Context()

	route, ok, err := s.store.Get(ctx, id)
	if err != nil {
		s.jsonErr(w, http.StatusInternalServerError, err.Error(), "store")
		return
	}
	if !ok {
		s.jsonErr(w, http.StatusNotFound, "sandbox not found", "")
		return
	}

	scheme := "https"
	if s.cfg.MTLSDisabled {
		scheme = "http"
	}
	baseURL := fmt.Sprintf("%s://%s:%d", scheme, route.ControllerIP, route.Port)

	if err := s.ctrlClient.DeleteSandbox(ctx, baseURL, DeleteSandboxReq{SandboxID: id}); err != nil {
		s.jsonErr(w, http.StatusBadGateway, err.Error(), "delete_sandbox")
		return
	}

	_ = s.store.Delete(ctx, id)
	_ = kube.IncrementSandboxCount(ctx, s.cfg.Kube, s.cfg.SandboxNamespace, route.ControllerPodName, -1)

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) StartReaper(ctx context.Context, wg *sync.WaitGroup) {
	if s.cfg.ReaperEvery <= 0 {
		return
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(s.cfg.ReaperEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				n, err := session.ReapOnce(context.Background(), s.cfg.Kube, s.cfg.SandboxNamespace)
				if err != nil {
					s.log.Error("reaper", "err", err)
				} else if n > 0 {
					s.log.Info("reaper deleted pods", "count", n)
				}
				m, err := kube.ReapControllerPods(context.Background(), s.cfg.Kube, s.cfg.SandboxNamespace)
				if err != nil {
					s.log.Error("controller reaper", "err", err)
				} else if m > 0 {
					s.log.Info("reaper deleted controller pods", "count", m)
				}
			}
		}
	}()
}
