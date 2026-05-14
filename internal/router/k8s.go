package router

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"boxy.dev/boxy/internal/kube"
)

// BuildK8sCache creates a controller-runtime cache scoped to the sandbox namespace.
func BuildK8sCache(scheme *runtime.Scheme, namespace string) (cache.Cache, error) {
	restCfg, err := kube.BuildConfig()
	if err != nil {
		return nil, err
	}
	return cache.New(restCfg, cache.Options{
		Scheme:            scheme,
		DefaultNamespaces: map[string]cache.Config{namespace: {}},
	})
}

// BuildK8sClient creates a controller-runtime client.
func BuildK8sClient(scheme *runtime.Scheme) (client.Client, error) {
	restCfg, err := kube.BuildConfig()
	if err != nil {
		return nil, err
	}
	return client.New(restCfg, client.Options{Scheme: scheme})
}

// BuildClientset creates a typed Kubernetes clientset used for TokenReview calls.
func BuildClientset() (kubernetes.Interface, error) {
	restCfg, err := kube.BuildConfig()
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(restCfg)
}
