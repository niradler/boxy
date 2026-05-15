package operator

import (
	"context"
	"fmt"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	boxyv1 "boxy.dev/boxy/api/v1alpha1"
)

func (r *SandboxReconciler) RunScaleDownLoop(ctx context.Context) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.tryScaleDown(ctx); err != nil {
				r.log.Error("scale-down check failed", "err", err)
			}
		}
	}
}

func (r *SandboxReconciler) tryScaleDown(ctx context.Context) error {
	key := types.NamespacedName{
		Namespace: r.cfg.Namespace,
		Name:      r.cfg.StatefulSetName,
	}
	var sts appsv1.StatefulSet
	if err := r.Get(ctx, key, &sts); err != nil {
		return fmt.Errorf("get statefulset: %w", err)
	}
	current := int32(1)
	if sts.Spec.Replicas != nil {
		current = *sts.Spec.Replicas
	}
	if current <= r.cfg.MinControllerReplicas {
		return nil
	}

	var podList corev1.PodList
	if err := r.List(ctx, &podList,
		client.InNamespace(r.cfg.Namespace),
		client.MatchingLabels{"app": "boxy-controller"},
	); err != nil {
		return fmt.Errorf("list controller pods: %w", err)
	}

	var sandboxList boxyv1.SandboxList
	if err := r.List(ctx, &sandboxList, client.InNamespace(r.cfg.Namespace)); err != nil {
		return fmt.Errorf("list sandboxes: %w", err)
	}

	podCounts := map[string]int{}
	for i := range sandboxList.Items {
		s := &sandboxList.Items[i]
		if s.Status.Phase == boxyv1.SandboxPhaseTerminated {
			continue
		}
		if s.Status.ControllerPod != "" {
			podCounts[s.Status.ControllerPod]++
		}
	}

	sort.Slice(podList.Items, func(i, j int) bool {
		return podList.Items[i].Name > podList.Items[j].Name
	})

	for _, p := range podList.Items {
		if current <= r.cfg.MinControllerReplicas {
			break
		}
		if podCounts[p.Name] > 0 {
			continue
		}
		if p.CreationTimestamp.Time.Add(r.cfg.ScaleDownCooldown).After(time.Now()) {
			continue
		}

		desired := current - 1
		sts.Spec.Replicas = &desired
		if err := r.Update(ctx, &sts); err != nil {
			return fmt.Errorf("scale down: %w", err)
		}
		r.log.Info("scaled down controller StatefulSet", "from", current, "to", desired, "removedPod", p.Name)
		current = desired
	}

	return nil
}
