package router

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	boxyv1 "boxy.dev/boxy/api/v1alpha1"
	"boxy.dev/boxy/internal/api"
	ctrlclient "boxy.dev/boxy/internal/controller"
)

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = boxyv1.AddToScheme(s)
	return s
}

func newTestServer(t *testing.T, controllerURL string, objs []runtime.Object, opts ...func(*Config)) *Server {
	t.Helper()
	u, err := url.Parse(controllerURL)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(u.Port())

	scheme := testScheme()
	fc := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).WithStatusSubresource(&boxyv1.Sandbox{}).Build()

	cfg := Config{
		DevToken:         "test-token",
		SandboxNamespace: "default",
		MaxConcurrency:   10,
		MaxBodyBytes:     1 << 20,
		MaxTimeoutSec:    3600,
		MaxSandboxTTLSec: 86400,
		ControllerPort:   int32(port),
		MTLSDisabled:     true,
	}
	for _, o := range opts {
		o(&cfg)
	}

	return &Server{
		cfg:       cfg,
		log:       slog.Default(),
		sem:       make(chan struct{}, cfg.MaxConcurrency),
		k8sClient: fc,
		k8sReader: fc,
		auth:      newTokenReviewer(kubefake.NewSimpleClientset(), 30*time.Second, "test-token"),
		ctrlClient: ctrlclient.NewClient(ctrlclient.ClientConfig{
			MTLSDisabled: true,
		}),
	}
}

func testSandbox(name, sandboxID, ctrlAddr string, port int32, phase boxyv1.SandboxPhase) *boxyv1.Sandbox {
	return &boxyv1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels:    map[string]string{boxyv1.LabelSandboxID: sandboxID},
		},
		Spec: boxyv1.SandboxSpec{
			SandboxID: sandboxID,
			SessionID: "test-session",
			Owner:     "test",
		},
		Status: boxyv1.SandboxStatus{
			Phase:             phase,
			ControllerPod:     "ctrl-0",
			ControllerAddress: ctrlAddr,
			Port:              port,
		},
	}
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

func parseToolResult(t *testing.T, raw json.RawMessage) toolResult {
	t.Helper()
	var tr toolResult
	if err := json.Unmarshal(raw, &tr); err != nil {
		t.Fatalf("parse tool result: %v (raw: %s)", err, raw)
	}
	return tr
}

func TestMCP_Initialize(t *testing.T) {
	ctrl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ctrl.Close()
	srv := newTestServer(t, ctrl.URL, nil)
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
	ctrl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ctrl.Close()
	srv := newTestServer(t, ctrl.URL, nil)
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
	sb := testSandbox("sb-1", "sb-1", u.Hostname(), int32(port), boxyv1.SandboxPhaseRunning)

	srv := newTestServer(t, ctrl.URL, []runtime.Object{sb})
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
	sb := testSandbox("default", "default", u.Hostname(), int32(port), boxyv1.SandboxPhaseRunning)

	srv := newTestServer(t, ctrl.URL, []runtime.Object{sb}, func(cfg *Config) {
		cfg.DefaultSandboxEnabled = true
		cfg.DefaultSandboxConfig = &api.SandboxCreateBody{
			SandboxID: "default", SessionID: "default-box", Owner: "system", TTLSeconds: 86400,
		}
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
	ctrl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ctrl.Close()
	srv := newTestServer(t, ctrl.URL, nil)
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
	ctrl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ctrl.Close()
	srv := newTestServer(t, ctrl.URL, nil)
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
