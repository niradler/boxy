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

	// mTLS — when MTLSDisabled is false the pod mounts MTLSSecretName at /tls.
	MTLSDisabled   bool
	MTLSSecretName string
	VMLogLevel     string
	VMMetricsIntMs int
	VMPullPolicy   string
	LibKrunfwPath  string

	// KVMMode controls how the controller pod accesses /dev/kvm.
	//   "device"   — request "devices.kubevirt.io/kvm: 1" (requires a KVM device plugin / KubeVirt installed)
	//   "hostpath" — bind-mount /dev/kvm via hostPath and run privileged (kind / clusters without a device plugin)
	// Empty defaults to "device".
	KVMMode string
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
	kvmMode := spec.KVMMode
	if kvmMode == "" {
		kvmMode = "device"
	}
	privileged := kvmMode == "hostpath"
	allowPriv := privileged
	drop := corev1.Capability("ALL")

	rustLog := "info"
	if spec.VMLogLevel != "" {
		rustLog = spec.VMLogLevel
	}
	env := []corev1.EnvVar{
		{Name: "BOXY_CONTROLLER_PORT", Value: strconv.Itoa(int(spec.Port))},
		{Name: "BOXY_MAX_SANDBOXES", Value: strconv.Itoa(spec.MaxSandboxes)},
		{Name: "BOXY_MTLS_DISABLED", Value: strconv.FormatBool(spec.MTLSDisabled)},
		{Name: "BOXY_TLS_CERT_PATH", Value: "/tls/tls.crt"},
		{Name: "BOXY_TLS_KEY_PATH", Value: "/tls/tls.key"},
		{Name: "BOXY_TLS_CA_PATH", Value: "/tls/ca.crt"},
		{Name: "RUST_LOG", Value: rustLog},
	}
	if spec.VMLogLevel != "" {
		env = append(env, corev1.EnvVar{Name: "BOXY_VM_LOG_LEVEL", Value: spec.VMLogLevel})
	}
	if spec.VMMetricsIntMs > 0 {
		env = append(env, corev1.EnvVar{Name: "BOXY_VM_METRICS_INTERVAL_MS", Value: strconv.Itoa(spec.VMMetricsIntMs)})
	}
	if spec.VMPullPolicy != "" {
		env = append(env, corev1.EnvVar{Name: "BOXY_VM_PULL_POLICY", Value: spec.VMPullPolicy})
	}
	if spec.LibKrunfwPath != "" {
		env = append(env, corev1.EnvVar{Name: "BOXY_LIBKRUNFW_PATH", Value: spec.LibKrunfwPath})
	}

	var volumeMounts []corev1.VolumeMount
	var volumes []corev1.Volume
	if kvmMode == "hostpath" {
		hpType := corev1.HostPathCharDev
		volumes = append(volumes, corev1.Volume{
			Name: "dev-kvm",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: "/dev/kvm", Type: &hpType},
			},
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name: "dev-kvm", MountPath: "/dev/kvm",
		})
	}
	if !spec.MTLSDisabled && spec.MTLSSecretName != "" {
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name: "tls", MountPath: "/tls", ReadOnly: true,
		})
		volumes = append(volumes, corev1.Volume{
			Name: "tls",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: spec.MTLSSecretName},
			},
		})
	}

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
			Volumes:       volumes,
			Containers: []corev1.Container{
				{
					Name:  "controller",
					Image: spec.ControllerImage,
					Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: spec.Port}},
					Env:   env,
					Resources: corev1.ResourceRequirements{
						Limits: kvmResourceLimits(kvmMode),
					},
					SecurityContext: kvmSecurityContext(kvmMode, &runAsNonRoot, &allowPriv, &privileged, drop),
					VolumeMounts:    volumeMounts,
					ReadinessProbe:  controllerReadinessProbe(spec),
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

// ReapControllerPods deletes controller pods whose expires-at annotation has passed.
// Returns the number of pods deleted.
func ReapControllerPods(ctx context.Context, c kubernetes.Interface, ns string) (int, error) {
	sel := labels.SelectorFromSet(selectorController())
	list, err := c.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	deleted := 0
	for i := range list.Items {
		p := &list.Items[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		exp := p.Annotations[api.AnnotationExpiresAtRFC3339]
		if exp == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, exp)
		if err != nil {
			continue
		}
		if !now.After(t) {
			continue
		}
		if err := c.CoreV1().Pods(ns).Delete(ctx, p.Name, metav1.DeleteOptions{}); err != nil {
			if !errors.IsNotFound(err) {
				return deleted, err
			}
			continue
		}
		deleted++
	}
	return deleted, nil
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

// WaitForPodIP polls until the pod has an assigned IP or the timeout elapses.
// Returns the updated pod on success.
func WaitForPodIP(ctx context.Context, c kubernetes.Interface, ns, podName string, timeout time.Duration) (*corev1.Pod, error) {
	deadline := time.Now().Add(timeout)
	for {
		pod, err := c.CoreV1().Pods(ns).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		if pod.Status.PodIP != "" {
			return pod, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for pod IP after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// ErrControllerFull signals to the caller to pick or create a different controller.
var ErrControllerFull = fmt.Errorf("controller pod is at capacity")

// ClaimSandboxSlot atomically reserves one slot if count < maxSandboxes,
// else returns ErrControllerFull. Retries on RV conflict.
func ClaimSandboxSlot(ctx context.Context, c kubernetes.Interface, ns, podName string, maxSandboxes int) error {
	const maxRetries = 5
	for range maxRetries {
		pod, err := c.CoreV1().Pods(ns).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		n, _ := strconv.Atoi(pod.Annotations[api.AnnotationSandboxCount])
		if n >= maxSandboxes {
			return ErrControllerFull
		}
		pod.Annotations[api.AnnotationSandboxCount] = strconv.Itoa(n + 1)
		_, err = c.CoreV1().Pods(ns).Update(ctx, pod, metav1.UpdateOptions{})
		if err == nil {
			return nil
		}
		if !errors.IsConflict(err) {
			return err
		}
	}
	return fmt.Errorf("claim sandbox slot conflict after %d retries", maxRetries)
}

// IncrementSandboxCount atomically bumps the sandbox-count annotation on a controller pod.
// Uses optimistic concurrency: retries on ResourceVersion conflict so two concurrent
// callers don't lose updates and oversubscribe a controller's MaxSandboxes budget.
func IncrementSandboxCount(ctx context.Context, c kubernetes.Interface, ns, podName string, delta int) error {
	const maxRetries = 5
	for range maxRetries {
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
		if err == nil {
			return nil
		}
		if !errors.IsConflict(err) {
			return err
		}
	}
	return fmt.Errorf("sandbox count update conflict after %d retries", maxRetries)
}

// kvmResourceLimits returns container resource limits based on KVM access mode.
// In "device" mode (production with a KVM device plugin installed) the controller
// requests one "devices.kubevirt.io/kvm" unit so kubelet wires /dev/kvm with the
// right cgroup. In "hostpath" mode access is via a hostPath volume + privileged
// container, so no special resource is required.
func kvmResourceLimits(mode string) corev1.ResourceList {
	if mode == "hostpath" {
		return nil
	}
	return corev1.ResourceList{
		"devices.kubevirt.io/kvm": resource.MustParse("1"),
	}
}

func kvmSecurityContext(mode string, runAsNonRoot, allowPriv, privileged *bool, drop corev1.Capability) *corev1.SecurityContext {
	sc := &corev1.SecurityContext{
		AllowPrivilegeEscalation: allowPriv,
		RunAsNonRoot:             runAsNonRoot,
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{drop}},
	}
	if mode == "hostpath" {
		sc.Privileged = privileged
	}
	return sc
}

func boolPtr(b bool) *bool { return &b }

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

func controllerReadinessProbe(spec ControllerPodSpec) *corev1.Probe {
	base := corev1.Probe{
		InitialDelaySeconds: 2,
		PeriodSeconds:       3,
		FailureThreshold:    15,
	}
	if spec.MTLSDisabled {
		base.ProbeHandler = corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path:   "/healthz",
				Port:   intstr.FromInt32(spec.Port),
				Scheme: corev1.URISchemeHTTP,
			},
		}
	} else {
		base.ProbeHandler = corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{
				Port: intstr.FromInt32(spec.Port),
			},
		}
	}
	return &base
}
