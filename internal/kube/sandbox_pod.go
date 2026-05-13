package kube

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
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

type SandboxPodSpec struct {
	Namespace      string
	WorkerImage    string
	ServiceAccount string
	WorkerPort     int32
	WorkerToken    string
	PullSecretName string
	ResourceCPU    string
	ResourceMemory string
}

func selectorSandbox(sandboxID string) labels.Set {
	return labels.Set{api.LabelSandboxID: sandboxID, api.LabelManagedBy: "boxy"}
}

func EnsureSandboxPod(
	ctx context.Context,
	c kubernetes.Interface,
	body *api.SandboxCreateBody,
	spec SandboxPodSpec,
) (*corev1.Pod, bool, error) {
	sel := labels.SelectorFromSet(selectorSandbox(body.SandboxID))
	list, err := c.CoreV1().Pods(spec.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return nil, false, err
	}
	for i := range list.Items {
		p := &list.Items[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		if p.Labels[api.LabelSessionID] != body.SessionID {
			continue
		}
		if p.Labels[api.LabelOwner] != body.Owner {
			return nil, false, fmt.Errorf("existing sandbox owner mismatch")
		}
		if err := assertExistingCompatible(p, body, spec); err != nil {
			return nil, false, err
		}
		return p, false, nil
	}
	pod, err := buildSandboxPod(body, spec)
	if err != nil {
		return nil, false, err
	}
	created, err := c.CoreV1().Pods(spec.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		if errors.IsAlreadyExists(err) {
			got, gerr := c.CoreV1().Pods(spec.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
			if gerr != nil {
				return nil, false, gerr
			}
			if cerr := assertExistingCompatible(got, body, spec); cerr != nil {
				return nil, false, cerr
			}
			return got, false, nil
		}
		return nil, false, err
	}
	return created, true, nil
}

func podPullSecretName(pod *corev1.Pod) string {
	if len(pod.Spec.ImagePullSecrets) == 0 {
		return ""
	}
	return pod.Spec.ImagePullSecrets[0].Name
}

func assertExistingCompatible(p *corev1.Pod, body *api.SandboxCreateBody, spec SandboxPodSpec) error {
	if len(p.Spec.Containers) == 0 {
		return fmt.Errorf("existing sandbox pod invalid")
	}
	ctr := p.Spec.Containers[0]
	wantImg := DesiredImage(body, spec)
	if ctr.Image != wantImg {
		return fmt.Errorf("existing sandbox image mismatch")
	}
	wantSA := DesiredServiceAccount(body, spec)
	if p.Spec.ServiceAccountName != wantSA {
		return fmt.Errorf("existing sandbox serviceAccount mismatch")
	}
	wantPort := DesiredWorkerPort(body, spec)
	if WorkerListenPort(p, spec.WorkerPort) != wantPort {
		return fmt.Errorf("existing sandbox worker port mismatch")
	}
	wantPull := DesiredPullSecretName(body, spec)
	if podPullSecretName(p) != wantPull {
		return fmt.Errorf("existing sandbox imagePullSecret mismatch")
	}
	wantRes := desiredContainerResources(body, spec)
	if !reflect.DeepEqual(ctr.Resources, wantRes) {
		return fmt.Errorf("existing sandbox resources mismatch")
	}
	return nil
}

func buildSandboxPod(body *api.SandboxCreateBody, spec SandboxPodSpec) (*corev1.Pod, error) {
	name := sanitizeName("boxy-" + body.SandboxID)
	if len(name) > 63 {
		name = name[:63]
	}
	now := time.Now().UTC()
	workerPort := DesiredWorkerPort(body, spec)
	img := DesiredImage(body, spec)
	baseAnn := map[string]string{
		api.AnnotationCreatedAt:        now.Format(time.RFC3339),
		api.AnnotationProvisionedImage: img,
	}
	if body.MaxLifetimeSec > 0 {
		baseAnn[api.AnnotationMaxLifetimeSec] = strconv.Itoa(body.MaxLifetimeSec)
	}
	if body.TTLSeconds > 0 {
		baseAnn[api.AnnotationTTLSeconds] = strconv.Itoa(body.TTLSeconds)
		exp := now.Add(time.Duration(body.TTLSeconds) * time.Second)
		baseAnn[api.AnnotationExpiresAtRFC3339] = exp.Format(time.RFC3339)
	}
	ann := mergeAnnotations(body, baseAnn)
	runAsNonRoot := true
	runAsUser := int64(65532)
	runAsGroup := int64(65532)
	fsReadOnly := true
	sec := corev1.PodSecurityContext{
		RunAsNonRoot: &runAsNonRoot,
		RunAsUser:    &runAsUser,
		RunAsGroup:   &runAsGroup,
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
	allowPrivilegeEscalation := false
	dropAll := corev1.Capability("ALL")
	pullPol := DesiredImagePullPolicy(body)
	container := corev1.Container{
		Name:            "worker",
		Image:           img,
		ImagePullPolicy: pullPol,
		Ports: []corev1.ContainerPort{{
			Name:          "http",
			ContainerPort: workerPort,
		}},
		Env:             mergeWorkerEnv(body, workerPort, spec.WorkerToken),
		Resources:       desiredContainerResources(body, spec),
		SecurityContext: workerSecurityContext(runAsNonRoot, runAsUser, runAsGroup, allowPrivilegeEscalation, fsReadOnly, dropAll),
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: "/healthz",
					Port: intstr.FromInt32(workerPort),
				},
			},
			InitialDelaySeconds: 1,
			PeriodSeconds:       2,
			FailureThreshold:    10,
		},
	}
	podLabels := mergePodLabels(body, map[string]string{
		api.LabelSandboxID:     body.SandboxID,
		api.LabelSessionID:     body.SessionID,
		api.LabelOwner:         body.Owner,
		api.LabelManagedBy:     "boxy",
		api.LabelWorkerRuntime: api.SandboxRuntimeInstrumented,
	})
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   spec.Namespace,
			Labels:      podLabels,
			Annotations: ann,
		},
		Spec: corev1.PodSpec{
			ServiceAccountName:           DesiredServiceAccount(body, spec),
			RestartPolicy:                corev1.RestartPolicyNever,
			SecurityContext:              &sec,
			Containers:                   []corev1.Container{container},
			AutomountServiceAccountToken: boolPtr(false),
		},
	}
	ps := DesiredPullSecretName(body, spec)
	if ps != "" {
		pod.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: ps}}
	}
	container.VolumeMounts = []corev1.VolumeMount{{Name: "tmp", MountPath: "/tmp"}}
	pod.Spec.Volumes = []corev1.Volume{{
		Name:         "tmp",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}}
	pod.Spec.Containers[0] = container
	return pod, nil
}

func workerSecurityContext(runAsNonRoot bool, runAsUser, runAsGroup int64, allowPriv bool, ro bool, dropAll corev1.Capability) *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: &allowPriv,
		ReadOnlyRootFilesystem:   &ro,
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{dropAll},
		},
		RunAsNonRoot: &runAsNonRoot,
		RunAsUser:    &runAsUser,
		RunAsGroup:   &runAsGroup,
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
}

func mergeWorkerEnv(body *api.SandboxCreateBody, workerPort int32, workerToken string) []corev1.EnvVar {
	var keys []string
	if body != nil && body.Env != nil {
		for k := range body.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
	}
	out := make([]corev1.EnvVar, 0, len(keys)+6)
	for _, k := range keys {
		out = append(out, corev1.EnvVar{Name: k, Value: body.Env[k]})
	}
	out = append(out,
		corev1.EnvVar{Name: "BOXY_WORKER_PORT", Value: fmt.Sprintf("%d", workerPort)},
		corev1.EnvVar{Name: "BOXY_WORKER_TOKEN", Value: workerToken},
		corev1.EnvVar{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
		}},
		corev1.EnvVar{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
		}},
		corev1.EnvVar{Name: "POD_UID", ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.uid"},
		}},
	)
	return out
}

func desiredContainerResources(body *api.SandboxCreateBody, spec SandboxPodSpec) corev1.ResourceRequirements {
	out := corev1.ResourceRequirements{}
	cpuReq := spec.ResourceCPU
	cpuLim := spec.ResourceCPU
	memReq := spec.ResourceMemory
	memLim := spec.ResourceMemory
	if body != nil && body.Resources != nil {
		r := body.Resources
		if trim := strings.TrimSpace(r.CPURequest); trim != "" {
			cpuReq = trim
		}
		if trim := strings.TrimSpace(r.CPULimit); trim != "" {
			cpuLim = trim
		}
		if trim := strings.TrimSpace(r.MemoryRequest); trim != "" {
			memReq = trim
		}
		if trim := strings.TrimSpace(r.MemoryLimit); trim != "" {
			memLim = trim
		}
	}
	if cpuReq == "" && cpuLim == "" && memReq == "" && memLim == "" {
		return out
	}
	out.Requests = corev1.ResourceList{}
	out.Limits = corev1.ResourceList{}
	if strings.TrimSpace(cpuReq) != "" {
		out.Requests[corev1.ResourceCPU] = mustParseQuantity(cpuReq)
	}
	if strings.TrimSpace(memReq) != "" {
		out.Requests[corev1.ResourceMemory] = mustParseQuantity(memReq)
	}
	if strings.TrimSpace(cpuLim) != "" {
		out.Limits[corev1.ResourceCPU] = mustParseQuantity(cpuLim)
	} else if strings.TrimSpace(cpuReq) != "" {
		out.Limits[corev1.ResourceCPU] = mustParseQuantity(cpuReq)
	}
	if strings.TrimSpace(memLim) != "" {
		out.Limits[corev1.ResourceMemory] = mustParseQuantity(memLim)
	} else if strings.TrimSpace(memReq) != "" {
		out.Limits[corev1.ResourceMemory] = mustParseQuantity(memReq)
	}
	return out
}

func mergePodLabels(body *api.SandboxCreateBody, base map[string]string) map[string]string {
	out := map[string]string{}
	if body != nil && body.Labels != nil {
		for k, v := range body.Labels {
			if api.ReservedMetadataKey(k) {
				continue
			}
			out[k] = v
		}
	}
	for k, v := range base {
		out[k] = v
	}
	return out
}

func mergeAnnotations(body *api.SandboxCreateBody, base map[string]string) map[string]string {
	out := map[string]string{}
	if body != nil && body.Annotations != nil {
		for k, v := range body.Annotations {
			if api.ReservedMetadataKey(k) {
				continue
			}
			out[k] = v
		}
	}
	for k, v := range base {
		out[k] = v
	}
	return out
}

func boolPtr(b bool) *bool { return &b }

func mustParseQuantity(s string) resource.Quantity {
	return resource.MustParse(s)
}

func sanitizeName(s string) string {
	b := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b = append(b, r)
		case r >= 'A' && r <= 'Z':
			b = append(b, r+('a'-'A'))
		default:
			b = append(b, '-')
		}
	}
	out := string(b)
	for len(out) > 0 && out[0] == '-' {
		out = out[1:]
	}
	if out == "" {
		return "boxy-sandbox"
	}
	return out
}

func GetSandboxByID(ctx context.Context, c kubernetes.Interface, ns, sandboxID string) (*corev1.Pod, error) {
	sel := labels.SelectorFromSet(selectorSandbox(sandboxID))
	list, err := c.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return nil, err
	}
	for i := range list.Items {
		p := &list.Items[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		return p, nil
	}
	return nil, errors.NewNotFound(corev1.Resource("pods"), sandboxID)
}

func GetSandboxPodForSession(ctx context.Context, c kubernetes.Interface, ns, sandboxID, sessionID string) (*corev1.Pod, error) {
	sel := labels.SelectorFromSet(selectorSandbox(sandboxID))
	list, err := c.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return nil, err
	}
	for i := range list.Items {
		p := &list.Items[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		if p.Labels[api.LabelSessionID] != sessionID {
			continue
		}
		return p, nil
	}
	return nil, errors.NewNotFound(corev1.Resource("pods"), sandboxID+"/"+sessionID)
}

func DeleteSandboxByID(ctx context.Context, c kubernetes.Interface, ns, sandboxID string) error {
	sel := labels.SelectorFromSet(selectorSandbox(sandboxID))
	list, err := c.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return err
	}
	var first error
	for i := range list.Items {
		p := &list.Items[i]
		err := c.CoreV1().Pods(ns).Delete(ctx, p.Name, metav1.DeleteOptions{})
		if err != nil && !errors.IsNotFound(err) {
			first = err
		}
	}
	return first
}
