package kube_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"boxy.dev/boxy/internal/kube"
)

func TestStore_SetAndGet(t *testing.T) {
	c := fake.NewSimpleClientset()
	store := kube.NewSandboxRouteStore(c, "default", "boxy-sandbox-routes")

	route := kube.SandboxRoute{
		ControllerPodName: "boxy-ctrl-1",
		ControllerIP:      "10.0.0.1",
		Port:              8080,
	}
	if err := store.Set(context.Background(), "sandbox-1", route); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.Get(context.Background(), "sandbox-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected entry")
	}
	if got.ControllerPodName != "boxy-ctrl-1" {
		t.Fatalf("got %v", got)
	}
}

func TestStore_Delete(t *testing.T) {
	c := fake.NewSimpleClientset()
	store := kube.NewSandboxRouteStore(c, "default", "boxy-sandbox-routes")
	route := kube.SandboxRoute{ControllerPodName: "boxy-ctrl-1", ControllerIP: "10.0.0.1", Port: 8080}
	_ = store.Set(context.Background(), "sandbox-1", route)
	if err := store.Delete(context.Background(), "sandbox-1"); err != nil {
		t.Fatal(err)
	}
	_, ok, err := store.Get(context.Background(), "sandbox-1")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected entry deleted")
	}
}

func TestStore_Set_ConflictRetry(t *testing.T) {
	c := fake.NewSimpleClientset(&corev1.ConfigMap{})
	var conflicts int32 = 3
	c.PrependReactor("update", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if atomic.LoadInt32(&conflicts) > 0 {
			atomic.AddInt32(&conflicts, -1)
			gr := schema.GroupResource{Group: "", Resource: "configmaps"}
			return true, nil, apierrors.NewConflict(gr, "boxy-sandbox-routes", fmt.Errorf("simulated"))
		}
		return false, nil, nil
	})
	store := kube.NewSandboxRouteStore(c, "default", "boxy-sandbox-routes")
	route := kube.SandboxRoute{ControllerPodName: "p", ControllerIP: "1.2.3.4", Port: 8080}
	if err := store.Set(context.Background(), "sb-1", route); err != nil {
		t.Fatalf("Set returned err after retries: %v", err)
	}
	got, ok, err := store.Get(context.Background(), "sb-1")
	if err != nil || !ok {
		t.Fatalf("Get after Set: ok=%v err=%v", ok, err)
	}
	if got.ControllerIP != "1.2.3.4" {
		t.Fatalf("unexpected route: %+v", got)
	}
}

func TestStore_Set_NoLostWrite(t *testing.T) {
	c := fake.NewSimpleClientset(&corev1.ConfigMap{})
	store := kube.NewSandboxRouteStore(c, "default", "boxy-sandbox-routes")
	const N = 16
	var wg sync.WaitGroup
	wg.Add(N)
	for i := range N {
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("sb-%02d", i)
			r := kube.SandboxRoute{ControllerPodName: id, ControllerIP: "10.0.0.1", Port: 8080}
			if err := store.Set(context.Background(), id, r); err != nil {
				t.Errorf("Set(%s): %v", id, err)
			}
		}()
	}
	wg.Wait()
	for i := range N {
		id := fmt.Sprintf("sb-%02d", i)
		_, ok, err := store.Get(context.Background(), id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if !ok {
			t.Fatalf("missing entry %s after concurrent writes", id)
		}
	}
}

func TestStore_ParseError_TriggersHook(t *testing.T) {
	c := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "boxy-sandbox-routes", Namespace: "default"},
		Data:       map[string]string{"sb-bad": "{not json"},
	})
	store := kube.NewSandboxRouteStore(c, "default", "boxy-sandbox-routes")
	var fired int32
	store.SetSyncHooks(nil, func(_ context.Context, _ string) {
		atomic.AddInt32(&fired, 1)
	})
	_, ok, err := store.Get(context.Background(), "sb-bad")
	if err == nil {
		t.Fatalf("expected parse error, got ok=%v", ok)
	}
	if atomic.LoadInt32(&fired) == 0 {
		t.Fatal("parse error hook did not fire")
	}
}
