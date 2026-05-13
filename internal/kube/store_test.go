package kube_test

import (
	"context"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

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
