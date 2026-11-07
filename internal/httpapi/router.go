package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/Stash088/agent-task-runner/internal/config"
	"github.com/Stash088/agent-task-runner/internal/runner"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// readinessTimeout bounds the Kubernetes call made by /readyz.
const readinessTimeout = 3 * time.Second

// NewRouter builds the public HTTP surface:
//
//	/mcp      MCP streamable HTTP endpoint (API key required)
//	/healthz  liveness, always 200 while the process is alive
//	/readyz   readiness, 200 only when the Kubernetes API is reachable
func NewRouter(svc *runner.Service, mcpServer *mcp.Server, cfg *config.Config, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthz)
	mux.HandleFunc("/readyz", readyz(svc))

	mcpHandler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return mcpServer },
		&mcp.StreamableHTTPOptions{
			Logger:         logger,
			SessionTimeout: 30 * time.Minute,
		},
	)
	mux.Handle("/mcp", Auth(cfg.APIKey, mcpHandler))
	return mux
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func readyz(svc *runner.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
		defer cancel()
		if err := svc.Ready(ctx); err != nil {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("not ready"))
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
}
