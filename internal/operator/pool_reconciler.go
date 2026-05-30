package operator

import (
	"context"
	"fmt"
	"log/slog"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	boxyv1 "boxy.dev/boxy/api/v1alpha1"
)

const conditionReady = "Ready"

type ControllerPoolReconciler struct {
	client.Client
	cfg ReconcilerConfig
	log *slog.Logger
}

func NewControllerPoolReconciler(c client.Client, cfg ReconcilerConfig) *ControllerPoolReconciler {
	return &ControllerPoolReconciler{
		Client: c,
		cfg:    cfg,
		log:    slog.Default(),
	}
}

func (r *ControllerPoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&boxyv1.ControllerPool{}).
		Watches(
			&boxyv1.Session{},
			handler.EnqueueRequestsFromMapFunc(r.sessionToPool),
		).
		Named("controllerpool").
		Complete(r)
}

func (r *ControllerPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pool boxyv1.ControllerPool
	if err := r.Get(ctx, req.NamespacedName, &pool); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	active, err := r.countActiveSessions(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}

	readyReplicas, err := r.readyReplicas(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}

	pool.Status.ActiveSandboxCount = active
	pool.Status.ReadyReplicas = readyReplicas
	r.cfg.Metrics.SetPoolState(readyReplicas, active)
	r.setReadyCondition(&pool, readyReplicas)

	if err := r.Status().Update(ctx, &pool); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *ControllerPoolReconciler) countActiveSessions(ctx context.Context) (int32, error) {
	var list boxyv1.SessionList
	if err := r.List(ctx, &list, client.InNamespace(r.cfg.Namespace)); err != nil {
		return 0, fmt.Errorf("list sessions: %w", err)
	}
	var n int32
	for i := range list.Items {
		switch list.Items[i].Status.Phase {
		case boxyv1.SandboxPhasePending, boxyv1.SandboxPhaseCreating, boxyv1.SandboxPhaseRunning:
			n++
		}
	}
	return n, nil
}

func (r *ControllerPoolReconciler) readyReplicas(ctx context.Context) (int32, error) {
	var sts appsv1.StatefulSet
	key := types.NamespacedName{Namespace: r.cfg.Namespace, Name: r.cfg.StatefulSetName}
	if err := r.Get(ctx, key, &sts); err != nil {
		if apierrors.IsNotFound(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("get statefulset: %w", err)
	}
	return sts.Status.ReadyReplicas, nil
}

func (r *ControllerPoolReconciler) setReadyCondition(pool *boxyv1.ControllerPool, readyReplicas int32) {
	minReplicas := pool.Spec.MinReplicas
	if minReplicas <= 0 {
		minReplicas = 1
	}

	status := metav1.ConditionTrue
	reason := "PoolReady"
	message := fmt.Sprintf("%d/%d replicas ready", readyReplicas, minReplicas)

	if readyReplicas < minReplicas {
		status = metav1.ConditionFalse
		reason = "InsufficientReplicas"
		message = fmt.Sprintf("need %d ready replicas, have %d", minReplicas, readyReplicas)
	}

	now := metav1.Now()
	updated := metav1.Condition{
		Type:               conditionReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
	}

	for i, c := range pool.Status.Conditions {
		if c.Type == conditionReady {
			if c.Status == status {
				updated.LastTransitionTime = c.LastTransitionTime
			}
			pool.Status.Conditions[i] = updated
			return
		}
	}
	pool.Status.Conditions = append(pool.Status.Conditions, updated)
}

func (r *ControllerPoolReconciler) sessionToPool(ctx context.Context, obj client.Object) []reconcile.Request {
	var poolList boxyv1.ControllerPoolList
	if err := r.List(ctx, &poolList, client.InNamespace(r.cfg.Namespace)); err != nil {
		r.log.Error("sessionToPool: list ControllerPools failed", "err", err)
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(poolList.Items))
	for _, p := range poolList.Items {
		reqs = append(reqs, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: p.Namespace,
				Name:      p.Name,
			},
		})
	}
	return reqs
}
