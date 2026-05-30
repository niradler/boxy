package main

import (
	"context"

	"boxy.dev/boxy/internal/api"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

func execResult(r *api.ExecResponseBody) string {
	switch {
	case r == nil:
		return "error"
	case r.TimedOut:
		return "timeout"
	case r.ExitCode != 0:
		return "nonzero"
	default:
		return "ok"
	}
}

// Exec instruments carry an unbounded sandbox_id attribute; watch Prometheus series cardinality.
type controllerMetrics struct {
	sandboxesActive metric.Int64UpDownCounter
	execTotal       metric.Int64Counter
	execDuration    metric.Float64Histogram
	outputBytes     metric.Int64Counter
	throttledTotal  metric.Int64Counter
}

func newControllerMetrics(m metric.Meter) (*controllerMetrics, error) {
	cm := &controllerMetrics{}
	var err error

	if cm.sandboxesActive, err = m.Int64UpDownCounter(
		"boxy.controller.sandboxes.active",
		metric.WithDescription("Number of sandboxes currently held by this controller"),
		metric.WithUnit("{sandbox}"),
	); err != nil {
		return nil, err
	}
	if cm.execTotal, err = m.Int64Counter(
		"boxy.controller.exec",
		metric.WithDescription("Total exec invocations admitted by this controller"),
		metric.WithUnit("{exec}"),
	); err != nil {
		return nil, err
	}
	if cm.execDuration, err = m.Float64Histogram(
		"boxy.controller.exec.duration",
		metric.WithDescription("Exec wall-clock duration"),
		metric.WithUnit("s"),
		// Seconds-scale buckets; the SDK default is millisecond-scale and collapses every exec into one bucket.
		metric.WithExplicitBucketBoundaries(0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 300),
	); err != nil {
		return nil, err
	}
	if cm.outputBytes, err = m.Int64Counter(
		"boxy.controller.exec.output",
		metric.WithDescription("Total stdout+stderr bytes produced by execs"),
		metric.WithUnit("By"),
	); err != nil {
		return nil, err
	}
	if cm.throttledTotal, err = m.Int64Counter(
		"boxy.controller.throttled",
		metric.WithDescription("Exec requests rejected with 429 due to concurrency limit"),
		metric.WithUnit("{request}"),
	); err != nil {
		return nil, err
	}

	if err = registerPodResourceGauges(m); err != nil {
		return nil, err
	}
	return cm, nil
}

func (cm *controllerMetrics) recordExec(ctx context.Context, sandboxID, result string, durationSeconds float64, outputBytes int) {
	if cm == nil {
		return
	}
	attrs := metric.WithAttributes(
		attribute.String("sandbox_id", sandboxID),
		attribute.String("result", result),
	)
	cm.execTotal.Add(ctx, 1, attrs)
	cm.execDuration.Record(ctx, durationSeconds, attrs)
	if outputBytes > 0 {
		cm.outputBytes.Add(ctx, int64(outputBytes), metric.WithAttributes(attribute.String("sandbox_id", sandboxID)))
	}
}

func (cm *controllerMetrics) sandboxCreated(ctx context.Context) {
	if cm == nil {
		return
	}
	cm.sandboxesActive.Add(ctx, 1)
}

func (cm *controllerMetrics) sandboxDeleted(ctx context.Context) {
	if cm == nil {
		return
	}
	cm.sandboxesActive.Add(ctx, -1)
}

func (cm *controllerMetrics) execThrottled(ctx context.Context) {
	if cm == nil {
		return
	}
	cm.throttledTotal.Add(ctx, 1)
}
