package exec

import (
	"context"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

type PodExec struct {
	Config    *rest.Config
	Clientset kubernetes.Interface
	Namespace string
	Pod       string
	Container string
	Command   []string
	Stdin     io.Reader
	Stdout    io.Writer
	Stderr    io.Writer
	TTY       bool
}

func (p *PodExec) Run(ctx context.Context) error {
	if p.Clientset == nil {
		return fmt.Errorf("clientset is nil")
	}
	if p.Container == "" {
		p.Container = "worker"
	}
	req := p.Clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(p.Pod).
		Namespace(p.Namespace).
		SubResource("exec")
	req.VersionedParams(&corev1.PodExecOptions{
		Container: p.Container,
		Command:   p.Command,
		Stdin:     p.Stdin != nil,
		Stdout:    true,
		Stderr:    true,
		TTY:       p.TTY,
	}, scheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(p.Config, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("spdy executor: %w", err)
	}
	return executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  p.Stdin,
		Stdout: p.Stdout,
		Stderr: p.Stderr,
		Tty:    p.TTY,
	})
}
