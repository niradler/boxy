package operator

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	boxyv1 "boxy.dev/boxy/api/v1alpha1"
	ctrlclient "boxy.dev/boxy/internal/controller"
)

type ReconcilerConfig struct {
	Namespace              string
	StatefulSetName        string
	HeadlessServiceName    string
	ControllerPoolName     string
	ControllerPort         int32
	MaxSandboxesPerCtrl    int
	MaxControllerReplicas  int32
	MinControllerReplicas  int32
	TerminatedRetentionSec int
	ScaleDownCooldown      time.Duration
	MTLSDisabled           bool
	Metrics                *OperatorMetrics
}

type SessionReconciler struct {
	client.Client
	ctrlClient *ctrlclient.Client
	cfg        ReconcilerConfig
	log        *slog.Logger
}

func NewSessionReconciler(c client.Client, cc *ctrlclient.Client, cfg ReconcilerConfig) *SessionReconciler {
	if cfg.MaxSandboxesPerCtrl <= 0 {
		cfg.MaxSandboxesPerCtrl = 20
	}
	if cfg.MaxControllerReplicas <= 0 {
		cfg.MaxControllerReplicas = 50
	}
	if cfg.MinControllerReplicas <= 0 {
		cfg.MinControllerReplicas = 1
	}
	if cfg.TerminatedRetentionSec <= 0 {
		cfg.TerminatedRetentionSec = 3600
	}
	if cfg.ScaleDownCooldown <= 0 {
		cfg.ScaleDownCooldown = 5 * time.Minute
	}
	if cfg.ControllerPort <= 0 {
		cfg.ControllerPort = 8080
	}
	return &SessionReconciler{
		Client:     c,
		ctrlClient: cc,
		cfg:        cfg,
		log:        slog.Default(),
	}
}

func (r *SessionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&boxyv1.Session{}).
		Named("session").
		Complete(r)
}

func (r *SessionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var session boxyv1.Session
	if err := r.Get(ctx, req.NamespacedName, &session); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !session.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &session)
	}

	switch session.Status.Phase {
	case "", boxyv1.SandboxPhasePending:
		return r.reconcilePending(ctx, &session)
	case boxyv1.SandboxPhaseCreating:
		return r.reconcileCreating(ctx, &session)
	case boxyv1.SandboxPhaseRunning:
		return r.reconcileRunning(ctx, &session)
	case boxyv1.SandboxPhaseDeleting:
		return r.reconcileDeleting(ctx, &session)
	case boxyv1.SandboxPhaseTerminated:
		return r.reconcileTerminated(ctx, &session)
	default:
		r.log.Warn("unknown session phase", "phase", session.Status.Phase, "name", session.Name)
		return ctrl.Result{}, nil
	}
}

func (r *SessionReconciler) handleDeletion(ctx context.Context, session *boxyv1.Session) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(session, boxyv1.FinalizerSessionCleanup) {
		return ctrl.Result{}, nil
	}

	if session.Status.Phase == boxyv1.SandboxPhaseRunning || session.Status.Phase == boxyv1.SandboxPhaseCreating {
		if session.Status.ControllerAddress != "" {
			baseURL := r.sessionControllerURL(session)
			err := r.ctrlClient.DeleteSandbox(ctx, baseURL, ctrlclient.DeleteSandboxReq{
				SandboxID: session.Spec.SessionID,
			})
			if err != nil && !ctrlclient.IsStaleRouteError(err) {
				r.log.Error("finalizer: failed to delete session on controller", "session", session.Name, "err", err)
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
		}
	}

	controllerutil.RemoveFinalizer(session, boxyv1.FinalizerSessionCleanup)
	if err := r.Update(ctx, session); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *SessionReconciler) reconcilePending(ctx context.Context, session *boxyv1.Session) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(session, boxyv1.FinalizerSessionCleanup) {
		controllerutil.AddFinalizer(session, boxyv1.FinalizerSessionCleanup)
		if err := r.Update(ctx, session); err != nil {
			return ctrl.Result{}, err
		}
	}

	sb, err := r.fetchSandboxConfig(ctx, session.Spec.SandboxID)
	if err != nil {
		return ctrl.Result{}, err
	}
	if sb == nil {
		r.log.Warn("sandbox config not found for session", "session", session.Name, "sandboxId", session.Spec.SandboxID)
		session.Status.Phase = boxyv1.SandboxPhaseDeleting
		session.Status.Message = "sandbox config not found"
		if err := r.Status().Update(ctx, session); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	podName, address, err := r.assignController(ctx)
	if err != nil {
		r.log.Info("no controller capacity, will retry", "session", session.Name, "err", err)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	now := metav1.Now()
	session.Status.Phase = boxyv1.SandboxPhaseCreating
	session.Status.ControllerPool = r.cfg.ControllerPoolName
	session.Status.ControllerPod = podName
	session.Status.ControllerAddress = address
	session.Status.Port = r.cfg.ControllerPort
	session.Status.CreatedAt = &now
	if sb.Spec.TTLSeconds > 0 {
		exp := metav1.NewTime(now.Add(time.Duration(sb.Spec.TTLSeconds) * time.Second))
		session.Status.ExpiresAt = &exp
	}

	if err := r.Status().Update(ctx, session); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

func (r *SessionReconciler) reconcileCreating(ctx context.Context, session *boxyv1.Session) (ctrl.Result, error) {
	sb, err := r.fetchSandboxConfig(ctx, session.Spec.SandboxID)
	if err != nil {
		return ctrl.Result{}, err
	}
	if sb == nil {
		session.Status.Phase = boxyv1.SandboxPhaseDeleting
		session.Status.Message = "sandbox config not found"
		if err := r.Status().Update(ctx, session); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	baseURL := r.sessionControllerURL(session)
	req := ctrlclient.CreateSandboxReq{
		SandboxID:       session.Spec.SessionID,
		Env:             sb.Spec.Env,
		AllowedBinaries: sb.Spec.AllowedBinaries,
		VM:              sb.Spec.VM,
		Network:         sb.Spec.Network,
		Volumes:         sb.Spec.Volumes,
		Patches:         sb.Spec.Patches,
		TTLSeconds:      sb.Spec.TTLSeconds,
		SetupScript:     sb.Spec.SetupScript,
		TeardownScript:  sb.Spec.TeardownScript,
		ScriptEnv:       sb.Spec.ScriptEnv,
	}

	if err := r.ctrlClient.CreateSandbox(ctx, baseURL, req); err != nil {
		if ctrlclient.IsStaleRouteError(err) {
			r.log.Warn("controller unreachable during create, resetting to Pending",
				"session", session.Name, "controller", session.Status.ControllerPod, "err", err)
			session.Status.Phase = boxyv1.SandboxPhasePending
			session.Status.ControllerPod = ""
			session.Status.ControllerAddress = ""
			session.Status.Message = fmt.Sprintf("controller unreachable: %v", err)
			if err := r.Status().Update(ctx, session); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		r.log.Error("create session on controller failed", "session", session.Name, "err", err)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	session.Status.Phase = boxyv1.SandboxPhaseRunning
	session.Status.Message = ""
	if err := r.Status().Update(ctx, session); err != nil {
		return ctrl.Result{}, err
	}

	if session.Status.ExpiresAt != nil {
		return ctrl.Result{RequeueAfter: time.Until(session.Status.ExpiresAt.Time)}, nil
	}
	return ctrl.Result{}, nil
}

func (r *SessionReconciler) reconcileRunning(ctx context.Context, session *boxyv1.Session) (ctrl.Result, error) {
	now := time.Now()

	if session.Status.ExpiresAt != nil {
		expiry := session.Status.ExpiresAt.Time

		if session.Status.LastExecAt != nil {
			sb, err := r.fetchSandboxConfig(ctx, session.Spec.SandboxID)
			if err == nil && sb != nil && sb.Spec.TTLSeconds > 0 {
				slidingExpiry := session.Status.LastExecAt.Time.Add(time.Duration(sb.Spec.TTLSeconds) * time.Second)
				if slidingExpiry.After(expiry) {
					expiry = slidingExpiry
					newExp := metav1.NewTime(expiry)
					session.Status.ExpiresAt = &newExp
					if err := r.Status().Update(ctx, session); err != nil {
						return ctrl.Result{}, err
					}
				}
			}
		}

		if now.After(expiry) {
			r.log.Info("session TTL expired", "session", session.Name)
			session.Status.Phase = boxyv1.SandboxPhaseDeleting
			if err := r.Status().Update(ctx, session); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{RequeueAfter: time.Until(expiry)}, nil
	}

	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

func (r *SessionReconciler) reconcileDeleting(ctx context.Context, session *boxyv1.Session) (ctrl.Result, error) {
	if session.Status.ControllerAddress != "" {
		baseURL := r.sessionControllerURL(session)
		err := r.ctrlClient.DeleteSandbox(ctx, baseURL, ctrlclient.DeleteSandboxReq{
			SandboxID: session.Spec.SessionID,
		})
		if err != nil && !ctrlclient.IsStaleRouteError(err) {
			r.log.Error("delete session on controller failed", "session", session.Name, "err", err)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	}

	now := metav1.Now()
	session.Status.Phase = boxyv1.SandboxPhaseTerminated
	session.Status.TerminatedAt = &now
	session.Status.Message = ""
	if err := r.Status().Update(ctx, session); err != nil {
		return ctrl.Result{}, err
	}

	retention := time.Duration(r.cfg.TerminatedRetentionSec) * time.Second
	if retention <= 0 {
		return ctrl.Result{Requeue: true}, nil
	}
	return ctrl.Result{RequeueAfter: retention}, nil
}

func (r *SessionReconciler) reconcileTerminated(ctx context.Context, session *boxyv1.Session) (ctrl.Result, error) {
	if session.Annotations != nil && session.Annotations["boxy.dev/retain"] == "true" {
		return ctrl.Result{}, nil
	}

	retention := time.Duration(r.cfg.TerminatedRetentionSec) * time.Second
	if session.Status.TerminatedAt != nil && retention > 0 {
		elapsed := time.Since(session.Status.TerminatedAt.Time)
		if elapsed < retention {
			return ctrl.Result{RequeueAfter: retention - elapsed}, nil
		}
	}

	r.log.Info("cleaning up terminated session CR", "session", session.Name)
	if err := r.Delete(ctx, session); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *SessionReconciler) sessionControllerURL(session *boxyv1.Session) string {
	scheme := "https"
	if r.cfg.MTLSDisabled {
		scheme = "http"
	}
	return fmt.Sprintf("%s://%s:%d", scheme, session.Status.ControllerAddress, session.Status.Port)
}

func (r *SessionReconciler) fetchSandboxConfig(ctx context.Context, sandboxID string) (*boxyv1.Sandbox, error) {
	var list boxyv1.SandboxList
	if err := r.List(ctx, &list,
		client.InNamespace(r.cfg.Namespace),
		client.MatchingLabels{boxyv1.LabelSandboxID: sandboxID},
	); err != nil {
		return nil, err
	}
	for i := range list.Items {
		sb := &list.Items[i]
		if sb.Spec.SandboxID == sandboxID && sb.DeletionTimestamp.IsZero() {
			return sb, nil
		}
	}
	return nil, nil
}

func (r *SessionReconciler) assignController(ctx context.Context) (podName, address string, err error) {
	var podList corev1.PodList
	if err := r.List(ctx, &podList,
		client.InNamespace(r.cfg.Namespace),
		client.MatchingLabels{"app": "boxy-controller"},
	); err != nil {
		return "", "", fmt.Errorf("list controller pods: %w", err)
	}

	var sessionList boxyv1.SessionList
	if err := r.List(ctx, &sessionList, client.InNamespace(r.cfg.Namespace)); err != nil {
		return "", "", fmt.Errorf("list sessions: %w", err)
	}

	podCounts := map[string]int{}
	for i := range sessionList.Items {
		s := &sessionList.Items[i]
		if s.Status.Phase == boxyv1.SandboxPhaseTerminated || s.Status.Phase == boxyv1.SandboxPhaseDeleting {
			continue
		}
		if s.Status.ControllerPod != "" {
			podCounts[s.Status.ControllerPod]++
		}
	}

	bestPod := ""
	bestAddr := ""
	bestRemaining := -1

	for i := range podList.Items {
		p := &podList.Items[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		if !podRunningReady(p) {
			continue
		}
		count := podCounts[p.Name]
		remaining := r.cfg.MaxSandboxesPerCtrl - count
		if remaining <= 0 {
			continue
		}
		if bestRemaining < 0 || remaining < bestRemaining {
			bestPod = p.Name
			bestAddr = fmt.Sprintf("%s.%s.%s.svc", p.Name, r.cfg.HeadlessServiceName, r.cfg.Namespace)
			bestRemaining = remaining
		}
	}

	if bestPod == "" {
		if err := r.scaleUp(ctx); err != nil {
			return "", "", fmt.Errorf("no capacity and scale-up failed: %w", err)
		}
		return "", "", fmt.Errorf("no capacity; scaled up, will retry")
	}

	return bestPod, bestAddr, nil
}

func (r *SessionReconciler) scaleUp(ctx context.Context) error {
	// adjustFn always returns current+1 here (or errors at max), so a nil result
	// means a real scale-up occurred.
	if err := r.scaleStatefulSet(ctx, func(current int32) (int32, error) {
		if current >= r.cfg.MaxControllerReplicas {
			return 0, fmt.Errorf("already at max controller replicas (%d)", r.cfg.MaxControllerReplicas)
		}
		return current + 1, nil
	}); err != nil {
		return err
	}
	r.cfg.Metrics.ScaledUp(ctx)
	return nil
}

func (r *SessionReconciler) scaleStatefulSet(ctx context.Context, adjustFn func(int32) (int32, error)) error {
	key := types.NamespacedName{Namespace: r.cfg.Namespace, Name: r.cfg.StatefulSetName}
	var sts appsv1.StatefulSet
	if err := r.Get(ctx, key, &sts); err != nil {
		return fmt.Errorf("get statefulset: %w", err)
	}
	current := int32(1)
	if sts.Spec.Replicas != nil {
		current = *sts.Spec.Replicas
	}
	desired, err := adjustFn(current)
	if err != nil {
		return err
	}
	if desired == current {
		return nil
	}
	sts.Spec.Replicas = &desired
	if err := r.Update(ctx, &sts); err != nil {
		return fmt.Errorf("update statefulset replicas: %w", err)
	}
	r.log.Info("scaled controller StatefulSet", "from", current, "to", desired)
	return nil
}

func podRunningReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
