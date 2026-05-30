// Package telemetry wires the OpenTelemetry metrics SDK for boxy's binaries.
//
// It builds a MeterProvider backed by a Prometheus exporter (served at GET
// /metrics) and, when OTEL_EXPORTER_OTLP_ENDPOINT is set, an additional OTLP
// gRPC push exporter. Standard OTEL_* env vars are honored by the SDK; setting
// OTEL_SDK_DISABLED=true makes Init a clean no-op (the global meter provider
// stays the built-in noop, and /metrics serves an empty body).
package telemetry

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.34.0"
)

// Provider owns the SDK lifecycle and the /metrics HTTP handler.
type Provider struct {
	meterProvider metric.MeterProvider
	handler       http.Handler
	shutdown      func(context.Context) error
}

// Options configures Init. ServiceName is required.
type Options struct {
	// ServiceName populates the service.name resource attribute (e.g. "boxy-controller").
	ServiceName string
	// Registerer, when non-nil, is where the Prometheus exporter registers its
	// collector instead of a fresh registry. Pass controller-runtime's metrics
	// registry here so the manager's existing /metrics endpoint exposes our
	// meters; in that case MetricsHandler returns an empty handler.
	Registerer prometheus.Registerer
}

// Init constructs a Provider, installs it as the global OTel meter provider, and
// returns it. When OTEL_SDK_DISABLED is truthy it returns a no-op Provider whose
// MetricsHandler serves an empty 200 so scrapers do not error.
func Init(ctx context.Context, opts Options) (*Provider, error) {
	if strings.TrimSpace(opts.ServiceName) == "" {
		return nil, fmt.Errorf("telemetry: ServiceName is required")
	}

	if disabled() {
		otel.SetMeterProvider(noop.NewMeterProvider())
		return &Provider{
			meterProvider: noop.NewMeterProvider(),
			handler:       http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
			shutdown:      func(context.Context) error { return nil },
		}, nil
	}

	res, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithAttributes(semconv.ServiceName(opts.ServiceName)),
	)
	if err != nil {
		// resource.New returns a usable resource alongside partial errors
		// (e.g. schema-url merge conflicts); only abort on a nil resource.
		if res == nil {
			return nil, fmt.Errorf("telemetry: build resource: %w", err)
		}
	}

	readerOpts := []sdkmetric.Option{sdkmetric.WithResource(res)}

	// Prometheus pull exporter (always on): serves /metrics.
	var promRegisterer prometheus.Registerer
	var promGatherer prometheus.Gatherer
	if opts.Registerer != nil {
		promRegisterer = opts.Registerer
	} else {
		reg := prometheus.NewRegistry()
		promRegisterer = reg
		promGatherer = reg
	}
	promExporter, err := otelprom.New(otelprom.WithRegisterer(promRegisterer))
	if err != nil {
		return nil, fmt.Errorf("telemetry: prometheus exporter: %w", err)
	}
	readerOpts = append(readerOpts, sdkmetric.WithReader(promExporter))

	// OTLP gRPC push exporter (optional): gated on OTEL_EXPORTER_OTLP_ENDPOINT.
	var otlpExporter *otlpmetricgrpc.Exporter
	if endpointConfigured() {
		exp, err := otlpmetricgrpc.New(ctx)
		if err != nil {
			return nil, fmt.Errorf("telemetry: otlp exporter: %w", err)
		}
		otlpExporter = exp
		readerOpts = append(readerOpts, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp)))
	}

	mp := sdkmetric.NewMeterProvider(readerOpts...)
	otel.SetMeterProvider(mp)

	var handler http.Handler
	if promGatherer != nil {
		handler = promhttp.HandlerFor(promGatherer, promhttp.HandlerOpts{})
	} else {
		// Registerer was supplied externally (operator): its owner serves /metrics.
		handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	}

	return &Provider{
		meterProvider: mp,
		handler:       handler,
		shutdown: func(ctx context.Context) error {
			err := mp.Shutdown(ctx)
			if otlpExporter != nil {
				if e := otlpExporter.Shutdown(ctx); e != nil && err == nil {
					err = e
				}
			}
			return err
		},
	}, nil
}

// MetricsHandler is the /metrics handler. Returns an empty 200 handler when the
// SDK is disabled or when an external Registerer owns the endpoint.
func (p *Provider) MetricsHandler() http.Handler { return p.handler }

// Meter returns a named meter from the installed provider.
func (p *Provider) Meter(name string) metric.Meter { return p.meterProvider.Meter(name) }

// Shutdown flushes and stops the exporters. Safe to call once on process exit.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p.shutdown == nil {
		return nil
	}
	return p.shutdown(ctx)
}

func disabled() bool {
	switch strings.TrimSpace(strings.ToLower(os.Getenv("OTEL_SDK_DISABLED"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func endpointConfigured() bool {
	return strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")) != "" ||
		strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT")) != ""
}

// ShutdownTimeout is the default budget callers should give Shutdown on exit.
const ShutdownTimeout = 5 * time.Second
