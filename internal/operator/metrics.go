package operator

import (
	"context"
	"sync/atomic"

	"go.opentelemetry.io/otel/metric"
)

// OperatorMetrics holds the OTel instruments for the operator. A nil
// *OperatorMetrics is valid: every method is a no-op, so reconcilers built
// without telemetry (e.g. in tests) keep working.
//
// Gauges are observable and backed by atomically-updated snapshots that the
// reconcilers refresh; the callbacks read those snapshots at scrape time
// instead of hitting the API server.
type OperatorMetrics struct {
	scaleUp   metric.Int64Counter
	scaleDown metric.Int64Counter

	readyReplicas   atomic.Int64
	activeSandboxes atomic.Int64
}

// NewOperatorMetrics registers the operator instruments on the given meter.
func NewOperatorMetrics(m metric.Meter) (*OperatorMetrics, error) {
	om := &OperatorMetrics{}
	var err error

	if om.scaleUp, err = m.Int64Counter(
		"boxy.operator.scale.up",
		metric.WithDescription("Controller StatefulSet scale-up operations"),
		metric.WithUnit("{operation}"),
	); err != nil {
		return nil, err
	}
	if om.scaleDown, err = m.Int64Counter(
		"boxy.operator.scale.down",
		metric.WithDescription("Controller StatefulSet scale-down operations"),
		metric.WithUnit("{operation}"),
	); err != nil {
		return nil, err
	}

	replicas, err := m.Int64ObservableGauge(
		"boxy.operator.controller.replicas.ready",
		metric.WithDescription("Ready controller replicas in the StatefulSet"),
		metric.WithUnit("{replica}"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(om.readyReplicas.Load())
			return nil
		}),
	)
	if err != nil {
		return nil, err
	}
	// Pool-wide active sandbox count; sandboxes-per-controller is derivable as
	// active / ready_replicas in the query layer.
	active, err := m.Int64ObservableGauge(
		"boxy.operator.sandboxes.active",
		metric.WithDescription("Active sandboxes (sessions) across the controller pool"),
		metric.WithUnit("{sandbox}"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(om.activeSandboxes.Load())
			return nil
		}),
	)
	if err != nil {
		return nil, err
	}
	_, _ = replicas, active

	return om, nil
}

// SetPoolState refreshes the gauge snapshots from a pool reconcile.
func (om *OperatorMetrics) SetPoolState(readyReplicas, activeSandboxes int32) {
	if om == nil {
		return
	}
	om.readyReplicas.Store(int64(readyReplicas))
	om.activeSandboxes.Store(int64(activeSandboxes))
}

// ScaledUp records a successful scale-up.
func (om *OperatorMetrics) ScaledUp(ctx context.Context) {
	if om == nil {
		return
	}
	om.scaleUp.Add(ctx, 1)
}

// ScaledDown records a successful scale-down.
func (om *OperatorMetrics) ScaledDown(ctx context.Context) {
	if om == nil {
		return
	}
	om.scaleDown.Add(ctx, 1)
}
