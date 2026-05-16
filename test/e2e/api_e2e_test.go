//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"boxy.dev/boxy/internal/api"
)

func testCreds(t *testing.T) (base, token string) {
	t.Helper()
	base = strings.TrimSuffix(strings.TrimSpace(os.Getenv("BOXY_E2E_BASE_URL")), "/")
	token = strings.TrimSpace(os.Getenv("BOXY_E2E_ROUTER_TOKEN"))
	if base == "" || token == "" {
		t.Skip("set BOXY_E2E_BASE_URL and BOXY_E2E_ROUTER_TOKEN")
	}
	return base, token
}

func httpClient() *http.Client {
	return &http.Client{Timeout: 120 * time.Second}
}

func TestHealth(t *testing.T) {
	base, _ := testCreds(t)
	resp, err := httpClient().Get(base + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func waitSessionReady(t *testing.T, base, tok, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, base+"/v1/sessions/"+sessionID, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		res, err := httpClient().Do(req)
		if err == nil && res.StatusCode == http.StatusOK {
			var cur api.SessionResponseBody
			_ = json.NewDecoder(res.Body).Decode(&cur)
			res.Body.Close()
			if cur.Ready {
				return
			}
		} else if res != nil {
			res.Body.Close()
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatal("session not ready")
}

type jsonRPCRequest struct {
	Jsonrpc string `json:"jsonrpc"`
	ID      any    `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	Jsonrpc string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func postMCP(t *testing.T, base, tok string, rpcReq jsonRPCRequest, sessionID string) jsonRPCResponse {
	t.Helper()
	payload, _ := json.Marshal(rpcReq)
	req, err := http.NewRequest(http.MethodPost, base+"/mcp", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sessionID != "" {
		req.Header.Set("X-Session-Id", sessionID)
	}
	res, err := httpClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("MCP HTTP status %d", res.StatusCode)
	}
	var resp jsonRPCResponse
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func createSandboxHelper(t *testing.T, base, tok string, body api.SandboxCreateBody) api.SandboxConfigResponse {
	t.Helper()
	payload, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/sandboxes", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	res, err := httpClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusCreated {
		t.Fatalf("create sandbox status %d", res.StatusCode)
	}
	var sb api.SandboxConfigResponse
	if err := json.NewDecoder(res.Body).Decode(&sb); err != nil {
		t.Fatal(err)
	}
	return sb
}

func deleteSession(t *testing.T, base, tok, sessionID string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, base+"/v1/sessions/"+sessionID, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	res, err := httpClient().Do(req)
	if err != nil {
		return
	}
	res.Body.Close()
}

func TestSandboxConfigAndSessionExec(t *testing.T) {
	base, tok := testCreds(t)
	sandboxID := "e2e-sandbox-" + strconv.FormatInt(time.Now().UnixNano(), 10)

	createSandboxHelper(t, base, tok, api.SandboxCreateBody{
		SandboxID:  sandboxID,
		TTLSeconds: 600,
		Env:        map[string]string{"E2E_MARKER": "provisioned"},
	})

	execBody := api.ExecRequestBody{
		SandboxID:      sandboxID,
		Command:        "echo",
		Args:           []string{"hi"},
		TimeoutSeconds: 120,
	}
	execRes := postExecRaw(t, base, tok, execBody)
	execRes.Body.Close()
	sessionID := execRes.Header.Get("X-Boxy-Session-Id")
	if sessionID == "" {
		t.Fatal("X-Boxy-Session-Id header missing from first exec response")
	}
	t.Cleanup(func() { deleteSession(t, base, tok, sessionID) })

	waitSessionReady(t, base, tok, sessionID)

	out := postExec(t, base, tok, api.ExecRequestBody{
		SandboxID:      sandboxID,
		SessionID:      sessionID,
		Command:        "sh",
		Args:           []string{"-c", "echo -n $E2E_MARKER"},
		Env:            map[string]string{},
		TimeoutSeconds: 120,
	})
	if out.Stdout != "provisioned" {
		t.Fatalf("stdout %q", out.Stdout)
	}
}

func TestMCPBashTool(t *testing.T) {
	base, tok := testCreds(t)

	sandboxID := "e2e-mcp-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	createSandboxHelper(t, base, tok, api.SandboxCreateBody{
		SandboxID:  sandboxID,
		TTLSeconds: 600,
	})

	warmupRes := postExecRaw(t, base, tok, api.ExecRequestBody{
		SandboxID:      sandboxID,
		Command:        "echo",
		Args:           []string{"hi"},
		TimeoutSeconds: 120,
	})
	warmupRes.Body.Close()
	sessionID := warmupRes.Header.Get("X-Boxy-Session-Id")
	if sessionID == "" {
		t.Fatal("X-Boxy-Session-Id header missing")
	}
	t.Cleanup(func() { deleteSession(t, base, tok, sessionID) })
	waitSessionReady(t, base, tok, sessionID)

	initResp := postMCP(t, base, tok, jsonRPCRequest{
		Jsonrpc: "2.0", ID: 1, Method: "initialize",
		Params: map[string]any{
			"protocolVersion": "2025-03-26",
			"clientInfo":      map[string]any{"name": "e2e-test", "version": "1.0.0"},
			"capabilities":    map[string]any{},
		},
	}, sessionID)
	if initResp.Error != nil {
		t.Fatalf("initialize error: %s", initResp.Error.Message)
	}

	listResp := postMCP(t, base, tok, jsonRPCRequest{
		Jsonrpc: "2.0", ID: 2, Method: "tools/list",
	}, sessionID)
	if listResp.Error != nil {
		t.Fatalf("tools/list error: %s", listResp.Error.Message)
	}
	if !bytes.Contains(listResp.Result, []byte(`"bash"`)) {
		t.Fatalf("tools/list missing bash tool: %s", listResp.Result)
	}

	callResp := postMCP(t, base, tok, jsonRPCRequest{
		Jsonrpc: "2.0", ID: 3, Method: "tools/call",
		Params: map[string]any{
			"name":      "bash",
			"arguments": map[string]any{"command": "echo -n mcp-works"},
		},
	}, sessionID)
	if callResp.Error != nil {
		t.Fatalf("tools/call error: %s", callResp.Error.Message)
	}
	if !bytes.Contains(callResp.Result, []byte("mcp-works")) {
		t.Fatalf("unexpected tools/call result: %s", callResp.Result)
	}
}

// TestMCPCrossSandboxIsolation verifies that the MCP bash tool cannot read
// files written in a different sandbox's session, even when both sandboxes
// are accessible with the same auth token.
func TestMCPCrossSandboxIsolation(t *testing.T) {
	base, tok := testCreds(t)

	ts := strconv.FormatInt(time.Now().UnixNano(), 10)
	sandboxAID := "e2e-mcp-iso-a-" + ts
	sandboxBID := "e2e-mcp-iso-b-" + ts

	createSandboxHelper(t, base, tok, api.SandboxCreateBody{
		SandboxID:  sandboxAID,
		TTLSeconds: 600,
	})
	createSandboxHelper(t, base, tok, api.SandboxCreateBody{
		SandboxID:  sandboxBID,
		TTLSeconds: 600,
	})

	warmupA := postExecRaw(t, base, tok, api.ExecRequestBody{
		SandboxID: sandboxAID, Command: "echo", Args: []string{"hi"}, TimeoutSeconds: 120,
	})
	warmupA.Body.Close()
	sessionAID := warmupA.Header.Get("X-Boxy-Session-Id")
	if sessionAID == "" {
		t.Fatal("X-Boxy-Session-Id header missing for sandbox A")
	}
	t.Cleanup(func() { deleteSession(t, base, tok, sessionAID) })
	waitSessionReady(t, base, tok, sessionAID)

	warmupB := postExecRaw(t, base, tok, api.ExecRequestBody{
		SandboxID: sandboxBID, Command: "echo", Args: []string{"hi"}, TimeoutSeconds: 120,
	})
	warmupB.Body.Close()
	sessionBID := warmupB.Header.Get("X-Boxy-Session-Id")
	if sessionBID == "" {
		t.Fatal("X-Boxy-Session-Id header missing for sandbox B")
	}
	t.Cleanup(func() { deleteSession(t, base, tok, sessionBID) })
	waitSessionReady(t, base, tok, sessionBID)

	writeResp := postMCP(t, base, tok, jsonRPCRequest{
		Jsonrpc: "2.0", ID: 1, Method: "tools/call",
		Params: map[string]any{
			"name":      "bash",
			"arguments": map[string]any{"command": "echo cross-sandbox-secret > /workspace/secret.txt && echo ok"},
		},
	}, sessionAID)
	if writeResp.Error != nil {
		t.Fatalf("write via MCP error: %s", writeResp.Error.Message)
	}
	if !bytes.Contains(writeResp.Result, []byte("ok")) {
		t.Fatalf("unexpected write result: %s", writeResp.Result)
	}

	readResp := postMCP(t, base, tok, jsonRPCRequest{
		Jsonrpc: "2.0", ID: 2, Method: "tools/call",
		Params: map[string]any{
			"name":      "bash",
			"arguments": map[string]any{"command": "cat /workspace/secret.txt 2>/dev/null || echo absent"},
		},
	}, sessionBID)
	if readResp.Error != nil {
		t.Fatalf("read via MCP error: %s", readResp.Error.Message)
	}

	var toolRes struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(readResp.Result, &toolRes); err != nil {
		t.Fatalf("parse tool result: %v", err)
	}
	got := ""
	if len(toolRes.Content) > 0 {
		got = strings.TrimSpace(toolRes.Content[0].Text)
	}
	if got != "absent" {
		t.Fatalf("cross-sandbox isolation FAILED: sandbox B read sandbox A's file, got %q", got)
	}

	// Invalid session ID must return a tool-level error (not HTTP 500).
	noSuchResp := postMCP(t, base, tok, jsonRPCRequest{
		Jsonrpc: "2.0", ID: 3, Method: "tools/call",
		Params: map[string]any{
			"name":      "bash",
			"arguments": map[string]any{"command": "echo hi"},
		},
	}, "session-does-not-exist")
	if noSuchResp.Error != nil {
		t.Fatalf("expected tool-level error, got rpc error: %s", noSuchResp.Error.Message)
	}
	var noSuchTool struct {
		IsError bool `json:"isError"`
	}
	_ = json.Unmarshal(noSuchResp.Result, &noSuchTool)
	if !noSuchTool.IsError {
		t.Fatal("expected tool-level error for non-existent session, got success")
	}
}

// TestExecTimedOut verifies that timedOut=true and exitCode=137 are returned
// when a command exceeds its configured timeout.
func TestExecTimedOut(t *testing.T) {
	base, tok := testCreds(t)
	ts := strconv.FormatInt(time.Now().UnixNano(), 10)
	sandboxID := "e2e-timeout-" + ts

	createSandboxHelper(t, base, tok, api.SandboxCreateBody{
		SandboxID:  sandboxID,
		TTLSeconds: 120,
	})

	// First exec auto-creates the session.
	warmupRes := postExecRaw(t, base, tok, api.ExecRequestBody{
		SandboxID:      sandboxID,
		Command:        "echo",
		Args:           []string{"hi"},
		TimeoutSeconds: 120,
	})
	warmupRes.Body.Close()
	sessionID := warmupRes.Header.Get("X-Boxy-Session-Id")
	if sessionID == "" {
		t.Fatal("X-Boxy-Session-Id header missing")
	}
	t.Cleanup(func() { deleteSession(t, base, tok, sessionID) })

	waitSessionReady(t, base, tok, sessionID)

	out := postExec(t, base, tok, api.ExecRequestBody{
		SandboxID:      sandboxID,
		SessionID:      sessionID,
		Command:        "sleep",
		Args:           []string{"60"},
		TimeoutSeconds: 2,
	})
	if !out.TimedOut {
		t.Fatalf("expected timedOut=true, got false (exitCode=%d stdout=%q stderr=%q)", out.ExitCode, out.Stdout, out.Stderr)
	}
	if out.ExitCode != 137 {
		t.Fatalf("expected exitCode=137 (SIGKILL), got %d", out.ExitCode)
	}
}

// TestInternetAccessDNS verifies that a sandbox with allowInternetAccess=true
// has a populated /etc/resolv.conf and can resolve hostnames.
func TestInternetAccessDNS(t *testing.T) {
	base, tok := testCreds(t)
	ts := strconv.FormatInt(time.Now().UnixNano(), 10)
	sandboxID := "e2e-dns-" + ts

	createSandboxHelper(t, base, tok, api.SandboxCreateBody{
		SandboxID:  sandboxID,
		TTLSeconds: 120,
		Network:    &api.SandboxNetworkConfig{AllowInternetAccess: true},
	})

	// First exec auto-creates the session.
	warmupRes := postExecRaw(t, base, tok, api.ExecRequestBody{
		SandboxID:      sandboxID,
		Command:        "echo",
		Args:           []string{"hi"},
		TimeoutSeconds: 120,
	})
	warmupRes.Body.Close()
	sessionID := warmupRes.Header.Get("X-Boxy-Session-Id")
	if sessionID == "" {
		t.Fatal("X-Boxy-Session-Id header missing")
	}
	t.Cleanup(func() { deleteSession(t, base, tok, sessionID) })

	waitSessionReady(t, base, tok, sessionID)

	// /etc/resolv.conf must have nameserver entries - validates the resolv.conf bind-mount.
	resolvOut := postExec(t, base, tok, api.ExecRequestBody{
		SandboxID:      sandboxID,
		SessionID:      sessionID,
		Command:        "sh",
		Args:           []string{"-c", "grep -c nameserver /etc/resolv.conf 2>/dev/null || echo 0"},
		TimeoutSeconds: 10,
	})
	if strings.TrimSpace(resolvOut.Stdout) == "0" {
		t.Fatalf("/etc/resolv.conf has no nameserver entries - resolv.conf bind-mount not applied (stderr=%q)", resolvOut.Stderr)
	}

	// DNS resolution requires controller.networkPolicy.allowInternetEgress=true.
	// Skip gracefully if egress is blocked at the network layer.
	dnsOut := postExec(t, base, tok, api.ExecRequestBody{
		SandboxID:      sandboxID,
		SessionID:      sessionID,
		Command:        "sh",
		Args:           []string{"-c", "getent hosts example.com 2>/dev/null | head -1 | awk '{print $1}' || echo blocked"},
		TimeoutSeconds: 15,
	})
	ip := strings.TrimSpace(dnsOut.Stdout)
	if ip == "blocked" || ip == "" {
		t.Skip("DNS resolution blocked - set controller.networkPolicy.allowInternetEgress=true to test end-to-end")
	}
	if !strings.Contains(ip, ".") && !strings.Contains(ip, ":") {
		t.Fatalf("DNS resolved to unexpected output %q", ip)
	}
}

// postExecRaw posts to /v1/sessions/exec and returns the raw response.
// Caller must close the body.
func postExecRaw(t *testing.T, base, tok string, body api.ExecRequestBody) *http.Response {
	t.Helper()
	payload, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, base+"/v1/sessions/exec", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	res, err := httpClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		t.Fatalf("exec status %d", res.StatusCode)
	}
	return res
}

func postExec(t *testing.T, base, tok string, body api.ExecRequestBody) api.ExecResponseBody {
	t.Helper()
	res := postExecRaw(t, base, tok, body)
	defer res.Body.Close()
	var out api.ExecResponseBody
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}
