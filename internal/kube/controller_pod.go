package kube

import (
	"context"
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"

	"boxy.dev/boxy/internal/api"
)

type ControllerPodSpec struct {
	Namespace       string
	ControllerImage string
	MaxSandboxes    int
	Port            int32
	TTLSeconds      int
	PullSecretName  string
	ServiceAccount  string
}

func selectorController() labels.Set {
	return labels.Set{api.LabelManagedBy: "boxy", api.LabelControllerPod: "true"}
}

// SelectOrCreateControllerPod finds a controller pod with available sandbox capacity,
// or creates a new one. Controller pods carry a TTL annotation — if the router dies
// and stops refreshing them, the reaper will clean them up.
func SelectOrCreateControllerPod(ctx context.Context, c kubernetes.Interface, spec ControllerPodSpec) (*corev1.Pod, error) {
	sel := labels.SelectorFromSet(selectorController())
	list, err := c.CoreV1().Pods(spec.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return nil, err
	}
	for i := range list.Items {
		p := &list.Items[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		if p.Status.Phase != corev1.PodRunning {
			continue
		}
		count := sandboxCount(p)
		if count < spec.MaxSandboxes {
			return p, nil
		}
	}
	return createControllerPod(ctx, c, spec)
}

func sandboxCount(p *corev1.Pod) int {
	if p.Annotations == nil {
		return 0
	}
	n, _ := strconv.Atoi(p.Annotations[api.AnnotationSandboxCount])
	return n
}

func createControllerPod(ctx context.Context, c kubernetes.Interface, spec ControllerPodSpec) (*corev1.Pod, error) {
	name := fmt.Sprintf("boxy-ctrl-%d", time.Now().UnixNano())
	if len(name) > 63 {
		name = name[:63]
	}
	now := time.Now().UTC()
	expiresAt := now.Add(time.Duration(spec.TTLSeconds) * time.Second)

	runAsNonRoot := false // controller runs as root to access /dev/kvm
	allowPriv := false
	drop := corev1.Capability("ALL")

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: spec.Namespace,
			Labels: map[string]string{
				api.LabelManagedBy:     "boxy",
				api.LabelControllerPod: "true",
			},
			Annotations: map[string]string{
				api.AnnotationCreatedAt:        now.Format(time.RFC3339),
				api.AnnotationExpiresAtRFC3339: expiresAt.Format(time.RFC3339),
				api.AnnotationSandboxCount:     "0",
				api.AnnotationControllerPort:   strconv.Itoa(int(spec.Port)),
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{
				{
					Name:  "controller",
					Image: spec.ControllerImage,
					Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: spec.Port}},
					Env: []corev1.EnvVar{
						{Name: "BOXY_CONTROLLER_PORT", Value: strconv.Itoa(int(spec.Port))},
						{Name: "BOXY_MAX_SANDBOXES", Value: strconv.Itoa(spec.MaxSandboxes)},
					},
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							"devices.kubevirt.io/kvm": resource.MustParse("1"),
						},
					},
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: &allowPriv,
						RunAsNonRoot:             &runAsNonRoot,
						Capabilities: &corev1.Capabilities{
							Drop: []corev1.Capability{drop},
						},
					},
					ReadinessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{
								Path: "/healthz",
								Port: intstr.FromInt32(spec.Port),
							},
						},
						InitialDelaySeconds: 2,
						PeriodSeconds:       3,
						FailureThreshold:    15,
					},
				},
			},
		},
	}

	if spec.ServiceAccount != "" {
		pod.Spec.ServiceAccountName = spec.ServiceAccount
		pod.Spec.AutomountServiceAccountToken = boolPtr(false)
	}
	if spec.PullSecretName != "" {
		pod.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: spec.PullSecretName}}
	}

	created, err := c.CoreV1().Pods(spec.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		if errors.IsAlreadyExists(err) {
			return c.CoreV1().Pods(spec.Namespace).Get(ctx, name, metav1.GetOptions{})
		}
		return nil, err
	}
	return created, nil
}

// RefreshControllerTTL extends the controller pod's expiry annotation.
// Call this on every operation that touches the controller so it stays alive.
func RefreshControllerTTL(ctx context.Context, c kubernetes.Interface, ns, podName string, ttlSeconds int) error {
	pod, err := c.CoreV1().Pods(ns).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	exp := time.Now().UTC().Add(time.Duration(ttlSeconds) * time.Second)
	pod.Annotations[api.AnnotationExpiresAtRFC3339] = exp.Format(time.RFC3339)
	_, err = c.CoreV1().Pods(ns).Update(ctx, pod, metav1.UpdateOptions{})
	return err
}

// IncrementSandboxCount atomically bumps the sandbox-count annotation on a controller pod.
func IncrementSandboxCount(ctx context.Context, c kubernetes.Interface, ns, podName string, delta int) error {
	pod, err := c.CoreV1().Pods(ns).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	n, _ := strconv.Atoi(pod.Annotations[api.AnnotationSandboxCount])
	n += delta
	if n < 0 {
		n = 0
	}
	pod.Annotations[api.AnnotationSandboxCount] = strconv.Itoa(n)
	_, err = c.CoreV1().Pods(ns).Update(ctx, pod, metav1.UpdateOptions{})
	return err
}
