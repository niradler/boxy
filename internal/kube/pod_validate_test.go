package kube

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"boxy.dev/boxy/internal/api"
)

func podFixture() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns",
			Name:      "p",
			UID:       "abc",
			Labels: map[string]string{
				api.LabelSandboxID: "sb",
				api.LabelSessionID: "se",
				api.LabelManagedBy: "boxy",
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: "10.0.0.1",
			Conditions: []corev1.PodCondition{{
				Type:   corev1.PodReady,
				Status: corev1.ConditionTrue,
			}},
		},
	}
}

func TestValidatePodForExecOK(t *testing.T) {
	k := fake.NewSimpleClientset(podFixture())
	p, err := ValidatePodForExec(context.Background(), k, "se", "sb", &api.PodRef{Namespace: "ns", Name: "p", UID: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "p" {
		t.Fatal(p.Name)
	}
}

func TestValidatePodForExecUID(t *testing.T) {
	k := fake.NewSimpleClientset(podFixture())
	_, err := ValidatePodForExec(context.Background(), k, "se", "sb", &api.PodRef{Namespace: "ns", Name: "p", UID: "wrong"})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestValidatePodForExecLabels(t *testing.T) {
	k := fake.NewSimpleClientset(podFixture())
	_, err := ValidatePodForExec(context.Background(), k, "se", "other", &api.PodRef{Namespace: "ns", Name: "p", UID: "abc"})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestWorkerHTTPAddr(t *testing.T) {
	addr, err := WorkerHTTPAddr(podFixture(), 8080)
	if err != nil {
		t.Fatal(err)
	}
	if addr != "http://10.0.0.1:8080" {
		t.Fatal(addr)
	}
}
