package session

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"boxy.dev/boxy/internal/api"
)

func TestShouldReapPodTTL(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				api.AnnotationExpiresAtRFC3339: "2023-12-31T23:59:59Z",
			},
		},
	}
	if ok, _ := ShouldReapPod(now, p); !ok {
		t.Fatal("expected reap")
	}
}

func TestShouldReapMaxLife(t *testing.T) {
	now := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				api.AnnotationCreatedAt:      "2024-01-01T00:00:00Z",
				api.AnnotationMaxLifetimeSec: "60",
			},
		},
	}
	if ok, _ := ShouldReapPod(now, p); !ok {
		t.Fatal("expected reap")
	}
}

func TestShouldReapNot(t *testing.T) {
	now := time.Date(2024, 1, 1, 1, 0, 0, 0, time.UTC)
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				api.AnnotationCreatedAt:      "2024-01-01T00:00:00Z",
				api.AnnotationMaxLifetimeSec: "7200",
			},
		},
	}
	if ok, _ := ShouldReapPod(now, p); ok {
		t.Fatal("unexpected reap")
	}
}
