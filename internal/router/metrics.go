package router

import (
	"context"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// routerMetrics holds the OTel instruments for the router edge. A nil
// *routerMetrics is valid: every method is a no-op, which is what tests that
// build a Server directly (without telemetry) rely on.
type routerMetrics struct {
	requests metric.Int64Counter
	duration metric.Float64Histogram
	inflight metric.Int64UpDownCounter
}

func newRouterMetrics(m metric.Meter, maxConcurrency int) (*routerMetrics, error) {
	rm := &routerMetrics{}
	var err error

	if rm.requests, err = m.Int64Counter(
		"boxy.router.requests",
		metric.WithDescription("Total HTTP requests handled by the router"),
		metric.WithUnit("{request}"),
	); err != nil {
		return nil, err
	}
	if rm.duration, err = m.Float64Histogram(
		"boxy.router.request.duration",
		metric.WithDescription("Router HTTP request duration"),
		metric.WithUnit("s"),
	); err != nil {
		return nil, err
	}
	if rm.inflight, err = m.Int64UpDownCounter(
		"boxy.router.requests.inflight",
		metric.WithDescription("In-flight HTTP requests currently being served"),
		metric.WithUnit("{request}"),
	); err != nil {
		return nil, err
	}

	limit, err := m.Int64ObservableGauge(
		"boxy.router.concurrency.limit",
		metric.WithDescription("Configured max concurrent exec requests (BOXY_MAX_CONCURRENCY)"),
		metric.WithUnit("{request}"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(int64(maxConcurrency))
			return nil
		}),
	)
	if err != nil {
		return nil, err
	}
	_ = limit

	return rm, nil
}

// statusRecorder captures the response status code for metrics.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (sr *statusRecorder) WriteHeader(code int) {
	if !sr.wroteHeader {
		sr.status = code
		sr.wroteHeader = true
	}
	sr.ResponseWriter.WriteHeader(code)
}

func (sr *statusRecorder) Write(b []byte) (int, error) {
	if !sr.wroteHeader {
		sr.status = http.StatusOK
		sr.wroteHeader = true
	}
	return sr.ResponseWriter.Write(b)
}

// Flush forwards flushes so streaming/NDJSON handlers keep working through the wrapper.
func (sr *statusRecorder) Flush() {
	if f, ok := sr.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// middleware records request count, duration, and in-flight gauge per request.
// Safe to use on a nil *routerMetrics (acts as a pass-through).
func (rm *routerMetrics) middleware(next http.Handler) http.Handler {
	if rm == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rm.inflight.Add(r.Context(), 1)
		defer rm.inflight.Add(r.Context(), -1)

		sr := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(sr, r)

		// r.Pattern is set by ServeMux during matching (Go 1.23+); it is the
		// templated route, which keeps label cardinality bounded. Unmatched
		// requests get "other".
		route := r.Pattern
		if route == "" {
			route = "other"
		}
		attrs := metric.WithAttributes(
			attribute.String("route", route),
			attribute.Int("status", sr.status),
			attribute.String("method", r.Method),
		)
		rm.requests.Add(r.Context(), 1, attrs)
		rm.duration.Record(r.Context(), time.Since(start).Seconds(), attrs)
	})
}
