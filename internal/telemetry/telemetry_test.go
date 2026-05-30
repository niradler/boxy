package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/metric"
)

func scrape(t *testing.T, p *Provider) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	p.MetricsHandler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("metrics handler status = %d, want 200", w.Code)
	}
	return w.Body.String()
}

func TestInitServesPrometheusMetrics(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	p, err := Init(context.Background(), Options{ServiceName: "boxy-test"})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer func() { _ = p.Shutdown(context.Background()) }()

	ctr, err := p.Meter("test").Int64Counter("boxy.test.widgets")
	if err != nil {
		t.Fatalf("counter: %v", err)
	}
	ctr.Add(context.Background(), 3)

	body := scrape(t, p)
	if !strings.Contains(body, "boxy_test_widgets") {
		t.Errorf("scrape missing instrument, got:\n%s", body)
	}
}

func TestInitRequiresServiceName(t *testing.T) {
	if _, err := Init(context.Background(), Options{}); err == nil {
		t.Fatal("expected error for missing ServiceName")
	}
}

func TestDisabledSDKIsNoOp(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "true")

	p, err := Init(context.Background(), Options{ServiceName: "boxy-test"})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer func() { _ = p.Shutdown(context.Background()) }()

	ctr, err := p.Meter("test").Int64Counter("boxy.test.widgets")
	if err != nil {
		t.Fatalf("counter: %v", err)
	}
	ctr.Add(context.Background(), 1)

	body := scrape(t, p)
	if strings.Contains(body, "boxy_test_widgets") {
		t.Errorf("disabled SDK should not export metrics, got:\n%s", body)
	}
}

func TestInitInstallsGlobalProvider(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "")
	p, err := Init(context.Background(), Options{ServiceName: "boxy-test"})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer func() { _ = p.Shutdown(context.Background()) }()

	var _ metric.Meter = p.Meter("x")
}
