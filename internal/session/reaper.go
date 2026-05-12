package session

import (
	"context"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"

	"boxy.dev/boxy/internal/api"
)

func ShouldReapPod(now time.Time, pod *corev1.Pod) (bool, string) {
	if pod == nil || pod.DeletionTimestamp != nil {
		return false, ""
	}
	ann := pod.Annotations
	if ann == nil {
		return false, ""
	}
	if exp, ok := ann[api.AnnotationExpiresAtRFC3339]; ok {
		t, err := time.Parse(time.RFC3339, exp)
		if err == nil && !now.Before(t) {
			return true, "ttl_expired"
		}
	}
	created := ann[api.AnnotationCreatedAt]
	if created == "" {
		return false, ""
	}
	ct, err := time.Parse(time.RFC3339, created)
	if err != nil {
		return false, ""
	}
	if ml := ann[api.AnnotationMaxLifetimeSec]; ml != "" {
		sec, err := strconv.Atoi(ml)
		if err == nil && sec > 0 {
			if now.Sub(ct) >= time.Duration(sec)*time.Second {
				return true, "max_lifetime_exceeded"
			}
		}
	}
	return false, ""
}

func ReapOnce(ctx context.Context, c kubernetes.Interface, namespace string) (int, error) {
	sel := labels.Set{api.LabelManagedBy: "boxy"}.AsSelector().String()
	pods, err := c.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: sel,
	})
	if err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	deleted := 0
	for i := range pods.Items {
		p := &pods.Items[i]
		if ok, _ := ShouldReapPod(now, p); ok {
			if err := c.CoreV1().Pods(namespace).Delete(ctx, p.Name, metav1.DeleteOptions{}); err == nil {
				deleted++
			}
		}
	}
	return deleted, nil
}
