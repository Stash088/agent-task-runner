// Command agent-task-runner is the control-plane service that creates
// Kubernetes Jobs for Hermes agent tasks and exposes them over MCP.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Stash088/agent-task-runner/internal/config"
	"github.com/Stash088/agent-task-runner/internal/httpapi"
	"github.com/Stash088/agent-task-runner/internal/mcpserver"
	"github.com/Stash088/agent-task-runner/internal/runner"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// version is reported through the MCP implementation metadata. It can be set
// at build time with -ldflags "-X main.version=<sha>".
var version = "dev"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	kubeconfig := flag.String("kubeconfig", "", "path to a kubeconfig (default: use in-cluster config)")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	restCfg, err := buildRestConfig(*kubeconfig)
	if err != nil {
		return err
	}
	client, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return err
	}

	svc := runner.NewService(client, nil, cfg, logger)
	mcpServer := mcpserver.New(svc, version, logger)
	handler := httpapi.NewRouter(svc, mcpServer.MCP(), cfg, logger)

	logger.Info("configuration loaded",
		"listen_addr", cfg.ListenAddr,
		"worker_namespace", cfg.WorkerNamespace,
	)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("server starting", "addr", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	select {
	case err := <-serverErr:
		// ListenAndServe failed immediately (e.g. port in use).
		return err
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.ShutdownTimeoutSeconds)*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	logger.Info("server stopped")
	return nil
}

// buildRestConfig prefers the in-cluster service account, matching the
// Deployment, and only falls back to an explicit kubeconfig when requested for
// local development.
func buildRestConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return rest.InClusterConfig()
}
