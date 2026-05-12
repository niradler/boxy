package kube

import (
	"strings"

	corev1 "k8s.io/api/core/v1"

	"boxy.dev/boxy/internal/api"
)

func DesiredImage(body *api.SandboxCreateBody, spec SandboxPodSpec) string {
	if body != nil && strings.TrimSpace(body.Image) != "" {
		return strings.TrimSpace(body.Image)
	}
	return spec.WorkerImage
}

func DesiredWorkerPort(body *api.SandboxCreateBody, spec SandboxPodSpec) int32 {
	if body != nil && body.WorkerPort != nil && *body.WorkerPort > 0 {
		return int32(*body.WorkerPort)
	}
	return spec.WorkerPort
}

func DesiredServiceAccount(body *api.SandboxCreateBody, spec SandboxPodSpec) string {
	if body != nil && strings.TrimSpace(body.ServiceAccountName) != "" {
		return strings.TrimSpace(body.ServiceAccountName)
	}
	return spec.ServiceAccount
}

func DesiredImagePullPolicy(body *api.SandboxCreateBody) corev1.PullPolicy {
	if body == nil {
		return corev1.PullIfNotPresent
	}
	switch strings.TrimSpace(body.ImagePullPolicy) {
	case string(corev1.PullAlways):
		return corev1.PullAlways
	case string(corev1.PullNever):
		return corev1.PullNever
	default:
		return corev1.PullIfNotPresent
	}
}

func DesiredPullSecretName(body *api.SandboxCreateBody, spec SandboxPodSpec) string {
	if body != nil && strings.TrimSpace(body.ImagePullSecretName) != "" {
		return strings.TrimSpace(body.ImagePullSecretName)
	}
	return spec.PullSecretName
}

func WorkerListenPort(pod *corev1.Pod, fallback int32) int32 {
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		if c.Name != "worker" {
			continue
		}
		for _, p := range c.Ports {
			if p.Name == "http" || p.ContainerPort > 0 {
				return p.ContainerPort
			}
		}
	}
	return fallback
}
