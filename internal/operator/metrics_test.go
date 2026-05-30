package operator

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"boxy.dev/boxy/internal/telemetry"
)

func scrapeText(t *testing.T, tp *telemetry.Provider) string {
	t.Helper()
	w := httptest.NewRecorder()
	tp.MetricsHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return w.Body.String()
}

func TestOperatorMetricsExported(t *testing.T) {
	tp, err := telemetry.Init(t.Context(), telemetry.Options{ServiceName: "boxy-operator-test"})
	if err != nil {
		t.Fatalf("telemetry init: %v", err)
	}
	defer func() { _ = tp.Shutdown(t.Context()) }()

	om, err := NewOperatorMetrics(tp.Meter("operator-test"))
	if err != nil {
		t.Fatalf("NewOperatorMetrics: %v", err)
	}

	om.SetPoolState(3, 12)
	om.ScaledUp(t.Context())
	om.ScaledDown(t.Context())

	body := scrapeText(t, tp)
	for _, want := range []string{
		"boxy_operator_scale_up",
		"boxy_operator_scale_down",
		"boxy_operator_controller_replicas_ready",
		"boxy_operator_sandboxes_active",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape missing %q, got:\n%s", want, body)
		}
	}
}

func TestNilOperatorMetricsNoOp(t *testing.T) {
	var om *OperatorMetrics
	om.SetPoolState(1, 2)
	om.ScaledUp(t.Context())
	om.ScaledDown(t.Context())
}
