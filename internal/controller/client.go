package controller

import (
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
