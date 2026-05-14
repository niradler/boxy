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

func TestSandboxCreateAndExec(t *testing.T) {
	base, tok := testCreds(t)
	create := api.SandboxCreateBody{
		SessionID:  "e2e-session",
		SandboxID:  "e2e-sandbox-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Owner:      "e2e",
		TTLSeconds: 600,
		Env:        map[string]string{"E2E_MARKER": "provisioned"},
	}
	payload, _ := json.Marshal(create)
	req, err := http.NewRequest(http.MethodPost, base+"/v1/sandboxes", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	res, err := httpClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusCreated {
		t.Fatalf("create status %d", res.StatusCode)
	}
	var sb api.SandboxResponseBody
	if err := json.NewDecoder(res.Body).Decode(&sb); err != nil {
		t.Fatal(err)
	}
	if sb.SandboxID != create.SandboxID {
		t.Fatalf("sandboxId %q want %q", sb.SandboxID, create.SandboxID)
	}

	waitReady(t, base, tok, sb)

	execBody := api.ExecRequestBody{
		SessionID:      create.SessionID,
		SandboxID:      create.SandboxID,
		Command:        "sh",
		Args:           []string{"-c", "echo -n $E2E_MARKER"},
		Env:            map[string]string{},
		TimeoutSeconds: 120,
	}
	out := postExec(t, base, tok, execBody)
	if out.Stdout != "provisioned" {
		t.Fatalf("stdout %q", out.Stdout)
	}
}

func waitReady(t *testing.T, base, tok string, sb api.SandboxResponseBody) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, base+"/v1/sandboxes/"+sb.SandboxID, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		res, err := httpClient().Do(req)
		if err == nil && res.StatusCode == http.StatusOK {
			var cur api.SandboxResponseBody
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
	t.Fatal("sandbox not ready")
}

// --- MCP tests ---

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

func postMCP(t *testing.T, base, tok string, rpcReq jsonRPCRequest, sandboxID string) jsonRPCResponse {
	t.Helper()
	payload, _ := json.Marshal(rpcReq)
	req, err := http.NewRequest(http.MethodPost, base+"/mcp", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sandboxID != "" {
		req.Header.Set("X-Sandbox-Id", sandboxID)
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

func TestMCPBashTool(t *testing.T) {
	base, tok := testCreds(t)

	// Create a sandbox.
	create := api.SandboxCreateBody{
		SessionID:  "e2e-mcp-session",
		SandboxID:  "e2e-mcp-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Owner:      "e2e",
		TTLSeconds: 600,
	}
	payload, _ := json.Marshal(create)
	req, err := http.NewRequest(http.MethodPost, base+"/v1/sandboxes", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	res, err := httpClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusCreated {
		t.Fatalf("create status %d", res.StatusCode)
	}
	var sb api.SandboxResponseBody
	_ = json.NewDecoder(res.Body).Decode(&sb)
	waitReady(t, base, tok, sb)

	// MCP initialize.
	initResp := postMCP(t, base, tok, jsonRPCRequest{
		Jsonrpc: "2.0", ID: 1, Method: "initialize",
		Params: map[string]any{
			"protocolVersion": "2025-03-26",
			"clientInfo":      map[string]any{"name": "e2e-test", "version": "1.0.0"},
			"capabilities":    map[string]any{},
		},
	}, "")
	if initResp.Error != nil {
		t.Fatalf("initialize error: %s", initResp.Error.Message)
	}

	// MCP tools/list.
	listResp := postMCP(t, base, tok, jsonRPCRequest{
		Jsonrpc: "2.0", ID: 2, Method: "tools/list",
	}, "")
	if listResp.Error != nil {
		t.Fatalf("tools/list error: %s", listResp.Error.Message)
	}
	if !bytes.Contains(listResp.Result, []byte(`"bash"`)) {
		t.Fatalf("tools/list missing bash tool: %s", listResp.Result)
	}

	// MCP tools/call — bash.
	callResp := postMCP(t, base, tok, jsonRPCRequest{
		Jsonrpc: "2.0", ID: 3, Method: "tools/call",
		Params: map[string]any{
			"name":      "bash",
			"arguments": map[string]any{"command": "echo -n mcp-works"},
		},
	}, create.SandboxID)
	if callResp.Error != nil {
		t.Fatalf("tools/call error: %s", callResp.Error.Message)
	}
	if !bytes.Contains(callResp.Result, []byte("mcp-works")) {
		t.Fatalf("unexpected tools/call result: %s", callResp.Result)
	}

	// Clean up.
	delReq, _ := http.NewRequest(http.MethodDelete, base+"/v1/sandboxes/"+create.SandboxID, nil)
	delReq.Header.Set("Authorization", "Bearer "+tok)
	_, _ = httpClient().Do(delReq)
}

func postExec(t *testing.T, base, tok string, body api.ExecRequestBody) api.ExecResponseBody {
	t.Helper()
	payload, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, base+"/v1/exec", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	res, err := httpClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("exec status %d", res.StatusCode)
	}
	var out api.ExecResponseBody
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}
