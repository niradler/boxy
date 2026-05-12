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

func testCreds(t *testing.T) (base, token, workerImage string) {
	t.Helper()
	base = strings.TrimSuffix(strings.TrimSpace(os.Getenv("BOXY_E2E_BASE_URL")), "/")
	token = strings.TrimSpace(os.Getenv("BOXY_E2E_ROUTER_TOKEN"))
	workerImage = strings.TrimSpace(os.Getenv("BOXY_E2E_WORKER_IMAGE"))
	if base == "" || token == "" {
		t.Skip("set BOXY_E2E_BASE_URL and BOXY_E2E_ROUTER_TOKEN")
	}
	return base, token, workerImage
}

func httpClient() *http.Client {
	return &http.Client{Timeout: 120 * time.Second}
}

func TestHealth(t *testing.T) {
	base, _, _ := testCreds(t)
	resp, err := httpClient().Get(base + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestCustomSandboxProvisioningAndExec(t *testing.T) {
	base, tok, wrkImg := testCreds(t)
	if wrkImg == "" {
		t.Skip("BOXY_E2E_WORKER_IMAGE not set")
	}
	create := api.SandboxCreateBody{
		SessionID:  "e2e-session",
		SandboxID:  "e2e-custom-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Owner:      "e2e",
		TTLSeconds: 600,
		Image:      wrkImg,
		Env:        map[string]string{"E2E_MARKER": "provisioned"},
		Labels:     map[string]string{"e2e.boxy.dev/run": "1"},
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
	if sb.Image != wrkImg {
		t.Fatalf("image %q want %q", sb.Image, wrkImg)
	}
	if strings.TrimSpace(sb.PodRef.UID) == "" {
		t.Fatal("missing uid")
	}
	waitReady(t, base, tok, sb)
	execBody := api.ExecRequestBody{
		SessionID:      create.SessionID,
		SandboxID:      create.SandboxID,
		PodRef:         sb.PodRef,
		Command:        "sh",
		Args:           []string{"-c", "echo -n $E2E_MARKER"},
		Env:            map[string]string{},
		TimeoutSeconds: 120,
		Mode:           api.ExecModeAPI,
	}
	out := postExec(t, base, tok, execBody)
	if out.Stdout != "provisioned" {
		t.Fatalf("stdout %q", out.Stdout)
	}
	execNoRef := api.ExecRequestBody{
		SessionID:      create.SessionID,
		SandboxID:      create.SandboxID,
		Command:        "sh",
		Args:           []string{"-c", "echo -n resolved"},
		Env:            map[string]string{},
		TimeoutSeconds: 120,
		Mode:           api.ExecModeAPI,
	}
	out2 := postExec(t, base, tok, execNoRef)
	if out2.Stdout != "resolved" {
		t.Fatalf("resolved exec stdout %q", out2.Stdout)
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
			if cur.Ready && cur.PodRef.UID == sb.PodRef.UID {
				return
			}
		} else if res != nil {
			res.Body.Close()
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatal("sandbox not ready")
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
