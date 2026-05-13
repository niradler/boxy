package kube_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
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

	"boxy.dev/boxy/internal/api"
	"boxy.dev/boxy/internal/kube"
)

// installPodRVCheck makes the fake client honor ResourceVersion on pod Updates
// (rejecting stale-RV writes with 409) — the default fake tracker stores RV
// but never validates it.
func installPodRVCheck(t *testing.T, c *fake.Clientset) {
	t.Helper()
	var mu sync.Mutex
	var rvCounter int64 = 1000
	c.PrependReactor("update", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		ua, ok := action.(clienttesting.UpdateAction)
		if !ok {
			return false, nil, nil
		}
		incoming := ua.GetObject().(*corev1.Pod).DeepCopy()
		current, err := c.Tracker().Get(action.GetResource(), action.GetNamespace(), incoming.Name)
		if err != nil {
			return false, nil, nil
		}
		curPod := current.(*corev1.Pod)
		if incoming.ResourceVersion != curPod.ResourceVersion {
			gr := schema.GroupResource{Resource: "pods"}
			return true, nil, apierrors.NewConflict(gr, incoming.Name, fmt.Errorf("rv %s != %s", incoming.ResourceVersion, curPod.ResourceVersion))
		}
		rvCounter++
		incoming.ResourceVersion = strconv.FormatInt(rvCounter, 10)
		if err := c.Tracker().Update(action.GetResource(), incoming, action.GetNamespace()); err != nil {
			return true, nil, err
		}
		return true, incoming, nil
	})
}

func newControllerPodWithCount(name string, count int) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels: map[string]string{
				api.LabelManagedBy:     "boxy",
				api.LabelControllerPod: "true",
			},
			Annotations: map[string]string{
				api.AnnotationSandboxCount: strconv.Itoa(count),
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.1"},
	}
}

func podSandboxCount(t *testing.T, c *fake.Clientset, name string) int {
	t.Helper()
	pod, err := c.CoreV1().Pods("default").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(pod.Annotations[api.AnnotationSandboxCount])
	return n
}

func TestClaimSandboxSlot_RefusesWhenFull(t *testing.T) {
	c := fake.NewSimpleClientset(newControllerPodWithCount("ctrl-1", 10))
	err := kube.ClaimSandboxSlot(context.Background(), c, "default", "ctrl-1", 10)
	if !errors.Is(err, kube.ErrControllerFull) {
		t.Fatalf("expected ErrControllerFull, got %v", err)
	}
	if got := podSandboxCount(t, c, "ctrl-1"); got != 10 {
		t.Fatalf("count must be unchanged on Full, got %d", got)
	}
}

func TestClaimSandboxSlot_IncrementsBelowCapacity(t *testing.T) {
	c := fake.NewSimpleClientset(newControllerPodWithCount("ctrl-1", 3))
	if err := kube.ClaimSandboxSlot(context.Background(), c, "default", "ctrl-1", 10); err != nil {
		t.Fatal(err)
	}
	if got := podSandboxCount(t, c, "ctrl-1"); got != 4 {
		t.Fatalf("expected count=4, got %d", got)
	}
}

func TestClaimSandboxSlot_ConcurrentNeverOversubscribes(t *testing.T) {
	const N = 32
	const K = 10
	c := fake.NewSimpleClientset(newControllerPodWithCount("ctrl-1", 0))
	installPodRVCheck(t, c)

	var success, full int32
	var wg sync.WaitGroup
	wg.Add(N)
	for i := range N {
		_ = i
		go func() {
			defer wg.Done()
			err := kube.ClaimSandboxSlot(context.Background(), c, "default", "ctrl-1", K)
			switch {
			case err == nil:
				atomic.AddInt32(&success, 1)
			case errors.Is(err, kube.ErrControllerFull):
				atomic.AddInt32(&full, 1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&success); got != K {
		t.Fatalf("expected %d successful claims, got %d", K, got)
	}
	if got := atomic.LoadInt32(&full); got != N-K {
		t.Fatalf("expected %d ErrControllerFull, got %d", N-K, got)
	}
	if got := podSandboxCount(t, c, "ctrl-1"); got != K {
		t.Fatalf("count must be exactly %d after concurrent claims, got %d", K, got)
	}
}

func TestIncrementSandboxCount_Decrement(t *testing.T) {
	c := fake.NewSimpleClientset(newControllerPodWithCount("ctrl-1", 5))
	if err := kube.IncrementSandboxCount(context.Background(), c, "default", "ctrl-1", -1); err != nil {
		t.Fatal(err)
	}
	if got := podSandboxCount(t, c, "ctrl-1"); got != 4 {
		t.Fatalf("expected 4, got %d", got)
	}
}
