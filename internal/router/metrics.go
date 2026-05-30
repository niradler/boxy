package router

import (
	"context"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// A nil *routerMetrics is valid: every method is a no-op.
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
		metric.WithExplicitBucketBoundaries(0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 300),
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

func (sr *statusRecorder) Flush() {
	if f, ok := sr.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

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
