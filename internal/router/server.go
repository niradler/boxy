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
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"boxy.dev/boxy/internal/api"
	"boxy.dev/boxy/internal/exec"
	"boxy.dev/boxy/internal/kube"
	"boxy.dev/boxy/internal/session"
)

type Config struct {
	ListenAddr        string
	AuthToken         string
	WorkerToken       string
	SandboxNamespace  string
	WorkerImage       string
	WorkerServiceAcct string
	WorkerPort        int32
	PullSecret        string
	ResourceCPU       string
	ResourceMemory    string
	MaxBodyBytes      int
	MaxOutputBytes    int
	MaxTimeoutSec     int
	MaxArgs           int
	MaxEnvKeys        int
	MaxConcurrency    int
	MaxSandboxTTLSec  int
	MaxSandboxLifeSec int
	ReaperEvery       time.Duration
	SandboxLimits     api.SandboxProvisionLimits
	Kube              kubernetes.Interface
	RESTConfig        *rest.Config
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

func mergeAllowCSV(csv string, defaults ...string) map[string]struct{} {
	m := map[string]struct{}{}
	for _, d := range defaults {
		d = strings.TrimSpace(d)
		if d != "" {
			m[d] = struct{}{}
		}
	}
	for _, p := range strings.Split(csv, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			m[p] = struct{}{}
		}
	}
	return m
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
	wtok := strings.TrimSpace(os.Getenv("BOXY_WORKER_TOKEN"))
	if wtok == "" {
		return nil, fmt.Errorf("BOXY_WORKER_TOKEN is required")
	}
	ns := strings.TrimSpace(os.Getenv("BOXY_SANDBOX_NAMESPACE"))
	if ns == "" {
		ns = metav1.NamespaceDefault
	}
	img := strings.TrimSpace(os.Getenv("BOXY_WORKER_IMAGE"))
	if img == "" {
		return nil, fmt.Errorf("BOXY_WORKER_IMAGE is required")
	}
	sa := strings.TrimSpace(os.Getenv("BOXY_WORKER_SERVICE_ACCOUNT"))
	if sa == "" {
		sa = "boxy-worker"
	}
	port := int32(envInt("BOXY_WORKER_PORT", 8080))
	pull := strings.TrimSpace(os.Getenv("BOXY_IMAGE_PULL_SECRET"))
	allowedSA := mergeAllowCSV(os.Getenv("BOXY_ALLOWED_SERVICE_ACCOUNTS"), sa)
	allowedPull := mergeAllowCSV(os.Getenv("BOXY_ALLOWED_PULL_SECRETS"), pull)
	limits := api.SandboxProvisionLimits{
		MaxEnvKeys:         envInt("BOXY_MAX_SANDBOX_ENV_KEYS", 32),
		MaxLabels:          envInt("BOXY_MAX_SANDBOX_LABELS", 16),
		MaxAnnotations:     envInt("BOXY_MAX_SANDBOX_ANNOTATIONS", 32),
		MaxImageRefLen:     envInt("BOXY_MAX_IMAGE_REF_BYTES", 512),
		MinWorkerPort:      envInt("BOXY_MIN_WORKER_PORT", 1),
		MaxWorkerPort:      envInt("BOXY_MAX_WORKER_PORT", 65535),
		MaxCPU:             strings.TrimSpace(os.Getenv("BOXY_MAX_SANDBOX_CPU")),
		MaxMemory:          strings.TrimSpace(os.Getenv("BOXY_MAX_SANDBOX_MEMORY")),
		AllowedServiceAcct: allowedSA,
		AllowedPullSecrets: allowedPull,
		DefaultServiceAcct: sa,
		GlobalPullSecret:   pull,
	}
	cfg := &Config{
		ListenAddr:        strings.TrimSpace(os.Getenv("BOXY_LISTEN_ADDR")),
		AuthToken:         auth,
		WorkerToken:       wtok,
		SandboxNamespace:  ns,
		WorkerImage:       img,
		WorkerServiceAcct: sa,
		WorkerPort:        port,
		PullSecret:        pull,
		ResourceCPU:       strings.TrimSpace(os.Getenv("BOXY_WORKER_CPU")),
		ResourceMemory:    strings.TrimSpace(os.Getenv("BOXY_WORKER_MEMORY")),
		MaxBodyBytes:      envInt("BOXY_MAX_BODY_BYTES", 1<<20),
		MaxOutputBytes:    envInt("BOXY_MAX_OUTPUT_BYTES", 2<<20),
		MaxTimeoutSec:     envInt("BOXY_MAX_TIMEOUT_SECONDS", 3600),
		MaxArgs:           envInt("BOXY_MAX_ARGS", 256),
		MaxEnvKeys:        envInt("BOXY_MAX_ENV_KEYS", 64),
		MaxConcurrency:    envInt("BOXY_MAX_CONCURRENCY", 100),
		MaxSandboxTTLSec:  envInt("BOXY_MAX_SANDBOX_TTL_SECONDS", 86400),
		MaxSandboxLifeSec: envInt("BOXY_MAX_SANDBOX_LIFETIME_SECONDS", 604800),
		ReaperEvery:       time.Duration(envInt("BOXY_REAPER_INTERVAL_SECONDS", 30)) * time.Second,
		SandboxLimits:     limits,
		Kube:              k,
		RESTConfig:        rc,
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
}

func NewServer(cfg Config) *Server {
	if cfg.MaxConcurrency <= 0 {
		cfg.MaxConcurrency = 1
	}
	return &Server{
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
	if api.PodRefEmpty(&body.PodRef) {
		p, err := kube.GetSandboxPodForSession(r.Context(), s.cfg.Kube, s.cfg.SandboxNamespace, body.SandboxID, body.SessionID)
		if err != nil {
			if errors.IsNotFound(err) {
				s.jsonErr(w, http.StatusNotFound, "sandbox not found", "sandbox_lookup")
				return
			}
			s.jsonErr(w, http.StatusInternalServerError, err.Error(), "")
			return
		}
		body.PodRef = api.PodRef{Namespace: p.Namespace, Name: p.Name, UID: string(p.UID)}
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(body.TimeoutSeconds)*time.Second)
	defer cancel()
	pod, err := kube.ValidatePodForExec(ctx, s.cfg.Kube, body.SessionID, body.SandboxID, &body.PodRef)
	if err != nil {
		s.jsonErr(w, http.StatusForbidden, err.Error(), "pod_validation")
		return
	}
	switch body.Mode {
	case api.ExecModeAPI:
		s.doAPIExec(w, ctx, pod, &body)
	case api.ExecModePod:
		s.doPodExec(w, ctx, pod, &body)
	default:
		s.jsonErr(w, http.StatusBadRequest, "invalid mode", "")
	}
}

func (s *Server) doAPIExec(w http.ResponseWriter, ctx context.Context, pod *corev1.Pod, body *api.ExecRequestBody) {
	base, err := kube.WorkerHTTPAddr(pod, s.cfg.WorkerPort)
	if err != nil {
		s.jsonErr(w, http.StatusBadGateway, err.Error(), "pod_ip")
		return
	}
	client := &http.Client{
		Transport: s.httpTransport,
		Timeout:   time.Duration(body.TimeoutSeconds)*time.Second + 5*time.Second,
	}
	apiExec := &exec.APIExecClient{HTTP: client, Token: s.cfg.WorkerToken, MaxBody: s.cfg.MaxBodyBytes}
	out, err := apiExec.Run(ctx, base, body, s.cfg.MaxOutputBytes)
	if err != nil {
		s.jsonErr(w, http.StatusBadGateway, err.Error(), "api_exec")
		return
	}
	s.writeJSON(w, http.StatusOK, out)
}

func (s *Server) doPodExec(w http.ResponseWriter, ctx context.Context, pod *corev1.Pod, body *api.ExecRequestBody) {
	script, err := exec.BuildRemoteShell(body.Command, body.Args, body.Env)
	if err != nil {
		s.jsonErr(w, http.StatusBadRequest, err.Error(), "")
		return
	}
	cmd := []string{"/bin/sh", "-lc", script}
	stdout := exec.NewLimitedWriter(s.cfg.MaxOutputBytes)
	stderr := exec.NewLimitedWriter(s.cfg.MaxOutputBytes)
	ctr := body.WorkerContainer
	if ctr == "" {
		ctr = "worker"
	}
	pe := &exec.PodExec{
		Config:    s.cfg.RESTConfig,
		Clientset: s.cfg.Kube,
		Namespace: pod.Namespace,
		Pod:       pod.Name,
		Container: ctr,
		Command:   cmd,
		Stdout:    stdout,
		Stderr:    stderr,
	}
	err = pe.Run(ctx)
	if stdout.HitLimit() || stderr.HitLimit() {
		s.jsonErr(w, http.StatusRequestEntityTooLarge, "output limit exceeded", "output_limit")
		return
	}
	exit := exec.ExitCodeFromExecError(err)
	resp := api.ExecResponseBody{
		ExitCode: exit,
		Stdout:   string(stdout.Bytes()),
		Stderr:   string(stderr.Bytes()),
	}
	s.writeJSON(w, http.StatusOK, &resp)
}

func (s *Server) writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) handleSandboxCreate(w http.ResponseWriter, r *http.Request) {
	var body api.SandboxCreateBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.jsonErr(w, http.StatusBadRequest, "invalid json", "")
		return
	}
	if err := api.ValidateSandboxProvisioning(&body, s.cfg.MaxSandboxTTLSec, s.cfg.MaxSandboxLifeSec, s.cfg.SandboxLimits); err != nil {
		s.jsonErr(w, http.StatusBadRequest, err.Error(), "provisioning")
		return
	}
	ctx := r.Context()
	spec := kube.SandboxPodSpec{
		Namespace:      s.cfg.SandboxNamespace,
		WorkerImage:    s.cfg.WorkerImage,
		ServiceAccount: s.cfg.WorkerServiceAcct,
		WorkerPort:     s.cfg.WorkerPort,
		WorkerToken:    s.cfg.WorkerToken,
		PullSecretName: s.cfg.PullSecret,
		ResourceCPU:    s.cfg.ResourceCPU,
		ResourceMemory: s.cfg.ResourceMemory,
	}
	pod, created, err := kube.EnsureSandboxPod(ctx, s.cfg.Kube, &body, spec)
	if err != nil {
		s.jsonErr(w, http.StatusConflict, err.Error(), "sandbox")
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	resp := sandboxResponse(pod, s.cfg.WorkerPort)
	s.writeJSON(w, status, resp)
}

func (s *Server) handleSandboxGet(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("sandboxId"))
	if id == "" {
		s.jsonErr(w, http.StatusBadRequest, "sandboxId required", "")
		return
	}
	ctx := r.Context()
	pod, err := kube.GetSandboxByID(ctx, s.cfg.Kube, s.cfg.SandboxNamespace, id)
	if err != nil {
		if errors.IsNotFound(err) {
			s.jsonErr(w, http.StatusNotFound, "not found", "")
			return
		}
		s.jsonErr(w, http.StatusInternalServerError, err.Error(), "")
		return
	}
	s.writeJSON(w, http.StatusOK, sandboxResponse(pod, s.cfg.WorkerPort))
}

func (s *Server) handleSandboxDelete(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("sandboxId"))
	if id == "" {
		s.jsonErr(w, http.StatusBadRequest, "sandboxId required", "")
		return
	}
	ctx := r.Context()
	if err := kube.DeleteSandboxByID(ctx, s.cfg.Kube, s.cfg.SandboxNamespace, id); err != nil {
		s.jsonErr(w, http.StatusInternalServerError, err.Error(), "")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func sandboxResponse(pod *corev1.Pod, defaultPort int32) *api.SandboxResponseBody {
	owner := ""
	if pod.Labels != nil {
		owner = pod.Labels[api.LabelOwner]
	}
	sid := ""
	session := ""
	if pod.Labels != nil {
		sid = pod.Labels[api.LabelSandboxID]
		session = pod.Labels[api.LabelSessionID]
	}
	img := ""
	if len(pod.Spec.Containers) > 0 {
		img = pod.Spec.Containers[0].Image
	}
	wp := int(kube.WorkerListenPort(pod, defaultPort))
	rt := api.SandboxRuntimeInstrumented
	if pod.Labels != nil {
		if v := pod.Labels[api.LabelWorkerRuntime]; v != "" {
			rt = v
		}
	}
	return &api.SandboxResponseBody{
		SandboxID:   sid,
		SessionID:   session,
		Owner:       owner,
		Runtime:     rt,
		ExecAPIPath: api.WorkerExecAPIPath,
		Image:       img,
		WorkerPort:  wp,
		PodRef: api.PodRef{
			Namespace: pod.Namespace,
			Name:      pod.Name,
			UID:       string(pod.UID),
		},
		Phase: string(pod.Status.Phase),
		Ready: kube.PodRunningReady(pod),
	}
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
					continue
				}
				if n > 0 {
					s.log.Info("reaper deleted pods", "count", n)
				}
			}
		}
	}()
}
