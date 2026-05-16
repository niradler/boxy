//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
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

func kubectlExec(t *testing.T, pod, namespace, command string) string {
	t.Helper()
	out, err := exec.Command("kubectl", "-n", namespace, "exec", pod, "--", "sh", "-c", command).CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl exec %s: %v\n%s", command, err, out)
	}
	return strings.TrimSpace(string(out))
}

func createSessionForSandbox(t *testing.T, base, tok, sandboxID string) string {
	t.Helper()
	warmup := postExecRaw(t, base, tok, api.ExecRequestBody{
		SandboxID: sandboxID, Command: "echo", Args: []string{"hi"}, TimeoutSeconds: 120,
	})
	warmup.Body.Close()
	sid := warmup.Header.Get("X-Boxy-Session-Id")
	if sid == "" {
		t.Fatal("X-Boxy-Session-Id missing")
	}
	t.Cleanup(func() { deleteSession(t, base, tok, sid) })
	waitSessionReady(t, base, tok, sid)
	return sid
}

func TestSetupScript_WritesMarkerFile(t *testing.T) {
	base, tok := testCreds(t)
	ts := strconv.FormatInt(time.Now().UnixNano(), 10)
	sandboxID := "e2e-hook-marker-" + ts

	scriptPath := "/tmp/boxy-test-setup-" + ts + ".sh"
	kubectlExec(t, "boxy-ctrl-0", "boxy",
		"printf '#!/bin/sh\\necho hook-was-here > \"$BOXY_WORKSPACE/hook-marker.txt\"\\n' > "+scriptPath+" && chmod +x "+scriptPath)
	t.Cleanup(func() {
		kubectlExec(t, "boxy-ctrl-0", "boxy", "rm -f "+scriptPath)
	})

	createSandboxHelper(t, base, tok, api.SandboxCreateBody{
		SandboxID:   sandboxID,
		TTLSeconds:  120,
		SetupScript: scriptPath,
	})

	sessionID := createSessionForSandbox(t, base, tok, sandboxID)

	out := postExec(t, base, tok, api.ExecRequestBody{
		SandboxID: sandboxID, SessionID: sessionID,
		Command: "cat", Args: []string{"/workspace/hook-marker.txt"},
		TimeoutSeconds: 10,
	})
	if strings.TrimSpace(out.Stdout) != "hook-was-here" {
		t.Fatalf("setup script did not write marker: stdout=%q stderr=%q", out.Stdout, out.Stderr)
	}
}

func TestSetupScript_ScriptEnvPerSandbox(t *testing.T) {
	base, tok := testCreds(t)
	ts := strconv.FormatInt(time.Now().UnixNano(), 10)

	scriptPath := "/tmp/boxy-test-env-" + ts + ".sh"
	kubectlExec(t, "boxy-ctrl-0", "boxy",
		"printf '#!/bin/sh\\necho \"$CUSTOMER_TIER\" > \"$BOXY_WORKSPACE/tier.txt\"\\n' > "+scriptPath+" && chmod +x "+scriptPath)
	t.Cleanup(func() {
		kubectlExec(t, "boxy-ctrl-0", "boxy", "rm -f "+scriptPath)
	})

	sandboxA := "e2e-hook-env-a-" + ts
	sandboxB := "e2e-hook-env-b-" + ts

	createSandboxHelper(t, base, tok, api.SandboxCreateBody{
		SandboxID: sandboxA, TTLSeconds: 120,
		SetupScript: scriptPath,
		ScriptEnv:   map[string]string{"CUSTOMER_TIER": "premium"},
	})
	createSandboxHelper(t, base, tok, api.SandboxCreateBody{
		SandboxID: sandboxB, TTLSeconds: 120,
		SetupScript: scriptPath,
		ScriptEnv:   map[string]string{"CUSTOMER_TIER": "free"},
	})

	sidA := createSessionForSandbox(t, base, tok, sandboxA)
	sidB := createSessionForSandbox(t, base, tok, sandboxB)

	outA := postExec(t, base, tok, api.ExecRequestBody{
		SandboxID: sandboxA, SessionID: sidA,
		Command: "cat", Args: []string{"/workspace/tier.txt"},
		TimeoutSeconds: 10,
	})
	outB := postExec(t, base, tok, api.ExecRequestBody{
		SandboxID: sandboxB, SessionID: sidB,
		Command: "cat", Args: []string{"/workspace/tier.txt"},
		TimeoutSeconds: 10,
	})

	if strings.TrimSpace(outA.Stdout) != "premium" {
		t.Fatalf("sandbox A: expected premium, got %q", outA.Stdout)
	}
	if strings.TrimSpace(outB.Stdout) != "free" {
		t.Fatalf("sandbox B: expected free, got %q", outB.Stdout)
	}
}

func TestSetupScript_ReadsStdinConfig(t *testing.T) {
	base, tok := testCreds(t)
	ts := strconv.FormatInt(time.Now().UnixNano(), 10)
	sandboxID := "e2e-hook-stdin-" + ts

	scriptPath := "/tmp/boxy-test-stdin-" + ts + ".sh"
	kubectlExec(t, "boxy-ctrl-0", "boxy",
		"printf '#!/bin/sh\\ncat > \"$BOXY_WORKSPACE/config-dump.json\"\\n' > "+scriptPath+" && chmod +x "+scriptPath)
	t.Cleanup(func() {
		kubectlExec(t, "boxy-ctrl-0", "boxy", "rm -f "+scriptPath)
	})

	createSandboxHelper(t, base, tok, api.SandboxCreateBody{
		SandboxID:   sandboxID,
		TTLSeconds:  120,
		SetupScript: scriptPath,
		Env:         map[string]string{"APP_MODE": "test"},
	})

	sessionID := createSessionForSandbox(t, base, tok, sandboxID)

	out := postExec(t, base, tok, api.ExecRequestBody{
		SandboxID: sandboxID, SessionID: sessionID,
		Command: "sh", Args: []string{"-c", "cat /workspace/config-dump.json"},
		TimeoutSeconds: 10,
	})

	var dumped map[string]interface{}
	if err := json.Unmarshal([]byte(out.Stdout), &dumped); err != nil {
		t.Fatalf("stdin config is not valid JSON: %v\nstdout: %s", err, out.Stdout)
	}
	envMap, _ := dumped["env"].(map[string]interface{})
	if envMap["APP_MODE"] != "test" {
		t.Fatalf("config env mismatch: got %v", envMap)
	}
	if dumped["setupScript"] != scriptPath {
		t.Fatalf("config setupScript mismatch: got %v", dumped["setupScript"])
	}
}

func TestSetupScript_FailureBlocksSession(t *testing.T) {
	base, tok := testCreds(t)
	ts := strconv.FormatInt(time.Now().UnixNano(), 10)
	sandboxID := "e2e-hook-fail-" + ts

	scriptPath := "/tmp/boxy-test-fail-" + ts + ".sh"
	kubectlExec(t, "boxy-ctrl-0", "boxy",
		"printf '#!/bin/sh\\nexit 1\\n' > "+scriptPath+" && chmod +x "+scriptPath)
	t.Cleanup(func() {
		kubectlExec(t, "boxy-ctrl-0", "boxy", "rm -f "+scriptPath)
	})

	createSandboxHelper(t, base, tok, api.SandboxCreateBody{
		SandboxID:   sandboxID,
		TTLSeconds:  120,
		SetupScript: scriptPath,
	})

	execPayload, _ := json.Marshal(api.ExecRequestBody{
		SandboxID: sandboxID, Command: "echo", Args: []string{"hi"}, TimeoutSeconds: 30,
	})
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/sessions/exec", bytes.NewReader(execPayload))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	res, err := httpClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	sid := res.Header.Get("X-Boxy-Session-Id")
	res.Body.Close()

	if sid == "" {
		if res.StatusCode >= 400 {
			return
		}
		t.Fatal("no session ID and no error")
	}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, base+"/v1/sessions/"+sid, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := httpClient().Do(req)
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}
		var sess api.SessionResponseBody
		json.NewDecoder(resp.Body).Decode(&sess)
		resp.Body.Close()
		if sess.Ready {
			t.Fatal("session should not become ready when setup script fails")
		}
		if resp.StatusCode == http.StatusNotFound {
			return
		}
		time.Sleep(2 * time.Second)
	}
}

func TestTeardownScript_Runs(t *testing.T) {
	base, tok := testCreds(t)
	ts := strconv.FormatInt(time.Now().UnixNano(), 10)
	sandboxID := "e2e-hook-teardown-" + ts
	markerFile := "/tmp/boxy-teardown-marker-" + ts

	setupPath := "/tmp/boxy-test-td-setup-" + ts + ".sh"
	teardownPath := "/tmp/boxy-test-td-tear-" + ts + ".sh"

	kubectlExec(t, "boxy-ctrl-0", "boxy",
		"printf '#!/bin/sh\\necho setup-done > \"$BOXY_WORKSPACE/setup.txt\"\\n' > "+setupPath+" && chmod +x "+setupPath)
	kubectlExec(t, "boxy-ctrl-0", "boxy",
		"printf '#!/bin/sh\\necho teardown-ran > "+markerFile+"\\n' > "+teardownPath+" && chmod +x "+teardownPath)
	t.Cleanup(func() {
		kubectlExec(t, "boxy-ctrl-0", "boxy", "rm -f "+setupPath+" "+teardownPath+" "+markerFile)
	})

	createSandboxHelper(t, base, tok, api.SandboxCreateBody{
		SandboxID:      sandboxID,
		TTLSeconds:     120,
		SetupScript:    setupPath,
		TeardownScript: teardownPath,
	})

	sessionID := createSessionForSandbox(t, base, tok, sandboxID)

	out := postExec(t, base, tok, api.ExecRequestBody{
		SandboxID: sandboxID, SessionID: sessionID,
		Command: "cat", Args: []string{"/workspace/setup.txt"},
		TimeoutSeconds: 10,
	})
	if strings.TrimSpace(out.Stdout) != "setup-done" {
		t.Fatalf("setup script didn't run: %q", out.Stdout)
	}

	deleteSession(t, base, tok, sessionID)

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		result, err := exec.Command("kubectl", "-n", "boxy", "exec", "boxy-ctrl-0", "--",
			"sh", "-c", "cat "+markerFile+" 2>/dev/null || echo missing").CombinedOutput()
		if err == nil && strings.TrimSpace(string(result)) == "teardown-ran" {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatal("teardown script did not run within 15s of session deletion")
}

func TestSetupScript_NetworkEgressRules(t *testing.T) {
	base, tok := testCreds(t)
	ts := strconv.FormatInt(time.Now().UnixNano(), 10)
	sandboxID := "e2e-hook-netblock-" + ts

	setupPath := "/tmp/boxy-test-netsetup-" + ts + ".sh"
	teardownPath := "/tmp/boxy-test-nettear-" + ts + ".sh"

	kubectlExec(t, "boxy-ctrl-0", "boxy",
		"printf '#!/bin/sh\\niptables -A OUTPUT -m owner --uid-owner 65534 -d 1.1.1.1 -j DROP\\n' > "+setupPath+" && chmod +x "+setupPath)
	kubectlExec(t, "boxy-ctrl-0", "boxy",
		"printf '#!/bin/sh\\niptables -D OUTPUT -m owner --uid-owner 65534 -d 1.1.1.1 -j DROP 2>/dev/null\\n' > "+teardownPath+" && chmod +x "+teardownPath)
	t.Cleanup(func() {
		exec.Command("kubectl", "-n", "boxy", "exec", "boxy-ctrl-0", "--",
			"sh", "-c", "iptables -D OUTPUT -m owner --uid-owner 65534 -d 1.1.1.1 -j DROP 2>/dev/null").Run()
		kubectlExec(t, "boxy-ctrl-0", "boxy", "rm -f "+setupPath+" "+teardownPath)
	})

	createSandboxHelper(t, base, tok, api.SandboxCreateBody{
		SandboxID:      sandboxID,
		TTLSeconds:     120,
		SetupScript:    setupPath,
		TeardownScript: teardownPath,
		Network:        &api.SandboxNetworkConfig{AllowInternetAccess: true},
	})

	sessionID := createSessionForSandbox(t, base, tok, sandboxID)

	blockedOut := postExec(t, base, tok, api.ExecRequestBody{
		SandboxID: sandboxID, SessionID: sessionID,
		Command: "sh", Args: []string{"-c", "echo test | nc -w 2 1.1.1.1 53 2>&1; echo exit=$?"},
		TimeoutSeconds: 10,
	})
	if strings.Contains(blockedOut.Stdout, "exit=0") {
		t.Fatalf("connection to blocked IP 1.1.1.1 should fail, got: %s", blockedOut.Stdout)
	}

	allowedOut := postExec(t, base, tok, api.ExecRequestBody{
		SandboxID: sandboxID, SessionID: sessionID,
		Command: "sh", Args: []string{"-c", "getent hosts example.com 2>/dev/null | head -1 | awk '{print $1}' || echo failed"},
		TimeoutSeconds: 10,
	})
	ip := strings.TrimSpace(allowedOut.Stdout)
	if ip == "failed" || ip == "" {
		t.Skip("DNS resolution blocked by network policy -- cannot validate allowed traffic")
	}
	if !strings.Contains(ip, ".") && !strings.Contains(ip, ":") {
		t.Fatalf("expected IP for example.com, got %q", ip)
	}
}
