package router

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"

	"boxy.dev/boxy/internal/api"
	"boxy.dev/boxy/internal/kube"
)

// SyncReconciler rebuilds routing state from controller ground truth.
// Triggers are singleflighted and rate-limited by Cooldown.
type SyncReconciler struct {
	kc            kubernetes.Interface
	namespace     string
	store         *kube.SandboxRouteStore
	httpClient    *http.Client
	scheme        string
	port          int32
	cooldown      time.Duration
	log           *slog.Logger
	listerTimeout time.Duration

	mu          sync.Mutex
	inFlight    *syncCall
	lastSuccess time.Time
}

type syncCall struct {
	done chan struct{}
	err  error
}

type SyncReconcilerConfig struct {
	Kube          kubernetes.Interface
	Namespace     string
	Store         *kube.SandboxRouteStore
	HTTPClient    *http.Client
	Scheme        string
	Port          int32
	Cooldown      time.Duration
	FanoutTimeout time.Duration
	Logger        *slog.Logger
}

func NewSyncReconciler(cfg SyncReconcilerConfig) *SyncReconciler {
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = 5 * time.Second
	}
	if cfg.FanoutTimeout <= 0 {
		cfg.FanoutTimeout = 5 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	if cfg.Scheme == "" {
		cfg.Scheme = "https"
	}
	return &SyncReconciler{
		kc:            cfg.Kube,
		namespace:     cfg.Namespace,
		store:         cfg.Store,
		httpClient:    cfg.HTTPClient,
		scheme:        cfg.Scheme,
		port:          cfg.Port,
		cooldown:      cfg.Cooldown,
		listerTimeout: cfg.FanoutTimeout,
		log:           cfg.Logger,
	}
}

// Trigger runs (or joins) one sync. Cooldown short-circuits to nil; concurrent
// callers share the in-flight result.
func (r *SyncReconciler) Trigger(ctx context.Context, reason string) error {
	r.mu.Lock()
	if !r.lastSuccess.IsZero() && time.Since(r.lastSuccess) < r.cooldown {
		r.mu.Unlock()
		return nil
	}
	if r.inFlight != nil {
		call := r.inFlight
		r.mu.Unlock()
		select {
		case <-call.done:
			return call.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	call := &syncCall{done: make(chan struct{})}
	r.inFlight = call
	r.mu.Unlock()

	r.log.Info("sync reconciler firing", "reason", reason)
	// Detach from caller ctx — a request-scoped trigger must not cancel the sync.
	syncCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := r.runOnce(syncCtx)

	r.mu.Lock()
	if err == nil {
		r.lastSuccess = time.Now()
	}
	call.err = err
	close(call.done)
	r.inFlight = nil
	r.mu.Unlock()

	if err != nil {
		r.log.Error("sync reconciler failed", "reason", reason, "err", err)
	} else {
		r.log.Info("sync reconciler succeeded", "reason", reason)
	}
	return err
}

// TriggerAsync fires-and-forgets; for use from request handlers.
func (r *SyncReconciler) TriggerAsync(reason string) {
	go func() {
		_ = r.Trigger(context.Background(), reason)
	}()
}

func (r *SyncReconciler) runOnce(ctx context.Context) error {
	sel := labels.SelectorFromSet(labels.Set{
		api.LabelManagedBy:     "boxy",
		api.LabelControllerPod: "true",
	})
	pods, err := r.kc.CoreV1().Pods(r.namespace).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return fmt.Errorf("list controller pods: %w", err)
	}

	routes := map[string]kube.SandboxRoute{}
	var anySuccess bool
	var anyAttempt bool
	for i := range pods.Items {
		p := &pods.Items[i]
		if !controllerReadyForSync(p) {
			continue
		}
		anyAttempt = true
		port := controllerPort(p, r.port)
		ids, perr := r.fetchSandboxIDs(ctx, p.Status.PodIP, port)
		if perr != nil {
			r.log.Warn("sync: controller fetch failed",
				"pod", p.Name, "ip", p.Status.PodIP, "err", perr)
			continue
		}
		anySuccess = true
		for _, id := range ids {
			routes[id] = kube.SandboxRoute{
				ControllerPodName: p.Name,
				ControllerIP:      p.Status.PodIP,
				Port:              port,
			}
		}
	}

	// Refuse to wipe the cache when controllers exist but all are unreachable —
	// would break exec for every live sandbox.
	if anyAttempt && !anySuccess {
		return fmt.Errorf("sync: every controller fetch failed")
	}

	if err := r.store.ReplaceAll(ctx, routes); err != nil {
		return fmt.Errorf("reconcile store: %w", err)
	}
	return nil
}

func (r *SyncReconciler) fetchSandboxIDs(ctx context.Context, ip string, port int32) ([]string, error) {
	url := fmt.Sprintf("%s://%s:%d/v1/sandboxes", r.scheme, ip, port)
	cctx, cancel := context.WithTimeout(ctx, r.listerTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("controller GET %s returned %d", url, resp.StatusCode)
	}
	var body struct {
		Sandboxes []struct {
			SandboxID string `json:"sandbox_id"`
		} `json:"sandboxes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode controller response: %w", err)
	}
	ids := make([]string, 0, len(body.Sandboxes))
	for _, s := range body.Sandboxes {
		if s.SandboxID != "" {
			ids = append(ids, s.SandboxID)
		}
	}
	return ids, nil
}

func controllerReadyForSync(p *corev1.Pod) bool {
	if p.DeletionTimestamp != nil {
		return false
	}
	if p.Status.Phase != corev1.PodRunning {
		return false
	}
	if p.Status.PodIP == "" {
		return false
	}
	return true
}

func controllerPort(p *corev1.Pod, fallback int32) int32 {
	if p.Annotations != nil {
		if v := p.Annotations[api.AnnotationControllerPort]; v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				return int32(n)
			}
		}
	}
	return fallback
}
