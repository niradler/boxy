package kube_test

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"boxy.dev/boxy/internal/api"
	"boxy.dev/boxy/internal/kube"
)

func TestSelectOrCreateControllerPod_CreatesNew(t *testing.T) {
	c := fake.NewSimpleClientset()
	spec := kube.ControllerPodSpec{
		Namespace:       "default",
		ControllerImage: "boxy-controller:latest",
		MaxSandboxes:    10,
		Port:            8080,
		TTLSeconds:      3600,
	}
	pod, err := kube.SelectOrCreateControllerPod(context.Background(), c, spec)
	if err != nil {
		t.Fatal(err)
	}
	if pod == nil {
		t.Fatal("expected pod")
	}
	if pod.Labels[api.LabelManagedBy] != "boxy" {
		t.Fatalf("missing managed-by label")
	}
}

func TestSelectOrCreateControllerPod_ReusesExisting(t *testing.T) {
	c := fake.NewSimpleClientset()
	spec := kube.ControllerPodSpec{
		Namespace:       "default",
		ControllerImage: "boxy-controller:latest",
		MaxSandboxes:    10,
		Port:            8080,
		TTLSeconds:      3600,
	}
	p1, err := kube.SelectOrCreateControllerPod(context.Background(), c, spec)
	if err != nil {
		t.Fatal(err)
	}
	// Fake clientset doesn't set Phase=Running; do it manually so reuse selector
	// considers the pod, and annotate it as having capacity.
	p1.Status.Phase = corev1.PodRunning
	p1.Annotations[api.AnnotationSandboxCount] = "5"
	_, err = c.CoreV1().Pods("default").Update(context.Background(), p1, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p2, err := kube.SelectOrCreateControllerPod(context.Background(), c, spec)
	if err != nil {
		t.Fatal(err)
	}
	if p2.Name != p1.Name {
		t.Fatalf("expected reuse of %s, got %s", p1.Name, p2.Name)
	}
}
