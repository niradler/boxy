package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"boxy.dev/boxy/internal/router"
)

func main() {
	cfg, err := router.ConfigFromEnv()
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}
	srv := router.NewServer(*cfg)
	mux := srv.Handler()
	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	var wg sync.WaitGroup
	srv.StartReaper(ctx, &wg)
	startupCtx, cancelStartup := context.WithTimeout(ctx, 30*time.Second)
	srv.StartupSync(startupCtx)
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
	wg.Wait()
}
