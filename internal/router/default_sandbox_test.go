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

func TestEnsureDefaultSandbox_AlreadyExists(t *testing.T) {
	ctrl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not call controller when sandbox already exists")
	}))
	defer ctrl.Close()

	sb := testSandbox("default", "default")
	srv := newTestServer(t, ctrl.URL, []runtime.Object{sb}, func(cfg *Config) {
		cfg.DefaultSandboxEnabled = true
		cfg.DefaultSandboxConfig = &api.SandboxCreateBody{
			SandboxID:  "default",
			TTLSeconds: 86400,
		}
	})

	if err := srv.EnsureDefaultSandbox(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
