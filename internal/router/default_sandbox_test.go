package router

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"

	"boxy.dev/boxy/internal/api"
)

func TestEnsureDefaultSandbox_Disabled(t *testing.T) {
	ctrl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not call controller when disabled")
	}))
	defer ctrl.Close()
	srv := newTestServer(t, ctrl.URL, nil)

	if err := srv.EnsureDefaultSandbox(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveDefaultSandboxID_Disabled(t *testing.T) {
	ctrl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ctrl.Close()
	srv := newTestServer(t, ctrl.URL, nil)

	_, err := srv.resolveDefaultSandboxID(context.Background())
	if err == nil {
		t.Fatal("expected error when default sandbox disabled")
	}
}

func TestEnsureDefaultSandbox_AlreadyExists(t *testing.T) {
	ctrl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not call controller when sandbox already exists")
	}))
	defer ctrl.Close()

	sb := testSandbox("default", "default", "127.0.0.1", 8080, "Running")
	srv := newTestServer(t, ctrl.URL, []runtime.Object{sb}, func(cfg *Config) {
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
}
