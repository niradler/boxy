package exec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"boxy.dev/boxy/internal/api"
)

type APIExecClient struct {
	HTTP    *http.Client
	Token   string
	MaxBody int
}

func (c *APIExecClient) Run(ctx context.Context, baseURL string, body *api.ExecRequestBody, maxOutput int) (*api.ExecResponseBody, error) {
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: 0}
	}
	u := baseURL
	if u[len(u)-1] == '/' {
		u = u[:len(u)-1]
	}
	u += "/v1/exec"
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	if c.MaxBody > 0 && len(payload) > c.MaxBody {
		return nil, fmt.Errorf("request body exceeds limit")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxOutput+1024)))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("worker returned %d: %s", resp.StatusCode, string(respBody))
	}
	var out api.ExecResponseBody
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("decode worker response: %w", err)
	}
	return &out, nil
}

func WorkerHTTPTimeout(d time.Duration) *http.Client {
	return &http.Client{
		Timeout: d,
	}
}
