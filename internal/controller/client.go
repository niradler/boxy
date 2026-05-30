package controller

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"

	"boxy.dev/boxy/internal/api"
)

// mtlsServerName is the CN expected in the controller's TLS certificate.
// Must match deploy/helm/boxy/values.yaml mtlsServerCN.
const mtlsServerName = "boxy-controller"

type HTTPError struct {
	Method string
	URL    string
	Status int
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("controller returned %d for %s %s", e.Status, e.Method, e.URL)
}

func IsStaleRouteError(err error) bool {
	if err == nil {
		return false
	}
	var he *HTTPError
	if errors.As(err, &he) {
		return he.Status == http.StatusNotFound
	}
	var ne net.Error
	return errors.As(err, &ne)
}

type ClientConfig struct {
	MTLSDisabled    bool
	CACertPath      string
	ClientCert      string
	ClientKey       string
	ControllerToken string
}

type Client struct {
	httpClient *http.Client
	cfg        ClientConfig
}

func NewClient(cfg ClientConfig) *Client {
	var transport http.RoundTripper
	if cfg.MTLSDisabled {
		transport = http.DefaultTransport
	} else {
		transport = buildMTLSTransport(cfg)
	}
	if cfg.ControllerToken != "" {
		transport = &tokenTransport{token: cfg.ControllerToken, base: transport}
	}
	return &Client{
		httpClient: &http.Client{Transport: transport},
		cfg:        cfg,
	}
}

// tokenTransport injects X-Boxy-Controller-Token on every request.
type tokenTransport struct {
	token string
	base  http.RoundTripper
}

func (t *tokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("X-Boxy-Controller-Token", t.token)
	return t.base.RoundTrip(r)
}

func buildMTLSTransport(cfg ClientConfig) http.RoundTripper {
	caCert, err := os.ReadFile(cfg.CACertPath)
	if err != nil {
		panic(fmt.Sprintf("read CA cert %s: %v", cfg.CACertPath, err))
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caCert) {
		panic("failed to append CA cert")
	}
	cert, err := tls.LoadX509KeyPair(cfg.ClientCert, cfg.ClientKey)
	if err != nil {
		panic(fmt.Sprintf("load client keypair: %v", err))
	}
	// ServerName must match the CN in the controller's TLS certificate (mtlsServerCN).
	// This ensures the router/operator only connects to genuine boxy-controller pods,
	// not any other pod that happens to hold a CA-signed cert.
	return &http.Transport{
		TLSClientConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			RootCAs:      pool,
			ServerName:   mtlsServerName,
		},
	}
}

func (c *Client) RawClient() *http.Client {
	return c.httpClient
}

type CreateSandboxReq struct {
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

type ExecReq struct {
	SandboxID      string            `json:"sandbox_id"`
	Command        string            `json:"command"`
	Args           []string          `json:"args,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
	PTY            bool              `json:"pty,omitempty"`
}

type ExecResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
	TimedOut bool   `json:"timed_out"`
}

type FileReadReq struct {
	SandboxID string `json:"sandbox_id"`
	Path      string `json:"path"`
}

type FileReadResult struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type FileWriteReq struct {
	SandboxID string `json:"sandbox_id"`
	Path      string `json:"path"`
	Content   string `json:"content"`
	Encoding  string `json:"encoding,omitempty"`
}

type FileWriteResult struct {
	Path         string `json:"path"`
	BytesWritten int    `json:"bytes_written"`
}

type FileEditReq struct {
	SandboxID  string `json:"sandbox_id"`
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

type FileEditResult struct {
	Path         string `json:"path"`
	Replacements int    `json:"replacements"`
}

type DeleteSandboxReq struct {
	SandboxID string `json:"sandbox_id"`
}

func (c *Client) CreateSandbox(ctx context.Context, baseURL string, req CreateSandboxReq) error {
	return c.postJSON(ctx, baseURL+"/v1/sandboxes", req, nil)
}

func (c *Client) Exec(ctx context.Context, baseURL string, req ExecReq) (*ExecResult, error) {
	var out ExecResult
	if err := c.postJSON(ctx, baseURL+"/v1/exec", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ExecStream calls POST /v1/exec/stream and delivers events to onEvent as they
// arrive. onEvent is called with ("stdout"|"stderr", data) for each chunk and
// ("truncated", "") when the output cap is hit. It returns the final exit info.
func (c *Client) ExecStream(ctx context.Context, baseURL string, req ExecReq, onEvent func(string, string)) (*ExecResult, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/exec/stream", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, &HTTPError{Method: http.MethodPost, URL: baseURL + "/v1/exec/stream", Status: resp.StatusCode}
	}

	var result ExecResult
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		var evt struct {
			Type     string `json:"type"`
			Data     string `json:"data"`
			Code     int    `json:"code"`
			TimedOut bool   `json:"timedOut"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &evt); err != nil {
			continue
		}
		switch evt.Type {
		case "stdout", "stderr", "truncated":
			onEvent(evt.Type, evt.Data)
		case "exit":
			result.ExitCode = evt.Code
			result.TimedOut = evt.TimedOut
		case "error":
			return nil, fmt.Errorf("exec stream: %s", evt.Data)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read stream: %w", err)
	}
	return &result, nil
}

func (c *Client) ReadFile(ctx context.Context, baseURL string, req FileReadReq) (*FileReadResult, error) {
	var out FileReadResult
	if err := c.postJSON(ctx, baseURL+"/v1/files/read", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) WriteFile(ctx context.Context, baseURL string, req FileWriteReq) (*FileWriteResult, error) {
	var out FileWriteResult
	if err := c.postJSON(ctx, baseURL+"/v1/files/write", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) EditFile(ctx context.Context, baseURL string, req FileEditReq) (*FileEditResult, error) {
	var out FileEditResult
	if err := c.postJSON(ctx, baseURL+"/v1/files/edit", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) DeleteSandbox(ctx context.Context, baseURL string, req DeleteSandboxReq) error {
	return c.deleteJSON(ctx, baseURL+"/v1/sandboxes", req)
}

func (c *Client) postJSON(ctx context.Context, url string, body, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return &HTTPError{Method: http.MethodPost, URL: url, Status: resp.StatusCode}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *Client) deleteJSON(ctx context.Context, url string, body any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return &HTTPError{Method: http.MethodDelete, URL: url, Status: resp.StatusCode}
	}
	return nil
}
