package main

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	boxyv1 "boxy.dev/boxy/api/v1alpha1"
	ctrlclient "boxy.dev/boxy/internal/controller"
	"boxy.dev/boxy/internal/operator"
)

func main() {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(boxyv1.AddToScheme(scheme))

	ctrl.SetLogger(zap.New())

	ns := envStr("BOXY_NAMESPACE", "default")
	stsName := envStr("BOXY_CONTROLLER_STATEFULSET_NAME", "boxy-ctrl")
	headlessSvc := envStr("BOXY_CONTROLLER_HEADLESS_SERVICE", stsName+"-headless")

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		LeaderElection:         true,
		LeaderElectionID:       "boxy-operator-leader",
		LeaderElectionNamespace: ns,
		HealthProbeBindAddress: ":8081",
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{ns: {}},
		},
	})
	if err != nil {
		slog.Error("unable to start manager", "err", err)
		os.Exit(1)
	}

	cc := ctrlclient.NewClient(ctrlclient.ClientConfig{
		MTLSDisabled: envBool("BOXY_MTLS_DISABLED", false),
		CACertPath:   envStr("BOXY_TLS_CA_PATH", "/tls/ca.crt"),
		ClientCert:   envStr("BOXY_TLS_CLIENT_CERT_PATH", "/tls/tls.crt"),
		ClientKey:    envStr("BOXY_TLS_CLIENT_KEY_PATH", "/tls/tls.key"),
	})

	reconciler := operator.NewSandboxReconciler(mgr.GetClient(), cc, operator.ReconcilerConfig{
		Namespace:              ns,
		StatefulSetName:        stsName,
		HeadlessServiceName:    headlessSvc,
		ControllerPort:         int32(envInt("BOXY_CONTROLLER_PORT", 8080)),
		MaxSandboxesPerCtrl:    envInt("BOXY_MAX_SANDBOXES_PER_CONTROLLER", 20),
		MaxControllerReplicas:  int32(envInt("BOXY_MAX_CONTROLLER_REPLICAS", 50)),
		MinControllerReplicas:  int32(envInt("BOXY_MIN_CONTROLLER_REPLICAS", 1)),
		TerminatedRetentionSec: envInt("BOXY_TERMINATED_RETENTION_SECONDS", 3600),
		ScaleDownCooldown:      time.Duration(envInt("BOXY_SCALE_DOWN_COOLDOWN_SECONDS", 300)) * time.Second,
		MTLSDisabled:           envBool("BOXY_MTLS_DISABLED", false),
	})

	if err := reconciler.SetupWithManager(mgr); err != nil {
		slog.Error("unable to create sandbox controller", "err", err)
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		slog.Error("unable to set up health check", "err", err)
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		slog.Error("unable to set up ready check", "err", err)
		os.Exit(1)
	}

	ctx := ctrl.SetupSignalHandler()

	go reconciler.RunScaleDownLoop(ctx)

	slog.Info("starting operator", "namespace", ns, "statefulset", stsName)
	if err := mgr.Start(ctx); err != nil {
		slog.Error("manager exited", "err", err)
		os.Exit(1)
	}
}

func envStr(key, def string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	return v
}

func envInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func envBool(key string, def bool) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	switch v {
	case "1", "true", "yes":
		return true
	case "0", "false", "no":
		return false
	default:
		return def
	}
}
