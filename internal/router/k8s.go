package router

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"boxy.dev/boxy/internal/kube"
)

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

func BuildK8sClient(scheme *runtime.Scheme) (client.Client, error) {
	restCfg, err := kube.BuildConfig()
	if err != nil {
		return nil, err
	}
	return client.New(restCfg, client.Options{Scheme: scheme})
}

func BuildClientset() (kubernetes.Interface, error) {
	restCfg, err := kube.BuildConfig()
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(restCfg)
}
