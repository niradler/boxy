package router

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"boxy.dev/boxy/internal/api"
)

type ControllerClientConfig struct {
	MTLSDisabled bool
	CACertPath   string
	ClientCert   string
	ClientKey    string
}

type ControllerClient struct {
	httpClient *http.Client
	cfg        ControllerClientConfig
}

func NewControllerClient(cfg ControllerClientConfig) *ControllerClient {
	var transport http.RoundTripper
	if cfg.MTLSDisabled {
		transport = http.DefaultTransport
	} else {
		transport = buildMTLSTransport(cfg)
	}
	return &ControllerClient{
		httpClient: &http.Client{Transport: transport},
		cfg:        cfg,
	}
}

func buildMTLSTransport(cfg ControllerClientConfig) http.RoundTripper {
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
	return &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:      pool,
			Certificates: []tls.Certificate{cert},
		},
	}
}

func (c *ControllerClient) RawClient() *http.Client {
	return c.httpClient
}

// CreateSandboxReq mirrors the controller's CreateSandboxRequest (see
// controller/src/types.rs). Field names use snake_case to match the
// Rust controller's JSON schema.
type CreateSandboxReq struct {
	SandboxID       string                    `json:"sandbox_id"`
	Env             map[string]string         `json:"env,omitempty"`
	AllowedBinaries []string                  `json:"allowed_binaries,omitempty"`
	VM              *api.VMConfig             `json:"vm,omitempty"`
	Network         *api.SandboxNetworkConfig `json:"network,omitempty"`
	Volumes         []api.VolumeMount         `json:"volumes,omitempty"`
	Patches         []api.SandboxPatch        `json:"patches,omitempty"`
	TTLSeconds      int                       `json:"ttl_seconds,omitempty"`
}

type ExecReq struct {
	SandboxID      string            `json:"sandbox_id"`
	Command        string            `json:"command"`
	Args           []string          `json:"args,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
}

type ExecResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
	TimedOut bool   `json:"timed_out"`
}

type DeleteSandboxReq struct {
	SandboxID string `json:"sandbox_id"`
}

func (c *ControllerClient) CreateSandbox(ctx context.Context, baseURL string, req CreateSandboxReq) error {
	return c.postJSON(ctx, baseURL+"/v1/sandboxes", req, nil)
}

func (c *ControllerClient) Exec(ctx context.Context, baseURL string, req ExecReq) (*ExecResult, error) {
	var out ExecResult
	if err := c.postJSON(ctx, baseURL+"/v1/exec", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *ControllerClient) DeleteSandbox(ctx context.Context, baseURL string, req DeleteSandboxReq) error {
	return c.deleteJSON(ctx, baseURL+"/v1/sandboxes", req)
}

func (c *ControllerClient) postJSON(ctx context.Context, url string, body, out any) error {
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
		return fmt.Errorf("controller returned %d for POST %s", resp.StatusCode, url)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *ControllerClient) deleteJSON(ctx context.Context, url string, body any) error {
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
		return fmt.Errorf("controller returned %d for DELETE %s", resp.StatusCode, url)
	}
	return nil
}

