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
}

type SandboxReconciler struct {
	client.Client
	ctrlClient *ctrlclient.Client
	cfg        ReconcilerConfig
	log        *slog.Logger
}

func NewSandboxReconciler(c client.Client, cc *ctrlclient.Client, cfg ReconcilerConfig) *SandboxReconciler {
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
	return &SandboxReconciler{
		Client:     c,
		ctrlClient: cc,
		cfg:        cfg,
		log:        slog.Default(),
	}
}

func (r *SandboxReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&boxyv1.Sandbox{}).
		Named("sandbox").
		Complete(r)
}

func (r *SandboxReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var sandbox boxyv1.Sandbox
	if err := r.Get(ctx, req.NamespacedName, &sandbox); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !sandbox.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &sandbox)
	}

	switch sandbox.Status.Phase {
	case "", boxyv1.SandboxPhasePending:
		return r.reconcilePending(ctx, &sandbox)
	case boxyv1.SandboxPhaseCreating:
		return r.reconcileCreating(ctx, &sandbox)
	case boxyv1.SandboxPhaseRunning:
		return r.reconcileRunning(ctx, &sandbox)
	case boxyv1.SandboxPhaseDeleting:
		return r.reconcileDeleting(ctx, &sandbox)
	case boxyv1.SandboxPhaseTerminated:
		return r.reconcileTerminated(ctx, &sandbox)
	default:
		r.log.Warn("unknown sandbox phase", "phase", sandbox.Status.Phase, "name", sandbox.Name)
		return ctrl.Result{}, nil
	}
}

func (r *SandboxReconciler) handleDeletion(ctx context.Context, sandbox *boxyv1.Sandbox) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(sandbox, boxyv1.FinalizerSandboxCleanup) {
		return ctrl.Result{}, nil
	}

	if sandbox.Status.Phase == boxyv1.SandboxPhaseRunning || sandbox.Status.Phase == boxyv1.SandboxPhaseCreating {
		if sandbox.Status.ControllerAddress != "" {
			baseURL := r.controllerURL(sandbox)
			err := r.ctrlClient.DeleteSandbox(ctx, baseURL, ctrlclient.DeleteSandboxReq{
				SandboxID: sandbox.Spec.SandboxID,
			})
			if err != nil && !ctrlclient.IsStaleRouteError(err) {
				r.log.Error("finalizer: failed to delete sandbox on controller", "sandbox", sandbox.Name, "err", err)
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
		}
	}

	controllerutil.RemoveFinalizer(sandbox, boxyv1.FinalizerSandboxCleanup)
	if err := r.Update(ctx, sandbox); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *SandboxReconciler) reconcilePending(ctx context.Context, sandbox *boxyv1.Sandbox) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(sandbox, boxyv1.FinalizerSandboxCleanup) {
		controllerutil.AddFinalizer(sandbox, boxyv1.FinalizerSandboxCleanup)
		if err := r.Update(ctx, sandbox); err != nil {
			return ctrl.Result{}, err
		}
	}

	podName, address, err := r.assignController(ctx)
	if err != nil {
		r.log.Info("no controller capacity, will retry", "sandbox", sandbox.Name, "err", err)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	sandbox.Status.Phase = boxyv1.SandboxPhaseCreating
	sandbox.Status.ControllerPool = r.cfg.ControllerPoolName
	sandbox.Status.ControllerPod = podName
	sandbox.Status.ControllerAddress = address
	sandbox.Status.Port = r.cfg.ControllerPort
	now := metav1.Now()
	sandbox.Status.CreatedAt = &now
	if sandbox.Spec.TTLSeconds > 0 {
		exp := metav1.NewTime(now.Add(time.Duration(sandbox.Spec.TTLSeconds) * time.Second))
		sandbox.Status.ExpiresAt = &exp
	}

	if err := r.Status().Update(ctx, sandbox); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

func (r *SandboxReconciler) reconcileCreating(ctx context.Context, sandbox *boxyv1.Sandbox) (ctrl.Result, error) {
	baseURL := r.controllerURL(sandbox)
	req := ctrlclient.CreateSandboxReq{
		SandboxID:       sandbox.Spec.SandboxID,
		Env:             sandbox.Spec.Env,
		AllowedBinaries: sandbox.Spec.AllowedBinaries,
		VM:              sandbox.Spec.VM,
		Network:         sandbox.Spec.Network,
		Volumes:         sandbox.Spec.Volumes,
		Patches:         sandbox.Spec.Patches,
		TTLSeconds:      sandbox.Spec.TTLSeconds,
	}

	if err := r.ctrlClient.CreateSandbox(ctx, baseURL, req); err != nil {
		var httpErr *ctrlclient.HTTPError
		if ctrlclient.IsStaleRouteError(err) {
			r.log.Warn("controller unreachable during create, resetting to Pending",
				"sandbox", sandbox.Name, "controller", sandbox.Status.ControllerPod, "err", err)
			sandbox.Status.Phase = boxyv1.SandboxPhasePending
			sandbox.Status.ControllerPod = ""
			sandbox.Status.ControllerAddress = ""
			sandbox.Status.Message = fmt.Sprintf("controller unreachable: %v", err)
			if err := r.Status().Update(ctx, sandbox); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		_ = httpErr
		r.log.Error("create sandbox on controller failed", "sandbox", sandbox.Name, "err", err)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	sandbox.Status.Phase = boxyv1.SandboxPhaseRunning
	sandbox.Status.Message = ""
	if err := r.Status().Update(ctx, sandbox); err != nil {
		return ctrl.Result{}, err
	}

	if sandbox.Status.ExpiresAt != nil {
		return ctrl.Result{RequeueAfter: time.Until(sandbox.Status.ExpiresAt.Time)}, nil
	}
	return ctrl.Result{}, nil
}

func (r *SandboxReconciler) reconcileRunning(ctx context.Context, sandbox *boxyv1.Sandbox) (ctrl.Result, error) {
	now := time.Now()

	if sandbox.Status.ExpiresAt != nil {
		expiry := sandbox.Status.ExpiresAt.Time
		if sandbox.Status.LastExecAt != nil && sandbox.Spec.TTLSeconds > 0 {
			slidingExpiry := sandbox.Status.LastExecAt.Time.Add(time.Duration(sandbox.Spec.TTLSeconds) * time.Second)
			if slidingExpiry.After(expiry) {
				expiry = slidingExpiry
				newExp := metav1.NewTime(expiry)
				sandbox.Status.ExpiresAt = &newExp
				if err := r.Status().Update(ctx, sandbox); err != nil {
					return ctrl.Result{}, err
				}
			}
		}

		if now.After(expiry) {
			r.log.Info("sandbox TTL expired", "sandbox", sandbox.Name)
			sandbox.Status.Phase = boxyv1.SandboxPhaseDeleting
			if err := r.Status().Update(ctx, sandbox); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{RequeueAfter: time.Until(expiry)}, nil
	}

	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

func (r *SandboxReconciler) reconcileDeleting(ctx context.Context, sandbox *boxyv1.Sandbox) (ctrl.Result, error) {
	if sandbox.Status.ControllerAddress != "" {
		baseURL := r.controllerURL(sandbox)
		err := r.ctrlClient.DeleteSandbox(ctx, baseURL, ctrlclient.DeleteSandboxReq{
			SandboxID: sandbox.Spec.SandboxID,
		})
		if err != nil && !ctrlclient.IsStaleRouteError(err) {
			r.log.Error("delete sandbox on controller failed", "sandbox", sandbox.Name, "err", err)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	}

	now := metav1.Now()
	sandbox.Status.Phase = boxyv1.SandboxPhaseTerminated
	sandbox.Status.TerminatedAt = &now
	sandbox.Status.Message = ""
	if err := r.Status().Update(ctx, sandbox); err != nil {
		return ctrl.Result{}, err
	}

	retention := time.Duration(r.cfg.TerminatedRetentionSec) * time.Second
	if sandbox.Spec.RetentionPeriod != nil {
		retention = sandbox.Spec.RetentionPeriod.Duration
	}
	if retention <= 0 {
		return ctrl.Result{Requeue: true}, nil
	}
	return ctrl.Result{RequeueAfter: retention}, nil
}

func (r *SandboxReconciler) reconcileTerminated(ctx context.Context, sandbox *boxyv1.Sandbox) (ctrl.Result, error) {
	if sandbox.Annotations != nil && sandbox.Annotations["boxy.dev/retain"] == "true" {
		return ctrl.Result{}, nil
	}

	retention := time.Duration(r.cfg.TerminatedRetentionSec) * time.Second
	if sandbox.Spec.RetentionPeriod != nil {
		retention = sandbox.Spec.RetentionPeriod.Duration
	}

	if sandbox.Status.TerminatedAt != nil && retention > 0 {
		elapsed := time.Since(sandbox.Status.TerminatedAt.Time)
		if elapsed < retention {
			return ctrl.Result{RequeueAfter: retention - elapsed}, nil
		}
	}

	r.log.Info("cleaning up terminated sandbox CR", "sandbox", sandbox.Name)
	if err := r.Delete(ctx, sandbox); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *SandboxReconciler) controllerURL(sandbox *boxyv1.Sandbox) string {
	scheme := "https"
	if r.cfg.MTLSDisabled {
		scheme = "http"
	}
	return fmt.Sprintf("%s://%s:%d", scheme, sandbox.Status.ControllerAddress, sandbox.Status.Port)
}

func (r *SandboxReconciler) assignController(ctx context.Context) (podName, address string, err error) {
	var podList corev1.PodList
	if err := r.List(ctx, &podList,
		client.InNamespace(r.cfg.Namespace),
		client.MatchingLabels{"app": "boxy-controller"},
	); err != nil {
		return "", "", fmt.Errorf("list controller pods: %w", err)
	}

	var sandboxList boxyv1.SandboxList
	if err := r.List(ctx, &sandboxList, client.InNamespace(r.cfg.Namespace)); err != nil {
		return "", "", fmt.Errorf("list sandboxes: %w", err)
	}

	podCounts := map[string]int{}
	for i := range sandboxList.Items {
		s := &sandboxList.Items[i]
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

func (r *SandboxReconciler) scaleUp(ctx context.Context) error {
	return r.scaleStatefulSet(ctx, func(current int32) (int32, error) {
		if current >= r.cfg.MaxControllerReplicas {
			return 0, fmt.Errorf("already at max controller replicas (%d)", r.cfg.MaxControllerReplicas)
		}
		return current + 1, nil
	})
}

func (r *SandboxReconciler) scaleStatefulSet(ctx context.Context, adjustFn func(int32) (int32, error)) error {
	key := types.NamespacedName{
		Namespace: r.cfg.Namespace,
		Name:      r.cfg.StatefulSetName,
	}
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
