package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"boxy.dev/boxy/internal/router"
	"boxy.dev/boxy/internal/telemetry"
)

func main() {
	cfg, err := router.ConfigFromEnv()
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}

	scheme := router.NewScheme()

	k8sCache, err := router.BuildK8sCache(scheme, cfg.SandboxNamespace)
	if err != nil {
		slog.Error("k8s cache", "err", err)
		os.Exit(1)
	}

	k8sClient, err := router.BuildK8sClient(scheme)
	if err != nil {
		slog.Error("k8s client", "err", err)
		os.Exit(1)
	}

	cs, err := router.BuildClientset()
	if err != nil {
		slog.Error("k8s clientset", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	tp, err := telemetry.Init(ctx, telemetry.Options{ServiceName: "boxy-router"})
	if err != nil {
		slog.Error("telemetry init", "err", err)
		os.Exit(1)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), telemetry.ShutdownTimeout)
		defer cancel()
		_ = tp.Shutdown(shutdownCtx)
	}()

	go func() {
		if err := k8sCache.Start(ctx); err != nil {
			slog.Error("cache start failed", "err", err)
			stop()
		}
	}()

	if !k8sCache.WaitForCacheSync(ctx) {
		slog.Error("cache sync failed")
		os.Exit(1)
	}
	slog.Info("informer cache synced")

	srv := router.NewServer(ctx, *cfg, k8sClient, k8sCache, cs, tp)
	mux := srv.Handler()
	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	startupCtx, cancelStartup := context.WithTimeout(ctx, 30*time.Second)
	if err := srv.EnsureDefaultSandbox(startupCtx); err != nil {
		slog.Warn("default sandbox creation on startup failed (will retry lazily)", "err", err)
	}
	cancelStartup()

	go func() {
		slog.Info("listening", "addr", cfg.ListenAddr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("http", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}
