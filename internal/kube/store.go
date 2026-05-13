package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const storeMaxRetries = 8

type SandboxRoute struct {
	ControllerPodName string `json:"podName"`
	ControllerIP      string `json:"ip"`
	Port              int32  `json:"port"`
}

type SandboxRouteStore struct {
	c              kubernetes.Interface
	namespace      string
	name           string
	mu             sync.RWMutex
	cache          map[string]SandboxRoute
	onConflictSync func(ctx context.Context, reason string)
	onParseError   func(ctx context.Context, reason string)
}

func NewSandboxRouteStore(c kubernetes.Interface, namespace, name string) *SandboxRouteStore {
	return &SandboxRouteStore{
		c:         c,
		namespace: namespace,
		name:      name,
		cache:     map[string]SandboxRoute{},
	}
}

// SetSyncHooks wires reconciler triggers for conflict-after-retries and
// JSON parse errors. Hooks must be non-blocking.
func (s *SandboxRouteStore) SetSyncHooks(onConflict, onParseError func(ctx context.Context, reason string)) {
	s.onConflictSync = onConflict
	s.onParseError = onParseError
}

func (s *SandboxRouteStore) Get(ctx context.Context, sandboxID string) (SandboxRoute, bool, error) {
	s.mu.RLock()
	r, ok := s.cache[sandboxID]
	s.mu.RUnlock()
	if ok {
		return r, true, nil
	}
	return s.getFromConfigMap(ctx, sandboxID)
}

func (s *SandboxRouteStore) Set(ctx context.Context, sandboxID string, route SandboxRoute) error {
	data, err := json.Marshal(route)
	if err != nil {
		return err
	}
	encoded := string(data)
	if err := s.mutateConfigMap(ctx, func(cm *corev1.ConfigMap) {
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[sandboxID] = encoded
	}); err != nil {
		return err
	}
	s.mu.Lock()
	s.cache[sandboxID] = route
	s.mu.Unlock()
	return nil
}

func (s *SandboxRouteStore) Delete(ctx context.Context, sandboxID string) error {
	if err := s.mutateConfigMap(ctx, func(cm *corev1.ConfigMap) {
		if cm.Data != nil {
			delete(cm.Data, sandboxID)
		}
	}); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.cache, sandboxID)
	s.mu.Unlock()
	return nil
}

func (s *SandboxRouteStore) mutateConfigMap(ctx context.Context, mutate func(*corev1.ConfigMap)) error {
	for range storeMaxRetries {
		cm, err := s.ensureConfigMap(ctx)
		if err != nil {
			return err
		}
		mutate(cm)
		_, err = s.c.CoreV1().ConfigMaps(s.namespace).Update(ctx, cm, metav1.UpdateOptions{})
		if err == nil {
			return nil
		}
		if !errors.IsConflict(err) {
			return err
		}
	}
	if s.onConflictSync != nil {
		s.onConflictSync(ctx, "store conflict after retries")
	}
	return fmt.Errorf("sandbox route store conflict after %d retries", storeMaxRetries)
}

func (s *SandboxRouteStore) getFromConfigMap(ctx context.Context, sandboxID string) (SandboxRoute, bool, error) {
	cm, err := s.c.CoreV1().ConfigMaps(s.namespace).Get(ctx, s.name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return SandboxRoute{}, false, nil
	}
	if err != nil {
		return SandboxRoute{}, false, err
	}
	raw, ok := cm.Data[sandboxID]
	if !ok {
		return SandboxRoute{}, false, nil
	}
	var r SandboxRoute
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		if s.onParseError != nil {
			s.onParseError(ctx, fmt.Sprintf("corrupt route for %s", sandboxID))
		}
		return SandboxRoute{}, false, fmt.Errorf("corrupt route for %s: %w", sandboxID, err)
	}
	s.mu.Lock()
	s.cache[sandboxID] = r
	s.mu.Unlock()
	return r, true, nil
}

func (s *SandboxRouteStore) ensureConfigMap(ctx context.Context) (*corev1.ConfigMap, error) {
	cm, err := s.c.CoreV1().ConfigMaps(s.namespace).Get(ctx, s.name, metav1.GetOptions{})
	if err == nil {
		return cm, nil
	}
	if !errors.IsNotFound(err) {
		return nil, err
	}
	cm = &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      s.name,
			Namespace: s.namespace,
		},
		Data: map[string]string{},
	}
	created, err := s.c.CoreV1().ConfigMaps(s.namespace).Create(ctx, cm, metav1.CreateOptions{})
	if err == nil {
		return created, nil
	}
	if errors.IsAlreadyExists(err) {
		return s.c.CoreV1().ConfigMaps(s.namespace).Get(ctx, s.name, metav1.GetOptions{})
	}
	return nil, err
}

// ReplaceAll overwrites both the memory cache and the backing ConfigMap.
func (s *SandboxRouteStore) ReplaceAll(ctx context.Context, routes map[string]SandboxRoute) error {
	encoded := make(map[string]string, len(routes))
	for id, r := range routes {
		data, err := json.Marshal(r)
		if err != nil {
			return fmt.Errorf("marshal route %s: %w", id, err)
		}
		encoded[id] = string(data)
	}
	if err := s.mutateConfigMap(ctx, func(cm *corev1.ConfigMap) {
		cm.Data = encoded
	}); err != nil {
		return err
	}
	next := make(map[string]SandboxRoute, len(routes))
	for id, r := range routes {
		next[id] = r
	}
	s.mu.Lock()
	s.cache = next
	s.mu.Unlock()
	return nil
}

// InvalidateCache drops one entry from memory; the ConfigMap is left for the
// reconciler to fix.
func (s *SandboxRouteStore) InvalidateCache(sandboxID string) {
	s.mu.Lock()
	delete(s.cache, sandboxID)
	s.mu.Unlock()
}
