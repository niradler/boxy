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

type SandboxRoute struct {
	ControllerPodName string `json:"podName"`
	ControllerIP      string `json:"ip"`
	Port              int32  `json:"port"`
}

type SandboxRouteStore struct {
	c         kubernetes.Interface
	namespace string
	name      string
	mu        sync.RWMutex
	cache     map[string]SandboxRoute
}

func NewSandboxRouteStore(c kubernetes.Interface, namespace, name string) *SandboxRouteStore {
	return &SandboxRouteStore{
		c:         c,
		namespace: namespace,
		name:      name,
		cache:     map[string]SandboxRoute{},
	}
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
	cm, err := s.ensureConfigMap(ctx)
	if err != nil {
		return err
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[sandboxID] = string(data)
	_, err = s.c.CoreV1().ConfigMaps(s.namespace).Update(ctx, cm, metav1.UpdateOptions{})
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.cache[sandboxID] = route
	s.mu.Unlock()
	return nil
}

func (s *SandboxRouteStore) Delete(ctx context.Context, sandboxID string) error {
	cm, err := s.ensureConfigMap(ctx)
	if err != nil {
		return err
	}
	if cm.Data != nil {
		delete(cm.Data, sandboxID)
	}
	_, err = s.c.CoreV1().ConfigMaps(s.namespace).Update(ctx, cm, metav1.UpdateOptions{})
	if err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.cache, sandboxID)
	s.mu.Unlock()
	return nil
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
	return s.c.CoreV1().ConfigMaps(s.namespace).Create(ctx, cm, metav1.CreateOptions{})
}
