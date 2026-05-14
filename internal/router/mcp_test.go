package router

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"boxy.dev/boxy/internal/api"
	"boxy.dev/boxy/internal/kube"
)

func newTestServer(t *testing.T, kc *fake.Clientset, controllerURL string, opts ...func(*Config)) *Server {
	t.Helper()
	u, err := url.Parse(controllerURL)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(u.Port())
	cfg := Config{
		AuthToken:                 "test-token",
		SandboxNamespace:          "default",
		MaxConcurrency:            10,
		MaxBodyBytes:              1 << 20,
		MaxTimeoutSec:             3600,
		MaxSandboxTTLSec:          86400,
		Kube:                      kc,
		ControllerPort:            int32(port),
		ControllerTTLSec:          3600,
		MaxSandboxesPerController: 20,
		MTLSDisabled:              true,
	}
	for _, o := range opts {
		o(&cfg)
	}
	return NewServer(cfg)
}

type rpcResp struct {
	Jsonrpc string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type toolResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

func mcpPost(t *testing.T, handler http.Handler, method string, params any, sandboxID string) (int, rpcResp) {
	t.Helper()
	req := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		req["params"] = params
	}
	body, _ := json.Marshal(req)
	httpReq := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	httpReq.Header.Set("Authorization", "Bearer test-token")
	if sandboxID != "" {
		httpReq.Header.Set("X-Sandbox-Id", sandboxID)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httpReq)
	if w.Code != http.StatusOK {
		return w.Code, rpcResp{}
	}
	var resp rpcResp
	_ = json.NewDecoder(w.Body).Decode(&resp)
	return w.Code, resp
}

var initializeParams = map[string]any{
	"protocolVersion": "2025-03-26",
	"clientInfo":      map[string]any{"name": "test", "version": "1.0.0"},
	"capabilities":    map[string]any{},
}

func mcpInitialize(t *testing.T, handler http.Handler) {
	t.Helper()
	code, resp := mcpPost(t, handler, "initialize", initializeParams, "")
	if code != http.StatusOK || resp.Error != nil {
		t.Fatalf("initialize failed: code=%d err=%+v", code, resp.Error)
	}
}

func parseToolResult(t *testing.T, raw json.RawMessage) toolResult {
	t.Helper()
	var tr toolResult
	if err := json.Unmarshal(raw, &tr); err != nil {
		t.Fatalf("parse tool result: %v (raw: %s)", err, raw)
	}
	return tr
}

func TestMCP_Initialize(t *testing.T) {
	kc := fake.NewSimpleClientset()
	ctrl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ctrl.Close()
	srv := newTestServer(t, kc, ctrl.URL)
	handler := srv.Handler()

	code, resp := mcpPost(t, handler, "initialize", initializeParams, "")
	if code != http.StatusOK {
		t.Fatalf("HTTP %d", code)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
		Capabilities struct {
			Tools any `json:"tools"`
		} `json:"capabilities"`
	}
	_ = json.Unmarshal(resp.Result, &result)
	if result.ServerInfo.Name != "boxy" {
		t.Fatalf("serverInfo.name = %q", result.ServerInfo.Name)
	}
	if result.Capabilities.Tools == nil {
		t.Fatal("capabilities.tools should be present")
	}
}

func TestMCP_ToolsList(t *testing.T) {
	kc := fake.NewSimpleClientset()
	ctrl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ctrl.Close()
	srv := newTestServer(t, kc, ctrl.URL)
	handler := srv.Handler()

	code, resp := mcpPost(t, handler, "tools/list", nil, "")
	if code != http.StatusOK {
		t.Fatalf("HTTP %d", code)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var result struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	_ = json.Unmarshal(resp.Result, &result)
	if len(result.Tools) != 1 || result.Tools[0].Name != "bash" {
		t.Fatalf("unexpected tools: %s", resp.Result)
	}
}

func TestMCP_ToolsCall_BashSuccess(t *testing.T) {
	ctrl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/exec" {
			_ = json.NewEncoder(w).Encode(ExecResult{Stdout: "hello", ExitCode: 0})
			return
		}
		http.NotFound(w, r)
	}))
	defer ctrl.Close()
	u, _ := url.Parse(ctrl.URL)
	port, _ := strconv.Atoi(u.Port())
	pod := newControllerPod("ctrl-1", u.Hostname(), int32(port))
	kc := fake.NewSimpleClientset(pod)
	srv := newTestServer(t, kc, ctrl.URL)
	_ = srv.store.Set(context.Background(), "sb-1", kube.SandboxRoute{
		ControllerPodName: "ctrl-1", ControllerIP: u.Hostname(), Port: int32(port),
	})

	handler := srv.Handler()
	code, resp := mcpPost(t, handler, "tools/call", map[string]any{
		"name":      "bash",
		"arguments": map[string]any{"command": "echo hello"},
	}, "sb-1")

	if code != http.StatusOK {
		t.Fatalf("HTTP %d", code)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected rpc error: %+v", resp.Error)
	}
	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("tool returned error: %v", tr.Content)
	}
	if len(tr.Content) == 0 || tr.Content[0].Text != "hello" {
		t.Fatalf("unexpected content: %+v", tr.Content)
	}
}

func TestMCP_ToolsCall_UsesDefaultSandbox(t *testing.T) {
	ctrl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/exec" {
			_ = json.NewEncoder(w).Encode(ExecResult{Stdout: "from-default", ExitCode: 0})
			return
		}
		http.NotFound(w, r)
	}))
	defer ctrl.Close()
	u, _ := url.Parse(ctrl.URL)
	port, _ := strconv.Atoi(u.Port())
	pod := newControllerPod("ctrl-1", u.Hostname(), int32(port))
	kc := fake.NewSimpleClientset(pod)
	srv := newTestServer(t, kc, ctrl.URL, func(cfg *Config) {
		cfg.DefaultSandboxEnabled = true
		cfg.DefaultSandboxConfig = &api.SandboxCreateBody{
			SandboxID: "default", SessionID: "default-box", Owner: "system", TTLSeconds: 86400,
		}
	})
	_ = srv.store.Set(context.Background(), "default", kube.SandboxRoute{
		ControllerPodName: "ctrl-1", ControllerIP: u.Hostname(), Port: int32(port),
	})

	handler := srv.Handler()
	code, resp := mcpPost(t, handler, "tools/call", map[string]any{
		"name":      "bash",
		"arguments": map[string]any{"command": "echo hi"},
	}, "")

	if code != http.StatusOK {
		t.Fatalf("HTTP %d", code)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected rpc error: %+v", resp.Error)
	}
	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("tool returned error: %v", tr.Content)
	}
	if len(tr.Content) == 0 || tr.Content[0].Text != "from-default" {
		t.Fatalf("unexpected content: %+v", tr.Content)
	}
}

func TestMCP_ToolsCall_NoSandbox_DefaultDisabled(t *testing.T) {
	kc := fake.NewSimpleClientset()
	ctrl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ctrl.Close()
	srv := newTestServer(t, kc, ctrl.URL)
	handler := srv.Handler()

	code, resp := mcpPost(t, handler, "tools/call", map[string]any{
		"name":      "bash",
		"arguments": map[string]any{"command": "echo hi"},
	}, "")

	if code != http.StatusOK {
		t.Fatalf("HTTP %d", code)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected rpc-level error: %+v", resp.Error)
	}
	tr := parseToolResult(t, resp.Result)
	if !tr.IsError {
		t.Fatal("expected tool-level error when no sandbox and default disabled")
	}
}

func TestMCP_Auth(t *testing.T) {
	kc := fake.NewSimpleClientset()
	ctrl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ctrl.Close()
	srv := newTestServer(t, kc, ctrl.URL)
	handler := srv.Handler()

	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": initializeParams,
	})
	httpReq := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httpReq)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}
