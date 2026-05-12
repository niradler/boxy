package kube

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"boxy.dev/boxy/internal/api"
)

func PodRunningReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func ValidatePodForExec(
	ctx context.Context,
	client kubernetes.Interface,
	sessionID, sandboxID string,
	ref *api.PodRef,
) (*corev1.Pod, error) {
	pod, err := client.CoreV1().Pods(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get pod: %w", err)
	}
	if string(pod.UID) != ref.UID {
		return nil, fmt.Errorf("pod uid mismatch: expected %s got %s", ref.UID, pod.UID)
	}
	ls := pod.Labels
	if ls == nil {
		return nil, fmt.Errorf("pod has no labels")
	}
	if v := ls[api.LabelSandboxID]; v != sandboxID {
		return nil, fmt.Errorf("label %s mismatch: expected %q got %q", api.LabelSandboxID, sandboxID, v)
	}
	if v := ls[api.LabelSessionID]; v != sessionID {
		return nil, fmt.Errorf("label %s mismatch: expected %q got %q", api.LabelSessionID, sessionID, v)
	}
	if ls[api.LabelManagedBy] != "boxy" {
		return nil, fmt.Errorf("pod not managed by boxy")
	}
	if !PodRunningReady(pod) {
		return nil, fmt.Errorf("pod not running/ready (phase=%s)", pod.Status.Phase)
	}
	return pod, nil
}

func WorkerHTTPAddr(pod *corev1.Pod, defaultPort int32) (string, error) {
	if pod.Status.PodIP == "" {
		return "", fmt.Errorf("pod has no IP")
	}
	ip := strings.TrimSpace(pod.Status.PodIP)
	if ip == "" {
		return "", fmt.Errorf("pod IP empty")
	}
	port := WorkerListenPort(pod, defaultPort)
	return fmt.Sprintf("http://%s:%d", ip, port), nil
}
