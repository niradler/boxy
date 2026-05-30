package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"boxy.dev/boxy/internal/api"
	"boxy.dev/boxy/internal/nsjail"
)

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
	PTY            bool              `json:"pty,omitempty"`
}

type execResp struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
	TimedOut bool   `json:"timed_out"`
}

type fileReadReq struct {
	SandboxID string `json:"sandbox_id"`
	Path      string `json:"path"`
}

type fileReadResp struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type fileWriteReq struct {
	SandboxID string `json:"sandbox_id"`
	Path      string `json:"path"`
	Content   string `json:"content"`
	Encoding  string `json:"encoding,omitempty"`
}

type fileWriteResp struct {
	Path         string `json:"path"`
	BytesWritten int    `json:"bytes_written"`
}

type fileEditReq struct {
	SandboxID  string `json:"sandbox_id"`
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

type fileEditResp struct {
	Path         string `json:"path"`
	Replacements int    `json:"replacements"`
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
	metrics        *controllerMetrics
	metricsHandler http.Handler
}

func newServer(cfg *config, adapter nsjail.Adapter, metrics *controllerMetrics, metricsHandler http.Handler) *server {
	concurrency := cfg.maxExecConcurrency
	if concurrency <= 0 {
		concurrency = 50
	}
	return &server{
		cfg:            cfg,
		adapter:        adapter,
		execSem:        make(chan struct{}, concurrency),
		maxOutputBytes: cfg.maxOutputBytes,
		metrics:        metrics,
		metricsHandler: metricsHandler,
	}
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	if s.metricsHandler != nil {
		mux.Handle("GET /metrics", s.metricsHandler)
	}
	mux.HandleFunc("POST /v1/sandboxes", s.handleCreate)
	mux.HandleFunc("GET /v1/sandboxes", s.handleList)
	mux.HandleFunc("DELETE /v1/sandboxes", s.handleDelete)
	mux.HandleFunc("POST /v1/exec", s.handleExec)
	mux.HandleFunc("POST /v1/exec/stream", s.handleExecStream)
	mux.HandleFunc("POST /v1/files/read", s.handleFileRead)
	mux.HandleFunc("POST /v1/files/write", s.handleFileWrite)
	mux.HandleFunc("POST /v1/files/edit", s.handleFileEdit)
	return s.tokenMiddleware(mux)
}

// Defence-in-depth when mTLS is off: the token is never exposed inside the sandbox.
func (s *server) tokenMiddleware(next http.Handler) http.Handler {
	if s.cfg.controllerToken == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/metrics" {
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

	s.metrics.sandboxCreated(r.Context())
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
	s.metrics.sandboxDeleted(r.Context())
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
		s.metrics.execThrottled(r.Context())
		writeErr(w, http.StatusTooManyRequests, "concurrency limit reached, try again later")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(req.TimeoutSeconds+5)*time.Second)
	defer cancel()

	start := time.Now()
	result, err := s.adapter.Exec(ctx, req.SandboxID, req.Command, req.Args, req.Env, req.TimeoutSeconds, req.PTY)
	if err != nil {
		s.metrics.recordExec(ctx, req.SandboxID, "error", time.Since(start).Seconds(), 0)
		code, msg := adapterErrToHTTP(err)
		writeErr(w, code, msg)
		return
	}

	s.metrics.recordExec(ctx, req.SandboxID, execResult(result), time.Since(start).Seconds(), len(result.Stdout)+len(result.Stderr))
	slog.Debug("exec", "sandbox", req.SandboxID, "cmd", req.Command, "exit", result.ExitCode)

	writeJSON(w, http.StatusOK, execResp{
		Stdout:   s.truncateOutput(result.Stdout),
		Stderr:   s.truncateOutput(result.Stderr),
		ExitCode: result.ExitCode,
		TimedOut: result.TimedOut,
	})
}

func (s *server) handleExecStream(w http.ResponseWriter, r *http.Request) {
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
		s.metrics.execThrottled(r.Context())
		writeErr(w, http.StatusTooManyRequests, "concurrency limit reached, try again later")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(req.TimeoutSeconds+5)*time.Second)
	defer cancel()

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	flusher, _ := w.(http.Flusher)

	var wmu sync.Mutex
	writeEvent := func(evtType, data string) {
		type event struct {
			Type string `json:"type"`
			Data string `json:"data,omitempty"`
		}
		line, _ := json.Marshal(event{Type: evtType, Data: data})
		wmu.Lock()
		_, _ = w.Write(append(line, '\n'))
		if flusher != nil {
			flusher.Flush()
		}
		wmu.Unlock()
	}

	start := time.Now()
	result, err := s.adapter.ExecStream(ctx, req.SandboxID, req.Command, req.Args, req.Env, req.TimeoutSeconds, func(evtType, data string) {
		writeEvent(evtType, data)
	})
	if err != nil {
		s.metrics.recordExec(ctx, req.SandboxID, "error", time.Since(start).Seconds(), 0)
		writeEvent("error", err.Error())
		return
	}

	s.metrics.recordExec(ctx, req.SandboxID, execResult(result), time.Since(start).Seconds(), len(result.Stdout)+len(result.Stderr))
	slog.Debug("exec/stream", "sandbox", req.SandboxID, "cmd", req.Command, "exit", result.ExitCode)

	type exitEvent struct {
		Type     string `json:"type"`
		Code     int    `json:"code"`
		TimedOut bool   `json:"timedOut,omitempty"`
	}
	line, _ := json.Marshal(exitEvent{Type: "exit", Code: result.ExitCode, TimedOut: result.TimedOut})
	_, _ = w.Write(append(line, '\n'))
	if flusher != nil {
		flusher.Flush()
	}
}

func (s *server) handleFileRead(w http.ResponseWriter, r *http.Request) {
	var req fileReadReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if strings.TrimSpace(req.SandboxID) == "" {
		writeErr(w, http.StatusBadRequest, "sandbox_id required")
		return
	}
	if strings.TrimSpace(req.Path) == "" {
		writeErr(w, http.StatusBadRequest, "path required")
		return
	}

	select {
	case s.execSem <- struct{}{}:
		defer func() { <-s.execSem }()
	default:
		s.metrics.execThrottled(r.Context())
		writeErr(w, http.StatusTooManyRequests, "concurrency limit reached, try again later")
		return
	}

	data, truncated, err := s.adapter.ReadFile(r.Context(), req.SandboxID, req.Path)
	if err != nil {
		code, msg := adapterErrToHTTP(err)
		writeErr(w, code, msg)
		return
	}

	if truncated {
		data = trimPartialRune(data)
	}
	if !utf8.Valid(data) {
		writeErr(w, http.StatusBadRequest, "file is not valid UTF-8 text; use the bash tool for binary files")
		return
	}
	content := string(data)
	if truncated {
		content += fmt.Sprintf("\n\n[boxy: output truncated at the %d-byte read limit; use the bash tool to read the full file]", s.maxOutputBytes)
	}
	writeJSON(w, http.StatusOK, fileReadResp{Path: req.Path, Content: content})
}

func (s *server) handleFileWrite(w http.ResponseWriter, r *http.Request) {
	var req fileWriteReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if strings.TrimSpace(req.SandboxID) == "" {
		writeErr(w, http.StatusBadRequest, "sandbox_id required")
		return
	}
	if strings.TrimSpace(req.Path) == "" {
		writeErr(w, http.StatusBadRequest, "path required")
		return
	}

	select {
	case s.execSem <- struct{}{}:
		defer func() { <-s.execSem }()
	default:
		s.metrics.execThrottled(r.Context())
		writeErr(w, http.StatusTooManyRequests, "concurrency limit reached, try again later")
		return
	}

	content := req.Content
	switch strings.ToLower(strings.TrimSpace(req.Encoding)) {
	case "", "utf-8", "utf8":
	case "base64":
		decoded, err := base64.StdEncoding.DecodeString(req.Content)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid base64 content")
			return
		}
		content = string(decoded)
	default:
		writeErr(w, http.StatusBadRequest, "encoding must be 'utf-8' or 'base64'")
		return
	}

	n, err := s.adapter.WriteFile(r.Context(), req.SandboxID, req.Path, content)
	if err != nil {
		code, msg := adapterErrToHTTP(err)
		writeErr(w, code, msg)
		return
	}
	writeJSON(w, http.StatusOK, fileWriteResp{Path: req.Path, BytesWritten: n})
}

func (s *server) handleFileEdit(w http.ResponseWriter, r *http.Request) {
	var req fileEditReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if strings.TrimSpace(req.SandboxID) == "" {
		writeErr(w, http.StatusBadRequest, "sandbox_id required")
		return
	}
	if strings.TrimSpace(req.Path) == "" {
		writeErr(w, http.StatusBadRequest, "path required")
		return
	}
	if req.OldString == "" {
		writeErr(w, http.StatusBadRequest, "old_string required")
		return
	}

	select {
	case s.execSem <- struct{}{}:
		defer func() { <-s.execSem }()
	default:
		s.metrics.execThrottled(r.Context())
		writeErr(w, http.StatusTooManyRequests, "concurrency limit reached, try again later")
		return
	}

	n, err := s.adapter.EditFile(r.Context(), req.SandboxID, req.Path, req.OldString, req.NewString, req.ReplaceAll)
	if err != nil {
		code, msg := adapterErrToHTTP(err)
		writeErr(w, code, msg)
		return
	}
	writeJSON(w, http.StatusOK, fileEditResp{Path: req.Path, Replacements: n})
}

func trimPartialRune(b []byte) []byte {
	for i := 0; i < utf8.UTFMax-1 && len(b) > 0; i++ {
		if utf8.Valid(b) {
			break
		}
		b = b[:len(b)-1]
	}
	return b
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
