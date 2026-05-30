package operator

import (
	"context"
	"sync/atomic"

	"go.opentelemetry.io/otel/metric"
)

// A nil *OperatorMetrics is valid: every method is a no-op.
type OperatorMetrics struct {
	scaleUp   metric.Int64Counter
	scaleDown metric.Int64Counter

	readyReplicas   atomic.Int64
	activeSandboxes atomic.Int64
}

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

func (om *OperatorMetrics) SetPoolState(readyReplicas, activeSandboxes int32) {
	if om == nil {
		return
	}
	om.readyReplicas.Store(int64(readyReplicas))
	om.activeSandboxes.Store(int64(activeSandboxes))
}

func (om *OperatorMetrics) ScaledUp(ctx context.Context) {
	if om == nil {
		return
	}
	om.scaleUp.Add(ctx, 1)
}

func (om *OperatorMetrics) ScaledDown(ctx context.Context) {
	if om == nil {
		return
	}
	om.scaleDown.Add(ctx, 1)
}
