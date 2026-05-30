package router

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"boxy.dev/boxy/internal/telemetry"
)

func TestRouterMetricsMiddlewareRecords(t *testing.T) {
	tp, err := telemetry.Init(context.Background(), telemetry.Options{ServiceName: "boxy-router-test"})
	if err != nil {
		t.Fatalf("telemetry init: %v", err)
	}
	defer func() { _ = tp.Shutdown(context.Background()) }()

	rm, err := newRouterMetrics(tp.Meter("router-test"), 7)
	if err != nil {
		t.Fatalf("newRouterMetrics: %v", err)
	}

	h := rm.middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/sessions", nil))

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	tp.MetricsHandler().ServeHTTP(w, req)

	body := w.Body.String()
	for _, want := range []string{"boxy_router_requests", "boxy_router_request_duration", "boxy_router_concurrency_limit"} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape missing %q, got:\n%s", want, body)
		}
	}
	if !strings.Contains(body, "418") {
		t.Errorf("expected status=418 label in scrape, got:\n%s", body)
	}
}

func TestRouterMetricsNilMiddlewareIsPassthrough(t *testing.T) {
	var rm *routerMetrics
	called := false
	h := rm.middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if !called {
		t.Fatal("nil routerMetrics middleware must pass through to next handler")
	}
}
