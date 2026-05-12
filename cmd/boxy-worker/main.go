package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"boxy.dev/boxy/internal/api"
	ibexec "boxy.dev/boxy/internal/exec"
)

func main() {
	port := strings.TrimSpace(os.Getenv("BOXY_WORKER_PORT"))
	if port == "" {
		port = "8080"
	}
	token := strings.TrimSpace(os.Getenv("BOXY_WORKER_TOKEN"))
	if token == "" {
		slog.Error("BOXY_WORKER_TOKEN required")
		os.Exit(1)
	}
	maxBody := envInt("BOXY_MAX_BODY_BYTES", 1<<20)
	maxOut := envInt("BOXY_MAX_OUTPUT_BYTES", 2<<20)
	maxTimeout := envInt("BOXY_MAX_TIMEOUT_SECONDS", 3600)
	maxArgs := envInt("BOXY_MAX_ARGS", 256)
	maxEnv := envInt("BOXY_MAX_ENV_KEYS", 64)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	h := func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		const p = "Bearer "
		if !strings.HasPrefix(h, p) || strings.TrimSpace(h[len(p):]) != token {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if maxBody > 0 {
			r.Body = http.MaxBytesReader(w, r.Body, int64(maxBody))
		}
		var body api.ExecRequestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid json")
			return
		}
		if err := api.ValidateExecRequest(&body, maxTimeout, maxArgs, maxEnv); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		script, err := ibexec.BuildRemoteShell(body.Command, body.Args, body.Env)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Duration(body.TimeoutSeconds)*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "/bin/sh", "-lc", script)
		stdout := ibexec.NewLimitedWriter(maxOut)
		stderr := ibexec.NewLimitedWriter(maxOut)
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		runErr := cmd.Run()
		if stdout.HitLimit() || stderr.HitLimit() {
			writeErr(w, http.StatusRequestEntityTooLarge, "output limit exceeded")
			return
		}
		code := 0
		if runErr != nil {
			if ee, ok := runErr.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else {
				code = 1
				_, _ = fmt.Fprintf(stderr, "%s\n", runErr.Error())
			}
		}
		resp := api.ExecResponseBody{ExitCode: code, Stdout: string(stdout.Bytes()), Stderr: string(stderr.Bytes())}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
	mux.HandleFunc("POST /v1/exec", h)
	addr := ":" + port
	slog.Info("worker listening", "addr", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		slog.Error("http", "err", err)
		os.Exit(1)
	}
}

func envInt(k string, def int) int {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(api.ErrorBody{Error: msg})
}
