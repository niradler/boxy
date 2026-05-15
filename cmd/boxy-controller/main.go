package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"boxy.dev/boxy/internal/nsjail"
)

type config struct {
	port               int
	mtlsDisabled       bool
	tlsCertPath        string
	tlsKeyPath         string
	tlsCAPath          string
	controllerToken    string
	maxSandboxes       int
	maxExecConcurrency int
	maxOutputBytes     int
	adapterConfig      nsjail.AdapterConfig
}

func configFromEnv() (*config, error) {
	cfg := &config{
		port:               envInt("BOXY_CONTROLLER_PORT", 8080),
		mtlsDisabled:       envBool("BOXY_MTLS_DISABLED", false),
		tlsCertPath:        envStr("BOXY_TLS_CERT_PATH", "/tls/tls.crt"),
		tlsKeyPath:         envStr("BOXY_TLS_KEY_PATH", "/tls/tls.key"),
		tlsCAPath:          envStr("BOXY_TLS_CA_PATH", "/tls/ca.crt"),
		controllerToken:    envStr("BOXY_CONTROLLER_TOKEN", ""),
		maxSandboxes:       envInt("BOXY_MAX_SANDBOXES", 20),
		maxExecConcurrency: envInt("BOXY_MAX_EXEC_CONCURRENCY", 50),
		maxOutputBytes:     envInt("BOXY_MAX_OUTPUT_BYTES", 6<<20),
		adapterConfig: nsjail.AdapterConfig{
			NsjailPath:     envStr("BOXY_NSJAIL_PATH", "/usr/sbin/nsjail"),
			DefaultRootfs:  envStr("BOXY_NSJAIL_ROOTFS", "/rootfs/ubuntu-24.04"),
			SandboxRoot:    envStr("BOXY_NSJAIL_SANDBOX_ROOT", "/var/lib/boxy/sandboxes"),
			BinariesDir:    envStr("BOXY_NSJAIL_BINARIES_DIR", "/usr/local/bin"),
			MaxOutputBytes: envInt("BOXY_MAX_OUTPUT_BYTES", 6<<20),
		},
	}
	return cfg, nil
}

func main() {
	cfg, err := configFromEnv()
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}

	adapter := nsjail.NewNsjailAdapter(cfg.adapterConfig)
	srv := newServer(cfg, adapter)
	mux := srv.handler()

	httpSrv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	if !cfg.mtlsDisabled {
		tlsCfg, err := buildTLSConfig(cfg.tlsCertPath, cfg.tlsKeyPath, cfg.tlsCAPath)
		if err != nil {
			slog.Error("tls config", "err", err)
			os.Exit(1)
		}
		httpSrv.TLSConfig = tlsCfg
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		if cfg.mtlsDisabled {
			slog.Info("boxy-controller starting (mTLS DISABLED)", "port", cfg.port)
			err = httpSrv.ListenAndServe()
		} else {
			slog.Info("boxy-controller starting (mTLS enabled)", "port", cfg.port)
			err = httpSrv.ListenAndServeTLS("", "")
		}
		if err != nil && err != http.ErrServerClosed {
			slog.Error("http", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}

func buildTLSConfig(certPath, keyPath, caPath string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load server keypair: %w", err)
	}
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read CA cert: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("parse CA cert: no valid certs found")
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

func envStr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	switch strings.TrimSpace(strings.ToLower(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}
