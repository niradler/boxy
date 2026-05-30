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

type Provider struct {
	meterProvider metric.MeterProvider
	handler       http.Handler
	shutdown      func(context.Context) error
}

type Options struct {
	ServiceName string
	// Registerer, when non-nil, is where the Prometheus exporter registers instead of a fresh registry; MetricsHandler then returns an empty handler.
	Registerer prometheus.Registerer
}

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
		// resource.New returns a usable resource alongside partial errors; only abort on a nil resource.
		if res == nil {
			return nil, fmt.Errorf("telemetry: build resource: %w", err)
		}
	}

	readerOpts := []sdkmetric.Option{sdkmetric.WithResource(res)}

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

func (p *Provider) MetricsHandler() http.Handler { return p.handler }

func (p *Provider) Meter(name string) metric.Meter { return p.meterProvider.Meter(name) }

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

const ShutdownTimeout = 5 * time.Second
