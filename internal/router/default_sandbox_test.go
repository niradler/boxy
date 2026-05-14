package router

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"boxy.dev/boxy/internal/api"
	"boxy.dev/boxy/internal/kube"
	"k8s.io/client-go/kubernetes/fake"
)

func TestEnsureDefaultSandbox_Disabled(t *testing.T) {
	kc := fake.NewSimpleClientset()
	ctrl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not call controller when disabled")
	}))
	defer ctrl.Close()
	srv := newTestServer(t, kc, ctrl.URL)

	if err := srv.EnsureDefaultSandbox(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEnsureDefaultSandbox_AlreadyExists(t *testing.T) {
	ctrl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not call controller when sandbox already in store")
	}))
	defer ctrl.Close()
	u, _ := url.Parse(ctrl.URL)
	port, _ := strconv.Atoi(u.Port())
	pod := newControllerPod("ctrl-1", u.Hostname(), int32(port))
	kc := fake.NewSimpleClientset(pod)

	srv := newTestServer(t, kc, ctrl.URL, func(cfg *Config) {
		cfg.DefaultSandboxEnabled = true
		cfg.DefaultSandboxConfig = &api.SandboxCreateBody{
			SandboxID:  "default",
			SessionID:  "default-box",
			Owner:      "system",
			TTLSeconds: 86400,
		}
	})

	_ = srv.store.Set(context.Background(), "default", kube.SandboxRoute{
		ControllerPodName: "ctrl-1",
		ControllerIP:      u.Hostname(),
		Port:              int32(port),
	})

	if err := srv.EnsureDefaultSandbox(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEnsureDefaultSandbox_CreatesNew(t *testing.T) {
	var createCalled bool
	ctrl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes" {
			createCalled = true
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes" {
			_ = json.NewEncoder(w).Encode(map[string]any{"sandboxes": []any{}})
			return
		}
		http.NotFound(w, r)
	}))
	defer ctrl.Close()
	u, _ := url.Parse(ctrl.URL)
	port, _ := strconv.Atoi(u.Port())
	pod := newControllerPod("ctrl-1", u.Hostname(), int32(port))
	kc := fake.NewSimpleClientset(pod)

	srv := newTestServer(t, kc, ctrl.URL, func(cfg *Config) {
		cfg.DefaultSandboxEnabled = true
		cfg.DefaultSandboxConfig = &api.SandboxCreateBody{
			SandboxID:  "default",
			SessionID:  "default-box",
			Owner:      "system",
			TTLSeconds: 86400,
		}
	})

	if err := srv.EnsureDefaultSandbox(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !createCalled {
		t.Fatal("expected controller create to be called")
	}
	_, ok, err := srv.store.Get(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected 'default' route in store after creation")
	}
}

func TestResolveDefaultSandboxID_Disabled(t *testing.T) {
	kc := fake.NewSimpleClientset()
	ctrl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ctrl.Close()
	srv := newTestServer(t, kc, ctrl.URL)

	_, err := srv.resolveDefaultSandboxID(context.Background())
	if err == nil {
		t.Fatal("expected error when default sandbox disabled")
	}
}

func TestResolveDefaultSandboxID_LazyCreation(t *testing.T) {
	ctrl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes" {
			_ = json.NewEncoder(w).Encode(map[string]any{"sandboxes": []any{}})
			return
		}
		http.NotFound(w, r)
	}))
	defer ctrl.Close()
	u, _ := url.Parse(ctrl.URL)
	port, _ := strconv.Atoi(u.Port())
	pod := newControllerPod("ctrl-1", u.Hostname(), int32(port))
	kc := fake.NewSimpleClientset(pod)

	srv := newTestServer(t, kc, ctrl.URL, func(cfg *Config) {
		cfg.DefaultSandboxEnabled = true
		cfg.DefaultSandboxConfig = &api.SandboxCreateBody{
			SandboxID:  "default",
			SessionID:  "default-box",
			Owner:      "system",
			TTLSeconds: 86400,
		}
	})

	id, err := srv.resolveDefaultSandboxID(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "default" {
		t.Fatalf("sandboxId = %q, want %q", id, "default")
	}
}
