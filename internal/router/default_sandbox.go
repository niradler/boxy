package router

import (
	"context"
	"fmt"
	"sync"
	"time"

	"boxy.dev/boxy/internal/api"
	"boxy.dev/boxy/internal/kube"
)

var defaultSandboxMu sync.Mutex

func (s *Server) EnsureDefaultSandbox(ctx context.Context) error {
	if !s.cfg.DefaultSandboxEnabled || s.cfg.DefaultSandboxConfig == nil {
		return nil
	}

	defaultSandboxMu.Lock()
	defer defaultSandboxMu.Unlock()

	id := s.cfg.DefaultSandboxConfig.SandboxID
	if _, ok, _ := s.store.Get(ctx, id); ok {
		return nil
	}

	_, err := s.createSandboxFromBody(ctx, s.cfg.DefaultSandboxConfig)
	if err != nil {
		return fmt.Errorf("create default sandbox: %w", err)
	}
	s.log.Info("default sandbox created", "sandboxId", id)
	return nil
}

func (s *Server) resolveDefaultSandboxID(ctx context.Context) (string, error) {
	if !s.cfg.DefaultSandboxEnabled || s.cfg.DefaultSandboxConfig == nil {
		return "", fmt.Errorf("default sandbox is disabled")
	}
	id := s.cfg.DefaultSandboxConfig.SandboxID
	if _, ok, _ := s.store.Get(ctx, id); ok {
		return id, nil
	}
	if err := s.EnsureDefaultSandbox(ctx); err != nil {
		return "", err
	}
	return id, nil
}

// createSandboxFromBody is the shared core for sandbox creation, used by both
// the HTTP handler and the default sandbox manager.
func (s *Server) createSandboxFromBody(ctx context.Context, body *api.SandboxCreateBody) (*api.SandboxResponseBody, error) {
	pod, err := s.claimControllerSeat(ctx)
	if err != nil {
		return nil, fmt.Errorf("controller pod: %w", err)
	}
	if pod.Status.PodIP == "" {
		podName := pod.Name
		var waitErr error
		pod, waitErr = kube.WaitForPodIP(ctx, s.cfg.Kube, s.cfg.SandboxNamespace, podName, 30*time.Second)
		if waitErr != nil {
			_ = kube.IncrementSandboxCount(ctx, s.cfg.Kube, s.cfg.SandboxNamespace, podName, -1)
			return nil, fmt.Errorf("controller pod not ready: %w", waitErr)
		}
	}

	if err := kube.RefreshControllerTTL(ctx, s.cfg.Kube, s.cfg.SandboxNamespace, pod.Name, s.ctrlSpec.TTLSeconds); err != nil {
		s.log.Warn("failed to refresh controller TTL before create", "pod", pod.Name, "err", err)
	}

	baseURL := s.controllerURL(kube.SandboxRoute{
		ControllerPodName: pod.Name,
		ControllerIP:      pod.Status.PodIP,
		Port:              s.ctrlSpec.Port,
	})

	req := CreateSandboxReq{
		SandboxID:       body.SandboxID,
		Env:             body.Env,
		AllowedBinaries: body.AllowedBinaries,
		VM:              body.VM,
		Network:         body.Network,
		Volumes:         body.Volumes,
		Patches:         body.Patches,
		TTLSeconds:      body.TTLSeconds,
	}
	if err := s.ctrlClient.CreateSandbox(ctx, baseURL, req); err != nil {
		if derr := kube.IncrementSandboxCount(ctx, s.cfg.Kube, s.cfg.SandboxNamespace, pod.Name, -1); derr != nil {
			s.log.Warn("release controller seat after create failure", "pod", pod.Name, "err", derr)
		}
		return nil, fmt.Errorf("create sandbox: %w", err)
	}

	route := kube.SandboxRoute{
		ControllerPodName: pod.Name,
		ControllerIP:      pod.Status.PodIP,
		Port:              s.ctrlSpec.Port,
	}
	if err := s.store.Set(ctx, body.SandboxID, route); err != nil {
		return nil, fmt.Errorf("store route: %w", err)
	}

	_ = kube.RefreshControllerTTL(ctx, s.cfg.Kube, s.cfg.SandboxNamespace, pod.Name, s.ctrlSpec.TTLSeconds)

	return &api.SandboxResponseBody{
		SandboxID: body.SandboxID,
		SessionID: body.SessionID,
		Owner:     body.Owner,
		Runtime:   "microsandbox",
		PodRef:    api.PodRef{Namespace: pod.Namespace, Name: pod.Name, UID: string(pod.UID)},
		Phase:     string(pod.Status.Phase),
		Ready:     kube.PodRunningReady(pod),
	}, nil
}
