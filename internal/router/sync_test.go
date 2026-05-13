package router

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"boxy.dev/boxy/internal/api"
	"boxy.dev/boxy/internal/kube"
)

func newControllerPod(name, ip string, port int32) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels: map[string]string{
				api.LabelManagedBy:     "boxy",
				api.LabelControllerPod: "true",
			},
			Annotations: map[string]string{
				api.AnnotationControllerPort: strconv.Itoa(int(port)),
				api.AnnotationSandboxCount:   "0",
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: ip},
	}
}

func listEndpoint(t *testing.T, ids ...string) (*httptest.Server, int32) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/sandboxes" {
			http.NotFound(w, r)
			return
		}
		type item struct {
			SandboxID string `json:"sandbox_id"`
		}
		out := struct {
			Sandboxes []item `json:"sandboxes"`
		}{}
		for _, id := range ids {
			out.Sandboxes = append(out.Sandboxes, item{SandboxID: id})
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := strconv.Atoi(u.Port())
	return srv, int32(p)
}

func TestSync_RebuildsStoreFromControllers(t *testing.T) {
	stub, port := listEndpoint(t, "sb-a", "sb-b")
	u, _ := url.Parse(stub.URL)
	pod := newControllerPod("ctrl-1", u.Hostname(), port)
	kc := fake.NewSimpleClientset(pod)
	store := kube.NewSandboxRouteStore(kc, "default", "boxy-sandbox-routes")

	rec := NewSyncReconciler(SyncReconcilerConfig{
		Kube:      kc,
		Namespace: "default",
		Store:     store,
		Scheme:    "http",
		Port:      port,
		Cooldown:  10 * time.Millisecond,
	})
	if err := rec.Trigger(context.Background(), "test"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	for _, id := range []string{"sb-a", "sb-b"} {
		got, ok, err := store.Get(context.Background(), id)
		if err != nil || !ok {
			t.Fatalf("store missing %s: ok=%v err=%v", id, ok, err)
		}
		if got.ControllerPodName != "ctrl-1" || got.Port != port {
			t.Fatalf("unexpected route for %s: %+v", id, got)
		}
	}
}

func TestSync_PrunesDeadSandboxes(t *testing.T) {
	stub, port := listEndpoint(t, "sb-a")
	u, _ := url.Parse(stub.URL)
	pod := newControllerPod("ctrl-1", u.Hostname(), port)
	kc := fake.NewSimpleClientset(pod)
	store := kube.NewSandboxRouteStore(kc, "default", "boxy-sandbox-routes")
	_ = store.Set(context.Background(), "sb-stale", kube.SandboxRoute{
		ControllerPodName: "ctrl-old", ControllerIP: "10.0.0.99", Port: 8080,
	})

	rec := NewSyncReconciler(SyncReconcilerConfig{
		Kube:      kc,
		Namespace: "default",
		Store:     store,
		Scheme:    "http",
		Port:      port,
		Cooldown:  10 * time.Millisecond,
	})
	if err := rec.Trigger(context.Background(), "test"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if _, ok, _ := store.Get(context.Background(), "sb-stale"); ok {
		t.Fatal("stale entry should have been pruned by sync")
	}
	if _, ok, _ := store.Get(context.Background(), "sb-a"); !ok {
		t.Fatal("expected sb-a to be present")
	}
}

func TestSync_RateLimitCooldown(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(`{"sandboxes":[]}`))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	pod := newControllerPod("ctrl-1", u.Hostname(), int32(port))
	kc := fake.NewSimpleClientset(pod)
	store := kube.NewSandboxRouteStore(kc, "default", "boxy-sandbox-routes")

	rec := NewSyncReconciler(SyncReconcilerConfig{
		Kube:      kc,
		Namespace: "default",
		Store:     store,
		Scheme:    "http",
		Port:      int32(port),
		Cooldown:  time.Hour,
	})
	if err := rec.Trigger(context.Background(), "first"); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := rec.Trigger(context.Background(), "second"); err != nil {
		t.Fatalf("second: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("expected one controller fetch (cooldown active), got %d", got)
	}
}

func TestSync_SingleflightCoalescesConcurrentTriggers(t *testing.T) {
	var hits int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		<-release
		_, _ = w.Write([]byte(`{"sandboxes":[]}`))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	pod := newControllerPod("ctrl-1", u.Hostname(), int32(port))
	kc := fake.NewSimpleClientset(pod)
	store := kube.NewSandboxRouteStore(kc, "default", "boxy-sandbox-routes")

	rec := NewSyncReconciler(SyncReconcilerConfig{
		Kube:      kc,
		Namespace: "default",
		Store:     store,
		Scheme:    "http",
		Port:      int32(port),
		Cooldown:  10 * time.Millisecond,
	})

	var wg sync.WaitGroup
	wg.Add(5)
	for range 5 {
		go func() {
			defer wg.Done()
			_ = rec.Trigger(context.Background(), "concurrent")
		}()
	}
	time.Sleep(50 * time.Millisecond) // let triggers converge on the in-flight call
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("expected exactly one controller fetch under singleflight, got %d", got)
	}
}

func TestSync_AllControllersFail_PreservesCache(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	pod := newControllerPod("ctrl-1", u.Hostname(), int32(port))
	kc := fake.NewSimpleClientset(pod)
	store := kube.NewSandboxRouteStore(kc, "default", "boxy-sandbox-routes")
	_ = store.Set(context.Background(), "sb-keep", kube.SandboxRoute{
		ControllerPodName: "ctrl-1", ControllerIP: u.Hostname(), Port: int32(port),
	})

	rec := NewSyncReconciler(SyncReconcilerConfig{
		Kube:      kc,
		Namespace: "default",
		Store:     store,
		Scheme:    "http",
		Port:      int32(port),
		Cooldown:  10 * time.Millisecond,
	})
	err := rec.Trigger(context.Background(), "all-fail")
	if err == nil {
		t.Fatal("expected sync to fail when every controller fetch fails")
	}
	if _, ok, _ := store.Get(context.Background(), "sb-keep"); !ok {
		t.Fatal("cache entry must not be wiped when sync cannot reach any controller")
	}
}

func TestSync_NoControllers_Succeeds(t *testing.T) {
	kc := fake.NewSimpleClientset()
	store := kube.NewSandboxRouteStore(kc, "default", "boxy-sandbox-routes")
	rec := NewSyncReconciler(SyncReconcilerConfig{
		Kube:      kc,
		Namespace: "default",
		Store:     store,
		Scheme:    "http",
		Cooldown:  10 * time.Millisecond,
	})
	if err := rec.Trigger(context.Background(), "empty"); err != nil {
		t.Fatalf("sync on empty cluster: %v", err)
	}
}

func TestIsStaleRouteError(t *testing.T) {
	if IsStaleRouteError(nil) {
		t.Fatal("nil error is not stale")
	}
	if !IsStaleRouteError(&ControllerHTTPError{Status: http.StatusNotFound}) {
		t.Fatal("404 should be stale")
	}
	if IsStaleRouteError(&ControllerHTTPError{Status: http.StatusBadGateway}) {
		t.Fatal("502 should not be stale")
	}
	if !IsStaleRouteError(fmt.Errorf("wrap: %w", &timeoutErr{})) {
		t.Fatal("net.Error should be stale")
	}
}

type timeoutErr struct{}

func (*timeoutErr) Error() string   { return "i/o timeout" }
func (*timeoutErr) Timeout() bool   { return true }
func (*timeoutErr) Temporary() bool { return false }
