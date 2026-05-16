package operator

import (
	"context"
	"log/slog"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	boxyv1 "boxy.dev/boxy/api/v1alpha1"
)

type SandboxConfigReconciler struct {
	client.Client
	cfg ReconcilerConfig
	log *slog.Logger
}

func NewSandboxConfigReconciler(c client.Client, cfg ReconcilerConfig) *SandboxConfigReconciler {
	return &SandboxConfigReconciler{Client: c, cfg: cfg, log: slog.Default()}
}

func (r *SandboxConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&boxyv1.Sandbox{}).
		Named("sandboxconfig").
		Complete(r)
}

func (r *SandboxConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var sb boxyv1.Sandbox
	if err := r.Get(ctx, req.NamespacedName, &sb); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !controllerutil.ContainsFinalizer(&sb, boxyv1.FinalizerSandboxConfigCleanup) {
		controllerutil.AddFinalizer(&sb, boxyv1.FinalizerSandboxConfigCleanup)
		return ctrl.Result{}, r.Update(ctx, &sb)
	}

	if sb.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	return r.handleDeletion(ctx, &sb)
}

func (r *SandboxConfigReconciler) handleDeletion(ctx context.Context, sb *boxyv1.Sandbox) (ctrl.Result, error) {
	var sessions boxyv1.SessionList
	if err := r.List(ctx, &sessions,
		client.InNamespace(r.cfg.Namespace),
		client.MatchingLabels{boxyv1.LabelSandboxID: sb.Spec.SandboxID},
	); err != nil {
		return ctrl.Result{}, err
	}

	active := 0
	for i := range sessions.Items {
		sess := &sessions.Items[i]
		if sess.Status.Phase == boxyv1.SandboxPhaseTerminated {
			continue
		}
		if !sess.DeletionTimestamp.IsZero() {
			continue
		}
		active++
		if sess.Status.Phase != boxyv1.SandboxPhaseDeleting {
			patch := client.MergeFrom(sess.DeepCopy())
			sess.Status.Phase = boxyv1.SandboxPhaseDeleting
			if err := r.Status().Patch(ctx, sess, patch); err != nil {
				r.log.Error("failed to transition session to Deleting", "session", sess.Name, "err", err)
			}
		}
	}

	if active > 0 {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	controllerutil.RemoveFinalizer(sb, boxyv1.FinalizerSandboxConfigCleanup)
	return ctrl.Result{}, r.Update(ctx, sb)
}
