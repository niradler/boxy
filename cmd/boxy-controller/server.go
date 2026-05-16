package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"boxy.dev/boxy/internal/api"
	"boxy.dev/boxy/internal/nsjail"
)

// Wire types match the snake_case JSON format used by internal/controller/client.go.

type createSandboxReq struct {
	SandboxID       string                    `json:"sandbox_id"`
	Env             map[string]string         `json:"env,omitempty"`
	AllowedBinaries []string                  `json:"allowed_binaries,omitempty"`
	VM              *api.VMConfig             `json:"vm,omitempty"`
	Network         *api.SandboxNetworkConfig `json:"network,omitempty"`
	Volumes         []api.VolumeMount         `json:"volumes,omitempty"`
	Patches         []api.SandboxPatch        `json:"patches,omitempty"`
	TTLSeconds      int                       `json:"ttl_seconds,omitempty"`
	SetupScript     string                    `json:"setup_script,omitempty"`
	TeardownScript  string                    `json:"teardown_script,omitempty"`
	ScriptEnv       map[string]string         `json:"script_env,omitempty"`
}

type createSandboxResp struct {
	SandboxID string `json:"sandbox_id"`
}

type execReq struct {
	SandboxID      string            `json:"sandbox_id"`
	Command        string            `json:"command"`
	Args           []string          `json:"args,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
}

type execResp struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
	TimedOut bool   `json:"timed_out"`
}

type deleteSandboxReq struct {
	SandboxID string `json:"sandbox_id"`
}

type sandboxSummary struct {
	SandboxID string `json:"sandbox_id"`
}

type listSandboxesResp struct {
	Sandboxes []sandboxSummary `json:"sandboxes"`
}

type server struct {
	cfg            *config
	adapter        nsjail.Adapter
	execSem        chan struct{}
	maxOutputBytes int
}

func newServer(cfg *config, adapter nsjail.Adapter) *server {
	concurrency := cfg.maxExecConcurrency
	if concurrency <= 0 {
		concurrency = 50
	}
	return &server{
		cfg:            cfg,
		adapter:        adapter,
		execSem:        make(chan struct{}, concurrency),
		maxOutputBytes: cfg.maxOutputBytes,
	}
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("POST /v1/sandboxes", s.handleCreate)
	mux.HandleFunc("GET /v1/sandboxes", s.handleList)
	mux.HandleFunc("DELETE /v1/sandboxes", s.handleDelete)
	mux.HandleFunc("POST /v1/exec", s.handleExec)
	return s.tokenMiddleware(mux)
}

// tokenMiddleware enforces BOXY_CONTROLLER_TOKEN on all non-healthz endpoints.
// This is a defence-in-depth layer: even when mTLS is disabled (e.g. in dev),
// a sandbox that shares the controller pod's network namespace cannot call the
// controller API because the token is never exposed inside the sandbox.
func (s *server) tokenMiddleware(next http.Handler) http.Handler {
	if s.cfg.controllerToken == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		if r.Header.Get("X-Boxy-Controller-Token") != s.cfg.controllerToken {
			writeErr(w, http.StatusUnauthorized, "missing or invalid controller token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (s *server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req createSandboxReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if strings.TrimSpace(req.SandboxID) == "" {
		writeErr(w, http.StatusBadRequest, "sandbox_id required")
		return
	}
	if s.adapter.Count() >= s.cfg.maxSandboxes {
		writeErr(w, http.StatusServiceUnavailable, "controller at capacity")
		return
	}

	body := &api.SandboxCreateBody{
		SandboxID:       req.SandboxID,
		Env:             req.Env,
		AllowedBinaries: req.AllowedBinaries,
		VM:              req.VM,
		Network:         req.Network,
		Volumes:         req.Volumes,
		Patches:         req.Patches,
		TTLSeconds:      req.TTLSeconds,
		SetupScript:     req.SetupScript,
		TeardownScript:  req.TeardownScript,
		ScriptEnv:       req.ScriptEnv,
	}

	if err := s.adapter.Create(r.Context(), body); err != nil {
		code, msg := adapterErrToHTTP(err)
		writeErr(w, code, msg)
		return
	}

	writeJSON(w, http.StatusCreated, createSandboxResp{SandboxID: req.SandboxID})
}

func (s *server) handleList(w http.ResponseWriter, r *http.Request) {
	ids := s.adapter.ListIDs()
	summaries := make([]sandboxSummary, len(ids))
	for i, id := range ids {
		summaries[i] = sandboxSummary{SandboxID: id}
	}
	writeJSON(w, http.StatusOK, listSandboxesResp{Sandboxes: summaries})
}

func (s *server) handleDelete(w http.ResponseWriter, r *http.Request) {
	var req deleteSandboxReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if strings.TrimSpace(req.SandboxID) == "" {
		writeErr(w, http.StatusBadRequest, "sandbox_id required")
		return
	}
	if err := s.adapter.Delete(r.Context(), req.SandboxID); err != nil {
		code, msg := adapterErrToHTTP(err)
		writeErr(w, code, msg)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleExec(w http.ResponseWriter, r *http.Request) {
	var req execReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if strings.TrimSpace(req.SandboxID) == "" {
		writeErr(w, http.StatusBadRequest, "sandbox_id required")
		return
	}
	if strings.TrimSpace(req.Command) == "" {
		writeErr(w, http.StatusBadRequest, "command required")
		return
	}
	if req.TimeoutSeconds <= 0 {
		req.TimeoutSeconds = 30
	}

	select {
	case s.execSem <- struct{}{}:
		defer func() { <-s.execSem }()
	default:
		writeErr(w, http.StatusTooManyRequests, "concurrency limit reached, try again later")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(req.TimeoutSeconds+5)*time.Second)
	defer cancel()

	result, err := s.adapter.Exec(ctx, req.SandboxID, req.Command, req.Args, req.Env, req.TimeoutSeconds)
	if err != nil {
		code, msg := adapterErrToHTTP(err)
		writeErr(w, code, msg)
		return
	}

	slog.Debug("exec", "sandbox", req.SandboxID, "cmd", req.Command, "exit", result.ExitCode)

	writeJSON(w, http.StatusOK, execResp{
		Stdout:   s.truncateOutput(result.Stdout),
		Stderr:   s.truncateOutput(result.Stderr),
		ExitCode: result.ExitCode,
		TimedOut: result.TimedOut,
	})
}

func (s *server) truncateOutput(out string) string {
	if s.maxOutputBytes <= 0 || len(out) <= s.maxOutputBytes {
		return out
	}
	const notice = "\n[output truncated]"
	cut := s.maxOutputBytes - len(notice)
	if cut < 0 {
		cut = 0
	}
	return out[:cut] + notice
}

func adapterErrToHTTP(err error) (int, string) {
	var ae *nsjail.AdapterError
	if errors.As(err, &ae) {
		return ae.Code, ae.Message
	}
	return http.StatusInternalServerError, err.Error()
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, api.ErrorBody{Error: msg})
}
