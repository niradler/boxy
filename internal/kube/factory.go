package kube

import (
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func BuildConfig() (*rest.Config, error) {
	if kube := os.Getenv("KUBECONFIG"); kube != "" {
		return clientcmd.BuildConfigFromFlags("", kube)
	}
	if home, err := os.UserHomeDir(); err == nil {
		def := filepath.Join(home, ".kube", "config")
		if _, err := os.Stat(def); err == nil {
			return clientcmd.BuildConfigFromFlags("", def)
		}
	}
	return rest.InClusterConfig()
}

func NewClientset() (*kubernetes.Clientset, error) {
	cfg, err := BuildConfig()
	if err != nil {
		return nil, fmt.Errorf("kube config: %w", err)
	}
	return kubernetes.NewForConfig(cfg)
}
