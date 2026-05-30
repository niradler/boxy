package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"boxy.dev/boxy/internal/api"
)

func TestExecResult(t *testing.T) {
	cases := []struct {
		name string
		in   *api.ExecResponseBody
		want string
	}{
		{"nil", nil, "error"},
		{"ok", &api.ExecResponseBody{ExitCode: 0}, "ok"},
		{"nonzero", &api.ExecResponseBody{ExitCode: 2}, "nonzero"},
		{"timeout", &api.ExecResponseBody{TimedOut: true, ExitCode: 137}, "timeout"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := execResult(c.in); got != c.want {
				t.Errorf("execResult = %q, want %q", got, c.want)
			}
		})
	}
}

func TestMetricsEndpointBypassesToken(t *testing.T) {
	stub := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("METRICS_OK"))
	})
	s := &server{cfg: &config{controllerToken: "secret"}, metricsHandler: stub}
	h := s.handler()

	// No token header: /metrics must still be reachable, like /healthz.
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if w.Body.String() != "METRICS_OK" {
		t.Errorf("body = %q, want METRICS_OK", w.Body.String())
	}
}

func TestNilMetricsAreNoOp(t *testing.T) {
	// A server built without metrics (as the other tests do) must not panic when
	// the recording helpers are called.
	var cm *controllerMetrics
	cm.sandboxCreated(t.Context())
	cm.sandboxDeleted(t.Context())
	cm.execThrottled(t.Context())
	cm.recordExec(t.Context(), "sb", "ok", 0.1, 10)
}
